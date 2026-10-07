// Package metadata 是可选的刮削层：向 Bangumi (bgm.tv) 查番剧的中文名、简介、
// 总集数与评分。
//
// 默认关闭。理由：玩客云常年在外网抖动环境下，刮削失败不该影响下载；
// 打开后也只做一次查询 + 进程内缓存，不落库、不落图（除非显式开启海报下载）。
//
// 蜜柑的番剧页没有简介，也没有"总集数"这一行；这两样只有 Bangumi 有，
// 所以番剧库的详情页和媒体库的 tvshow.nfo 都靠这里补。
package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Info 是刮削结果。
//
// 字段与 api.bgm.tv v0 的响应一一对应，除了 Score：
// v0 把评分放在 rating.score 里，这里摊平到 Score，调用方不用管嵌套。
type Info struct {
	ID       int64  `json:"id"`
	Title    string `json:"name"`
	TitleCN  string `json:"name_cn"`
	Summary  string `json:"summary"`
	Date     string `json:"date"`
	Poster   string `json:"image"`
	Episodes int    `json:"eps"`

	Images struct {
		Large  string `json:"large"`
		Common string `json:"common"`
	} `json:"images"`

	Rating struct {
		Score float64 `json:"score"`
		Total int     `json:"total"`
	} `json:"rating"`

	// Score 是 rating.score 的平铺副本，json:"-" 避免和上面的 Rating 打架。
	Score float64 `json:"-"`
}

// normalize 把海报地址统一收进 Poster。
//
// v0 搜索接口给的是 images.large，早期接口给的是顶层 image；只读顶层字段的话，
// 真机上拿到的永远是空海报 —— 媒体库里就一直没有封面。这里补一次，
// 后面所有消费者（入库、番剧详情页）拿到的都是同一条能用的地址。
func (i *Info) normalize() {
	if i == nil {
		return
	}
	if i.Poster == "" {
		i.Poster = i.Better()
	}
}

// Better 返回更合适的海报地址（大图优先），没有就返回空。
func (i *Info) Better() string {
	if i == nil {
		return ""
	}
	if i.Images.Large != "" {
		return i.Images.Large
	}
	if i.Images.Common != "" {
		return i.Images.Common
	}
	return i.Poster
}

// Name 返回展示名字：中文名优先。
func (i *Info) Name() string {
	if i == nil {
		return ""
	}
	if i.TitleCN != "" {
		return i.TitleCN
	}
	return i.Title
}

// Client 是 Bangumi 客户端（api.bgm.tv v0 搜索接口）。
type Client struct {
	enabled bool
	token   string
	hc      *http.Client
	base    string
	cache   sync.Map // keyword -> *Info
	fails   int
	until   time.Time
	mu      sync.Mutex
}

// New 构造客户端。token 可选：匿名也能搜（实测 2026-10-06 可用），
// 带上 token 只影响配额，所以没 token 也能开刮削。
func New(enabled bool, token string, hc *http.Client) *Client {
	return &Client{enabled: enabled, token: token, hc: hc, base: "https://api.bgm.tv"}
}

// Enabled 报告刮削是否开启。
func (c *Client) Enabled() bool { return c != nil && c.enabled }

// SetBase 换接口地址，测试用。
func (c *Client) SetBase(base string) {
	if c != nil && base != "" {
		c.base = strings.TrimRight(base, "/")
	}
}

const bgmUA = "YoisakiKnd/IWantWatchAnime/0.1 (https://github.com/YoisakiKnd/IWantWatchAnime)"

type bgmSearchResp struct {
	Data []Info `json:"data"`
	// bgm 的搜索接口在限流时会返回 200 + 空 data，只能靠这个字段区分
	// "真没有" 和 "被限流了"，这里不额外处理，交给空结果逻辑。
	Total int `json:"total"`
}

// Search 按番剧名搜索，返回第一条动画条目。
//
// 失败策略：连续失败后熔断 10 分钟。外网站点抽风时保持安静，
// 不要让每一集入库都卡在 8 秒超时上。
func (c *Client) Search(ctx context.Context, keyword string) (*Info, error) {
	if !c.Enabled() {
		return nil, nil
	}
	keyword = CleanKeyword(keyword)
	if keyword == "" {
		return nil, nil
	}
	if v, ok := c.cache.Load(keyword); ok {
		info, _ := v.(*Info)
		return info, nil
	}

	c.mu.Lock()
	if time.Now().Before(c.until) {
		c.mu.Unlock()
		return nil, nil
	}
	c.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"keyword": keyword,
		"filter":  map[string]any{"type": []int{2}}, // 2 = 动画
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/v0/search/subjects", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", bgmUA)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		c.noteFailure()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		c.noteFailure()
		return nil, fmt.Errorf("bgm HTTP %d", resp.StatusCode)
	}
	var out bgmSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.noteFailure()
		return nil, err
	}
	c.mu.Lock()
	c.fails = 0
	c.mu.Unlock()
	if len(out.Data) == 0 {
		c.cache.Store(keyword, (*Info)(nil))
		return nil, nil
	}
	best := out.Data[0]
	best.Score = best.Rating.Score
	best.normalize()
	// name_cn 为空的条目（很多冷门番）退回原名，避免面板上出现空白标题。
	if best.Name() == "" {
		best.Title = keyword
	}
	c.cache.Store(keyword, &best)
	return &best, nil
}

func (c *Client) noteFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails++
	if c.fails >= 3 {
		c.until = time.Now().Add(10 * time.Minute)
		c.fails = 0
	}
}

// CleanKeyword 把发布标题收拾成适合搜索的关键词。
//
// 蜜柑的标题形如「葬送的芙莉莲 / Sousou no Frieren [01-28 修正合集] ...」，
// 直接拿去搜 Bangumi 会命中一堆无关条目，所以只取第一个分隔符之前的部分。
func CleanKeyword(raw string) string {
	s := strings.TrimSpace(raw)
	// 只切「名字后面跟着补充信息」的分隔符；开头的括号（【我推的孩子】）不动，
	// 否则关键词会被切成空串。
	for _, sep := range []string{" / ", "／", " | ", " [", "【", "（", "(", "[", "："} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(strings.TrimRight(s, "/-—"))
	return s
}
