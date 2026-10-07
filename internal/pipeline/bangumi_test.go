package pipeline

import (
	"context"
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
	"github.com/YoisakiKnd/IWantWatchAnime/internal/library"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// 在番剧库点一下订阅，默认只跟新出的集数：
// 首次轮询用这一批里最新的一集当起点并写回数据库，
// 往期集数记成「集数低于起始集」，不会一口气把整部番拉下来。
func TestFirstPollStartsFromNewest(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	feed := `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0"><channel><title>跟新测试</title>
  <item><title>[A组] 测试番剧 - 01 [1080p][简日双语]</title><guid>g1</guid>
    <enclosure url="https://example.invalid/1.torrent" length="1073741824" type="application/x-bittorrent"/></item>
  <item><title>[A组] 测试番剧 - 02 [1080p][简日双语]</title><guid>g2</guid>
    <enclosure url="https://example.invalid/2.torrent" length="1073741824" type="application/x-bittorrent"/></item>
  <item><title>[A组] 测试番剧 - 03 [1080p][简日双语]</title><guid>g3</guid>
    <enclosure url="https://example.invalid/3.torrent" length="1073741824" type="application/x-bittorrent"/></item>
</channel></rss>`

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = io.WriteString(w, feed)
	}))
	defer feedSrv.Close()

	aria := &fakeAria2{status: map[string]map[string]any{}}
	ariaSrv := httptest.NewServer(aria.handler())
	defer ariaSrv.Close()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "aria2"
	cfg.Engine.Aria2.Endpoint = ariaSrv.URL
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Library.MinFreeGiB = 0
	cfg.Meta.Enabled = false
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// -1 就是「订阅时没说从哪集开始」的默认值。
	subID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "测试番剧 [A组]", FeedURL: feedSrv.URL, Group: "2024秋",
		Include: []string{"1080"}, Prefer: []string{"A组"},
		IntervalMin: 30, Enabled: true, StartEp: -1, MikanID: 0,
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

	p.PollSubscription(ctx, subID)

	sub, err := st.GetSubscription(ctx, subID)
	if err != nil || sub == nil {
		t.Fatal(err)
	}
	if sub.StartEp != 3 {
		t.Fatalf("首次轮询应当把起点定在最新一集 3，实际 %v", sub.StartEp)
	}

	// 01、02 被起点挡掉，只有 03 投递出去。
	tasks, err := st.ListActiveTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || !strings.Contains(tasks[0].Title, "- 03 ") {
		t.Fatalf("期望只投递第 03 集，实际 %+v", tasks)
	}

	items, err := st.ListItems(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var below int
	for _, it := range items {
		if it.Episode < 3 {
			below++
			if it.Status != model.ItemRejected || it.Reason != "集数低于起始集" {
				t.Errorf("往期集数应当记成「集数低于起始集」：%+v", it)
			}
		}
	}
	if below != 2 {
		t.Errorf("期望 2 条往期被挡，实际 %d", below)
	}

	// 勾了「连往期一起下」（StartEp=0）时不该被改写，往期照下。
	oldID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "测试番剧 [A组·往期]", FeedURL: feedSrv.URL + "/?old=1", Group: "2024秋",
		Include: []string{"1080"}, Prefer: []string{"A组"},
		IntervalMin: 30, Enabled: true, StartEp: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.PollSubscription(ctx, oldID)
	old, err := st.GetSubscription(ctx, oldID)
	if err != nil || old == nil {
		t.Fatal(err)
	}
	if old.StartEp != 0 {
		t.Errorf("勾了往期就不该自动改起点，实际 %v", old.StartEp)
	}
}

// 搜索后应当顺手把前几部的详情预热掉：用户点开第一张卡不用再等 10 秒。
// 这里同时验证两件事：
//  1. 预热真的落库了（字幕组出来了）；
//  2. 同一部番剧并发抓取只打一次站点（in-flight 去重）。
func TestSearchWarmsTopResultsOnce(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	searchHTML, err := os.ReadFile(filepath.Join("..", "mikan", "testdata", "search.html"))
	if err != nil {
		t.Fatalf("读搜索固件: %v", err)
	}
	detailHTML, err := os.ReadFile(filepath.Join("..", "mikan", "testdata", "bangumi.html"))
	if err != nil {
		t.Fatalf("读详情固件: %v", err)
	}

	var mu sync.Mutex
	searchHits, detailHits := 0, 0
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := r.URL.Path
		if strings.HasPrefix(path, "/Home/Search") {
			searchHits++
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(searchHTML)
			return
		}
		if strings.HasPrefix(path, "/Home/Bangumi/") {
			detailHits++
			// 慢慢回：让并发的第二个请求真的落在 in-flight 窗口里。
			time.Sleep(80 * time.Millisecond)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(detailHTML)
			return
		}
		http.NotFound(w, r)
	}))
	defer site.Close()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "aria2"
	cfg.Engine.Aria2.Endpoint = "http://127.0.0.1:1/jsonrpc"
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Meta.Enabled = true
	cfg.Meta.MikanBase = site.URL
	cfg.Meta.MinGapMs = 5 // 单测里不需要真的给站点留间隔
	cfg.Meta.CacheH = 12
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hc := &http.Client{Timeout: 5 * time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(cfg, st, nil, hc, log) // dl = nil：这个测试不碰下载

	list, err := p.SearchBangumi(ctx, "葬送的芙莉莲")
	if err != nil {
		t.Fatalf("SearchBangumi: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("期望 2 张卡，实际 %d", len(list))
	}

	// 预热在后台跑：等它把第一部写进库（字幕组是只有详情页才有的字段）。
	deadline := time.Now().Add(5 * time.Second)
	var warmed *model.Bangumi
	for time.Now().Before(deadline) {
		if got, err := st.GetBangumi(ctx, list[0].ID); err == nil && got != nil && len(got.Subgroups) > 0 {
			warmed = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if warmed == nil {
		t.Fatal("预热没有把第一部番剧的详情写进库")
	}
	if warmed.CoverPath == "" || warmed.AirText == "" {
		t.Errorf("预热出来的记录不完整: %+v", warmed)
	}

	// 再搜一次：搜索本身也在 mikan 客户端里缓存了，站点不该被重复打扰。
	mu.Lock()
	beforeSearch, beforeDetail := searchHits, detailHits
	mu.Unlock()
	if _, err := p.SearchBangumi(ctx, "葬送的芙莉莲"); err != nil {
		t.Fatalf("第二次 SearchBangumi: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	afterSearch, afterDetail := searchHits, detailHits
	mu.Unlock()
	if afterSearch != beforeSearch {
		t.Errorf("同一个关键词应当命中搜索缓存：%d → %d", beforeSearch, afterSearch)
	}
	if afterDetail != beforeDetail {
		t.Errorf("库里已有新缓存，不该再抓详情：%d → %d", beforeDetail, afterDetail)
	}

	// 并发点同一个番剧：只打一次站点。
	mu.Lock()
	detailHits = 0
	mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.mikan.Detail(ctx, 999999); err != nil {
				t.Errorf("并发 Detail: %v", err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	hits := detailHits
	mu.Unlock()
	if hits != 1 {
		t.Errorf("4 个并发请求应当只抓 1 次，实际 %d 次", hits)
	}
}

// 简介/评分比入库晚到时的完整链路：番剧详情抓回来 → Bangumi 补齐 →
// 库里那份没有 plot 的 tvshow.nfo 自动对齐（用户什么都不用做）。
//
// 固件用的是蜜柑的《地狱乐》页面（13 集、8 个字幕组），标题也以它为准。
func TestBangumiEnrichAlignsLibraryNFO(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	detailHTML, err := os.ReadFile(filepath.Join("..", "mikan", "testdata", "bangumi.html"))
	if err != nil {
		t.Fatalf("读详情固件: %v", err)
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/Home/Bangumi/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(detailHTML)
			return
		}
		http.NotFound(w, r)
	}))
	defer site.Close()

	// 假 Bangumi：一条带简介与评分的搜索结果（eps=28 是官方总数，
	// 而蜜柑页面写的是 13 —— 两边都有值时以蜜柑为准，这里顺便验证这条规则）。
	bgm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":1,"data":[{"id":400602,"name":"地狱乐",
			"name_cn":"地狱乐","summary":"为了找到仙药，画眉丸踏上极乐世界的旅途。",
			"date":"2023-04-01","eps":28,"rating":{"score":8.5},
			"images":{"large":"https://example.invalid/p.jpg"}}]}`))
	}))
	defer bgm.Close()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "aria2"
	cfg.Engine.Aria2.Endpoint = "http://127.0.0.1:1/jsonrpc"
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Meta.Enabled = true
	cfg.Meta.MikanBase = site.URL
	cfg.Meta.BGMBase = bgm.URL
	cfg.Meta.MinGapMs = 5
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hc := &http.Client{Timeout: 5 * time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(cfg, st, nil, hc, log)

	// 造一份"入库时还没有简介"的现场：番剧行 + 入库记录 + 半成品 NFO。
	if err := st.UpsertBangumi(ctx, &model.Bangumi{ID: 2996, Title: "地狱乐", AirText: "星期六", StartAt: "4/1/2023"}); err != nil {
		t.Fatal(err)
	}
	b0, err := st.GetBangumi(ctx, 2996)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.org.Organize(ctx, library.Input{
		Source: writeTemp(t, dir, "ep1.mkv", "第一集"),
		Parsed: matcher.Parsed{Title: "地狱乐", Season: 1, Episode: 1, Kind: matcher.KindTV},
		Meta:   p.bangumiMeta(b0),
		Group:  "2023春",
	})
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}
	// 入库记录的标题故意写成"别的写法"：对齐时必须靠番剧 id 找到它，
	// 靠标题匹配在真实库里迟早会失手（蜜柑与 Bangumi 的名字不一样）。
	if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{
		SeriesID: 2996, Title: "地狱乐（2023）", Path: res.Path, Episode: 1,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(res.TVNFOPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "<plot>") || strings.Contains(string(before), "<rating>") {
		t.Fatalf("入库时还没有简介/评分，不该有这两个标签:\n%s", before)
	}

	// 刮削补上简介/评分，磁盘上那份 NFO 要跟着对齐。
	b, err := p.BangumiInfo(ctx, 2996)
	if err != nil {
		t.Fatalf("BangumiInfo: %v", err)
	}
	if b.Summary == "" || b.Score != 8.5 {
		t.Fatalf("Bangumi 没补齐简介/评分: %+v", b)
	}
	if b.Episodes != 13 {
		t.Errorf("蜜柑页面有 13 集时不该被 Bangumi 的 28 覆盖，实际 %d", b.Episodes)
	}
	after, err := os.ReadFile(res.TVNFOPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<plot>为了找到仙药，画眉丸踏上极乐世界的旅途。</plot>",
		"<rating>8.5</rating>", "<title>地狱乐</title>", "2996",
	} {
		if !strings.Contains(string(after), want) {
			t.Errorf("对齐后的 tvshow.nfo 缺少 %q:\n%s", want, after)
		}
	}
	if before == nil || string(after) == string(before) {
		t.Error("NFO 应当被重写")
	}

	// 库里没有入库记录时也不该报错（还没下到任何一集）。
	if err := st.UpsertBangumi(ctx, &model.Bangumi{ID: 2997, Title: "地狱乐 第二季"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.BangumiInfo(ctx, 2997); err != nil {
		t.Fatalf("没有入库记录的番剧也要能抓: %v", err)
	}
}

// writeTemp 造一个临时文件（正片/封面用），返回路径。
func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, "tmp", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 蜜柑那份封面缺失时（离线订阅过、缓存被清），Bangumi 的封面要补进同一个封面槽：
// 之后入库、面板缩略图、"元数据迟到"的重写三条路径都当它是本地封面用，
// 媒体库最终拿到 poster.jpg 与指向它的 <thumb>。
func TestBangumiCoverFillsMissingMikanCover(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	detailHTML, err := os.ReadFile(filepath.Join("..", "mikan", "testdata", "bangumi.html"))
	if err != nil {
		t.Fatalf("读详情固件: %v", err)
	}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x11, 0x22, 0x33}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/Home/Bangumi/"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(detailHTML)
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpeg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer site.Close()

	bgm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":1,"data":[{"id":400602,"name":"地狱乐",
			"name_cn":"地狱乐","summary":"为了找到仙药，画眉丸踏上极乐世界的旅途。",
			"date":"2023-04-01","eps":28,"rating":{"score":8.5},
			"images":{"large":"` + site.URL + `/cover-large.jpg"}}]}`))
	}))
	defer bgm.Close()

	cfg := config.Default()
	cfg.Storage.DataDir = filepath.Join(dir, "data")
	cfg.Engine.Kind = "aria2"
	cfg.Engine.Aria2.Endpoint = "http://127.0.0.1:1/jsonrpc"
	cfg.Engine.Aria2.Dir = filepath.Join(dir, "downloads")
	cfg.Library.Root = filepath.Join(dir, "library")
	cfg.Meta.Enabled = true
	cfg.Meta.MikanBase = site.URL
	cfg.Meta.BGMBase = bgm.URL
	cfg.Meta.MinGapMs = 5
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p := New(cfg, st, nil, &http.Client{Timeout: 5 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 现场：番剧行（没有封面缓存）、一条订阅、一集已入库、一份没海报的 NFO。
	subID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "地狱乐 [某字幕组]", FeedURL: site.URL + "/RSS/Bangumi?bangumiId=2996",
		MikanID: 2996, IntervalMin: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = subID
	if err := st.UpsertBangumi(ctx, &model.Bangumi{ID: 2996, Title: "地狱乐", AirText: "星期六", StartAt: "4/1/2023"}); err != nil {
		t.Fatal(err)
	}
	b0, err := st.GetBangumi(ctx, 2996)
	if err != nil || b0 == nil {
		t.Fatalf("读番剧: %v", err)
	}
	res, err := p.org.Organize(ctx, library.Input{
		Source: writeTemp(t, dir, "ep1.mkv", "第一集"),
		Parsed: matcher.Parsed{Title: "地狱乐", Season: 1, Episode: 1, Kind: matcher.KindTV},
		Meta:   p.bangumiMeta(b0),
		Group:  "2023春",
	})
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}
	if res.Poster != "" {
		t.Fatalf("这时候还没有任何封面，不该有海报: %s", res.Poster)
	}
	if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{
		SeriesID: 2996, Title: "地狱乐", Path: res.Path, Episode: 1,
	}); err != nil {
		t.Fatal(err)
	}
	nfoBefore, _ := os.ReadFile(res.TVNFOPath)
	if strings.Contains(string(nfoBefore), "poster.jpg") {
		t.Fatalf("还没有封面时不该写 <thumb>：\n%s", nfoBefore)
	}

	// 维护循环的刮削：蜜柑详情 + Bangumi 补简介/评分，并把封面补进封面槽。
	p.RefreshBangumi(ctx)

	cover := filepath.Join(cfg.CoverDir(), "2996.jpg")
	if b, err := os.ReadFile(cover); err != nil || len(b) != len(jpeg) {
		t.Fatalf("封面槽没被 Bangumi 的封面填上（%s）: %v", cover, err)
	}
	root := filepath.Dir(res.TVNFOPath)
	poster := filepath.Join(root, "poster.jpg")
	if b, err := os.ReadFile(poster); err != nil || len(b) != len(jpeg) {
		t.Fatalf("媒体库没有拿到 poster.jpg: %v", err)
	}
	nfo, err := os.ReadFile(res.TVNFOPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<thumb>poster.jpg</thumb>", "<plot>为了找到仙药", "<rating>8.5</rating>"} {
		if !strings.Contains(string(nfo), want) {
			t.Errorf("对齐后的 tvshow.nfo 缺少 %q:\n%s", want, nfo)
		}
	}
}
