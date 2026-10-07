package pipeline

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/downloader"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// seedingDL 是一个只报告「下完在做种」的假内核。
//
// 真机上这条路径很容易被忽略：aria2 的 seed_time_min > 0、qBittorrent 默认
// 下完就做种，两家都不会回到 complete/完成态。对账如果只认 completed，
// 这一集就永远进不了媒体库。
type seedingDL struct {
	savePath string
	total    int64
	removed  int
}

func (d *seedingDL) Kind() string { return "fake-seeding" }
func (d *seedingDL) Add(context.Context, downloader.AddRequest) (string, error) {
	return "hash-seeding", nil
}

func (d *seedingDL) Status(context.Context, string) (downloader.Status, error) {
	return downloader.Status{
		GID:        "hash-seeding",
		State:      model.TaskSeeding,
		Progress:   100,
		TotalBytes: d.total,
		DoneBytes:  d.total,
		SavePath:   d.savePath,
	}, nil
}

func (d *seedingDL) Pause(context.Context, string) error  { return nil }
func (d *seedingDL) Resume(context.Context, string) error { return nil }
func (d *seedingDL) Remove(context.Context, string) error { d.removed++; return nil }
func (d *seedingDL) Ping(context.Context) error           { return nil }

// 做种中的任务要按「已完成」记账并入库，而且只能入一次。
func TestReconcileFinishesSeedingTask(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "none"
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Library.NameRule = "{title}/第 {season:02d} 季/{title} - S{season:02d}E{span}"
	cfg.Library.LinkMode = "hardlink"
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	subID, err := st.CreateSubscription(ctx, &model.Subscription{Name: "真机做种测试", IntervalMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	itemID, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g1", Title: "[测试组] 别当欧尼酱了 - 01 [1080p]",
		URI: "http://127.0.0.1:8097/webseed.torrent", Episode: 1, Status: model.ItemMatched,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, &model.Task{
		ItemID: itemID, Title: "[测试组] 别当欧尼酱了 - 01 [1080p]", Downloader: "qbittorrent",
		GID: "hash-seeding", State: model.TaskQueued, TotalBytes: 4096,
	}); err != nil {
		t.Fatal(err)
	}

	// 下载器已经下完，文件就在它报的落点上。
	file := filepath.Join(dir, "downloads", "demo.ep01.mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Repeat("demo", 1024)), 0o644); err != nil {
		t.Fatal(err)
	}

	dl := &seedingDL{savePath: file, total: 4096}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(cfg, st, dl, &http.Client{Timeout: 5 * time.Second}, log)

	p.Reconcile(ctx)

	tasks, err := st.ListActiveTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("做完种的任务不该再挂在活动列表里: %+v", tasks)
	}
	got, err := st.GetItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != model.ItemDone {
		t.Errorf("条目应当标为已入库: %+v", got)
	}
	ents, err := st.ListLibrary(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("应当入库一条，实际 %d 条", len(ents))
	}
	// 标题清洗由 matcher 负责（会补全角标点），这里只断言结构与落点。
	if !strings.HasPrefix(ents[0].Path, cfg.Library.Root) ||
		!strings.Contains(ents[0].Path, "第 01 季") || !strings.HasSuffix(ents[0].Path, "- S01E01.mkv") {
		t.Errorf("入库路径不对: %s", ents[0].Path)
	}
	if !strings.HasSuffix(ents[0].NFOPath, "- S01E01.nfo") {
		t.Errorf("应当同时写出单集 nfo: %s", ents[0].NFOPath)
	}
	if b, err := os.ReadFile(ents[0].Path); err != nil || len(b) != 4096 {
		t.Errorf("媒体库里没有正片: %v", err)
	}

	// 再对账一次：任务已经不在活动列表里，不该重复入库。
	p.Reconcile(ctx)
	ents2, _ := st.ListLibrary(ctx, 10)
	if len(ents2) != 1 {
		t.Errorf("重复对账不该重复入库: %d 条", len(ents2))
	}
}

// 只下了一半、却因为状态被内核报成做种的，不能入库（宁可不入，不可入错）。
func TestReconcileIgnoresPartialSeeding(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "none"
	cfg.Library.Root = filepath.Join(dir, "library")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	subID, err := st.CreateSubscription(ctx, &model.Subscription{Name: "半成品", IntervalMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	itemID, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g1", Title: "[测试组] 别当欧尼酱了 - 02 [1080p]", Episode: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, &model.Task{
		ItemID: itemID, Title: "[测试组] 别当欧尼酱了 - 02 [1080p]", GID: "hash-seeding",
		State: model.TaskQueued,
	}); err != nil {
		t.Fatal(err)
	}

	dl := &seedingDL{savePath: filepath.Join(dir, "nope.mkv"), total: 8192}
	// DoneBytes 只有一半：状态虽然被报成做种，也不该当完成。
	dl.total = 8192
	p := New(cfg, st, dl, &http.Client{Timeout: 5 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.dl = &halfSeedingDL{}
	p.Reconcile(ctx)

	ents, _ := st.ListLibrary(ctx, 10)
	if len(ents) != 0 {
		t.Errorf("没下完的不能入库: %+v", ents)
	}
	tasks, _ := st.ListActiveTasks(ctx)
	if len(tasks) != 1 {
		t.Errorf("没下完的任务要留在活动列表里继续对账: %+v", tasks)
	}
}

// halfSeedingDL 报「做种但只下了一半」：内核吹牛的时候不能跟着错。
type halfSeedingDL struct{}

func (d *halfSeedingDL) Kind() string { return "fake-half" }
func (d *halfSeedingDL) Add(context.Context, downloader.AddRequest) (string, error) {
	return "hash-seeding", nil
}
func (d *halfSeedingDL) Status(context.Context, string) (downloader.Status, error) {
	return downloader.Status{GID: "hash-seeding", State: model.TaskSeeding, Progress: 50, TotalBytes: 8192, DoneBytes: 4096}, nil
}
func (d *halfSeedingDL) Pause(context.Context, string) error  { return nil }
func (d *halfSeedingDL) Resume(context.Context, string) error { return nil }
func (d *halfSeedingDL) Remove(context.Context, string) error { return nil }
func (d *halfSeedingDL) Ping(context.Context) error           { return nil }
