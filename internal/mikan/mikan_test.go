package mikan

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// 固定样本来自 2026-10 的真实页面，只保留了前 250KB（解析需要的部分全在里面）。
// 用真实 HTML 而不是手写片段，是因为蜜柑的排版被改过好几次，
// 只有真实样本才能在它下次改版时第一时间把这个测试跑红。

func TestScanSearch(t *testing.T) {
	f, err := os.Open("testdata/search.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	list, err := scanSearch(f)
	if err != nil {
		t.Fatalf("解析搜索页: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("期望 2 张番剧卡片，实际 %d 张：%+v", len(list), list)
	}
	if list[0].ID != 3141 || list[0].Title != "葬送的芙莉莲" {
		t.Errorf("第一张卡片解析错误: id=%d title=%q", list[0].ID, list[0].Title)
	}
	if list[0].CoverPath != "/images/Bangumi/202309/5ce9fed1.jpg" {
		t.Errorf("封面路径应去掉缩放参数，实际 %q", list[0].CoverPath)
	}
	if list[1].ID != 3821 || list[1].Title != "葬送的芙莉莲 第二季" {
		t.Errorf("第二张卡片解析错误: id=%d title=%q", list[1].ID, list[1].Title)
	}
}

func TestScanDetail(t *testing.T) {
	f, err := os.Open("testdata/bangumi.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	b, err := scanDetail(f)
	if err != nil {
		t.Fatalf("解析番剧页: %v", err)
	}
	if b.Title != "地狱乐" {
		t.Errorf("标题 = %q，期望 地狱乐", b.Title)
	}
	if b.CoverPath != "/images/Bangumi/202304/ba3dc557.jpg" {
		t.Errorf("封面 = %q", b.CoverPath)
	}
	if b.AirText != "星期六" {
		t.Errorf("放送日期 = %q，期望 星期六", b.AirText)
	}
	if b.Episodes != 13 {
		t.Errorf("总集数 = %d，期望 13", b.Episodes)
	}
	if b.StartAt != "4/1/2023" {
		t.Errorf("放送开始 = %q", b.StartAt)
	}
	if b.Official != "https://www.jigokuraku.com/" {
		t.Errorf("官网 = %q", b.Official)
	}
	if len(b.Subgroups) != 8 {
		t.Fatalf("字幕组 = %d 个，期望 8 个：%+v", len(b.Subgroups), b.Subgroups)
	}
	want := map[int64]string{
		1254: "7³ACG", 370: "LoliHouse", 243: "风之圣殿", 213: "爱恋字幕社",
		382: "喵萌奶茶屋", 615: "Kirara Fantasia", 571: "Amor个人发布", 592: "Amor字幕组",
	}
	for _, sg := range b.Subgroups {
		if want[sg.ID] != sg.Name {
			t.Errorf("字幕组 %d 名字 = %q，期望 %q", sg.ID, sg.Name, want[sg.ID])
		}
		if !strings.Contains(sg.RSS, "subgroupid=") || subgroupID(sg.RSS) != sg.ID {
			t.Errorf("字幕组 %d 的 RSS 不对: %q", sg.ID, sg.RSS)
		}
	}
}

// 页面被截断（蜜柑偶尔会在中途断流）时，已经解析到的部分必须照常返回。
func TestScanTruncated(t *testing.T) {
	raw, err := os.ReadFile("testdata/bangumi.html")
	if err != nil {
		t.Fatal(err)
	}
	// 只喂前 20KB：够拿到封面与标题，字幕组只可能拿到第一个。
	b, err := scanDetail(strings.NewReader(string(raw[:20<<10])))
	if err != nil {
		t.Fatalf("截断页面不应报错: %v", err)
	}
	if b.Title != "地狱乐" || b.CoverPath == "" {
		t.Errorf("截断后仍应拿到标题与封面: %+v", b)
	}
	if len(b.Subgroups) > 1 {
		t.Errorf("截断到 20KB 只该看到 1 个字幕组，实际 %d", len(b.Subgroups))
	}
}

// 读取上限必须真的生效：给一个超大的假页面，解析器不能把内存吃穿。
func TestSearchBudget(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<ul class="list-inline an-ul">`)
	for i := 0; i < 40000; i++ {
		sb.WriteString(`<li><a href="/Home/Bangumi/1"><div class="an-text" title="x">x</div></a></li>`)
	}
	huge := sb.String()
	if len(huge) < maxSearchBytes {
		t.Fatalf("样本不够大：%d 字节", len(huge))
	}
	list, err := scanSearch(strings.NewReader(huge))
	if err != nil {
		t.Fatalf("超长页面应被上限截断而非报错: %v", err)
	}
	// 同一个 id 会被去重，这里只关心「没有读完全部 40000 条」。
	if len(list) != 1 {
		t.Errorf("重复 id 应去重为 1 条，实际 %d", len(list))
	}
}

func TestPickSubgroups(t *testing.T) {
	c := New(Options{Enabled: true, Base: "https://mikanani.me", MinGap: time.Millisecond}, nil, nil)
	b := &model.Bangumi{ID: 2996, Subgroups: []model.Subgroup{
		{ID: 1, Name: "爱恋字幕社", RSS: "/RSS/Bangumi?bangumiId=2996&subgroupid=1"},
		{ID: 2, Name: "喵萌奶茶屋", RSS: "/RSS/Bangumi?bangumiId=2996&subgroupid=2"},
		{ID: 3, Name: "LoliHouse", RSS: "/RSS/Bangumi?bangumiId=2996&subgroupid=3"},
	}}
	got := c.PickSubgroups(b, []string{"喵萌", "LoliHouse"})
	if got[0].Name != "喵萌奶茶屋" || got[1].Name != "LoliHouse" || got[2].Name != "爱恋字幕社" {
		t.Fatalf("偏好排序错误: %s / %s / %s", got[0].Name, got[1].Name, got[2].Name)
	}
	if best, ok := c.Best(b, []string{"lolihouse"}); !ok || best.Name != "LoliHouse" {
		t.Errorf("Best 应命中小写偏好，实际 %+v", best)
	}
	// 偏好为空时保持蜜柑原序（通常是活跃度顺序）。
	got = c.PickSubgroups(b, nil)
	if got[0].ID != 1 {
		t.Errorf("无偏好时应保持原序，实际第一个是 %s", got[0].Name)
	}
}

func TestURLBuilding(t *testing.T) {
	c := New(Options{Enabled: true, Base: "https://mikanani.me/", MinGap: time.Millisecond}, nil, nil)
	if got := c.CoverURL("/images/Bangumi/202304/ba3dc557.jpg"); got !=
		"https://mikanani.me/images/Bangumi/202304/ba3dc557.jpg?width=400&height=560&format=jpg" {
		t.Errorf("CoverURL = %q", got)
	}
	if got := c.RSSURL(model.Subgroup{RSS: "/RSS/Bangumi?bangumiId=2996&subgroupid=1254"}); got !=
		"https://mikanani.me/RSS/Bangumi?bangumiId=2996&subgroupid=1254" {
		t.Errorf("RSSURL = %q", got)
	}
	if got := c.BangumiURL(2996); got != "https://mikanani.me/Home/Bangumi/2996" {
		t.Errorf("BangumiURL = %q", got)
	}
	if !c.Enabled() {
		t.Error("显式开启时 Enabled() 应为 true")
	}
	if New(Options{Enabled: false}, nil, nil).Enabled() {
		t.Error("关闭时 Enabled() 应为 false")
	}
}

// 关掉刮削后每个入口都要优雅报错，而不是静默返回空数据。
func TestDisabled(t *testing.T) {
	c := New(Options{Enabled: false, Base: "https://mikanani.me"}, nil, nil)
	ctx := context.Background()
	if _, err := c.Search(ctx, "芙莉莲"); err == nil {
		t.Error("关闭刮削时 Search 应报错")
	}
	if _, err := c.Detail(ctx, 2996); err == nil {
		t.Error("关闭刮削时 Detail 应报错")
	}
	if _, err := c.EnsureCover(ctx, &model.Bangumi{ID: 1, CoverPath: "/x.jpg"}, t.TempDir()); err == nil {
		t.Error("关闭刮削时 EnsureCover 应报错")
	}
}

// 真实网络冒烟测试：默认跳过，只有显式 MIKAN_LIVE=1 时才跑。
// 它不进 CI、不进日常构建——家里的小机器不需要每次 make test 都去打扰蜜柑。
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("MIKAN_LIVE") != "1" {
		t.Skip("设置 MIKAN_LIVE=1 才跑真实抓取")
	}
	c := New(Options{Enabled: true, Base: "https://mikanani.me", MinGap: 1200 * time.Millisecond},
		nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	list, err := c.Search(ctx, "葬送的芙莉莲")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("搜索没结果")
	}
	t.Logf("搜索到 %d 部番剧，第一部 %d %s", len(list), list[0].ID, list[0].Title)

	b, err := c.Detail(ctx, list[0].ID)
	if err != nil {
		t.Fatalf("取详情失败: %v", err)
	}
	t.Logf("详情：%s 放送=%s 集数=%d 字幕组=%d 个", b.Title, b.AirText, b.Episodes, len(b.Subgroups))
	if b.Title == "" || len(b.Subgroups) == 0 {
		t.Fatalf("详情缺字段: %+v", b)
	}
	for _, sg := range b.Subgroups[:min(3, len(b.Subgroups))] {
		t.Logf("  字幕组 %s → %s", sg.Name, c.RSSURL(sg))
	}
	dir := t.TempDir()
	p, err := c.EnsureCover(ctx, b, dir)
	if err != nil {
		t.Fatalf("下载封面失败: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() < 1024 {
		t.Fatalf("封面文件异常: %v %d", err, st.Size())
	}
	t.Logf("封面已缓存: %s（%d 字节）", p, st.Size())
}
