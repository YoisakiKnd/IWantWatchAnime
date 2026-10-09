package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// 番剧表是后加的，老库要能就地升级：Open 走一遍列检查，
// 缺 mikan_id 就 ALTER TABLE 补上，用户的订阅不能因为升级丢掉。
func TestMigrateAddsMikanColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// 先按「老版本」造一个旧库：建完表再把新增的列删掉，模拟上一版程序留下的库。
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "老订阅", FeedURL: "http://example.invalid/old.xml", IntervalMin: 30, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 番剧表里塞一条老数据：补列之后它必须还在，只是新字段为空。
	if err := st.UpsertBangumi(ctx, &model.Bangumi{
		ID: 2996, Title: "地狱乐", AirText: "星期六", Episodes: 13,
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE subscriptions DROP COLUMN mikan_id`); err != nil {
		t.Fatalf("造旧库失败: %v", err)
	}
	for _, col := range []string{"summary", "score"} {
		if _, err := raw.Exec(`ALTER TABLE bangumi DROP COLUMN ` + col); err != nil {
			t.Fatalf("造旧版番剧表失败（%s）: %v", col, err)
		}
	}
	raw.Close()

	// 重新打开：应当补回列、建回番剧表，并且旧数据还在。
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("升级旧库失败: %v", err)
	}
	defer st2.Close()

	sub, err := st2.GetSubscription(ctx, id)
	if err != nil || sub == nil {
		t.Fatalf("升级后订阅丢了: %v", err)
	}
	if sub.Name != "老订阅" || sub.MikanID != 0 {
		t.Errorf("升级后订阅内容不对: %+v", sub)
	}
	old, err := st2.GetBangumi(ctx, 2996)
	if err != nil || old == nil {
		t.Fatalf("升级后老番剧记录丢了: %v", err)
	}
	if old.Title != "地狱乐" || old.Episodes != 13 {
		t.Errorf("升级后番剧内容不对: %+v", old)
	}
	if old.Summary != "" || old.Score != 0 {
		t.Errorf("补出来的新列应当是空的: %+v", old)
	}
	// 补列之后要能正常写新字段。
	if err := st2.UpsertBangumi(ctx, &model.Bangumi{
		ID: 2996, Title: "地狱乐", Summary: "江户时代末期…", Score: 7.6,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st2.GetBangumi(ctx, 2996); got.Summary == "" || got.Score != 7.6 {
		t.Errorf("补列后写入失败: %+v", got)
	}
}

// 搜索页拿到的卡片只有标题和封面，不能把已经刮到的字幕组洗掉；
// 详情页拿到的整条记录则可以覆盖。
func TestUpsertBangumiKeepsSubgroups(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "suzu.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	full := &model.Bangumi{
		ID: 3141, Title: "葬送的芙莉莲", CoverPath: "/images/Bangumi/202309/5ce9fed1.jpg",
		AirText: "星期五", Episodes: 28, StartAt: "9/29/2023",
		Summary: "改编自山田钟人负责故事、アベツカサ作画的漫画。", Score: 8.5,
		Subgroups: []model.Subgroup{
			{ID: 382, Name: "喵萌奶茶屋", RSS: "/RSS/Bangumi?bangumiId=3141&subgroupid=382"},
			{ID: 370, Name: "LoliHouse", RSS: "/RSS/Bangumi?bangumiId=3141&subgroupid=370"},
		},
	}
	if err := st.UpsertBangumi(ctx, full); err != nil {
		t.Fatal(err)
	}

	// 只有标题的搜索结果：字幕组与集数必须保留。
	if err := st.UpsertBangumi(ctx, &model.Bangumi{
		ID: 3141, Title: "葬送的芙莉莲", CoverPath: "/images/Bangumi/202309/5ce9fed1.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBangumi(ctx, 3141)
	if err != nil || got == nil {
		t.Fatalf("读番剧失败: %v", err)
	}
	if len(got.Subgroups) != 2 {
		t.Fatalf("字幕组被搜索结果洗掉了: %+v", got.Subgroups)
	}
	if got.Episodes != 28 || got.AirText != "星期五" {
		t.Errorf("集数/放送时间被覆盖: %+v", got)
	}
	// 简介与评分只有 Bangumi 有，搜索卡片（蜜柑）永远带不上来，也必须留住。
	if got.Summary == "" || got.Score != 8.5 {
		t.Errorf("简介/评分被搜索结果洗掉了: summary=%q score=%v", got.Summary, got.Score)
	}

	// 反向：番剧详情把简介刮回来时，要能写进去（空则不覆盖不等于写不进）。
	if err := st.UpsertBangumi(ctx, &model.Bangumi{
		ID: 3141, Title: "葬送的芙莉莲", Summary: "第二版简介", Score: 8.7,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetBangumi(ctx, 3141); got.Summary != "第二版简介" || got.Score != 8.7 {
		t.Errorf("简介/评分更新没生效: %+v", got)
	}

	// 详情页的完整记录：可以更新。
	full.Episodes, full.Title = 28, "葬送的芙莉莲 第一季"
	if err := st.UpsertBangumi(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetBangumi(ctx, 3141); got.Title != "葬送的芙莉莲 第一季" {
		t.Errorf("详情页更新没生效: %+v", got)
	}

	list, err := st.ListBangumi(ctx, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListBangumi 结果不对: %v %v", list, err)
	}
	if _, err := st.GetBangumi(ctx, 999); err != nil {
		t.Errorf("查不存在的番剧应当返回 nil, nil，而不是错误: %v", err)
	}
}

// 断种换链要用到的两个查询：同集候选池、换链计数。
func TestAltItemsAndRelinkCount(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "relink.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	subID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "测试 [A组]", FeedURL: "https://example.invalid/rss", IntervalMin: 30,
		Enabled: true, Prefer: []string{"A组"},
	})
	if err != nil {
		t.Fatal(err)
	}

	items := []model.Item{
		{SubID: subID, GUID: "a5", Title: "[A组] 测试 - 05 [1080p]", URI: "magnet:a5",
			Episode: 5, Fansub: "A组", SizeBytes: 2000000000, Status: model.ItemFailed, Reason: "断种换链：下载失败"},
		{SubID: subID, GUID: "b5", Title: "[B组] 测试 - 05 [720p]", URI: "magnet:b5",
			Episode: 5, Fansub: "B组", SizeBytes: 1000000000, Status: model.ItemRejected, Reason: "同集已有更优版本"},
		{SubID: subID, GUID: "c5", Title: "[C组] 测试 - 05 [1080p]", URI: "magnet:c5",
			Episode: 5, Fansub: "C组", SizeBytes: 3000000000, Status: model.ItemRejected, Reason: "同集已有更优版本"},
		{SubID: subID, GUID: "d6", Title: "[A组] 测试 - 06 [1080p]", URI: "magnet:d6",
			Episode: 6, Fansub: "A组", SizeBytes: 2000000000, Status: model.ItemDone},
	}
	ids := make([]int64, 0, len(items))
	for i := range items {
		id, _, err := st.InsertItemIfNew(ctx, &items[i])
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	alts, err := st.AltItemsForEpisode(ctx, subID, 5, ids[0], 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(alts) != 2 {
		t.Fatalf("第 5 集应当有 2 个候选（B组、C组），实际 %d 个", len(alts))
	}
	if alts[0].Fansub != "C组" {
		t.Errorf("候选应当按体积倒序（C组 3GB 在前），实际 %s", alts[0].Fansub)
	}
	for _, it := range alts {
		if it.ID == ids[0] {
			t.Error("刚失败的条目不该出现在候选里")
		}
		if it.Episode != 5 {
			t.Errorf("候选串集了：%s 是第 %v 集", it.Title, it.Episode)
		}
	}

	// 已入库（done）的条目不能当候选，第 6 集因此没有候选。
	alts6, err := st.AltItemsForEpisode(ctx, subID, 6, ids[3], 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(alts6) != 0 {
		t.Errorf("第 6 集已入库，不该有候选，实际 %d 个", len(alts6))
	}

	n, err := st.CountRelinks(ctx, subID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("换链计数应当为 1（只有 A组 那条被标记），实际 %d", n)
	}
}

// 升级老库：tasks 没有 progress_at 时补列并用 created_at 回填，
// 否则一升级所有在跑的任务都会被当成「断种几万小时」。
// byPathOnly 方便断言：把某个路径的 series_id 渲染成字符串。
func byPathOnly(ents []model.LibraryEntry, path string) string {
	for _, e := range ents {
		if e.Path == path {
			return fmt.Sprintf("%d", e.SeriesID)
		}
	}
	return "缺失"
}

func TestMigrateBackfillsProgressAt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	subID, _ := st.CreateSubscription(ctx, &model.Subscription{
		Name: "老库番剧", FeedURL: "https://example.invalid/rss", IntervalMin: 30, Enabled: true,
	})
	itID, _, err := st.InsertItemIfNew(ctx, &model.Item{
		SubID: subID, GUID: "g1", Title: "[A组] 老番 - 01 [1080p]", URI: "magnet:g1", Episode: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := st.CreateTask(ctx, &model.Task{
		ItemID: itID, Title: "[A组] 老番 - 01 [1080p]", Downloader: "aria2",
		GID: "gid-old", State: model.TaskDownloading, SavePath: "/tmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 造出「老结构」：去掉 progress_at 这一列。
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE tasks DROP COLUMN progress_at`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("升级老库失败: %v", err)
	}
	defer st2.Close()
	task, err := st2.GetTask(ctx, taskID)
	if err != nil || task == nil {
		t.Fatal(err)
	}
	if task.ProgressAt.IsZero() {
		t.Fatal("progress_at 没有回填，会让在跑的任务立刻被判成断种")
	}
	if !task.ProgressAt.Equal(task.CreatedAt) {
		t.Errorf("回填值应当等于 created_at：progress_at=%v created_at=%v", task.ProgressAt, task.CreatedAt)
	}

	// 进度没变时对账不该推进 progress_at，变了才推进。
	before := task.ProgressAt
	if err := st2.UpdateTaskProgress(ctx, taskID, "gid-old", model.TaskDownloading, 0, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	task, _ = st2.GetTask(ctx, taskID)
	if !task.ProgressAt.Equal(before) {
		t.Error("进度没变化时 progress_at 不该动")
	}
	// 时间戳精度是秒：等一秒才能看出「推进了」。
	time.Sleep(1100 * time.Millisecond)
	if err := st2.UpdateTaskProgress(ctx, taskID, "gid-old", model.TaskDownloading, 3.5, 100, 3, ""); err != nil {
		t.Fatal(err)
	}
	task, _ = st2.GetTask(ctx, taskID)
	if !task.ProgressAt.After(before) {
		t.Error("进度变了就该推进 progress_at")
	}
}

// 刷新番剧级元数据前要先拿到一条已入库的文件路径：这里验证按番剧名找得到，
// 没入库的番剧（或空名字）安静返回空串，不报错。
func TestLibraryPathForSeries(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "suzu.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got, err := st.LibraryPathForSeries(ctx, 3141, "葬送的芙莉莲"); err != nil || got != "" {
		t.Fatalf("空库应当返回空串: %q %v", got, err)
	}
	if got, err := st.LibraryPathForSeries(ctx, 0, ""); err != nil || got != "" {
		t.Fatalf("没有任何线索时应当直接返回: %q %v", got, err)
	}

	for i, p := range []string{"/lib/a/ep1.mkv", "/lib/a/ep2.mkv"} {
		if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{
			SeriesID: 3141, Title: "葬送的芙莉莲 / Sousou no Frieren", Path: p, Episode: float64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{SeriesID: 2996, Title: "地狱乐", Path: "/lib/b/ep1.mkv"}); err != nil {
		t.Fatal(err)
	}

	// 按 id 找：标题改了也照样命中。
	got, err := st.LibraryPathForSeries(ctx, 3141, "葬送のフリーレン")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/lib/a/ep2.mkv" {
		t.Errorf("按番剧 id 应当拿到最新入库的那条路径: %q", got)
	}
	if other, err := st.LibraryPathForSeries(ctx, 2996, ""); err != nil || other != "/lib/b/ep1.mkv" {
		t.Errorf("别的番剧也要找得到: %q %v", other, err)
	}
	// 老库（这个列还不存在时入库的）只有标题可用：退回按标题匹配。
	if old, err := st.LibraryPathForSeries(ctx, 0, "地狱乐"); err != nil || old != "/lib/b/ep1.mkv" {
		t.Errorf("按标题的兜底查询失效: %q %v", old, err)
	}
}

// 加 series_id 之前入库的老记录：升级时要按番剧名回填一次，
// 否则刷新番剧元数据时找不到媒体库里的文件。
func TestMigrateBackfillsSeriesID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "suzu.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBangumi(ctx, &model.Bangumi{ID: 3141, Title: "葬送的芙莉莲"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{
		Title: "葬送的芙莉莲", Path: "/lib/a/ep1.mkv", Episode: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 标题对不上，但文件路径里有番剧目录（媒体库命名规则里 {title} 就是番剧名）。
	if err := st.UpsertBangumi(ctx, &model.Bangumi{ID: 2996, Title: "地狱乐"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{
		Title: "[字幕组] 地狱乐 - 01 [1080p]", Path: "/lib/2023春/地狱乐/第 01 季/地狱乐 - S01E01.mkv", Episode: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLibraryEntry(ctx, &model.LibraryEntry{
		Title: "手动订的番（库里没有番剧记录）", Path: "/lib/c/ep1.mkv", Episode: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 造一个"旧库"：把这一列删掉（SQLite 3.35+ 支持 DROP COLUMN）。
	if _, err := st.db.Exec(`ALTER TABLE library DROP COLUMN series_id`); err != nil {
		t.Fatalf("造旧库失败: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重开旧库失败: %v", err)
	}
	defer st2.Close()

	if got, err := st2.LibraryPathForSeries(ctx, 3141, "葬送的芙莉莲"); err != nil || got != "/lib/a/ep1.mkv" {
		t.Errorf("回填后应当按番剧 id 找到: %q %v", got, err)
	}
	// 库里没有对应番剧记录的那条保持 0，还能靠标题兜底查到。
	if got, err := st2.LibraryPathForSeries(ctx, 0, "手动订的番（库里没有番剧记录）"); err != nil || got != "/lib/c/ep1.mkv" {
		t.Errorf("没有番剧记录的入库条目应当保持可用: %q %v", got, err)
	}
	ents, err := st2.ListLibrary(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]int64{}
	for _, e := range ents {
		byPath[e.Path] = e.SeriesID
	}
	if byPath["/lib/a/ep1.mkv"] != 3141 || byPath["/lib/c/ep1.mkv"] != 0 {
		t.Errorf("回填结果不对: %+v", byPath)
	}
	if !strings.Contains(byPathOnly(ents, "/lib/2023春/地狱乐/第 01 季/地狱乐 - S01E01.mkv"), "2996") {
		t.Errorf("按路径回填失败: %+v", byPath)
	}
}

// 「补下往期」清掉的是被集数起点挡掉的记录，且只清这一种。
//
// 清早了/清多了都会出事：条目按 guid 去重，不清就补不回来（用户改了设置却
// 什么都没发生）；而把「命中排除条件」或已投递的记录一起删掉，则会让下一轮
// 轮询重新投递用户明确不要的东西。
func TestClearEpBlockedOnlyRemovesEpisodeFilter(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	subID, err := st.CreateSubscription(ctx, &model.Subscription{
		Name: "完结番 [A组]", FeedURL: "https://example.invalid/rss",
		IntervalMin: 30, Enabled: true, StartEp: 13,
	})
	if err != nil {
		t.Fatal(err)
	}
	put := func(it model.Item) {
		it.SubID = subID
		if _, _, err := st.InsertItemIfNew(ctx, &it); err != nil {
			t.Fatalf("写入条目 %s: %v", it.GUID, err)
		}
	}
	for i := 1; i <= 3; i++ {
		put(model.Item{GUID: fmt.Sprintf("e%d", i), Title: fmt.Sprintf("[A组] 完结番 - %02d", i),
			URI: "magnet:x", Episode: float64(i), Fansub: "A组",
			Status: model.ItemRejected, Reason: model.ReasonEpBelowStart})
	}
	// 被排除条件挡掉的：补下往期后仍然不该下
	put(model.Item{GUID: "ex", Title: "[B组] 完结番 - 05 特典", URI: "magnet:x",
		Episode: 5, Fansub: "B组", Status: model.ItemRejected, Reason: "命中排除条件：特典"})
	// 已经投递出去的
	put(model.Item{GUID: "done", Title: "[A组] 完结番 - 13", URI: "magnet:x",
		Episode: 13, Fansub: "A组", Status: model.ItemDone})

	counts, err := st.CountEpBlockedBySub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[subID] != 3 {
		t.Fatalf("面板要显示「补下往期 3」，实际 %d", counts[subID])
	}

	cleared, err := st.ClearEpBlocked(ctx, subID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 3 {
		t.Fatalf("应当清掉 3 条被集数挡掉的记录，实际 %d", cleared)
	}

	items, err := st.ListItems(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("其余记录不该被删，期望还剩 2 条，实际 %d：%+v", len(items), items)
	}
	for _, it := range items {
		if it.GUID == "ex" && it.Status != model.ItemRejected {
			t.Errorf("被排除条件挡掉的条目状态被改了：%+v", it)
		}
	}
	if after, err := st.CountEpBlockedBySub(ctx); err != nil || after[subID] != 0 {
		t.Errorf("清完之后计数应当归零，实际 %d（err=%v）", after[subID], err)
	}
}
