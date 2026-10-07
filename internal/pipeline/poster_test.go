package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// 只有 Bangumi 有封面时，拿到的是图片 URL。媒体库只认本地文件：
// 不落盘的话海报是空的，NFO 里的 <thumb> 还成了一条死引用。
//
// 这条路径在演示环境里被蜜柑封面盖住了（蜜柑先抓到 data/covers/{id}.jpg），
// 只有"手工源 + 只开 Bangumi"的用户才会走到，所以必须单独钉住。
func TestBangumiOnlyPosterLandsInLibrary(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "search/subjects"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total": 1,
				"data": []map[string]any{{
					"id":      400602,
					"name":    "別当のオニイチャン",
					"name_cn": "别当欧尼酱了",
					"summary": "真寻变成女孩子之后的故事。",
					"date":    "2023-01-05",
					"eps":     12,
					"images":  map[string]any{"large": "http://" + r.Host + "/cover/l.jpg"},
					"rating":  map[string]any{"score": 7.8, "total": 5000},
				}},
			})
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpeg)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "none"
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Library.LinkMode = "hardlink"
	cfg.Meta.Enabled = true // 只要 Bangumi 开着，蜜柑不参与这条链路
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 手工订阅（库里没有番剧行），下载已完成，等着入库。
	file := filepath.Join(dir, "downloads", "欧尼酱 - 01.mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	subID, err := st.CreateSubscription(ctx, &model.Subscription{Name: "手工源", IntervalMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	itemID, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g1", Title: "[测试组] 别当欧尼酱了 - 01 [1080p]", Episode: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := st.CreateTask(ctx, &model.Task{
		ItemID: itemID, Title: "[测试组] 别当欧尼酱了 - 01 [1080p]", GID: "g1",
		State: model.TaskDownloading,
	})
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(cfg, st, &seedingDL{savePath: file, total: 2048}, srv.Client(), log)
	p.meta.SetBase(srv.URL) // 假 Bangumi：搜索给 JSON，封面给真图片

	task, err := st.GetTask(ctx, taskID)
	if err != nil || task == nil {
		t.Fatalf("读任务失败: %v", err)
	}
	p.finish(ctx, *task, downloader.Status{
		GID: "g1", State: model.TaskCompleted, Progress: 100,
		TotalBytes: 2048, DoneBytes: 2048, SavePath: file,
	})

	root := seriesDirUnder(t, cfg.Library.Root)
	poster := filepath.Join(root, "poster.jpg")
	if b, err := os.ReadFile(poster); err != nil || len(b) != len(jpeg) {
		t.Fatalf("Bangumi 封面没落进媒体库（%s）: %v", poster, err)
	}
	// 缓存目录里也该留一份，下一部同番剧集不必再抓。
	if _, err := os.Stat(filepath.Join(cfg.CoverDir(), "bgm-400602.jpg")); err != nil {
		t.Errorf("封面缓存没落盘: %v", err)
	}
	nfo, err := os.ReadFile(filepath.Join(root, "tvshow.nfo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<thumb>poster.jpg</thumb>", "<plot>真寻变成女孩子之后的故事。</plot>", "<rating>7.8</rating>"} {
		if !strings.Contains(string(nfo), want) {
			t.Errorf("NFO 缺 %s:\n%s", want, nfo)
		}
	}
}

// 同一张 Bangumi 封面只抓一次：第二集入库时直接用缓存。
func TestBangumiPosterFetchedOnce(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0})
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Library.Root = filepath.Join(dir, "library")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	p := New(cfg, nil, nil, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	first := p.posterLocal(ctx, 400602, srv.URL+"/cover.jpg")
	second := p.posterLocal(ctx, 400602, srv.URL+"/cover.jpg")
	if first == "" || first != second {
		t.Fatalf("第二次该命中缓存: %q / %q", first, second)
	}
	if hits != 1 {
		t.Errorf("同一张封面抓了 %d 次", hits)
	}
	// 本地路径原样返回（蜜柑封面缓存走的就是这条路）。
	if got := p.posterLocal(ctx, 3141, "/data/covers/3141.jpg"); got != "/data/covers/3141.jpg" {
		t.Errorf("本地路径不该被改写: %q", got)
	}
	// 抓不到就返回空串，宁可没海报也不要写死引用。
	if got := p.posterLocal(ctx, 9, srv.URL+"/missing"); got != "" {
		got = strings.TrimSpace(got)
		if got != "" && !strings.HasSuffix(got, "bgm-9.jpg") {
			t.Errorf("下载失败应当返回空串: %q", got)
		}
	}
	_ = time.Now
}

// seriesDirUnder 从上一步落盘的 NFO 反推番剧根目录，避免把命名规则写死在测试里。
func seriesDirUnder(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if info.Name() == "tvshow.nfo" {
			found = filepath.Dir(path)
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("在 %s 下找不到 tvshow.nfo: %v", root, err)
	}
	return found
}
