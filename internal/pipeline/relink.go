package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/notify"
)

// 断种自动换链。
//
// 场景：老番的种子常年没人做种，任务永远停在 0% —— 既不报错也不完成，
// 面板上只有一条"下载中"，夜里没人看就一直挂着。这里给它一条出路：
//
//	卡住 / 失败 → 找同一集被择优刷掉的另一个字幕组版本 → 改投它
//
// 两条边界：
//  1. 只有"同一订阅的同一集"能换，绝不去别的番剧里找（换错片比不换更糟）；
//  2. 每集换链次数有上限（默认 2 次），候选池来自 RSS 里已有的条目，
//     所以最坏情况是有限次的收敛，不会来回投递刷爆下载器。
//
// 候选来自「被择优刷掉的条目」这件事本身很有意思：平时它们被规则挡在门外，
// 主链一断，它们就是现成的替补。

// relinkReason 把状态翻译成人话，写进通知与面板。
func relinkReason(st model.TaskState, t model.Task) string {
	if st == model.TaskError {
		return "下载失败"
	}
	if t.Progress <= 0 {
		since := t.ProgressAt
		if since.IsZero() {
			since = t.UpdatedAt
		}
		return fmt.Sprintf("卡在 0%%（%s没动静，多半断种）", humanDur(time.Since(since)))
	}
	return "长时间没有完成"
}

// humanDur 把时长说成人话：3 小时、2 天。
func humanDur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
}

// relinkStuckHours 返回配置里的卡住阈值（已由 config 归一化到 1~72 小时）。
func (p *Pipeline) relinkStuckHours() time.Duration {
	h := p.cfg.Relink.StuckHours
	if h <= 0 {
		h = 6
	}
	return time.Duration(h) * time.Hour
}

// relinkNeeded 判断这个任务是否该换链。
//
// 两种情况：
//   - 已经报错：下载器明确说失败了；
//   - 卡死：还在"下载中"但进度一直是 0，且更新时间早于阈值（断种的典型样子）。
//
// 注意两件不能算的事：
//   - 「进度为 0 但刚投递」不算：新任务还没有 peer 是正常的；
//   - 「用户手动暂停」不算：那是在等一个明确的指令，程序不该跟人抢方向盘。
func (p *Pipeline) relinkNeeded(t model.Task, st model.TaskState) bool {
	if !p.cfg.Relink.Enabled || p.dl == nil {
		return false
	}
	// 库里记着是暂停的（用户按的），就彻底别碰：哪怕下载器还报"进行中"。
	if t.State == model.TaskPaused {
		return false
	}
	switch st {
	case model.TaskError:
		return true
	case model.TaskDownloading, model.TaskQueued:
		if t.Progress > 0 || t.DoneBytes > 0 {
			return false
		}
		// 看的是「进度最后一次变化」而不是「最后一次对账」：
		// 对账每轮都写 UpdatedAt，拿它比会永远算成刚刚。
		since := t.ProgressAt
		if since.IsZero() {
			since = t.UpdatedAt // 老库没回填上时的兜底
		}
		return time.Since(since) > p.relinkStuckHours()
	default:
		return false
	}
}

// relink 尝试为一条失败/卡死的任务改投另一个版本。
func (p *Pipeline) relink(ctx context.Context, t model.Task, why string) {
	item, err := p.st.GetItem(ctx, t.ItemID)
	if err != nil || item == nil {
		p.log.Debug("换链：条目不存在，跳过", "任务", t.ID, "err", err)
		return
	}
	ep := item.Episode
	if ep <= 0 {
		// 老数据可能没写集数，从标题里现推一次。
		ep = matcher.ParseTitle(item.Title).Episode
	}
	if ep <= 0 {
		p.log.Debug("换链：认不出集数，跳过", "任务", t.ID, "标题", item.Title)
		return
	}
	// 加上这次，最多换 MaxPerEp 次。换链过的候选会被标成「断种换链」，
	// 计数就从这里来——进程重启也不丢。
	done, err := p.st.CountRelinks(ctx, item.SubID, ep)
	if err != nil {
		p.log.Debug("换链：统计失败", "err", err)
		return
	}
	if done >= p.cfg.Relink.MaxPerEp {
		p.log.Info("换链：这一集已经换过，不再重试", "订阅", item.SubID, "集", ep, "次数", done)
		return
	}

	alts, err := p.st.AltItemsForEpisode(ctx, item.SubID, ep, item.ID, 5)
	if err != nil {
		p.log.Debug("换链：查候选失败", "err", err)
		return
	}
	if len(alts) == 0 {
		p.log.Info("换链：这一集没有别的版本可换", "标题", item.Title, "集", ep)
		_ = p.st.Log(ctx, "relink", fmt.Sprintf("%s 第 %s 集：没有其它版本可换", item.Title, trimEp(ep)))
		return
	}
	sub, err := p.st.GetSubscription(ctx, item.SubID)
	if err != nil || sub == nil {
		p.log.Debug("换链：订阅不存在", "err", err)
		return
	}

	// 优先挑字幕组偏好命中的那个；候选已按体积倒序，所以「偏好命中里最大的」
	// 就是最想要的替补。用和订阅时同一套匹配规则，避免两处判断不一致。
	best := alts[0]
	if len(sub.Prefer) > 0 {
		for _, cand := range alts {
			if p.mikan.Matches(cand.Fansub, sub.Prefer) {
				best = cand
				break
			}
		}
	}

	if err := p.enqueue(ctx, sub, &best, "断种换链：原版本下不动"); err != nil {
		p.log.Warn("换链投递失败", "标题", best.Title, "err", err)
		return
	}
	_ = p.st.SetItemStatus(ctx, best.ID, model.ItemMatched, "断种换链：原版本下不动")
	_ = p.st.SetItemStatus(ctx, item.ID, model.ItemFailed, "断种换链："+why)

	// 原任务不再顶着"下载中"占着面板：暂停它（保留现场），并在面板里说清楚。
	if t.GID != "" {
		if err := p.dl.Pause(ctx, t.GID); err != nil {
			p.log.Debug("换链：暂停原任务失败", "任务", t.ID, "err", err)
		}
	}
	_ = p.st.UpdateTaskProgress(ctx, t.ID, t.GID, model.TaskError, t.Progress,
		t.TotalBytes, t.DoneBytes, "断种换链：已改投 "+best.Fansub)
	_ = p.st.Log(ctx, "relink", fmt.Sprintf("换链：%s → %s", item.Title, best.Title))
	p.log.Info("已换链", "原", item.Title, "新", best.Title, "原因", why)
	p.notify.Send(ctx, notify.Message{
		Title: "断种换链：" + sub.Name,
		Body: fmt.Sprintf("第 %s 集原版本%s，已改投 %s\n%s",
			trimEp(ep), why, firstNonEmptyStr(best.Fansub, "同集另一个版本"), best.Title),
	})
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
