// Package pipeline 把各个部件串成一条流水线：
//
//	调度轮询 → 解析匹配 → 去重 → 投递下载 → 对账 → 整理入库 → 通知
//
// 它是唯一知道全局状态的地方；其他包都只做单一职责的事。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/downloader"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/feed"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/library"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/metadata"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/mikan"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/notify"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// Pipeline 是流水线本体。
type Pipeline struct {
	cfg    config.Config
	st     *store.Store
	dl     downloader.Downloader
	fetch  *feed.Fetcher
	sched  *feed.Scheduler
	org    *library.Organizer
	meta   *metadata.Client
	mikan  *mikan.Client
	notify *notify.Multi
	hc     *http.Client // 抓 Bangumi 封面这类零散请求复用同一个客户端
	log    *slog.Logger

	coverDir string // 封面缓存目录：面板缩略图与媒体库海报共用

	// sem 同时限制并发拉取数量：单核 A5 上 2 个并发已经够用。
	sem chan struct{}

	// 搜索后的番剧详情预热：同一时间只跑一批（见 warmAhead）。
	warmMu  sync.Mutex
	warming bool
}

// New 组装流水线。
func New(cfg config.Config, st *store.Store, dl downloader.Downloader, hc *http.Client, log *slog.Logger) *Pipeline {
	p := &Pipeline{
		cfg:   cfg,
		st:    st,
		dl:    dl,
		hc:    hc,
		fetch: feed.NewFetcher(hc, cfg.Runtime.FeedMaxBytes),
		org:   library.New(cfg.Library, log),
		// meta 只留 Bangumi（番剧简介只有它有），番剧信息主体走 mikan。
		// 匿名也能查 Bangumi（实测可用），token 只影响配额，所以不要求配 token。
		meta:     newMetaClient(cfg, hc),
		mikan:    mikan.New(mikan.OptionsFrom(cfg.Meta), hc, log),
		coverDir: cfg.CoverDir(),
		notify:   notify.New(cfg.Notify, hc, log),
		log:      log,
		sem:      make(chan struct{}, cfg.Runtime.PollWorkers),
	}
	p.sched = feed.NewScheduler(p.fire)
	return p
}

// Bootstrap 载入订阅、铺开调度、确认下载内核可用。
func (p *Pipeline) Bootstrap(ctx context.Context) error {
	subs, err := p.st.ListSubscriptions(ctx)
	if err != nil {
		return err
	}
	if len(subs) == 0 && len(p.cfg.Feeds) > 0 {
		for _, f := range p.cfg.Feeds {
			_, err := p.st.CreateSubscription(ctx, &model.Subscription{
				Name:        f.Name,
				FeedURL:     f.URL,
				Group:       f.Group,
				Include:     f.Include,
				Exclude:     f.Exclude,
				Prefer:      f.Prefer,
				StartEp:     f.StartEp,
				IntervalMin: orInt(f.Interval, 30),
				Enabled:     true,
			})
			if err != nil {
				p.log.Warn("导入种子订阅失败", "name", f.Name, "err", err)
				continue
			}
		}
		subs, err = p.st.ListSubscriptions(ctx)
		if err != nil {
			return err
		}
		p.log.Info("已从配置文件导入种子订阅", "count", len(subs))
	}

	spread := 0
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		p.sched.Upsert(sub.ID, time.Duration(sub.IntervalMin)*time.Minute, p.jitter(sub.IntervalMin, spread))
		spread++
	}
	p.log.Info("调度就绪", "订阅数", len(subs))

	if p.dl != nil {
		if err := p.dl.Ping(ctx); err != nil {
			return fmt.Errorf("%s 内核未就绪: %w（请先启动 aria2c --enable-rpc，或用 engine.kind = \"none\" 只跑匹配调试规则）",
				p.dl.Kind(), err)
		}
		p.log.Info("下载内核就绪", "kind", p.dl.Kind())
	} else {
		p.log.Warn("下载内核已关闭（engine.kind = none），只会匹配不会下载")
	}
	return nil
}

// jitter 把首次轮询在时间轴上摊开：
// 50 个订阅如果同时开火，玩客云会瞬间跑满四个核然后卡死。
func (p *Pipeline) jitter(intervalMin, index int) time.Duration {
	window := time.Duration(intervalMin) * time.Minute
	// 首次只摊开 45 秒：重启/首次部署后要能马上看到结果，
	// 同时又不至于让几十个源在同一秒一起开火。
	if window > 45*time.Second {
		window = 45 * time.Second
	}
	if window <= 0 {
		return time.Duration(index) * 2 * time.Second
	}
	return time.Duration(rand.Int63n(int64(window))) + time.Duration(index)*2*time.Second
}

// Run 阻塞运行：一个调度循环 + 一个对账循环 + 一个维护循环。
func (p *Pipeline) Run(ctx context.Context) {
	go p.sched.Run(ctx)

	reconcile := time.NewTicker(time.Duration(p.cfg.Runtime.ReconcileSec) * time.Second)
	maintain := time.NewTicker(5 * time.Minute)
	defer reconcile.Stop()
	defer maintain.Stop()

	p.Reconcile(ctx) // 启动即对一次账，接管上次退出时没跑完的任务

	for {
		select {
		case <-ctx.Done():
			p.log.Info("流水线停止")
			return
		case <-reconcile.C:
			p.Reconcile(ctx)
		case <-maintain.C:
			p.Maintain(ctx)
		}
	}
}

// fire 是调度器的回调：抢一个并发名额再拉取。
func (p *Pipeline) fire(ctx context.Context, subID int64) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-p.sem }()
	p.PollSubscription(ctx, subID)
}

// PollNow 供面板「立即刷新」使用，不等调度周期。
func (p *Pipeline) PollNow(subID int64) {
	go p.fire(context.Background(), subID)
}

// PollSubscription 是流水线的入口：一次完整的抓取 + 匹配 + 投递。
// newestEp 取一批条目里最新的一集；合集条目按它的结束集算。
// 全是剧场版之类没有集号的条目时返回 0，表示定不了起点。
func newestEp(items []model.Item) float64 {
	var max float64
	for _, it := range items {
		parsed := matcher.ParseTitle(it.Title)
		ep := parsed.Episode
		if parsed.EpisodeTo > ep {
			ep = parsed.EpisodeTo
		}
		if ep > max {
			max = ep
		}
	}
	return max
}

func (p *Pipeline) PollSubscription(ctx context.Context, subID int64) {
	sub, err := p.st.GetSubscription(ctx, subID)
	if err != nil {
		p.log.Error("读取订阅失败", "id", subID, "err", err)
		return
	}
	if sub == nil || !sub.Enabled {
		return
	}

	res, err := p.fetch.Fetch(ctx, sub.FeedURL, sub.ETag, sub.LastMod)
	if err != nil {
		_ = p.st.SetSubscriptionError(ctx, subID, err.Error())
		p.log.Warn("拉取失败", "订阅", sub.Name, "err", err)
		return
	}
	if res.NotModified {
		_ = p.st.Touched(ctx, subID)
		return
	}
	_ = p.st.SetPollResult(ctx, subID, res.ETag, res.LastMod)

	// 新订阅默认「只跟新的」：StartEp 为负数表示还没定起点，
	// 用这一批里最新的一集当起点并写回。这样在番剧库点一下订阅，
	// 不会把整部番的往期一口气拉下来（合集条目也顺带被挡掉）。
	// 想要往期的，订阅时勾上「连往期一起下」，或者事后把起始集数改小。
	if sub.StartEp < 0 {
		if start := newestEp(res.Items); start > 0 {
			sub.StartEp = start
			if err := p.st.UpdateSubscription(ctx, sub); err != nil {
				p.log.Warn("写入起始集数失败", "订阅", sub.Name, "err", err)
			} else {
				p.UpsertSchedule(sub) // 调度里存的是订阅副本，同步一份
				_ = p.st.Log(ctx, "poll", fmt.Sprintf("%s：从第 %g 集开始跟新", sub.Name, start))
				p.log.Info("首次轮询确定起点", "订阅", sub.Name, "起始集", start)
			}
		}
	}

	rule := matcher.Rule{
		Include: sub.Include,
		Exclude: sub.Exclude,
		Prefer:  sub.Prefer,
		StartEp: sub.StartEp,
	}

	type cand struct {
		item   model.Item
		parsed matcher.Parsed
		score  int
		reason string
	}
	// best 做单轮内的「同集择优」：一集同时有 3 个字幕组发布时只投递分最高的。
	best := map[string]cand{}
	rejected := 0

	for _, raw := range res.Items {
		raw.SubID = subID // 条目必须挂回订阅，否则外键约束会直接拒绝写入
		parsed := matcher.ParseTitle(raw.Title)
		raw.Episode, raw.EpisodeTo, raw.Batch, raw.Fansub = parsed.Episode, parsed.EpisodeTo, parsed.Batch, parsed.Fansub
		raw.Status = model.ItemNew

		if strings.TrimSpace(raw.URI) == "" {
			rejected++
			p.recordRejected(ctx, subID, raw, "条目里没有 magnet/torrent 地址")
			continue
		}
		d := rule.Judge(parsed, raw.Title)
		if !d.OK {
			rejected++
			p.recordRejected(ctx, subID, raw, d.Reason)
			continue
		}
		key := episodeKey(parsed)
		cur, exists := best[key]
		if exists && cur.score >= d.Score {
			rejected++
			p.recordRejected(ctx, subID, raw, fmt.Sprintf("同集已有更优版本（%s）", cur.parsed.Fansub))
			continue
		}
		if exists {
			rejected++
			p.recordRejected(ctx, subID, cur.item, fmt.Sprintf("同集已有更优版本（%s）", parsed.Fansub))
		}
		raw.Reason = d.Reason
		best[key] = cand{item: raw, parsed: parsed, score: d.Score, reason: d.Reason}
	}

	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	queued, skipped := 0, 0
	for _, k := range keys {
		c := best[k]
		if taken, err := p.st.EpisodeTaken(ctx, subID, c.parsed.Episode); err == nil && taken {
			skipped++
			p.recordRejected(ctx, subID, c.item, "该集已有任务或已入库")
			continue
		}
		if _, created, err := p.st.InsertItemIfNew(ctx, &c.item); err != nil {
			p.log.Warn("写入条目失败", "title", c.item.Title, "err", err)
			continue
		} else if !created {
			continue // 上一轮已处理过这条
		}
		if p.dl == nil {
			_ = p.st.SetItemStatus(ctx, c.item.ID, model.ItemMatched, "调试模式：不投递下载")
			continue
		}
		if err := p.enqueue(ctx, sub, &c.item, c.reason); err != nil {
			p.log.Warn("投递失败", "title", c.item.Title, "err", err)
			continue
		}
		queued++
	}

	msg := fmt.Sprintf("%s：feed 共 %d 条，投递 %d，过滤 %d，跳过 %d",
		sub.Name, len(res.Items), queued, rejected, skipped)
	p.log.Info("轮询完成", "订阅", sub.Name, "条目", len(res.Items), "投递", queued, "过滤", rejected, "耗时", res.Cost.Round(time.Millisecond))
	_ = p.st.Log(ctx, "poll", msg)

	if queued > 0 {
		p.notify.Send(ctx, notify.Message{
			Title: fmt.Sprintf("已投递 %d 个新任务", queued),
			Body:  msg,
		})
	}
}

// enqueue 把条目交给下载内核并建任务记录。
func (p *Pipeline) enqueue(ctx context.Context, sub *model.Subscription, it *model.Item, reason string) error {
	dir := sub.SavePath
	if dir == "" && p.dl.Kind() == "aria2" {
		dir = p.cfg.Engine.Aria2.Dir
	}
	gid, err := p.dl.Add(ctx, downloader.AddRequest{URI: it.URI, SaveDir: dir, Name: it.Title})
	if err != nil {
		_ = p.st.SetItemStatus(ctx, it.ID, model.ItemFailed, err.Error())
		_ = p.st.Log(ctx, "error", it.Title+" 投递失败："+err.Error())
		return err
	}
	if gid == "" {
		// qBittorrent 对 .torrent 直链不返回句柄，靠「投递前后的任务差集」认领；
		// 同一瞬间投递多条时会认不出来。这种情况必须当场说清楚，
		// 否则用户只会看到一条「缺少 infohash，无法对账」的怪错误。
		msg := "下载内核没有返回任务句柄（qBittorrent 并发投递 .torrent 时可能认领不到），请稍后重试这一条"
		_ = p.st.SetItemStatus(ctx, it.ID, model.ItemFailed, msg)
		_ = p.st.Log(ctx, "error", it.Title+" 投递未认领："+msg)
		p.notify.Send(ctx, notify.Message{Title: "投递未认领", Body: it.Title + "\n" + msg})
		return errors.New(msg)
	}
	if _, err := p.st.CreateTask(ctx, &model.Task{
		ItemID:     it.ID,
		Title:      it.Title,
		Downloader: p.dl.Kind(),
		GID:        gid,
		State:      model.TaskQueued,
		SavePath:   dir,
	}); err != nil {
		return err
	}
	_ = p.st.SetItemStatus(ctx, it.ID, model.ItemMatched, reason)
	_ = p.st.Log(ctx, "download", fmt.Sprintf("投递下载：%s", it.Title))
	return nil
}

// Reconcile 对账：把下载器的真实状态同步进库，并在完成时触发整理入库。
//
// 刻意做成「轮询式对账」而不是「回调/事件」：
// 下载器可能重启、任务可能被外部删掉，只有定期拉取状态才对得上，
// 而且它天然支持进程重启后继续接管。
func (p *Pipeline) Reconcile(ctx context.Context) {
	if p.dl == nil {
		return
	}
	tasks, err := p.st.ListActiveTasks(ctx)
	if err != nil {
		p.log.Error("读取活动任务失败", "err", err)
		return
	}
	for _, t := range tasks {
		st, err := p.dl.Status(ctx, t.GID)
		if err != nil {
			// 单次查询失败不改任务状态：网络抖动不该把任务判死。
			p.log.Debug("查询下载状态失败", "任务", t.ID, "err", err)
			continue
		}
		// aria2 对 .torrent 地址会先下元数据再 followedBy 出真正的任务，这里接手。
		if st.FollowedBy != "" && st.FollowedBy != t.GID {
			p.log.Info("任务已跟进到 BT 主体", "任务", t.ID, "old", t.GID, "new", st.FollowedBy)
			if err := p.st.UpdateTaskProgress(ctx, t.ID, st.FollowedBy, model.TaskQueued, 0, 0, 0, ""); err != nil {
				p.log.Warn("更新任务句柄失败", "err", err)
			}
			continue
		}

		// 暂停是「粘住」的：面板上按了暂停之后，下载器再报"进行中"也不改状态，
		// 直到用户明确点继续。否则下一轮对账（15 秒后）就把暂停按钮的效果抹掉了。
		next := st.State
		if t.State == model.TaskPaused && (st.State == model.TaskDownloading || st.State == model.TaskQueued) {
			next = model.TaskPaused
		}
		// 下完在做种：对「收番」来说这一集已经到手了，必须按完成记账。
		// 两家内核都把「已下完但在做种」报成还在跑（aria2 是 active + seeder，
		// qBittorrent 是 seeding/uploading），以前只有 complete 才入库，
		// 于是 seed_time_min > 0 的 aria2 和默认配置的 qBittorrent 会一直挂在
		// 活动列表里，媒体库里始终没有这一集。
		if next == model.TaskSeeding && (st.Progress >= 99.99 || (st.TotalBytes > 0 && st.DoneBytes >= st.TotalBytes)) {
			p.log.Info("下载已完成（在做种），按完成入库", "任务", t.ID, "进度", st.Progress)
			next = model.TaskCompleted
		}
		if err := p.st.UpdateTaskProgress(ctx, t.ID, st.GID, next, st.Progress, st.TotalBytes, st.DoneBytes, st.Error); err != nil {
			p.log.Warn("更新任务进度失败", "err", err)
			continue
		}
		switch next {
		case model.TaskError:
			_ = p.st.SetItemStatus(ctx, t.ItemID, model.ItemFailed, st.Error)
			_ = p.st.Log(ctx, "error", t.Title+" 下载失败："+st.Error)
			p.notify.Send(ctx, notify.Message{Title: "下载失败", Body: t.Title + "\n" + st.Error})
		case model.TaskCompleted:
			p.finish(ctx, t, st)
		}
		// 失败与「卡在 0% 的断种」都走换链：这是老番能不能收全的关键一步。
		// t 是查询前的快照，进度/更新时间以库里的最新值为准。
		if cur, err := p.st.GetTask(ctx, t.ID); err == nil && cur != nil {
			t = *cur
		}
		if p.relinkNeeded(t, next) {
			p.relink(ctx, t, relinkReason(next, t))
		}
	}
}

// finish 整理入库并记录结果。
func (p *Pipeline) finish(ctx context.Context, t model.Task, st downloader.Status) {
	item, err := p.st.GetItem(ctx, t.ItemID)
	if err != nil || item == nil {
		p.log.Warn("任务对应的条目已不存在，改为按任务标题整理", "任务", t.ID)
		item = &model.Item{Title: t.Title}
	}
	parsed := matcher.ParseTitle(item.Title)
	group := ""
	var sub *model.Subscription
	if s, _ := p.st.GetSubscription(ctx, item.SubID); s != nil {
		sub = s
		group = s.Group
	}

	// 刮削信息优先走蜜柑：订阅本来就来自蜜柑，标题/集数/放送时间天然对得上；
	// 封面用本地缓存文件（订阅时抓好的），整理这一步不会再产生任何网络请求。
	var metaInfo *library.Meta
	// 入库记录跟着番剧 id 走（不是标题）：以后要按番剧找媒体库里的文件，
	// 标题会因为刮削来源不同而变，id 不会。
	seriesID := int64(0)
	if sub != nil && sub.MikanID > 0 {
		if b, err := p.st.GetBangumi(ctx, sub.MikanID); err == nil && b != nil {
			metaInfo = p.bangumiMeta(b)
			seriesID = b.ID
		}
	}
	// Bangumi 补蜜柑没有的简介/评分。关键词优先用番剧名（干净），
	// 没有番剧记录时才退回发布标题的前半段。
	keyword := parsed.Title
	if metaInfo != nil && metaInfo.Title != "" {
		keyword = metaInfo.Title
	}
	if p.meta.Enabled() {
		if mi, err := p.meta.Search(ctx, keyword); err == nil && mi != nil {
			if metaInfo == nil {
				metaInfo = &library.Meta{
					ID:      fmt.Sprint(mi.ID),
					Title:   mi.Title,
					TitleCN: mi.Name(),
					Summary: mi.Summary,
					Date:    mi.Date,
					// Bangumi 只给图片 URL：先落成本地文件再交给媒体库，
					// 否则 organizer 只认路径、海报永远落不了盘。
					Poster: p.posterLocal(ctx, mi.ID, mi.Poster),
					Score:  mi.Score,
				}
			} else {
				// 蜜柑有封面、标题、开播日期，Bangumi 有简介、评分、总集数，
				// 各补各缺的，谁都不覆盖谁。
				if metaInfo.Summary == "" {
					metaInfo.Summary = mi.Summary
				}
				if metaInfo.Score == 0 {
					metaInfo.Score = mi.Score
				}
				if metaInfo.Episodes == 0 {
					metaInfo.Episodes = mi.Episodes
				}
				if metaInfo.Poster == "" {
					// 蜜柑那份封面不在（没订过详情页、缓存被清）：用 Bangumi 的顶上。
					metaInfo.Poster = p.posterLocal(ctx, mi.ID, mi.Poster)
				}
			}
		}
	}

	res, err := p.org.Organize(ctx, library.Input{
		TaskID: t.ID,
		Source: st.SavePath,
		Parsed: parsed,
		Meta:   metaInfo,
		Group:  group,
	})
	if err != nil {
		_ = p.st.UpdateTaskProgress(ctx, t.ID, t.GID, model.TaskError, 100, st.TotalBytes, st.DoneBytes, err.Error())
		_ = p.st.SetItemStatus(ctx, t.ItemID, model.ItemFailed, err.Error())
		_ = p.st.Log(ctx, "error", fmt.Sprintf("入库失败：%s（%v）", parsed.Title, err))
		p.notify.Send(ctx, notify.Message{Title: "入库失败", Body: parsed.Title + "\n" + err.Error()})
		return
	}

	entry := &model.LibraryEntry{
		SeriesID: seriesID,
		TaskID:   t.ID,
		Title:    parsed.Title,
		Path:     res.Path,
		Linked:   res.Linked,
		NFOPath:  res.NFOPath,
		Episode:  parsed.Episode,
	}
	if err := p.st.AddLibraryEntry(ctx, entry); err != nil {
		p.log.Warn("写入入库记录失败", "err", err)
	}
	_ = p.st.SetItemStatus(ctx, t.ItemID, model.ItemDone, "")
	mode := "复制"
	if res.Linked {
		mode = "硬链接"
	}
	_ = p.st.Log(ctx, "library", fmt.Sprintf("%s（%s）→ %s", parsed.Title, mode, res.Path))
	p.log.Info("入库完成", "标题", parsed.Title, "方式", mode, "路径", res.Path)
	p.notify.Send(ctx, notify.Message{
		Title: "入库完成：" + parsed.Title,
		Body:  fmt.Sprintf("第 %s 集\n%s\n%s", trimEp(parsed.Episode), mode, res.Path),
	})
}

// Maintain 定期维护：WAL 落盘、裁剪活动流、按需生成快照。
func (p *Pipeline) Maintain(ctx context.Context) {
	if err := p.st.Checkpoint(ctx); err != nil {
		p.log.Debug("WAL 落盘失败", "err", err)
	}
	if keep := p.cfg.Storage.KeepEventsRows; keep > 0 {
		if err := p.st.TrimEvents(ctx, keep); err != nil {
			p.log.Debug("裁剪活动流失败", "err", err)
		}
	}
	if p.cfg.Storage.SnapshotMin > 0 {
		_ = p.st.Snapshot(ctx, p.cfg.SnapshotPath())
	}
	// 番剧信息刷新：内部按 cache_hours 判断，绝大多数轮次是空转。
	p.RefreshBangumi(ctx)
}

// UpsertSchedule 在订阅变更后同步调度器。
func (p *Pipeline) UpsertSchedule(sub *model.Subscription) {
	if sub == nil {
		return
	}
	if !sub.Enabled {
		p.sched.Remove(sub.ID)
		return
	}
	p.sched.Upsert(sub.ID, time.Duration(sub.IntervalMin)*time.Minute, 2*time.Second)
}

// RemoveSchedule 删除订阅后同步调度器。
func (p *Pipeline) RemoveSchedule(id int64) { p.sched.Remove(id) }

// ControlTask 处理面板上的暂停/继续/移除。
func (p *Pipeline) ControlTask(ctx context.Context, taskID int64, action string) error {
	if p.dl == nil {
		return fmt.Errorf("下载内核未启用")
	}
	t, err := p.st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("任务不存在")
	}
	switch action {
	case "pause":
		if err := p.dl.Pause(ctx, t.GID); err != nil {
			return err
		}
		_ = p.st.Log(ctx, "download", "暂停："+t.Title)
		return p.st.UpdateTaskProgress(ctx, t.ID, t.GID, model.TaskPaused, t.Progress, t.TotalBytes, t.DoneBytes, "")
	case "resume":
		if err := p.dl.Resume(ctx, t.GID); err != nil {
			return err
		}
		_ = p.st.Log(ctx, "download", "继续："+t.Title)
		return p.st.UpdateTaskProgress(ctx, t.ID, t.GID, model.TaskDownloading, t.Progress, t.TotalBytes, t.DoneBytes, "")
	case "remove":
		if err := p.dl.Remove(ctx, t.GID); err != nil {
			return err
		}
		_ = p.st.SetItemStatus(ctx, t.ItemID, model.ItemFailed, "已手动移除")
		_ = p.st.Log(ctx, "download", "移除："+t.Title)
		return p.st.UpdateTaskProgress(ctx, t.ID, t.GID, model.TaskError, 0, t.TotalBytes, t.DoneBytes, "已手动移除")
	default:
		return fmt.Errorf("未知动作 %q", action)
	}
}

func (p *Pipeline) recordRejected(ctx context.Context, subID int64, it model.Item, reason string) {
	it.SubID = subID
	it.Status = model.ItemRejected
	it.Reason = reason
	if _, _, err := p.st.InsertItemIfNew(ctx, &it); err != nil {
		p.log.Debug("记录被过滤条目失败", "err", err)
	}
}

// episodeKey 是单轮内「同一集」的归并键；剧场版按标题归并。
func episodeKey(p matcher.Parsed) string {
	if p.Kind != matcher.KindTV {
		return "k:" + p.Title
	}
	return fmt.Sprintf("e:%.2f", p.Episode)
}

func trimEp(ep float64) string {
	s := fmt.Sprintf("%.2f", ep)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
