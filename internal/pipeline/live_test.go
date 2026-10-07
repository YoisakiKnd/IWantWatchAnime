package pipeline

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/library"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// 真机链路：蜜柑详情 + Bangumi 简介/评分 + 封面 → 媒体库 poster.jpg 与 tvshow.nfo。
//
// 前面的单测都是假的站点、假的响应；这一条真打 mikanani.me 与 api.bgm.tv，
// 走的是用户装完之后第一次入库时真正会走的那条路。
// 用 MIKAN_LIVE=1 BGM_LIVE=1 go test ./internal/pipeline/ -run TestLiveCover -v 跑。
func TestLiveCoverAndIntroToLibraryNFO(t *testing.T) {
	if os.Getenv("MIKAN_LIVE") != "1" || os.Getenv("BGM_LIVE") != "1" {
		t.Skip("设置 MIKAN_LIVE=1 BGM_LIVE=1 才跑真机链路")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "none"
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Library.LinkMode = "hardlink"
	cfg.Meta.Enabled = true
	cfg.Meta.MinGapMs = 1200
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	hcx := &http.Client{Timeout: 20 * time.Second}
	p := New(cfg, st, nil, hcx, log)

	// 1) 真蜜柑：番剧详情（标题、放送日、开播、字幕组）
	b, err := p.BangumiInfo(ctx, 3141)
	if err != nil {
		t.Fatalf("真抓蜜柑详情失败: %v", err)
	}
	if b.Title == "" || len(b.Subgroups) == 0 {
		t.Fatalf("详情解析不完整: %+v", b)
	}
	t.Logf("蜜柑：%s 放送=%s 开播=%s 字幕组=%d", b.Title, b.AirText, b.StartAt, len(b.Subgroups))

	// 2) 真封面：落进封面缓存（面板缩略图与媒体库海报共用这张）
	cover, err := p.EnsureCover(ctx, b)
	if err != nil {
		t.Fatalf("真抓封面失败: %v", err)
	}
	st1, err := os.Stat(cover)
	if err != nil || st1.Size() < 4096 {
		t.Fatalf("封面没落盘: %v %v", cover, err)
	}
	t.Logf("封面：%s %d 字节", cover, st1.Size())

	// 3) 真 Bangumi：简介与评分
	b, err = p.BangumiInfo(ctx, 3141)
	if err != nil {
		t.Fatalf("Bangumi 补简介失败: %v", err)
	}
	if b.Summary == "" || b.Score == 0 {
		t.Fatalf("简介/评分没补上: 简介 %d 字 评分 %.1f", len(b.Summary), b.Score)
	}
	t.Logf("Bangumi：评分 %.1f 简介 %d 字 总集数 %d", b.Score, len(b.Summary), b.Episodes)

	// 4) 入库一集：番剧级 NFO 与海报都该到位
	m := p.bangumiMeta(b)
	if m == nil || m.Poster == "" {
		t.Fatalf("番剧元数据没带上海报: %+v", m)
	}
	res, err := p.org.Organize(ctx, library.Input{
		Source: writeTemp(t, dir, "ep1.mkv", "第一集"),
		Parsed: matcher.Parsed{Title: b.Title, Season: 1, Episode: 1, Kind: matcher.KindTV},
		Meta:   m,
		Group:  "2024秋",
	})
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}
	nfo, err := os.ReadFile(res.TVNFOPath)
	if err != nil {
		t.Fatalf("读 NFO: %v", err)
	}
	if res.Poster == "" {
		t.Fatalf("媒体库没有海报: %+v", res)
	}
	if st2, err := os.Stat(res.Poster); err != nil || st2.Size() < 4096 {
		t.Fatalf("海报文件不对: %v %v", res.Poster, err)
	}
	for _, want := range []string{"<thumb>poster.jpg</thumb>", "<plot>", "<rating>"} {
		if !strings.Contains(string(nfo), want) {
			t.Errorf("tvshow.nfo 缺 %q:\n%s", want, nfo)
		}
	}
	p.log.Info("真机链路完成", "NFO", res.TVNFOPath, "海报", res.Poster)
	t.Logf("媒体库：%s（海报 %s）", res.TVNFOPath, res.Poster)
}
