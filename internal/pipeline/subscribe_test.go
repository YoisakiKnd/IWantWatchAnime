package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// 完结番的订阅默认值：没勾「连往期一起下」也要把整部拉下来。
//
// 这是真实投诉的回归测试。番剧播完之后，「最新一集」就是最后一集，
// 于是「只跟新的」等于一集都不给 —— 用户订阅一部完结番，看到的是
// 「订阅了，但什么都没下」。同时钉住三个不变量：
//  1. 完结番 + 调用方没表态 → 从第 1 集拉，13 集全投递；
//  2. 用户在页面上把勾去掉（页面会带 backfill_chosen）→ 尊重用户，只跟新的；
//  3. 还在播的番 → 保持原来的「只跟新的」，除非用户明确要求往期。
func TestSubscribeDefaultsToBackfillForFinishedShow(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// 完结番的 RSS 里仍然留着全部剧集 —— 蜜柑实测如此（向日葵马戏团 4020 的
	// 几个字幕组都还在供 E1~E13），所以「补全」是拿得到的，不是无解的。
	var feed strings.Builder
	feed.WriteString(`<?xml version="1.0" encoding="utf-8"?><rss version="2.0"><channel><title>完结番</title>`)
	for i := 1; i <= 13; i++ {
		fmt.Fprintf(&feed, `<item><title>[A组] 完结番 - %02d [1080p]</title><guid>g%d</guid>`+
			`<enclosure url="https://example.invalid/%d.torrent" length="1073741824" type="application/x-bittorrent"/></item>`,
			i, i, i)
	}
	feed.WriteString(`</channel></rss>`)

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/RSS/") {
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = io.WriteString(w, feed.String())
			return
		}
		http.NotFound(w, r) // 封面之类抓不到，不影响订阅
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
	p := New(cfg, st, nil, hc, log) // dl = nil：这个测试不碰下载器

	// 番剧行直接写库并标成「刚刮过」：BangumiInfo 走缓存，全程不出网。
	subgroup := func(id int64, name string) model.Subgroup {
		return model.Subgroup{ID: id, Name: name,
			RSS: fmt.Sprintf("/RSS/Bangumi?bangumiId=4020&subgroupid=%d", id)}
	}
	seed := func(b *model.Bangumi) {
		b.FetchedAt = time.Now()
		if err := st.UpsertBangumi(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	seed(&model.Bangumi{
		ID: 4020, Title: "Grow Up Show ～向日葵马戏团～",
		Episodes: 13, StartAt: "7/4/2026", AirText: "星期六",
		Subgroups: []model.Subgroup{subgroup(1256, "A组"), subgroup(370, "B组")},
	})
	seed(&model.Bangumi{
		ID: 9999, Title: "还在播的番",
		Episodes: 12, StartAt: "10/2/2026", AirText: "星期五",
		Subgroups: []model.Subgroup{subgroup(7, "A组"), subgroup(8, "B组")},
	})

	subscribe := func(t *testing.T, req SubscribeRequest) model.Subscription {
		t.Helper()
		res, err := p.SubscribeBangumi(ctx, req)
		if err != nil {
			t.Fatalf("SubscribeBangumi: %v", err)
		}
		if len(res.Created) != 1 {
			t.Fatalf("期望建成 1 条订阅，实际 %d 条（跳过 %v）", len(res.Created), res.Skipped)
		}
		sub := res.Created[0]
		if got := res.Backfill; got != (sub.StartEp == 0) {
			t.Errorf("结果里的 Backfill=%v 与起点 %v 不一致", got, sub.StartEp)
		}
		return sub
	}

	// 1) 完结番，调用方没表态（直接打接口的脚本不会带 backfill_chosen）→ 连往期
	fin := subscribe(t, SubscribeRequest{MikanID: 4020, SubgroupIDs: []int64{1256}, IntervalMin: 30})
	if fin.StartEp != 0 {
		t.Fatalf("已播完的番应当从第 1 集拉（StartEp=0），实际 %v", fin.StartEp)
	}

	// 真跑一轮：13 集全投递，一条都不该被起点挡掉。
	p.PollSubscription(ctx, fin.ID)
	items, err := st.ListItems(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var delivered, blocked int
	for _, it := range items {
		switch {
		case it.Status == model.ItemMatched || it.Status == model.ItemDone:
			delivered++
		case it.Reason == model.ReasonEpBelowStart:
			blocked++
		}
	}
	if delivered != 13 || blocked != 0 {
		t.Errorf("完结番应当 13 集全投递、0 条被挡，实际投递 %d、被挡 %d", delivered, blocked)
	}

	// 2) 同一个完结番，用户在页面上主动把勾去掉 → 只跟新的。
	//    页面总会带 backfill_chosen，所以「没勾」是明确选择，不能被自动判定推翻。
	only := subscribe(t, SubscribeRequest{MikanID: 4020, SubgroupIDs: []int64{370},
		IntervalMin: 30, BackfillChosen: true})
	if only.StartEp != -1 {
		t.Fatalf("用户明确不要往期时应当只跟新的（StartEp=-1），实际 %v", only.StartEp)
	}
	p.PollSubscription(ctx, only.ID)
	// 换一个新库视角统计：第 13 集之外全被起点挡住。
	if got := blockedCount(t, ctx, st, only.ID); got != 12 {
		t.Errorf("只跟新的那条应当挡下 12 集，实际 %d", got)
	}

	// 3) 还在播的番：没表态时保持「只跟新的」。
	airing := subscribe(t, SubscribeRequest{MikanID: 9999, SubgroupIDs: []int64{7}, IntervalMin: 30})
	if airing.StartEp != -1 {
		t.Errorf("在播的番默认应当只跟新的（StartEp=-1），实际 %v", airing.StartEp)
	}

	// 4) 但用户在在播番上明确勾了往期 → 听用户的。
	asked := subscribe(t, SubscribeRequest{MikanID: 9999, SubgroupIDs: []int64{8},
		IntervalMin: 30, Backfill: true, BackfillChosen: true})
	if asked.StartEp != 0 {
		t.Errorf("用户明确要往期时应当从第 1 集拉，实际 %v", asked.StartEp)
	}
}

// blockedCount 数一条订阅里被集数起点挡掉的条目。
func blockedCount(t *testing.T, ctx context.Context, st *store.Store, subID int64) int {
	t.Helper()
	items, err := st.ListItems(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range items {
		if it.SubID == subID && it.Reason == model.ReasonEpBelowStart {
			n++
		}
	}
	return n
}
