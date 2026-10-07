package feed

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// FireFunc 是到点后要执行的动作，由上层（pipeline）注入。
type FireFunc func(ctx context.Context, subID int64)

// entry 是轮询队列中的一项。
type entry struct {
	id       int64
	interval time.Duration
	next     time.Time
	index    int
}

type queue []*entry

func (q queue) Len() int           { return len(q) }
func (q queue) Less(i, j int) bool { return q[i].next.Before(q[j].next) }
func (q queue) Swap(i, j int)      { q[i], q[j] = q[j], q[i]; q[i].index = i; q[j].index = j }
func (q *queue) Push(x any)        { e := x.(*entry); e.index = len(*q); *q = append(*q, e) }
func (q *queue) Pop() any {
	old := *q
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return e
}

// Scheduler 是单定时器的最小堆调度器。
//
// 为什么不用 cron 类库：50 个订阅在 cron 里就是 50 个常驻定时器，
// 而这里全局只有 1 个 goroutine + 1 个 timer；空闲时 CPU 占用严格为 0，
// 因为整个进程都阻塞在 select 上。
type Scheduler struct {
	mu   sync.Mutex
	q    queue
	at   map[int64]*entry
	wake chan struct{}
	fire FireFunc
}

// NewScheduler 构造调度器；fire 为到点回调。
func NewScheduler(fire FireFunc) *Scheduler {
	return &Scheduler{at: map[int64]*entry{}, wake: make(chan struct{}, 1), fire: fire}
}

// Upsert 新增或更新一个订阅的轮询周期。
// firstDelay 控制首次触发时间：从数据库恢复时铺开 jitter，
// 用户刚添加的订阅则给 0，让面板上立刻能看到抓取结果。
func (s *Scheduler) Upsert(id int64, interval, firstDelay time.Duration) {
	if interval < time.Minute {
		interval = time.Minute
	}
	if firstDelay < 0 {
		firstDelay = 0
	}
	s.mu.Lock()
	if e, ok := s.at[id]; ok {
		e.interval = interval
		heap.Fix(&s.q, e.index)
	} else {
		e := &entry{id: id, interval: interval, next: time.Now().Add(firstDelay)}
		s.at[id] = e
		heap.Push(&s.q, e)
	}
	s.mu.Unlock()
	s.notify()
}

// Remove 移除订阅。
func (s *Scheduler) Remove(id int64) {
	s.mu.Lock()
	if e, ok := s.at[id]; ok {
		heap.Remove(&s.q, e.index)
		delete(s.at, id)
	}
	s.mu.Unlock()
	s.notify()
}

// NextTimes 返回每个订阅的下次轮询时间，面板用它渲染「排期」。
func (s *Scheduler) NextTimes() map[int64]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]time.Time, len(s.at))
	for id, e := range s.at {
		out[id] = e.next
	}
	return out
}

func (s *Scheduler) notify() {
	select {
	case s.wake <- struct{}{}:
	default: // 已经有待处理的唤醒信号，不必重复
	}
}

// Run 阻塞运行调度循环，直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		if len(s.q) == 0 {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		wait := time.Until(s.q[0].next)
		s.mu.Unlock()

		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.wake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}

		s.mu.Lock()
		if len(s.q) == 0 {
			s.mu.Unlock()
			continue
		}
		e := heap.Pop(&s.q).(*entry)
		e.next = time.Now().Add(e.interval)
		heap.Push(&s.q, e)
		fire := s.fire
		id := e.id
		s.mu.Unlock()

		if fire != nil {
			// 真正并发的是 poll worker 池，这里只是不阻塞调度循环本身。
			go fire(ctx, id)
		}
	}
}
