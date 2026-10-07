package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/pipeline"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// 模板是运行期才求值的：模板里写错函数名或传错类型，编译期一律发现不了。
// 这个测试把每个页面与片段都真正渲染一遍，把这类错误挡在提交之前。
func TestRenderAllTemplates(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "none" // 不连下载器，只验证渲染
	// 本用例只关心渲染，先把认证关掉（认证在 TestAuthAndAPI 里单独验）
	cfg.Server.User, cfg.Server.Password = "", ""
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
		Name: "葬送的芙莉莲", FeedURL: "http://example.invalid/feed.xml", Group: "2024秋",
		Include: []string{"1080"}, Prefer: []string{"喵萌奶茶屋"}, IntervalMin: 30, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 再放一条停用的和一条报错的，覆盖排期行的三种状态样式。
	if _, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "已停用的订阅", FeedURL: "http://example.invalid/off.xml", IntervalMin: 60, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSubscriptionError(ctx, subID, "HTTP 404"); err != nil {
		t.Fatal(err)
	}

	itemID, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g1", Title: "[喵萌奶茶屋] 葬送的芙莉莲 - 03 [1080p][简日双语]",
		URI: "magnet:?xt=urn:btih:deadbeef", Episode: 3, Fansub: "喵萌奶茶屋",
		Status: model.ItemMatched, Reason: "偏好命中：喵萌奶茶屋",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g2", Title: "[其它组] 葬送的芙莉莲 - 03 [720p]",
		URI: "magnet:?xt=urn:btih:cafebabe", Episode: 3, Status: model.ItemRejected,
		Reason: "同集已有更优版本（喵萌奶茶屋）",
	}); err != nil {
		t.Fatal(err)
	}
	// 合集条目会走 epLabel 的「合集 1-25」分支
	if _, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g3", Title: "[SubGroup] 夏日重现 [01-25][合集]", URI: "magnet:?xt=urn:btih:ab",
		Episode: 1, EpisodeTo: 25, Batch: true, Status: model.ItemDone,
	}); err != nil {
		t.Fatal(err)
	}

	taskID, err := st.CreateTask(ctx, &model.Task{
		ItemID: itemID, Title: "[喵萌奶茶屋] 葬送的芙莉莲 - 03 [1080p][简日双语]",
		Downloader: "aria2", GID: "gid-1", State: model.TaskDownloading,
		Progress: 42.5, TotalBytes: 1317011456, DoneBytes: 559730000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{
		TaskID: taskID, Title: "葬送的芙莉莲", Path: "/mnt/usb/library/2024秋/葬送的芙莉莲/第 01 季/葬送的芙莉莲 - S01E03.mkv",
		Linked: true, NFOPath: "/mnt/usb/library/x.nfo", Episode: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Log(ctx, "library", "葬送的芙莉莲（硬链接）入库完成"); err != nil {
		t.Fatal(err)
	}
	if err := st.Log(ctx, "error", "某个源拉取失败：HTTP 404"); err != nil {
		t.Fatal(err)
	}

	// 番剧刮削结果：让放送轴带上封面列与放送日，也覆盖番剧库的详情状态。
	// fetched_at 取当前时刻，于是详情页直接读库、不产生任何网络请求。
	if err := st.UpsertBangumi(ctx, &model.Bangumi{
		ID: 3141, Title: "葬送的芙莉莲", CoverPath: "/images/Bangumi/202309/5ce9fed1.jpg",
		AirText: "星期五", Episodes: 28, StartAt: "9/29/2023", FetchedAt: time.Now(),
		Summary: "勇者一行打倒魔王之后，精灵魔法使芙莉莲踏上理解人类之旅。", Score: 8.5,
		Subgroups: []model.Subgroup{
			{ID: 382, Name: "喵萌奶茶屋", RSS: "/RSS/Bangumi?bangumiId=3141&subgroupid=382"},
			{ID: 370, Name: "LoliHouse", RSS: "/RSS/Bangumi?bangumiId=3141&subgroupid=370"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := pipeline.New(cfg, st, nil, &http.Client{Timeout: time.Second}, log)
	srv, err := New(cfg, st, p, log)
	if err != nil {
		t.Fatalf("解析模板失败: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/", "/subscribe", "/partials/schedule", "/partials/queue", "/partials/library",
		"/partials/activity", "/partials/verdict"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s 返回 %d: %s", path, resp.StatusCode, body)
		}
		if bytes.Contains(body, []byte("ZgotmplZ")) {
			t.Errorf("GET %s 出现被 CSS 过滤器转义的值（ZgotmplZ），检查 barStyle 的用法", path)
		}
		if bytes.Contains(body, []byte("<no value>")) {
			t.Errorf("GET %s 出现未求值的模板占位符", path)
		}
	}

	// 番剧库的详情状态：走库里的刮削数据，不联网，必须列出字幕组与订阅表单。
	detailResp, err := http.Get(ts.URL + "/subscribe?id=3141")
	if err != nil {
		t.Fatal(err)
	}
	detailBody, _ := io.ReadAll(detailResp.Body)
	detailResp.Body.Close()
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /subscribe?id=3141 返回 %d: %s", detailResp.StatusCode, detailBody)
	}
	// 简介来自 Bangumi、评分同一行显示：这两个字段蜜柑没有，页面必须带出来。
	for _, want := range []string{
		"葬送的芙莉莲", "喵萌奶茶屋", "LoliHouse", "/api/v1/bangumi/subscribe", "星期五",
		"精灵魔法使芙莉莲", "BGM 8.5",
	} {
		if !bytes.Contains(detailBody, []byte(want)) {
			t.Errorf("番剧详情页缺少 %q", want)
		}
	}
	if bytes.Contains(detailBody, []byte("ZgotmplZ")) {
		t.Error("番剧详情页出现 ZgotmplZ")
	}

	// 首页必须真的带出内容，而不是空壳。
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)
	for _, want := range []string{"放送轴", "葬送的芙莉莲", "下载队列", "最近入库", "活动流", "最近判定", "现在", "硬链接"} {
		if !strings.Contains(html, want) {
			t.Errorf("首页缺少内容 %q", want)
		}
	}
	if !strings.Contains(html, "width:42.5%") {
		t.Error("下载进度没有渲染成 CSS 宽度")
	}
}

func TestAuthAndAPI(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "none"
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Server.User = "tianyinling"
	cfg.Server.Password = "suzu"
	cfg.Server.Token = "tok-123"
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := pipeline.New(cfg, st, nil, &http.Client{Timeout: time.Second}, log)
	srv, err := New(cfg, st, p, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 没凭据要被挡住
	resp, err := http.Get(ts.URL + "/api/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未认证请求应返回 401，实际 %d", resp.StatusCode)
	}

	// 令牌可放行
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/state", nil)
	req.Header.Set("X-Api-Token", "tok-123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("带令牌的请求应返回 200，实际 %d", resp.StatusCode)
	}

	// 健康检查不需要认证（给 systemd / uptime 探活用）
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz 应返回 200，实际 %d", resp.StatusCode)
	}

	// 建订阅：应当 303 跳回首页，并且真的落库
	form := url.Values{"name": {"测试番剧"}, "url": {"http://example.invalid/f.xml"}, "interval_min": {"15"}}
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/subscriptions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("tianyinling", "suzu")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("创建订阅应 303，实际 %d", resp.StatusCode)
	}
	subs, err := st.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Name != "测试番剧" || subs[0].IntervalMin != 15 {
		t.Fatalf("订阅未正确落库: %+v", subs)
	}

	// 空地址要被明确拒绝
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/subscriptions", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("tianyinling", "suzu")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空订阅地址应返回 400，实际 %d", resp.StatusCode)
	}
	var payload map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if payload["error"] == "" {
		t.Error("错误响应必须带可读原因")
	}
}
