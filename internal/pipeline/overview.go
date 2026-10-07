package pipeline

import (
	"context"
	"runtime"
	"sort"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// SubView 是订阅 + 它的下次轮询时刻，面板的放送轴直接用这个排序。
type SubView struct {
	model.Subscription
	NextPoll time.Time
	Pending  int // 进行中的任务数

	// 以下来自番剧刮削，订阅带 mikan_id 时才有值。
	Show     string // 番剧名（可能比订阅名更干净）
	AirText  string // 放送日期：星期六
	Episodes int    // 总集数，0 = 未知
	Day      string // 放送日的单字，给面板的「放送日」竖排用：六 / 五 / …
	HasCover bool   // 本地是否已有封面缓存
}

// Overview 是面板需要的全部状态快照。
//
// 一次查询打包所有数据，前端轮询只打一个接口；
// 在 armv7 上这比多次分页查询省得多（SQLite 查询本身微秒级）。
type Overview struct {
	Now          time.Time
	Engine       string
	EngineOK     bool
	ActivePolls  int
	NextPoll     time.Time
	NextPollName string
	Subs         []SubView
	Tasks        []model.Task
	Items        []model.Item
	Library      []model.LibraryEntry
	Events       []model.Event
	Counts       map[string]int
	LibraryTotal int
	ActiveTasks  int
	MemAllocMiB  float64
	Goroutines   int
	Uptime       time.Duration
	Version      string
}

// Version 是构建版本，由 main 注入。
var Version = "dev"

var started = time.Now()

// Overview 汇总当前状态。
func (p *Pipeline) Overview(ctx context.Context) (*Overview, error) {
	ov := &Overview{
		Now:         time.Now(),
		Version:     Version,
		Uptime:      time.Since(started),
		ActivePolls: len(p.sem),
	}
	if p.dl != nil {
		ov.Engine = p.dl.Kind()
		ov.EngineOK = true
	} else {
		ov.Engine = "none"
	}

	subs, err := p.st.ListSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	next := p.sched.NextTimes()

	// 番剧刮削信息整表读进内存（家用规模几十行），面板渲染时零额外查询。
	shows := make(map[int64]model.Bangumi, 64)
	if rows, err := p.st.ListBangumi(ctx, 500); err == nil {
		for _, b := range rows {
			shows[b.ID] = b
		}
	}

	tasks, err := p.st.ListTasks(ctx, 30)
	if err != nil {
		return nil, err
	}
	pendingBySub := map[int64]int{}
	for _, t := range tasks {
		if !t.State.Finished() {
			ov.ActiveTasks++
		}
	}

	for _, s := range subs {
		v := SubView{Subscription: s, NextPoll: next[s.ID]}
		v.Pending = pendingBySub[s.ID]
		// 番剧信息来自刮削表：一次全量读出（家用规模几十行），避免每行一次查询。
		if s.MikanID > 0 {
			if b, ok := shows[s.MikanID]; ok {
				v.Show, v.AirText, v.Episodes = b.Title, b.AirText, b.Episodes
				v.Day = airDay(b.AirText)
				v.HasCover = p.CachedCover(b.ID) != ""
			}
		}
		ov.Subs = append(ov.Subs, v)
	}
	sort.SliceStable(ov.Subs, func(i, j int) bool {
		a, b := ov.Subs[i].NextPoll, ov.Subs[j].NextPoll
		if a.IsZero() {
			return false
		}
		if b.IsZero() {
			return true
		}
		return a.Before(b)
	})
	for _, s := range ov.Subs {
		if !s.NextPoll.IsZero() && (ov.NextPoll.IsZero() || s.NextPoll.Before(ov.NextPoll)) {
			ov.NextPoll, ov.NextPollName = s.NextPoll, s.Name
		}
	}

	ov.Tasks = tasks
	if ov.Items, err = p.st.ListItems(ctx, 40); err != nil {
		return nil, err
	}
	if ov.Library, err = p.st.ListLibrary(ctx, 12); err != nil {
		return nil, err
	}
	if ov.Events, err = p.st.RecentEvents(ctx, 40); err != nil {
		return nil, err
	}
	if ov.Counts, err = p.st.CountItemsByStatus(ctx); err != nil {
		return nil, err
	}
	if ov.LibraryTotal, err = p.st.CountLibrary(ctx); err != nil {
		return nil, err
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ov.MemAllocMiB = float64(ms.Alloc) / (1 << 20)
	ov.Goroutines = runtime.NumGoroutine()
	return ov, nil
}

// airDay 从「星期六」里取出「六」，面板用一个大字竖排显示放送日。
func airDay(air string) string {
	for _, r := range air {
		switch r {
		case '一', '二', '三', '四', '五', '六', '日', '天':
			return string(r)
		}
	}
	return ""
}

// LastSync 返回最近一次轮询时间（面板显示「多久没动静了」）。
func (o *Overview) LastSync() time.Time {
	var last time.Time
	for _, s := range o.Subs {
		if s.LastPollAt.After(last) {
			last = s.LastPollAt
		}
	}
	return last
}
