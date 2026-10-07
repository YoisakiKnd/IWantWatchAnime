package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/downloader"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"

	_ "modernc.org/sqlite"
)

// 一集两个版本：A组是偏好、体积大；B组非偏好、体积小。
// 第一轮判定会把 B组 择优刷掉，于是它就成了断种时的现成替补。
const relinkFeed = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0"><channel><title>换链测试</title>
  <item><title>[A组] 测试番剧 - 05 [1080p][HEVC][简日双语]</title><guid>r-a</guid>
    <enclosure url="https://example.invalid/a5.torrent" length="2147483648" type="application/x-bittorrent"/></item>
  <item><title>[B组] 测试番剧 - 05 [720p][AVC][简]</title><guid>r-b</guid>
    <enclosure url="https://example.invalid/b5.torrent" length="1073741824" type="application/x-bittorrent"/></item>
</channel></rss>`

type relinkEnv struct {
	p     *Pipeline
	st    *store.Store
	aria  *fakeAria2
	cfg   config.Config
	notes *[]string
	subID int64
}

func newRelinkEnv(t *testing.T) *relinkEnv {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, relinkFeed)
	}))
	t.Cleanup(feedSrv.Close)

	aria := &fakeAria2{status: map[string]map[string]any{}}
	ariaSrv := httptest.NewServer(aria.handler())
	t.Cleanup(ariaSrv.Close)

	notes := &[]string{}
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Title, Body string }
		_ = decodeJSON(r, &payload)
		*notes = append(*notes, payload.Title+"\n"+payload.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hookSrv.Close)

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "aria2"
	cfg.Engine.Aria2.Endpoint = ariaSrv.URL
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Library.MinFreeGiB = 0
	cfg.Meta.Enabled = false
	cfg.Notify.Webhooks = []string{hookSrv.URL}
	cfg.Relink.Enabled = true
	cfg.Relink.StuckHours = 6
	cfg.Relink.MaxPerEp = 2
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	subID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "测试番剧 [A组]", FeedURL: feedSrv.URL, Group: "2024秋",
		Include: []string{"1080", "720"}, Prefer: []string{"A组"},
		IntervalMin: 30, Enabled: true, StartEp: 0,
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
		t.Fatal(err)
	}
	return &relinkEnv{p: p, st: st, aria: aria, cfg: cfg, notes: notes, subID: subID}
}

func decodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// ageTask 把任务的更新时间往前挪，模拟「卡了很久」。
// 直接改库是有意的：真实场景里这个时间就是随时间流逝变旧的。
func (e *relinkEnv) ageTask(t *testing.T, taskID int64, d time.Duration) {
	t.Helper()
	raw, err := sql.Open("sqlite", e.cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ts := time.Now().Add(-d).Unix()
	if _, err := raw.Exec(`UPDATE tasks SET progress_at = ?, updated_at = ? WHERE id = ?`, ts, ts, taskID); err != nil {
		t.Fatal(err)
	}
}

func (e *relinkEnv) activeTasks(t *testing.T) []model.Task {
	t.Helper()
	tasks, err := e.st.ListActiveTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

func (e *relinkEnv) sawNote(sub string) bool {
	for _, n := range *e.notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// 断种换链（下载失败这条分支）：换投 B组，原条目记为失败，通知出去。
func TestRelinkOnFailure(t *testing.T) {
	ctx := context.Background()
	env := newRelinkEnv(t)
	env.p.PollSubscription(ctx, env.subID)

	tasks := env.activeTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("第一轮应当只投递 A组 那一条，实际 %d 个任务", len(tasks))
	}
	if !strings.Contains(tasks[0].Title, "A组") {
		t.Fatalf("偏好版本没被选中: %s", tasks[0].Title)
	}
	// B组 应当已经被择优刷掉，成为候选。
	items, _ := env.st.ListItems(ctx, 20)
	var rejected int
	for _, it := range items {
		if it.Status == model.ItemRejected {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("期望 1 条被择优刷掉的条目（就是替补），实际 %d", rejected)
	}

	// 下载器报错 → 对账时换链。
	env.aria.setStatus(tasks[0].GID, map[string]any{
		"gid": tasks[0].GID, "status": "error", "errorCode": "1",
		"errorMessage": "连接 tracker 超时", "totalLength": "0", "completedLength": "0",
		"dir": env.cfg.Engine.Aria2.Dir, "files": []any{},
	})
	env.p.Reconcile(ctx)

	after := env.activeTasks(t)
	if len(after) != 1 {
		t.Fatalf("换链后应当有 1 个新任务在跑（原任务已终结），实际 %d", len(after))
	}
	if !strings.Contains(after[0].Title, "B组") {
		t.Errorf("换链应当改投 B组，实际 %s", after[0].Title)
	}
	if !env.sawNote("断种换链") {
		t.Errorf("换链要通知，实际收到：%v", *env.notes)
	}

	// 原条目必须留下「为什么换了」的痕迹，否则面板上会出现两条同名记录。
	var failed int
	items, _ = env.st.ListItems(ctx, 20)
	for _, it := range items {
		if it.Status == model.ItemFailed && strings.Contains(it.Reason, "断种换链") {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("原条目应当记为「断种换链」，实际 %d 条", failed)
	}

	// 再对账一次：原任务已经终结，不该无限换链。
	env.p.Reconcile(ctx)
	if got := len(env.activeTasks(t)); got != 1 {
		t.Errorf("第二次对账不该再投递，实际 %d 个活动任务", got)
	}
}

// 卡在 0% 的断种：没有报错，只是永远下不动，也要换。
func TestRelinkOnStuck(t *testing.T) {
	ctx := context.Background()
	env := newRelinkEnv(t)
	env.p.PollSubscription(ctx, env.subID)
	tasks := env.activeTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个任务，实际 %d", len(tasks))
	}

	// 刚刚投递（进度 0、时间很新）不该算断种。
	env.p.Reconcile(ctx)
	if got := len(env.activeTasks(t)); got != 1 {
		t.Fatalf("刚投递就换链是误判，实际 %d 个任务", got)
	}

	// 把更新时间挪到 8 小时前：还是在下载中，但一直没有动静。
	env.ageTask(t, tasks[0].ID, 8*time.Hour)
	env.p.Reconcile(ctx)

	after := env.activeTasks(t)
	if len(after) != 1 || !strings.Contains(after[0].Title, "B组") {
		t.Fatalf("卡住的 A组 应当被换成 B组，实际 %+v", after)
	}
	if env.aria.pausedCount() == 0 {
		t.Error("换链后应当暂停原任务，别让它继续占着下载队列")
	}
	if !env.sawNote("卡在 0%") {
		t.Errorf("通知里应当说明是卡住断种，实际：%v", *env.notes)
	}

	// 原任务的错误原因要写清楚，面板上一眼能看出是换链而不是失败。
	old, err := env.st.GetTask(ctx, tasks[0].ID)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	if old.State != model.TaskError || !strings.Contains(old.Error, "换链") {
		t.Errorf("原任务状态不对: %+v", old)
	}
}

// 用户手动暂停的任务不动：那是等一个明确的指令，程序不该跟人抢方向盘。
func TestRelinkSkipsUserPaused(t *testing.T) {
	ctx := context.Background()
	env := newRelinkEnv(t)
	env.p.PollSubscription(ctx, env.subID)
	tasks := env.activeTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("期望 1 个任务，实际 %d", len(tasks))
	}
	if err := env.st.UpdateTaskProgress(ctx, tasks[0].ID, tasks[0].GID,
		model.TaskPaused, 0, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	env.ageTask(t, tasks[0].ID, 8*time.Hour)
	env.p.Reconcile(ctx)

	after := env.activeTasks(t)
	if len(after) != 1 || !strings.Contains(after[0].Title, "A组") {
		t.Fatalf("暂停的任务不该被换掉，实际 %+v", after)
	}
	if env.aria.pausedCount() > 0 {
		t.Error("不该再对用户暂停的任务调暂停")
	}
}

// 换链次数到上限就停手：候选池再大也不能无限投递。
func TestRelinkStopsAtMaxPerEpisode(t *testing.T) {
	ctx := context.Background()
	env := newRelinkEnv(t)
	env.cfg.Relink.MaxPerEp = 1
	// 配置是值拷贝，改完要重新装配流水线的配置。
	env.p.cfg.Relink.MaxPerEp = 1

	env.p.PollSubscription(ctx, env.subID)
	tasks := env.activeTasks(t)
	env.aria.setStatus(tasks[0].GID, map[string]any{
		"gid": tasks[0].GID, "status": "error", "errorMessage": "断种",
		"totalLength": "0", "completedLength": "0", "dir": env.cfg.Engine.Aria2.Dir, "files": []any{},
	})
	env.p.Reconcile(ctx)

	// 第一次换链成功（B组 顶上），把 B组 也判失败后，应当停手。
	after := env.activeTasks(t)
	if len(after) != 1 || !strings.Contains(after[0].Title, "B组") {
		t.Fatalf("第一次换链没成功: %+v", after)
	}
	env.aria.setStatus(after[0].GID, map[string]any{
		"gid": after[0].GID, "status": "error", "errorMessage": "断种",
		"totalLength": "0", "completedLength": "0", "dir": env.cfg.Engine.Aria2.Dir, "files": []any{},
	})
	env.p.Reconcile(ctx)

	if got := len(env.activeTasks(t)); got != 0 {
		t.Errorf("到上限后不该再投递，实际还剩 %d 个活动任务", got)
	}
	n, err := env.st.CountRelinks(ctx, env.subID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应当只换过 1 次，实际 %d 次", n)
	}
}

// 关掉换链就完全不介入。
func TestRelinkDisabled(t *testing.T) {
	ctx := context.Background()
	env := newRelinkEnv(t)
	env.p.cfg.Relink.Enabled = false
	env.p.PollSubscription(ctx, env.subID)
	tasks := env.activeTasks(t)
	env.aria.setStatus(tasks[0].GID, map[string]any{
		"gid": tasks[0].GID, "status": "error", "errorMessage": "断种",
		"totalLength": "0", "completedLength": "0", "dir": env.cfg.Engine.Aria2.Dir, "files": []any{},
	})
	env.p.Reconcile(ctx)

	if got := len(env.activeTasks(t)); got != 0 {
		t.Errorf("关掉换链后不该投递新任务，实际 %d 个", got)
	}
	if env.sawNote("断种换链") {
		t.Error("关掉后不该发换链通知")
	}
}

func TestHumanDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0 分钟"},
		{45 * time.Minute, "45 分钟"},
		{3 * time.Hour, "3 小时"},
		{50 * time.Hour, "2 天"},
	}
	for _, c := range cases {
		if got := humanDur(c.d); got != c.want {
			t.Errorf("humanDur(%v) = %q，期望 %q", c.d, got, c.want)
		}
	}
}
