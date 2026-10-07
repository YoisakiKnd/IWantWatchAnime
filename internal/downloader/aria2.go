package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// Aria2 走 JSON-RPC over HTTP。
//
// 这是默认内核：aria2 在 armv7 上的可获得性最好（OpenWrt / Entware / 各发行版
// 都有包），而 JSON-RPC 只是 HTTP 上的一个 JSON 体，客户端零依赖、约 200 行。
type Aria2 struct {
	endpoint string
	secret   string
	dir      string
	maxPeers int
	seedMin  int
	hc       *http.Client

	mu   sync.Mutex
	next int64 // JSON-RPC 请求 id
}

// NewAria2 构造 aria2 内核。
func NewAria2(cfg config.Aria2, hc *http.Client) *Aria2 {
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = 30
	}
	return &Aria2{
		endpoint: cfg.Endpoint,
		secret:   cfg.Secret,
		dir:      cfg.Dir,
		maxPeers: cfg.MaxPeers,
		seedMin:  cfg.SeedTimeMin,
		hc:       hc,
	}
}

// Kind 实现 Downloader。
func (a *Aria2) Kind() string { return "aria2" }

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (a *Aria2) call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	a.mu.Lock()
	a.next++
	id := strconv.FormatInt(a.next, 10)
	a.mu.Unlock()

	// 必须始终发数组：aria2 的 JSON-RPC 不接受 params 为 null，
	// 而 Go 把 nil 切片编成 null。
	if params == nil {
		params = []any{}
	}
	if a.secret != "" {
		params = append([]any{"token:" + a.secret}, params...)
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 aria2: %w", err)
	}
	defer resp.Body.Close()
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 aria2 响应: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("aria2 错误 %d: %s", out.Error.Code, out.Error.Message)
	}
	return out.Result, nil
}

// Ping 检查 aria2 是否在线，并顺带确认版本。
func (a *Aria2) Ping(ctx context.Context) error {
	raw, err := a.call(ctx, "aria2.getVersion")
	if err != nil {
		return err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.Version == "" {
		return fmt.Errorf("aria2 未返回版本号，检查 endpoint/secret 配置")
	}
	return nil
}

// Add 投递任务。
//
// 注意：直接给 .torrent 的 HTTP 地址时，aria2 会先建一个「元数据下载任务」，
// 完成后再 followedBy 出真正的 BT 任务；对账循环通过 Status.FollowedBy 跟进。
func (a *Aria2) Add(ctx context.Context, req AddRequest) (string, error) {
	opts := map[string]string{
		"dir":                       req.SaveDir,
		"bt-max-peers":              strconv.Itoa(a.maxPeers),
		"seed-time":                 strconv.Itoa(a.seedMin),
		"follow-torrent":            "true",
		"bt-enable-lpd":             "false", // 局域网 peer 发现对本场景无意义，省一次组播
		"max-connection-per-server": "4",
	}
	if opts["dir"] == "" {
		opts["dir"] = a.dir
	}
	raw, err := a.call(ctx, "aria2.addUri", []string{req.URI}, opts)
	if err != nil {
		return "", err
	}
	var gid string
	if err := json.Unmarshal(raw, &gid); err != nil {
		return "", fmt.Errorf("aria2 未返回 gid: %w", err)
	}
	return gid, nil
}

type ariaFile struct {
	Path            string `json:"path"`
	Length          string `json:"length"`
	CompletedLength string `json:"completedLength"`
	Selected        string `json:"selected"`
}

type ariaStatus struct {
	GID             string     `json:"gid"`
	Status          string     `json:"status"`
	TotalLength     string     `json:"totalLength"`
	CompletedLength string     `json:"completedLength"`
	DownloadSpeed   string     `json:"downloadSpeed"`
	ErrorCode       string     `json:"errorCode"`
	ErrorMessage    string     `json:"errorMessage"`
	Dir             string     `json:"dir"`
	Files           []ariaFile `json:"files"`
	FollowedBy      []string   `json:"followedBy"`
	Bittorrent      *struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	} `json:"bittorrent"`
	Seeder string `json:"seeder"`
}

// Status 查询单任务状态并映射到统一状态机。
func (a *Aria2) Status(ctx context.Context, gid string) (Status, error) {
	keys := []string{"gid", "status", "totalLength", "completedLength", "downloadSpeed",
		"errorCode", "errorMessage", "dir", "files", "followedBy", "bittorrent", "seeder"}
	raw, err := a.call(ctx, "aria2.tellStatus", gid, keys)
	if err != nil {
		return Status{}, err
	}
	var st ariaStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return Status{}, err
	}

	out := Status{
		GID:        st.GID,
		TotalBytes: atoi64(st.TotalLength),
		DoneBytes:  atoi64(st.CompletedLength),
		SavePath:   st.Dir,
		Error:      st.ErrorMessage,
	}
	if n := len(st.FollowedBy); n > 0 {
		out.FollowedBy = st.FollowedBy[n-1]
	}
	if out.TotalBytes > 0 {
		out.Progress = clamp100(float64(out.DoneBytes) / float64(out.TotalBytes) * 100)
	}

	switch st.Status {
	case "active":
		if out.TotalBytes > 0 && out.DoneBytes >= out.TotalBytes {
			out.State = model.TaskSeeding
		} else {
			out.State = model.TaskDownloading
		}
	case "waiting":
		out.State = model.TaskQueued
	case "paused":
		out.State = model.TaskPaused
	case "complete":
		out.State = model.TaskCompleted
		out.Progress = 100
	default: // error / removed
		out.State = model.TaskError
		if out.Error == "" {
			out.Error = "aria2 状态: " + st.Status + " (code " + st.ErrorCode + ")"
		}
	}

	// 定位最终落地的视频文件：BT 目录里可能混着字幕与样片，取体积最大的那个。
	// 单文件任务只返回 dir，这里补全成完整路径，入库阶段就不用再猜。
	if st.Bittorrent != nil && st.Bittorrent.Name != "" && len(st.Files) == 1 {
		out.SavePath = st.Files[0].Path
	} else if len(st.Files) == 1 && st.Files[0].Path != "" {
		out.SavePath = st.Files[0].Path
	} else if st.Bittorrent != nil && st.Bittorrent.Name != "" {
		out.SavePath = filepath.Join(st.Dir, st.Bittorrent.Name)
	}
	return out, nil
}

// Pause 暂停。
func (a *Aria2) Pause(ctx context.Context, gid string) error {
	_, err := a.call(ctx, "aria2.pause", gid)
	return err
}

// Resume 继续。
func (a *Aria2) Resume(ctx context.Context, gid string) error {
	_, err := a.call(ctx, "aria2.unpause", gid)
	return err
}

// Remove 取消任务并清理结果记录，避免 aria2 的历史列表无限增长。
func (a *Aria2) Remove(ctx context.Context, gid string) error {
	if _, err := a.call(ctx, "aria2.remove", gid); err != nil {
		// 已经处于停止态时 remove 会报错，接着清结果即可。
		if _, err2 := a.call(ctx, "aria2.removeDownloadResult", gid); err2 != nil {
			return err
		}
		return nil
	}
	_, err := a.call(ctx, "aria2.removeDownloadResult", gid)
	return err
}

// ActiveCount 返回 aria2 侧进行中的任务数，供上层做并发闸门判断。
func (a *Aria2) ActiveCount(ctx context.Context) (int, error) {
	raw, err := a.call(ctx, "aria2.tellActive", []string{"gid"})
	if err != nil {
		return 0, err
	}
	var list []struct {
		GID string `json:"gid"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0, err
	}
	return len(list), nil
}

func atoi64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
