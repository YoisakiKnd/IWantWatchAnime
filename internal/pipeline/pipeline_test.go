package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/downloader"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// 这个测试把整条流水线跑一遍：本地假 RSS + 假 aria2 JSON-RPC。
// 它不依赖任何外网服务，因此可以在 CI 里稳定验证
// 「解析 → 规则过滤 → 同集择优 → 投递 → 对账 → 硬链接入库 → 写 NFO」。

const feedXML = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0">
<channel>
  <title>测试番剧订阅</title>
  <item>
    <title>[B组] 测试番剧 - 01 [720p][AVC][CHS]</title>
    <link>https://example.invalid/1</link>
    <guid>guid-ep1-b</guid>
    <enclosure url="https://example.invalid/1.torrent" length="1073741824" type="application/x-bittorrent"/>
    <pubDate>Mon, 06 Oct 2025 12:00:00 +0800</pubDate>
  </item>
  <item>
    <title>[A组] 测试番剧 - 01 [1080p][HEVC][简日双语]</title>
    <link>https://example.invalid/2</link>
    <guid>guid-ep1-a</guid>
    <enclosure url="https://example.invalid/2.torrent" length="2147483648" type="application/x-bittorrent"/>
    <pubDate>Mon, 06 Oct 2025 12:05:00 +0800</pubDate>
  </item>
  <item>
    <title>[A组] 测试番剧 - 02 [1080p][HEVC][简日双语]</title>
    <link>https://example.invalid/3</link>
    <guid>guid-ep2-a</guid>
    <enclosure url="https://example.invalid/3.torrent" length="2147483648" type="application/x-bittorrent"/>
    <pubDate>Mon, 06 Oct 2025 12:10:00 +0800</pubDate>
  </item>
  <item>
    <title>[A组] 测试番剧 - 03 [1080p][招募翻译]</title>
    <link>https://example.invalid/4</link>
    <guid>guid-ep3-a</guid>
    <enclosure url="https://example.invalid/4.torrent" length="2147483648" type="application/x-bittorrent"/>
  </item>
</channel>
</rss>`

// fakeAria2 实现流水线真正用到的三个 RPC 方法。
type fakeAria2 struct {
	mu     sync.Mutex
	urls   []string
	paused []string
	status map[string]map[string]any
}

func (f *fakeAria2) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string `json:"id"`
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()

		var result any
		var errMsg *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		switch req.Method {
		case "aria2.getVersion":
			result = map[string]any{"version": "1.36.0"}
		case "aria2.addUri":
			uri := fmt.Sprint(req.Params[0].([]any)[0])
			f.urls = append(f.urls, uri)
			gid := fmt.Sprintf("gid-%d", len(f.urls))
			f.status[gid] = map[string]any{
				"gid": gid, "status": "active", "totalLength": "0", "completedLength": "0",
				"dir": "", "files": []any{},
			}
			result = gid
		case "aria2.pause":
			f.paused = append(f.paused, fmt.Sprint(req.Params[0]))
			result = "OK"
		case "aria2.tellStatus":
			gid := fmt.Sprint(req.Params[0])
			st, ok := f.status[gid]
			if !ok {
				errMsg = &struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				}{Code: 1, Message: "GID not found"}
			} else {
				result = st
			}
		default:
			errMsg = &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -32601, Message: "method not found: " + req.Method}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if errMsg != nil {
			resp["error"] = errMsg
		} else {
			resp["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func (f *fakeAria2) pausedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paused)
}

func (f *fakeAria2) setStatus(gid string, s map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[gid] = s
}

func (f *fakeAria2) completed(gid, path string) {
	f.setStatus(gid, map[string]any{
		"gid": gid, "status": "complete", "totalLength": "2147483648",
		"completedLength": "2147483648", "dir": filepath.Dir(path),
		"files": []any{map[string]any{
			"path": path, "length": "2147483648", "completedLength": "2147483648", "selected": "true",
		}},
	})
}

func TestPipelineEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, feedXML)
	}))
	defer feedSrv.Close()

	aria := &fakeAria2{status: map[string]map[string]any{}}
	ariaSrv := httptest.NewServer(aria.handler())
	defer ariaSrv.Close()

	// 通知出口也要跑真的：假 webhook 收 JSON，验证「投递」和「入库完成」
	// 两条消息确实出了网，而不是只调了个函数。
	var noteMu sync.Mutex
	var notes []string
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Title, Body string }
		_ = json.NewDecoder(r.Body).Decode(&payload)
		noteMu.Lock()
		notes = append(notes, payload.Title+"\n"+payload.Body)
		noteMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hookSrv.Close()
	sawNote := func(sub string) bool {
		noteMu.Lock()
		defer noteMu.Unlock()
		for _, n := range notes {
			if strings.Contains(n, sub) {
				return true
			}
		}
		return false
	}

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "aria2"
	cfg.Engine.Aria2.Endpoint = ariaSrv.URL
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Library.LinkMode = "hardlink"
	cfg.Library.NameRule = "{group}/{title}/第 {season:02d} 季/{title} - S{season:02d}E{episode:02d}"
	cfg.Library.MinFreeGiB = 0
	cfg.Meta.Enabled = false
	cfg.Notify.Webhooks = []string{hookSrv.URL}
	cfg.Notify.ThrottleMin = 0
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 故意把 B 组放在前面：验证同集择优会把先到的低分版本换掉。
	subID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name:        "测试番剧",
		FeedURL:     feedSrv.URL,
		Group:       "2024秋",
		Include:     []string{"1080", "720"},
		Exclude:     []string{"招募"},
		Prefer:      []string{"A组"},
		IntervalMin: 30,
		Enabled:     true,
	})
	if err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Timeout: 5 * time.Second}
	dl, err := downloader.New(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(cfg, st, dl, hc, log)

	if err := p.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// ---- 第一轮轮询：应投递 2 集（A 组 01 与 02），03 被排除规则挡掉 ----
	p.PollSubscription(ctx, subID)

	tasks, err := st.ListActiveTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("期望投递 2 个任务，实际 %d 个（%+v）", len(tasks), tasks)
	}
	if !strings.Contains(tasks[0].Title, "A组") && !strings.Contains(tasks[1].Title, "A组") {
		t.Fatalf("同集择优失败，投递的都不是偏好字幕组：%+v", tasks)
	}
	for _, tk := range tasks {
		if strings.Contains(tk.Title, "720p") {
			t.Fatalf("低分版本不该被投递：%s", tk.Title)
		}
	}

	// 条目状态与「为什么没下」的原因都要落库，面板才能解释给用户。
	items, err := st.ListItems(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var rejected int
	for _, it := range items {
		if it.Status == model.ItemRejected {
			rejected++
			if it.Reason == "" {
				t.Errorf("被过滤的条目缺少原因：%s", it.Title)
			}
		}
	}
	if rejected < 2 {
		t.Errorf("期望至少 2 条被过滤记录（720p 与 招募），实际 %d", rejected)
	}

	// 投递出去的每个任务都要有通知：无人值守时这是唯一能看到的现场。
	if !sawNote("已投递 2 个新任务") {
		noteMu.Lock()
		t.Errorf("没收到投递通知，实际收到：%v", notes)
		noteMu.Unlock()
	}

	// ---- 第二轮：内容没变，304 时不应产生新任务 ----
	p.PollSubscription(ctx, subID)
	again, _ := st.ListActiveTasks(ctx)
	if len(again) != len(tasks) {
		t.Fatalf("304 轮询产生了重复任务：%d → %d", len(tasks), len(again))
	}

	// ---- 让 aria2 报告第 1 个任务完成，落一个假视频文件 ----
	victim := tasks[0]
	srcDir := filepath.Join(cfg.Engine.Aria2.Dir, "test-show")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(srcDir, "[A组] 测试番剧 - 01 [1080p][HEVC][简日双语].mkv")
	if err := os.WriteFile(video, []byte("not a real video but real bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 混入一个更小的干扰文件，验证「取体积最大的媒体文件」这一启发式。
	if err := os.WriteFile(filepath.Join(srcDir, "menu.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	aria.completed(victim.GID, video)

	p.Reconcile(ctx)

	lib, err := st.ListLibrary(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib) != 1 {
		t.Fatalf("期望入库 1 条，实际 %d 条", len(lib))
	}
	entry := lib[0]
	wantPath := filepath.Join(cfg.Library.Root, "2024秋", "测试番剧", "第 01 季", "测试番剧 - S01E01.mkv")
	if entry.Path != wantPath {
		t.Errorf("入库路径不符\n 期望 %q\n 实际 %q", wantPath, entry.Path)
	}
	if !entry.Linked {
		t.Error("同一文件系统上应当使用硬链接")
	}
	if _, err := os.Stat(entry.Path); err != nil {
		t.Fatalf("入库文件不存在: %v", err)
	}
	if entry.NFOPath == "" {
		t.Fatal("未写出 NFO")
	}
	nfo, err := os.ReadFile(entry.NFOPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nfo), "<episode>1</episode>") {
		t.Errorf("NFO 内容不正确:\n%s", nfo)
	}

	// 硬链接必须是同一个 inode，否则「不占额外空间」就是空话。
	srcInfo, err := os.Stat(video)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(entry.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Error("入库后不是同一个 inode，说明实际做的是复制")
	}

	// 入库完成也要通知，且正文里要有集数与落点，方便手机上直接确认。
	if !sawNote("入库完成：测试番剧") || !sawNote("硬链接") {
		noteMu.Lock()
		t.Errorf("没收到入库通知，实际收到：%v", notes)
		noteMu.Unlock()
	}

	// 任务应进入终态并停止被对账循环拾取。
	active, _ := st.ListActiveTasks(ctx)
	if len(active) != 1 {
		t.Errorf("完成的任务应当不再出现在活动任务里，实际剩 %d 个", len(active))
	}
}

// 假 aria2 报完成但文件其实不在时，必须给出可读的失败原因而不是panic。
func TestFinishWithoutFile(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "aria2"
	cfg.Engine.Aria2.Endpoint = "http://127.0.0.1:1/jsonrpc"
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	subID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "测试番剧", FeedURL: "http://127.0.0.1/fake.xml", IntervalMin: 30, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g1", Title: "[A组] 测试番剧 - 01 [1080p]", Status: model.ItemMatched,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := st.CreateTask(ctx, &model.Task{
		ItemID: itemID, Title: "[A组] 测试番剧 - 01 [1080p]", Downloader: "aria2",
		GID: "gid-1", State: model.TaskQueued,
	})
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(cfg, st, nil, &http.Client{Timeout: time.Second}, log)
	task, _ := st.GetTask(ctx, taskID)
	p.finish(ctx, *task, downloader.Status{
		GID: "gid-1", State: model.TaskCompleted, Progress: 100,
		SavePath: filepath.Join(dir, "downloads", "不存在的目录"),
	})

	got, _ := st.GetTask(ctx, taskID)
	if got.State != model.TaskError {
		t.Fatalf("入库失败后任务应标记为 error，实际 %q", got.State)
	}
	if !strings.Contains(got.Error, "找不到视频文件") {
		t.Errorf("失败原因不够可读：%q", got.Error)
	}
}
