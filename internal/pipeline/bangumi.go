package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/library"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/metadata"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/mikan"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// 番剧刮削相关的流水线入口：搜索、详情、一键订阅、定时刷新、封面缓存。
//
// 分层原则：这一层负责「网络 → 数据库 → 调度器」的连线，
// internal/mikan 只管抓，internal/store 只管存，谁也不认识谁。

// SearchBangumi 搜番剧，并把结果落库。
//
// 落库是为了面板：封面、标题、放送时间直接读本地，
// 断网、蜜柑抽风、或者单纯刷新页面都不会再产生请求。
func (p *Pipeline) SearchBangumi(ctx context.Context, keyword string) ([]model.Bangumi, error) {
	list, err := p.mikan.Search(ctx, keyword)
	if err != nil {
		return nil, err
	}
	for i := range list {
		// 搜索卡片没有字幕组，UpsertBangumi 会保留库里已有的详情，不会擦掉。
		if err := p.st.UpsertBangumi(ctx, &list[i]); err != nil {
			p.log.Debug("写入番剧卡片失败", "id", list[i].ID, "err", err)
		}
		// 库里若有更完整的旧数据（总集数、放送时间、字幕组），补回卡片上：
		// 已经刮过的番剧再搜一次就能直接看到集数与放送日。
		if full, err := p.st.GetBangumi(ctx, list[i].ID); err == nil && full != nil {
			fillEmpty(&list[i], full)
		}
	}
	p.warmAhead(ctx, list)
	return list, nil
}

// warmAhead 是搜索后顺手预热的番剧数。
//
// 搜完几乎总会点第一张卡，而蜜柑的番剧页要抓 300KB~1MB（1~10 秒）。
// 搜索一返回就在后台把前几部的详情抓下来，点开就是瞬间出结果；
// 抓的同时还会补上 Bangumi 的简介与总集数，番剧记录一次就齐。
//
// 只在库里的缓存过期时才抓，所以重复搜索不会给站点加压力；
// 全程串行（走 mikan 客户端自己的节流），失败只记日志。
const warmAhead = 2

func (p *Pipeline) warmAhead(ctx context.Context, list []model.Bangumi) {
	if !p.mikan.Enabled() || len(list) == 0 {
		return
	}
	ttl := time.Duration(p.cfg.Meta.CacheH) * time.Hour
	var ids []int64
	for _, b := range list {
		if len(ids) >= warmAhead {
			break
		}
		if b.ID <= 0 {
			continue
		}
		if cached, err := p.st.GetBangumi(ctx, b.ID); err == nil && cached != nil &&
			cached.Summary != "" && len(cached.Subgroups) > 0 && ttl > 0 && time.Since(cached.FetchedAt) < ttl {
			continue // 库里就是新的，不用抓
		}
		ids = append(ids, b.ID)
	}
	if len(ids) == 0 {
		return
	}

	// 同一时间只允许一批预热：连点搜索不该起一堆协程。
	p.warmMu.Lock()
	if p.warming {
		p.warmMu.Unlock()
		return
	}
	p.warming = true
	p.warmMu.Unlock()

	go func() {
		defer func() {
			p.warmMu.Lock()
			p.warming = false
			p.warmMu.Unlock()
		}()
		// 请求 context 在 HTTP handler 返回时就取消了，预热要用自己的。
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		for _, id := range ids {
			if _, err := p.BangumiInfo(wctx, id); err != nil {
				p.log.Debug("预热番剧详情失败", "id", id, "err", err)
				continue
			}
			p.log.Debug("已预热番剧详情", "id", id)
		}
	}()
}

// fillEmpty 只把库里有、卡片上没有的字段补上，绝不反向覆盖新抓到的数据。
func fillEmpty(dst *model.Bangumi, src *model.Bangumi) {
	if dst.CoverPath == "" {
		dst.CoverPath = src.CoverPath
	}
	if dst.AirText == "" {
		dst.AirText = src.AirText
	}
	if dst.Episodes == 0 {
		dst.Episodes = src.Episodes
	}
	if dst.StartAt == "" {
		dst.StartAt = src.StartAt
	}
	if dst.Official == "" {
		dst.Official = src.Official
	}
	if len(dst.Subgroups) == 0 {
		dst.Subgroups = src.Subgroups
	}
	if dst.FetchedAt.IsZero() {
		dst.FetchedAt = src.FetchedAt
	}
}

// BangumiInfo 取番剧详情：库里够新就用库里的，否则抓一次；抓失败退回旧数据。
func (p *Pipeline) BangumiInfo(ctx context.Context, id int64) (*model.Bangumi, error) {
	ttl := time.Duration(p.cfg.Meta.CacheH) * time.Hour
	if b, err := p.st.GetBangumi(ctx, id); err == nil && b != nil && b.Title != "" {
		if ttl <= 0 || time.Since(b.FetchedAt) < ttl {
			return b, nil
		}
	}
	b, err := p.mikan.Detail(ctx, id)
	if err != nil {
		// 外网不通时，旧数据比「什么都没有」有用得多。
		if cached, cerr := p.st.GetBangumi(ctx, id); cerr == nil && cached != nil && cached.Title != "" {
			p.log.Debug("刮削失败，改用库里的番剧信息", "id", id, "err", err)
			return cached, nil
		}
		return nil, err
	}
	p.enrichFromBGM(ctx, b)
	if err := p.st.UpsertBangumi(ctx, b); err != nil {
		p.log.Warn("写入番剧详情失败", "id", id, "err", err)
	}
	// 刮削补齐了简介/评分，顺手把已经入库的那份 tvshow.nfo 对齐。
	p.syncLibraryMeta(ctx, b)
	return b, nil
}

// enrichFromBGM 用 Bangumi 补蜜柑缺的字段：简介、评分、总集数。
//
// 蜜柑的番剧页只给放送日、开播日期和字幕组，没有简介，也不写"总集数"，
// 所以《葬送的芙莉莲》在蜜柑上的集数永远是 0。这三样恰好是媒体库最需要的
// （tvshow.nfo 的 plot、季集数），因此在这里补一次，结果跟着番剧行一起落库，
// 后面的入库、面板都直接读库，不会重复请求外网。
//
// 失败一律忽略：刮削是锦上添花，断了也不该影响订阅和下载。
func (p *Pipeline) enrichFromBGM(ctx context.Context, b *model.Bangumi) {
	if b == nil || !p.meta.Enabled() || b.Title == "" {
		return
	}
	if b.Summary != "" && b.Score > 0 && b.Episodes > 0 {
		return // 齐了就不再问外网
	}
	mi, err := p.meta.Search(ctx, b.Title)
	if err != nil || mi == nil {
		return
	}
	if b.Summary == "" {
		b.Summary = mi.Summary
	}
	if b.Score == 0 {
		b.Score = mi.Score
	}
	if b.Episodes == 0 {
		// Bangumi 的 eps 是官方总集数，比"页面上没有就不写"更有用；
		// 蜜柑自己有值时仍然以蜜柑为准（见上面的 UpsertBangumi 空则不覆盖）。
		b.Episodes = mi.Episodes
	}
	if b.StartAt == "" {
		b.StartAt = mi.Date
	}
	// 蜜柑的封面没抓到（离线订阅过、缓存被清掉）时，把 Bangumi 的封面抓成
	// 同一个封面槽里的文件。这样下游三条路径——入库、番剧信息迟到后的重写、
	// 面板缩略图——全都当它是本地封面，不必各自再判断一次"这张图从哪来"。
	if p.CachedCover(b.ID) == "" && mi.Poster != "" {
		if pth := p.fetchImage(ctx, p.mikan.CoverPath(b.ID, p.coverDir), mi.Poster); pth != "" {
			p.log.Info("蜜柑没有封面，已用 Bangumi 封面补齐", "番剧", b.Title, "路径", pth)
		}
	}
}

// SubscribeRequest 是一次「一键订阅」请求。
type SubscribeRequest struct {
	MikanID     int64    // 知道 id 就直接用，省一次搜索
	Keyword     string   // 否则给名字或链接，交给 Resolve
	SubgroupIDs []int64  // 指定字幕组；为空则按偏好自动挑
	Group       string   // 入库分组目录，默认用番剧名
	Prefer      []string // 字幕组偏好，默认取全局配置
	IntervalMin int      // 轮询间隔，默认 30 分钟
	AutoPick    int      // 自动挑几个字幕组，默认 1
	StartEp     float64  // 只收 >= 该集数的条目；0 表示未指定
	Backfill    bool     // 勾了「连往期一起下」
	// BackfillChosen 表示调用方是否明确表过态 —— 勾选框所在的页面总会带上它。
	// 没表态时后端按番剧是否播完自动决定：对已播完的番，「只跟新的」等于一集都不给。
	BackfillChosen bool
}

// SubscribeResult 是一键订阅的结果。
type SubscribeResult struct {
	Bangumi  model.Bangumi
	Created  []model.Subscription
	Skipped  []string
	Backfill bool // 本次是否从第 1 集拉（含自动判定），面板据此补一句说明
}

// subscribeStartEp 决定一条新订阅的起始集数：0 = 从第 1 集拉，-1 = 首次轮询时
// 从最新一集开始跟。
//
// 「只跟新的」这个默认对正在播的番很合理，但对已经播完的番是个陷阱：
// 这时「最新一集」就是最后一集，等于一集都不给 —— 用户订阅一部完结番，
// 期望的是把这部番拿全，结果一条没下（本项目的第一个真实投诉就是这个）。
// 所以调用方没表态时看番剧状态：已播完就从第 1 集拉，还在播才只跟新的。
func subscribeStartEp(req SubscribeRequest, b *model.Bangumi, now time.Time) float64 {
	if req.StartEp != 0 {
		return req.StartEp // 调用方自己指了集数，听它的
	}
	if req.Backfill {
		return 0
	}
	if !req.BackfillChosen && b.Finished(now) {
		return 0
	}
	return -1
}

// SubscribeBangumi 一键订阅：挑字幕组 → 建订阅 → 落调度 → 立刻拉一次。
//
// 一个字幕组一条订阅（而不是一条订阅多个 RSS），理由：
// 订阅的轮询间隔、开始集数、偏好都是逐条可调的，合在一起就没法单独调；
// 而且哪条源出问题，面板上一眼能看出来是哪个字幕组。
func (p *Pipeline) SubscribeBangumi(ctx context.Context, req SubscribeRequest) (SubscribeResult, error) {
	var res SubscribeResult
	if !p.mikan.Enabled() {
		return res, mikan.ErrDisabled
	}

	// 1. 拿到番剧详情
	var (
		b   *model.Bangumi
		err error
	)
	if req.MikanID > 0 {
		b, err = p.BangumiInfo(ctx, req.MikanID)
	} else {
		b, err = p.mikan.Resolve(ctx, req.Keyword)
	}
	if err != nil {
		return res, err
	}
	if b == nil || b.ID == 0 {
		return res, errors.New("没找到对应的番剧")
	}
	if err := p.st.UpsertBangumi(ctx, b); err != nil {
		p.log.Warn("写入番剧详情失败", "id", b.ID, "err", err)
	}
	res.Bangumi = *b

	// 2. 挑字幕组
	ranked := p.mikan.PickSubgroups(b, req.Prefer)
	if len(ranked) == 0 {
		return res, fmt.Errorf("《%s》在蜜柑上还没有任何字幕组发布，可能还没开播", b.Title)
	}
	var picked []model.Subgroup
	if len(req.SubgroupIDs) > 0 {
		for _, id := range req.SubgroupIDs {
			for _, sg := range ranked {
				if sg.ID == id {
					picked = append(picked, sg)
					break
				}
			}
		}
		if len(picked) == 0 {
			return res, errors.New("选中的字幕组不在这个番剧的字幕组列表里")
		}
	} else {
		n := req.AutoPick
		if n <= 0 {
			n = 1
		}
		if n > len(ranked) {
			n = len(ranked)
		}
		picked = ranked[:n]
	}

	// 3. 建订阅。feed_url 有唯一约束，先查一遍给出人话的提示，而不是抛约束错误。
	existing := make(map[string]bool, 16)
	if subs, err := p.st.ListSubscriptions(ctx); err == nil {
		for _, s := range subs {
			existing[s.FeedURL] = true
		}
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = b.Title
	}
	interval := req.IntervalMin
	if interval <= 0 {
		interval = 30
	}
	prefer := req.Prefer
	if len(prefer) == 0 {
		prefer = p.mikan.Prefer()
	}
	// 起始集数：默认 -1 = 首次轮询时从最新一集开始跟；勾了「连往期一起下」才从第 1 集拉。
	// 番剧已经播完时这个默认会一集都不给，所以没表态时由 subscribeStartEp 自动改判。
	startEp := subscribeStartEp(req, b, time.Now())
	res.Backfill = startEp == 0
	if res.Backfill && !req.Backfill {
		p.log.Info("番剧已播完，本次订阅连往期一起下",
			"番剧", b.Title, "总集数", b.Episodes, "放送开始", b.StartAt)
	}
	names := make([]string, 0, len(picked))
	for _, sg := range picked {
		u := p.mikan.RSSURL(sg)
		if existing[u] {
			res.Skipped = append(res.Skipped, sg.Name+"（已订阅过）")
			continue
		}
		sub := &model.Subscription{
			Name:        fmt.Sprintf("%s [%s]", b.Title, sg.Name),
			FeedURL:     u,
			Group:       group,
			Prefer:      prefer,
			StartEp:     startEp,
			IntervalMin: interval,
			Enabled:     true,
			MikanID:     b.ID,
		}
		id, err := p.st.CreateSubscription(ctx, sub)
		if err != nil {
			res.Skipped = append(res.Skipped, sg.Name+"（"+err.Error()+"）")
			continue
		}
		sub.ID = id
		existing[u] = true
		res.Created = append(res.Created, *sub)
		names = append(names, sg.Name)
	}

	// 4. 封面缓存：订阅时抓一次，之后面板和媒体库都用本地这张。
	//    失败不影响订阅本身——没有海报的番剧照样能下。
	if p.coverDir != "" {
		if _, err := p.mikan.EnsureCover(ctx, b, p.coverDir); err != nil {
			p.log.Debug("缓存封面失败", "番剧", b.Title, "err", err)
		}
	}

	// 5. 立刻铺开调度并拉一次，用户不用等下一个轮询窗口就能看到条目。
	for i := range res.Created {
		p.UpsertSchedule(&res.Created[i])
		p.PollNow(res.Created[i].ID)
	}
	if len(res.Created) > 0 {
		_ = p.st.Log(ctx, "subscribe", fmt.Sprintf("订阅《%s》：%s", b.Title, strings.Join(names, "、")))
	}
	return res, nil
}

// RefreshBangumi 刷新过期的番剧信息（由 Maintain 定期调用）。
//
// 只刷新「有订阅在用」的番剧，而且过了 cache_hours 才动网络：
// 面板上放送时间/集数变了能自动跟上，同时保证空闲时零请求。
func (p *Pipeline) RefreshBangumi(ctx context.Context) {
	if !p.mikan.Enabled() {
		return
	}
	subs, err := p.st.ListSubscriptions(ctx)
	if err != nil {
		return
	}
	ttl := time.Duration(p.cfg.Meta.CacheH) * time.Hour
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	seen := make(map[int64]bool, len(subs))
	for _, sub := range subs {
		id := sub.MikanID
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		old, err := p.st.GetBangumi(ctx, id)
		if err == nil && old != nil && time.Since(old.FetchedAt) < ttl {
			continue
		}
		b, err := p.mikan.Detail(ctx, id)
		if err != nil {
			p.log.Debug("刷新番剧信息失败", "id", id, "err", err)
			continue
		}
		// 顺手补上蜜柑没有的简介/评分（字段已齐时不会产生网络请求）。
		p.enrichFromBGM(ctx, b)
		if err := p.st.UpsertBangumi(ctx, b); err != nil {
			p.log.Debug("刷新番剧信息落库失败", "id", id, "err", err)
			continue
		}
		if p.coverDir != "" {
			_, _ = p.mikan.EnsureCover(ctx, b, p.coverDir)
		}
		p.syncLibraryMeta(ctx, b)
		p.log.Info("番剧信息已刷新", "番剧", b.Title, "集数", b.Episodes, "字幕组", len(b.Subgroups))
	}
}

// Mikan 暴露刮削客户端给面板层用（封面地址、字幕组排序等）。
func (p *Pipeline) Mikan() *mikan.Client { return p.mikan }

// CoverDir 返回封面缓存目录。
func (p *Pipeline) CoverDir() string { return p.coverDir }

// CachedCover 返回已缓存的封面路径；没有缓存时返回空串（不发起下载）。
func (p *Pipeline) CachedCover(id int64) string {
	if p.mikan == nil || p.coverDir == "" || id <= 0 {
		return ""
	}
	pth := p.mikan.CoverPath(id, p.coverDir)
	if st, err := os.Stat(pth); err == nil && st.Size() > 0 {
		return pth
	}
	return ""
}

// EnsureCover 确保封面已缓存，返回本地路径。面板按需调用（首次访问时抓一张）。
func (p *Pipeline) EnsureCover(ctx context.Context, b *model.Bangumi) (string, error) {
	if p.mikan == nil || p.coverDir == "" {
		return "", errors.New("未配置封面缓存目录")
	}
	if pth := p.CachedCover(b.ID); pth != "" {
		return pth, nil
	}
	return p.mikan.EnsureCover(ctx, b, p.coverDir)
}

// bangumiMeta 把番剧行翻译成媒体库元数据（入库与刷新元数据共用同一份翻译）。
func (p *Pipeline) bangumiMeta(b *model.Bangumi) *library.Meta {
	if b == nil || b.Title == "" {
		return nil
	}
	return &library.Meta{
		ID:       strconv.FormatInt(b.ID, 10),
		Title:    b.Title,
		TitleCN:  b.Title,
		Date:     b.StartAt,
		Episodes: b.Episodes,
		Summary:  b.Summary,
		Score:    b.Score,
		Poster:   p.CachedCover(b.ID),
	}
}

// posterLocal 把「只有 Bangumi 有封面」时拿到的那条 https 链接落成本地文件。
//
// 为什么不直接把 URL 交给媒体库：
//   - organizer 只认本地文件（os.Stat），URL 在它眼里就是"文件不存在"，
//     结果是海报没有、NFO 里的 <thumb>poster.jpg</thumb> 还是一条死引用；
//   - Jellyfin/Kodi 会在每次打开媒体库时去联网抓那张图（可能被墙，也可能
//     哪天图挂了），而外接硬盘上的媒体库本该是"断网也能看"的。
//
// 失败一律返回空串：宁可没有海报，也不要让 NFO 指着一个不存在的文件。
func (p *Pipeline) posterLocal(ctx context.Context, id int64, url string) string {
	if url == "" {
		return ""
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return url // 已经是本地路径（蜜柑封面缓存）
	}
	return p.fetchImage(ctx, filepath.Join(p.coverDir, fmt.Sprintf("bgm-%d.jpg", id)), url)
}

// fetchImage 把一张网图抓成本地文件，返回落点；任何一步不对就返回空串。
func (p *Pipeline) fetchImage(ctx context.Context, dst, url string) string {
	if url == "" || dst == "" {
		return ""
	}
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return dst // 抓过一次就不再抓
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		p.log.Warn("抓封面失败", "地址", url, "err", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.log.Warn("抓封面失败", "地址", url, "状态", resp.StatusCode)
		return ""
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		p.log.Warn("封面不是图片，跳过", "地址", url, "类型", ct)
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPosterBytes+1))
	if err != nil || len(data) == 0 || int64(len(data)) > maxPosterBytes {
		p.log.Warn("封面下载不完整，跳过", "地址", url, "字节", len(data), "err", err)
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return ""
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return ""
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return ""
	}
	p.log.Info("封面已缓存", "字节", len(data), "路径", dst)
	return dst
}

// maxPosterBytes 给封面下载封顶：条目图片正常几十 KB，超过这个数说明下错了东西。
const maxPosterBytes = 8 << 20

// syncLibraryMeta 把刮削到的简介/评分/封面回写到「已经入库」的番剧目录。
//
// 为什么需要它：tvshow.nfo 是入库那一刻写的，而简介与评分来自 Bangumi，
// 入库时可能还没回来（外网慢、当时没配、或者番剧是更早订阅的）。
// 结果就是媒体库里躺着一份没有 plot、没有 rating 的 NFO，而用户永远不会
// 为了改一个 NFO 去删文件重下。所以刮削补齐之后主动对齐一次。
//
// 找不到已入库的文件（还没下过任何一集）就直接返回：等第一次入库时自然会写。
func (p *Pipeline) syncLibraryMeta(ctx context.Context, b *model.Bangumi) {
	if p.org == nil {
		return
	}
	m := p.bangumiMeta(b)
	if m == nil {
		return
	}
	path, err := p.st.LibraryPathForSeries(ctx, b.ID, b.Title)
	if err != nil {
		p.log.Debug("查找番剧入库路径失败", "番剧", b.Title, "err", err)
		return
	}
	if path == "" {
		return
	}
	tv, poster, err := p.org.RefreshSeries(path, m)
	if err != nil {
		p.log.Warn("刷新番剧级元数据失败", "番剧", b.Title, "err", err)
		return
	}
	p.log.Info("番剧级元数据已对齐", "番剧", b.Title, "nfo", tv, "海报", poster)
}

// newMetaClient 造 Bangumi 客户端：默认打官方 api.bgm.tv，配了 bgm_base 就打镜像。
func newMetaClient(cfg config.Config, hc *http.Client) *metadata.Client {
	c := metadata.New(cfg.Meta.Enabled, cfg.Meta.BGMToken, hc)
	c.SetBase(cfg.Meta.BGMBase)
	return c
}
