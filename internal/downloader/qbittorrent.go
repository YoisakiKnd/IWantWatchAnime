package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// QBit 对接 qBittorrent WebAPI v2。
//
// 定位是「可选内核」：已经在内网跑着 qBittorrent 的用户直接接上，
// 不必再装 aria2；没有的话用 aria2 更省资源。
type QBit struct {
	endpoint string
	user     string
	pass     string
	category string
	hc       *http.Client
	base     string

	mu     sync.Mutex
	logged bool
}

// NewQBit 构造 qBittorrent 内核，并挂上独立的 cookie jar 保存 SID。
func NewQBit(endpoint, user, pass, category string, hc *http.Client) *QBit {
	jar, _ := cookiejar.New(nil)
	client := *hc
	client.Jar = jar
	return &QBit{
		endpoint: strings.TrimRight(endpoint, "/"),
		user:     user,
		pass:     pass,
		category: category,
		hc:       &client,
		base:     strings.TrimRight(endpoint, "/") + "/api/v2",
	}
}

// Kind 实现 Downloader。
func (q *QBit) Kind() string { return "qbittorrent" }

// qbTorrent 是 /api/v2/torrents/info 返回的条目（只留用得上的字段）。
type qbTorrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"`
	Size        int64   `json:"size"`
	Completed   int64   `json:"completed"`
	SavePath    string  `json:"save_path"`
	ContentPath string  `json:"content_path"`
}

// list 取任务列表：按「最近添加」倒序，用来反查刚投递的任务。
func (q *QBit) list(ctx context.Context, limit int) ([]qbTorrent, error) {
	if err := q.login(ctx); err != nil {
		return nil, err
	}
	endpoint := q.base + "/torrents/info?filter=all&sort=added_on&reverse=true"
	if limit > 0 {
		endpoint += "&limit=" + strconv.Itoa(limit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", q.endpoint)
	resp, err := q.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("查询任务列表 HTTP %d", resp.StatusCode)
	}
	var out []qbTorrent
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (q *QBit) login(ctx context.Context) error {
	q.mu.Lock()
	if q.logged {
		q.mu.Unlock()
		return nil
	}
	q.mu.Unlock()

	form := url.Values{"username": {q.user}, "password": {q.pass}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.base+"/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", q.endpoint)
	resp, err := q.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 qBittorrent: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if !strings.Contains(string(body), "Ok") {
		return fmt.Errorf("qBittorrent 登录失败：%s", strings.TrimSpace(string(body)))
	}
	q.mu.Lock()
	q.logged = true
	q.mu.Unlock()
	return nil
}

// Ping 登录一次即视为就绪。
func (q *QBit) Ping(ctx context.Context) error { return q.login(ctx) }

// Add 投递任务（url 字段支持 magnet 与 .torrent 直链）。
func (q *QBit) Add(ctx context.Context, req AddRequest) (string, error) {
	if err := q.login(ctx); err != nil {
		return "", err
	}
	// 磁力链自带 btih，就是任务句柄；.torrent 直链没有，只能投递完反查。
	// 蜜柑的订阅全是 .torrent 直链（/Download/xxx.t），所以这条分支是主路，不是补丁。
	var before map[string]bool
	if infoHashFromMagnet(req.URI) == "" {
		before = q.snapshot(ctx)
	}
	form := url.Values{
		"urls":     {req.URI},
		"savepath": {req.SaveDir},
		"paused":   {"false"},
	}
	if q.category != "" {
		form.Set("category", q.category)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, q.base+"/torrents/add",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Referer", q.endpoint)
	resp, err := q.hc.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("投递失败 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// WebAPI 不返回 hash，只能反查：用磁力链里的 btih 最稳，
	// 否则对比投递前后的任务列表，把新冒出来的那个认领回来。
	if h := infoHashFromMagnet(req.URI); h != "" {
		return h, nil
	}
	if h := q.resolveHash(ctx, req.Name, before); h != "" {
		return h, nil
	}
	// 认领不到就返回空：对账时会明确报「缺少 infohash」，
	// 比悄悄把任务挂在错误的 hash 上强。
	return "", nil
}

// snapshot 取当前任务列表的 hash 集合，用于识别「刚投递的那一个」。
func (q *QBit) snapshot(ctx context.Context) map[string]bool {
	list, err := q.list(ctx, 120)
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, t := range list {
		out[strings.ToLower(t.Hash)] = true
	}
	return out
}

// resolveHash 反查刚投递的任务。
//
// 规则：投递前不在列表里、投递后出现的任务里挑一个——
// 优先名字（忽略大小写与空白）与 RSS 标题吻合的，其次只有一个新任务时直接认领。
// 查询带软等待：qBittorrent 落库有几十毫秒延迟，查太早会看到空列表。
func (q *QBit) resolveHash(ctx context.Context, name string, before map[string]bool) string {
	want := normalizeName(name)
	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(time.Duration(120*(attempt+1)) * time.Millisecond):
		}
		list, err := q.list(ctx, 60)
		if err != nil {
			continue
		}
		var fresh []qbTorrent
		for _, t := range list {
			if !before[strings.ToLower(t.Hash)] {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		for _, t := range fresh {
			if want != "" && normalizeName(t.Name) == want {
				return trimGID(t.Hash)
			}
		}
		if len(fresh) == 1 {
			return trimGID(fresh[0].Hash)
		}
		// 多个新任务：名字都还没解析出来（metaDL），这次认领不了，
		// 留空让对账提示，避免张冠李戴。
		return ""
	}
	return ""
}

// normalizeName 把标题归一化，用于「RSS 标题 vs qBittorrent 显示名」的比对。
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case ' ', '\t', '\n', '\r', '_', '-', '.', '　':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Status 查询任务状态并映射到统一状态机。
func (q *QBit) Status(ctx context.Context, gid string) (Status, error) {
	if err := q.login(ctx); err != nil {
		return Status{}, err
	}
	if gid == "" {
		return Status{}, fmt.Errorf("任务缺少 infohash，无法对账")
	}
	endpoint := q.base + "/torrents/info?hashes=" + url.QueryEscape(trimGID(gid))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Status{}, err
	}
	req.Header.Set("Referer", q.endpoint)
	resp, err := q.hc.Do(req)
	if err != nil {
		return Status{}, err
	}
	defer resp.Body.Close()
	var list []qbTorrent
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return Status{}, err
	}
	if len(list) == 0 {
		// 任务被外部删除，标记为错误以便面板提示用户。
		return Status{GID: gid, State: model.TaskError, Error: "qBittorrent 中已不存在该任务"}, nil
	}
	t := list[0]
	out := Status{
		GID:        t.Hash,
		Progress:   clamp100(t.Progress * 100),
		TotalBytes: t.Size,
		DoneBytes:  t.Completed,
		SavePath:   t.ContentPath,
	}
	if out.SavePath == "" {
		out.SavePath = t.SavePath
	}
	switch t.State {
	case "downloading", "metaDL", "forcedDL", "checkingDL", "allocating", "stalledDL":
		out.State = model.TaskDownloading
	case "uploading", "forcedUP", "stalledUP", "checkingUP":
		out.State = model.TaskSeeding
	case "pausedDL", "pausedUP", "stoppedDL", "stoppedUP":
		out.State = model.TaskPaused
	case "queuedDL", "queuedUP", "queuedForChecking", "checkingResumeData", "moving":
		out.State = model.TaskQueued
	case "error", "missingFiles", "unknown":
		out.State = model.TaskError
		out.Error = "qBittorrent 状态: " + t.State
	case "completed":
		out.State = model.TaskCompleted
		out.Progress = 100
	default:
		out.State = model.TaskDownloading
	}
	return out, nil
}

// Pause 暂停（WebAPI 5.x 用 stop，旧版用 pause，这里都试一次）。
func (q *QBit) Pause(ctx context.Context, gid string) error {
	return q.simple(ctx, "/torrents/stop", gid)
}

// Resume 继续。
func (q *QBit) Resume(ctx context.Context, gid string) error {
	return q.simple(ctx, "/torrents/start", gid)
}

// Remove 移除任务（保留文件，文件已入库或被用户保留）。
func (q *QBit) Remove(ctx context.Context, gid string) error {
	if err := q.login(ctx); err != nil {
		return err
	}
	form := url.Values{"hashes": {trimGID(gid)}, "deleteFiles": {"false"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.base+"/torrents/delete",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", q.endpoint)
	resp, err := q.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func (q *QBit) simple(ctx context.Context, path, gid string) error {
	if err := q.login(ctx); err != nil {
		return err
	}
	form := url.Values{"hashes": {trimGID(gid)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", q.endpoint)
	resp, err := q.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// infoHashFromMagnet 从磁力链里取 btih，作为任务句柄。
func infoHashFromMagnet(uri string) string {
	if !strings.HasPrefix(strings.ToLower(uri), "magnet:") {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	if h := u.Query().Get("xt"); strings.HasPrefix(h, "urn:btih:") {
		return trimGID(strings.TrimPrefix(h, "urn:btih:"))
	}
	return ""
}
