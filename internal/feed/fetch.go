// Package feed 负责把 RSS 拉回来、翻译成统一的条目结构，并按时间轮调度轮询。
package feed

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// UserAgent 用可识别的 UA：多数番剧站会拦截默认 Go UA。
const UserAgent = "Suzu/0.1 (RSS archiver; +https://github.com/YoisakiKnd/IWantWatchAnime)"

// Result 是一次拉取的结果。
type Result struct {
	Items       []model.Item
	ETag        string
	LastMod     string
	NotModified bool
	FeedTitle   string
	Cost        time.Duration
}

// Fetcher 负责单次 HTTP 拉取 + 解析。
//
// 关键优化（针对玩客云）：
//  1. 带 ETag / If-Modified-Since，命中的源直接 304，零解析开销；
//  2. 响应体用 LimitReader 封顶，坏源不会把 1GB 内存吃满；
//  3. 传输层自动 gzip，省的是 USB 网口的带宽。
type Fetcher struct {
	hc       *http.Client
	maxBytes int64
}

// NewFetcher 构造拉取器。
func NewFetcher(hc *http.Client, maxBytes int64) *Fetcher {
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	return &Fetcher{hc: hc, maxBytes: maxBytes}
}

// Fetch 拉取并解析一个 feed。
func (f *Fetcher) Fetch(ctx context.Context, feedURL, etag, lastMod string) (*Result, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, */*;q=0.5")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastMod != "" {
		req.Header.Set("If-Modified-Since", lastMod)
	}

	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		// 复用连接：把剩下的字节丢进 io.Discard，否则 keep-alive 会被断开。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}()

	res := &Result{Cost: time.Since(start)}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		res.NotModified = true
		return res, nil
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	parsed, err := gofeed.NewParser().Parse(io.LimitReader(resp.Body, f.maxBytes))
	if err != nil {
		return nil, fmt.Errorf("解析 feed: %w", err)
	}
	res.ETag = resp.Header.Get("ETag")
	res.LastMod = resp.Header.Get("Last-Modified")
	res.FeedTitle = parsed.Title

	now := time.Now()
	for _, it := range parsed.Items {
		if it == nil {
			continue
		}
		entry := model.Item{
			Title:   strings.TrimSpace(it.Title),
			Link:    strings.TrimSpace(it.Link),
			URI:     pickURI(it),
			GUID:    pickGUID(it),
			PubDate: pickTime(it, now),
			Status:  model.ItemNew,
		}
		if entry.Title == "" {
			entry.Title = entry.GUID
		}
		entry.SizeBytes = pickSize(it)
		res.Items = append(res.Items, entry)
	}
	return res, nil
}

// pickURI 选出真正要交给下载器的东西：优先 .torrent / magnet，
// 其次是任意 enclosure（部分源直接给直链），最后退回详情页。
func pickURI(it *gofeed.Item) string {
	var fallback string
	for _, enc := range it.Enclosures {
		if enc == nil || enc.URL == "" {
			continue
		}
		u := strings.TrimSpace(enc.URL)
		switch {
		case looksTorrent(u):
			return u
		case fallback == "":
			fallback = u
		}
	}
	for _, l := range it.Links {
		if looksTorrent(l) {
			return l
		}
	}
	if looksTorrent(it.Link) {
		return it.Link
	}
	if fallback != "" {
		return fallback
	}
	return it.Link
}

func looksTorrent(u string) bool {
	lu := strings.ToLower(u)
	return strings.HasPrefix(lu, "magnet:") ||
		strings.Contains(lu, ".torrent") ||
		strings.HasPrefix(lu, "thunder:") ||
		strings.HasPrefix(lu, "ed2k:")
}

// pickGUID 保证同一发布在多次轮询里得到同一个指纹。
// 很多番剧源的 guid 是空的或每次变，所以退化为「标题+发布时间」哈希。
func pickGUID(it *gofeed.Item) string {
	if g := strings.TrimSpace(it.GUID); g != "" {
		return g
	}
	base := it.Title
	if it.PublishedParsed != nil {
		base = it.Title + "|" + it.PublishedParsed.UTC().Format(time.RFC3339)
	} else if it.Published != "" {
		base = it.Title + "|" + it.Published
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(base))
	return "h" + strconv.FormatUint(h.Sum64(), 16)
}

func pickTime(it *gofeed.Item, fallback time.Time) time.Time {
	if it.PublishedParsed != nil {
		return *it.PublishedParsed
	}
	if it.UpdatedParsed != nil {
		return *it.UpdatedParsed
	}
	return fallback
}

func pickSize(it *gofeed.Item) int64 {
	for _, enc := range it.Enclosures {
		if enc == nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(enc.Length), 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}
