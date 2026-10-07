// Package mikan 刮蜜柑计划（mikanani.me）的番剧信息：搜索、番剧详情、字幕组及其 RSS。
//
// 为什么以蜜柑为唯一信息源：本项目的每一条订阅链接本来就来自蜜柑，
// 用它当来源，番剧名、封面、字幕组、RSS 天然对齐，不会出现
// 「标题来自 A 站、字幕组来自 B 站、RSS 指向第三个地方」的错位。
// 番剧简介只有 Bangumi 有，蜜柑页面上没有，所以这里不假装能提供。
//
// 三条纪律（小机器常年在线，不能给人家的站点添麻烦）：
//  1. 节流：两次请求之间至少间隔 min_gap_ms，全局串行；
//  2. 缓存：番剧详情默认记 12 小时，搜索按关键词缓存，重复点击不再打网络；
//  3. 熔断：连续失败 3 次就安静 10 分钟，绝不在外网抽风时把面板一起拖死。
package mikan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// ErrNotFound 表示蜜柑上没有这个番剧（id 失效或页面被删）。
var ErrNotFound = errors.New("蜜柑上没有这个番剧")

// ErrDisabled 表示刮削被配置关掉了。
var ErrDisabled = errors.New("番剧刮削未开启（metadata.enabled = false）")

const ua = "iwantwatchanime/0.1 (+https://github.com/YoisakiKnd/IWantWatchAnime)"

// Options 是刮削客户端参数。
type Options struct {
	Enabled  bool
	Base     string
	Prefer   []string
	MinGap   time.Duration
	CacheTTL time.Duration
}

// OptionsFrom 从配置推导参数，调用方一行接线。
func OptionsFrom(m config.Meta) Options {
	return Options{
		Enabled:  m.Enabled && m.Provider != "none",
		Base:     strings.TrimRight(m.MikanBase, "/"),
		Prefer:   m.Prefer,
		MinGap:   time.Duration(m.MinGapMs) * time.Millisecond,
		CacheTTL: time.Duration(m.CacheH) * time.Hour,
	}
}

// Client 是蜜柑刮削客户端。并发安全。
type Client struct {
	base    string
	enabled bool
	prefer  []string
	hc      *http.Client
	log     *slog.Logger
	gap     time.Duration
	ttl     time.Duration

	mu     sync.Mutex
	last   time.Time // 上次请求时刻，用于节流
	fails  int
	until  time.Time // 熔断截止时间
	detail map[int64]cached
	// inflight 让同一部番剧的并发详情请求只抓一次：
	// 后台预热和用户点开可能同时发生，两个请求不该变成两次抓取。
	inflight map[int64]*detailCall
	search   map[string]cached
}

// detailCall 是一次进行中的详情抓取：并发的第二个请求等它出结果。
type detailCall struct {
	done chan struct{}
	b    *model.Bangumi
	err  error
}

type cached struct {
	b     *model.Bangumi
	list  []model.Bangumi
	at    time.Time
	empty bool // 负缓存：查过，确实没有
}

// New 构造客户端。hc 为 nil 时用默认的超时客户端。
func New(opts Options, hc *http.Client, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Base == "" {
		opts.Base = "https://mikanani.me"
	}
	return &Client{
		base:     strings.TrimRight(opts.Base, "/"),
		enabled:  opts.Enabled,
		prefer:   opts.Prefer,
		hc:       hc,
		log:      log,
		gap:      opts.MinGap,
		ttl:      opts.CacheTTL,
		detail:   make(map[int64]cached),
		search:   make(map[string]cached),
		inflight: make(map[int64]*detailCall),
	}
}

// Enabled 报告刮削是否可用（配置关闭时为 false，所有功能优雅退化为报错）。
func (c *Client) Enabled() bool { return c != nil && c.enabled && c.base != "" }

// Base 返回站点根地址。
func (c *Client) Base() string { return c.base }

// Prefer 返回全局字幕组偏好。
func (c *Client) Prefer() []string { return c.prefer }

// AbsURL 把站内相对路径补成绝对地址。
func (c *Client) AbsURL(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "http") {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return c.base + p
}

// CoverURL 返回番剧封面地址。400×560 是海报比例，媒体库和面板共用这一张。
func (c *Client) CoverURL(coverPath string) string {
	if coverPath == "" {
		return ""
	}
	return c.AbsURL(coverPath) + "?width=400&height=560&format=jpg"
}

// ThumbURL 返回面板缩略图地址，比起海报更省流量。
func (c *Client) ThumbURL(coverPath string) string {
	if coverPath == "" {
		return ""
	}
	return c.AbsURL(coverPath) + "?width=200&height=280&format=jpg"
}

// RSSURL 返回字幕组的 RSS 绝对地址。
func (c *Client) RSSURL(sg model.Subgroup) string { return c.AbsURL(sg.RSS) }

// BangumiURL 返回番剧详情页地址。
func (c *Client) BangumiURL(id int64) string {
	return fmt.Sprintf("%s/Home/Bangumi/%d", c.base, id)
}

// ── 网络层 ──────────────────────────────────────────────────────────────

// wait 遵守节流窗口。所有出网请求都先过这里，全局串行。
func (c *Client) wait(ctx context.Context) error {
	c.mu.Lock()
	wait := c.gap - time.Since(c.last)
	c.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	c.last = time.Now()
	c.mu.Unlock()
	return nil
}

// get 发一次 GET，带节流、重试与熔断。返回的 body 由调用方关闭。
func (c *Client) get(ctx context.Context, path string) (io.ReadCloser, error) {
	c.mu.Lock()
	if until := c.until; time.Now().Before(until) {
		c.mu.Unlock()
		return nil, fmt.Errorf("蜜柑连续失败，冷却到 %s", until.Format("15:04:05"))
	}
	c.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			// 退避一次再试：小机器上的网卡偶发超时很常见。
			t := time.NewTimer(800 * time.Millisecond)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			}
			t.Stop()
		}
		if err := c.wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			// 404 是有效答复（番剧被删/换 id），不算站点故障，不参与熔断。
			resp.Body.Close()
			c.clearFails()
			return nil, ErrNotFound
		case resp.StatusCode >= 400:
			resp.Body.Close()
			lastErr = fmt.Errorf("蜜柑返回 HTTP %d", resp.StatusCode)
			continue
		default:
			c.clearFails()
			return resp.Body, nil
		}
	}
	c.noteFailure()
	return nil, lastErr
}

func (c *Client) clearFails() {
	c.mu.Lock()
	c.fails = 0
	c.mu.Unlock()
}

func (c *Client) noteFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails++
	if c.fails >= 3 {
		c.until = time.Now().Add(10 * time.Minute)
		c.fails = 0
		c.log.Warn("蜜柑刮削连续失败，暂停 10 分钟", "until", c.until.Format("15:04:05"))
	}
}

// ── 对外功能 ────────────────────────────────────────────────────────────

// Search 按关键词搜番剧，返回番剧卡片（不含字幕组，字幕组要再看详情）。
func (c *Client) Search(ctx context.Context, keyword string) ([]model.Bangumi, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	key := strings.ToLower(keyword)
	c.mu.Lock()
	if e, ok := c.search[key]; ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		return e.list, nil
	}
	c.mu.Unlock()

	body, err := c.get(ctx, "/Home/Search?searchstr="+url.QueryEscape(keyword))
	if err != nil {
		return nil, err
	}
	defer body.Close()
	list, err := scanSearch(body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.search[key] = cached{list: list, at: time.Now()}
	// 缓存表别无限长：家用规模下几十条，超过 256 条就整体清掉，简单可靠。
	if len(c.search) > 256 {
		c.search = map[string]cached{key: {list: list, at: time.Now()}}
	}
	c.mu.Unlock()
	return list, nil
}

// Detail 取番剧详情（含字幕组与各自的 RSS）。
func (c *Client) Detail(ctx context.Context, id int64) (*model.Bangumi, error) {
	if id <= 0 {
		return nil, errors.New("番剧 id 无效")
	}
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	c.mu.Lock()
	if e, ok := c.detail[id]; ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		if e.empty {
			return nil, ErrNotFound
		}
		return e.b, nil
	}
	if call, ok := c.inflight[id]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-call.done:
			return call.b, call.err
		}
	}
	// 抢占：谁先到谁抓，后来的等结果。
	call := &detailCall{done: make(chan struct{})}
	c.inflight[id] = call
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, id)
		c.mu.Unlock()
		close(call.done)
	}()

	body, err := c.get(ctx, "/Home/Bangumi/"+strconv.FormatInt(id, 10))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.mu.Lock()
			c.detail[id] = cached{empty: true, at: time.Now()}
			c.mu.Unlock()
		}
		call.err = err
		return nil, err
	}
	defer body.Close()
	b, err := scanDetail(body)
	if err != nil {
		call.err = err
		return nil, err
	}
	if b.ID == 0 {
		b.ID = id
	}
	if b.Title == "" && len(b.Subgroups) == 0 {
		// 页面结构变了或者 id 失效。别把空壳缓存下来，也别让面板显示空卡片。
		call.err = fmt.Errorf("蜜柑番剧页 %d 没解析出内容（可能站点改版）", id)
		return nil, call.err
	}
	b.FetchedAt = time.Now()
	call.b = b
	c.mu.Lock()
	c.detail[id] = cached{b: b, at: time.Now()}
	c.mu.Unlock()
	return b, nil
}

// Resolve 支持「贴链接 / 贴 id / 打名字」三种输入，返回最匹配的番剧详情。
//
// 这是面板一键订阅的入口：用户从任何地方复制一段东西过来都能用。
func (c *Client) Resolve(ctx context.Context, input string) (*model.Bangumi, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return nil, errors.New("请输入番剧名或蜜柑番剧链接")
	}
	// 直接给 id 或链接：跳过搜索，少一次请求。
	if id := bangumiID(raw); id != 0 {
		return c.Detail(ctx, id)
	}
	if id := parseInt(raw); id > 0 {
		return c.Detail(ctx, id)
	}
	list, err := c.Search(ctx, raw)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("蜜柑上没搜到「%s」", raw)
	}
	// 名字完全一致的优先，否则取第一个（蜜柑的相关度排序本来就够用）。
	for _, b := range list {
		if strings.EqualFold(strings.TrimSpace(b.Title), raw) {
			return c.Detail(ctx, b.ID)
		}
	}
	return c.Detail(ctx, list[0].ID)
}

// PickSubgroups 按偏好给字幕组排序。
//
// 语义和订阅规则里的 prefer 完全一致：命中的越靠前分越高，
// 没命中的排在最后并保持蜜柑原有的顺序（通常是活跃度顺序）。
func (c *Client) PickSubgroups(b *model.Bangumi, prefer []string) []model.Subgroup {
	if b == nil {
		return nil
	}
	if len(prefer) == 0 {
		prefer = c.prefer
	}
	out := make([]model.Subgroup, len(b.Subgroups))
	copy(out, b.Subgroups)
	sort.SliceStable(out, func(i, j int) bool {
		return rank(out[i].Name, prefer) < rank(out[j].Name, prefer)
	})
	return out
}

// Best 返回排在最前面的字幕组；没有字幕组时 ok=false。
func (c *Client) Best(b *model.Bangumi, prefer []string) (model.Subgroup, bool) {
	list := c.PickSubgroups(b, prefer)
	if len(list) == 0 {
		return model.Subgroup{}, false
	}
	return list[0], true
}

// Matches 报告某个字幕组名字是否命中偏好，面板用它标出「会自动挑中的那个」。
func (c *Client) Matches(name string, prefer []string) bool {
	if len(prefer) == 0 {
		prefer = c.prefer
	}
	return rank(name, prefer) < len(prefer)
}

func rank(name string, prefer []string) int {
	lower := strings.ToLower(name)
	for i, p := range prefer {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(p)) {
			return i
		}
	}
	return len(prefer) + 1000
}

// ── 封面 ────────────────────────────────────────────────────────────────

// EnsureCover 把封面下载到 dir/{id}.jpg 并返回路径。
//
// 同一张图两处用：面板缩略图直接由本进程提供（不热链接人家站点、
// 断网也能看），整理入库时再复制成媒体库的 poster.jpg。
// 已存在就跳过——封面基本不会变，重下纯属浪费。
func (c *Client) EnsureCover(ctx context.Context, b *model.Bangumi, dir string) (string, error) {
	if b == nil || b.ID == 0 || b.CoverPath == "" {
		return "", errors.New("这条番剧没有封面信息")
	}
	if !c.Enabled() {
		return "", ErrDisabled
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, strconv.FormatInt(b.ID, 10)+".jpg")
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return dst, nil
	}

	body, err := c.get(ctx, coverPathWithQuery(b.CoverPath))
	if err != nil {
		return "", err
	}
	defer body.Close()

	// 先写 .part 再改名：中断或并发都不会留下半张图，
	// 而「文件存在就跳过」的逻辑一旦被半张图骗过，就永远是坏的。
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(body, 4<<20))
	cerr := f.Close()
	if err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if n < 512 {
		os.Remove(tmp)
		return "", fmt.Errorf("封面数据过小（%d 字节），疑似被拦截", n)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// CoverPath 返回封面在缓存目录里的路径（不下载，仅用于判断有没有）。
func (c *Client) CoverPath(id int64, dir string) string {
	return filepath.Join(dir, strconv.FormatInt(id, 10)+".jpg")
}

// coverPathWithQuery 拼上 400×560 的缩放参数（海报比例）。
func coverPathWithQuery(p string) string {
	p = cleanCover(p)
	if p == "" {
		return ""
	}
	return p + "?width=400&height=560&format=jpg"
}
