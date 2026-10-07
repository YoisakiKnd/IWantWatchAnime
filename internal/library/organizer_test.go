package library

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
)

// 番剧级刮削落地：一次整理要写出 tvshow.nfo 与 poster.jpg，
// 让 Jellyfin / Kodi 打开番剧目录就直接有海报和番剧信息，不用装插件。
func TestOrganizeWritesSeriesMeta(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "downloads", "ep.mkv")
	writeBytes(t, src, "假正片内容")
	cover := filepath.Join(dir, "covers", "3141.jpg")
	writeBytes(t, cover, "假封面字节")

	cfg := config.Default().Library
	cfg.Root = filepath.Join(dir, "library")
	cfg.NameRule = "{group}/{title}/第 {season:02d} 季/{title} - S{season:02d}E{episode:02d}"
	cfg.LinkMode = "hardlink"

	o := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	in := Input{
		TaskID: 1,
		Source: src,
		Parsed: matcher.Parsed{Title: "葬送的芙莉莲", Season: 1, Episode: 3, Kind: matcher.KindTV},
		Meta: &Meta{
			ID: "3141", Title: "Sousou no Frieren", TitleCN: "葬送的芙莉莲",
			Summary: "勇者一行打倒魔王之后的故事。", Date: "2023-09-29", Poster: cover, Score: 8.5,
		},
		Group: "2024秋",
	}
	res, err := o.Organize(context.Background(), in)
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}

	seasonDir := filepath.Join(cfg.Root, "2024秋", "葬送的芙莉莲", "第 01 季")
	if res.Path != filepath.Join(seasonDir, "葬送的芙莉莲 - S01E03.mkv") {
		t.Fatalf("正片落点不对: %s", res.Path)
	}
	// 番剧级文件要落在「第 01 季」的上一层。
	seriesDir := filepath.Dir(seasonDir)
	if res.TVNFOPath != filepath.Join(seriesDir, "tvshow.nfo") {
		t.Errorf("tvshow.nfo 落点不对: %s", res.TVNFOPath)
	}
	if res.Poster != filepath.Join(seriesDir, "poster.jpg") {
		t.Errorf("poster.jpg 落点不对: %s", res.Poster)
	}

	nfo, err := os.ReadFile(res.TVNFOPath)
	if err != nil {
		t.Fatalf("读 tvshow.nfo: %v", err)
	}
	// plot/rating/thumb 三样都来自刮削：蜜柑给封面，Bangumi 给简介与评分。
	for _, want := range []string{
		"<tvshow>", "葬送的芙莉莲", "Sousou no Frieren", "2023-09-29", "3141",
		"勇者一行", "<rating>8.5</rating>", "<thumb>poster.jpg</thumb>", "<year>2023</year>",
	} {
		if !strings.Contains(string(nfo), want) {
			t.Errorf("tvshow.nfo 缺少 %q", want)
		}
	}

	gotCover, err := os.ReadFile(res.Poster)
	if err != nil {
		t.Fatalf("读 poster.jpg: %v", err)
	}
	if string(gotCover) != "假封面字节" {
		t.Errorf("poster.jpg 内容不是封面缓存: %q", gotCover)
	}

	// 没有简介时不能写出空 <plot/>：媒体库对空标签的处理比缺标签更难看。
	blank := filepath.Join(dir, "library2", "2024秋", "无简介番剧", "第 01 季")
	cfg2 := cfg
	cfg2.Root = filepath.Join(dir, "library2")
	o2 := New(cfg2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	in2 := in
	in2.Meta = &Meta{ID: "1", Title: "无简介番剧", TitleCN: "无简介番剧", Poster: cover}
	in2.Parsed = matcher.Parsed{Title: "无简介番剧", Season: 1, Episode: 1, Kind: matcher.KindTV}
	in2.Source = filepath.Join(dir, "downloads", "ep1.mkv")
	writeBytes(t, in2.Source, "第一集")
	r2, err := o2.Organize(context.Background(), in2)
	if err != nil {
		t.Fatalf("无简介整理失败: %v", err)
	}
	nfo2, _ := os.ReadFile(r2.TVNFOPath)
	if strings.Contains(string(nfo2), "<plot>") || strings.Contains(string(nfo2), "<rating>") {
		t.Errorf("没有简介/评分时不该写出空标签:\n%s", nfo2)
	}
	if !strings.Contains(string(nfo2), "<thumb>poster.jpg</thumb>") {
		t.Errorf("有海报时应当写 <thumb>:\n%s", nfo2)
	}
	_ = blank

	// 第二集再跑一遍：番剧级文件内容没变就不该落盘（海报是"存在即跳过"，
	// NFO 是"内容变了才写"），白写一遍只是磨损存储卡。
	epoch := time.Unix(1000000000, 0)
	if err := os.Chtimes(res.TVNFOPath, epoch, epoch); err != nil {
		t.Fatal(err)
	}
	in.Parsed.Episode = 4
	in.Source = filepath.Join(dir, "downloads", "ep4.mkv")
	writeBytes(t, in.Source, "第四集")
	res2, err := o.Organize(context.Background(), in)
	if err != nil {
		t.Fatalf("第二次整理失败: %v", err)
	}
	st, err := os.Stat(res2.TVNFOPath)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(epoch) {
		t.Error("tvshow.nfo 内容没变却被改写")
	}
	if res2.Poster == "" {
		t.Error("第二次整理应当仍然返回已有海报路径")
	}
}

// 没有番剧信息（纯手动订的 RSS）时不能写 tvshow.nfo，但正片照样入库。
func TestOrganizeWithoutMeta(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mkv")
	writeBytes(t, src, "x")

	cfg := config.Default().Library
	cfg.Root = filepath.Join(dir, "library")
	o := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := o.Organize(context.Background(), Input{
		Source: src,
		Parsed: matcher.Parsed{Title: "手动订阅的番", Season: 1, Episode: 1, Kind: matcher.KindTV},
	})
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}
	if res.TVNFOPath != "" || res.Poster != "" {
		t.Errorf("没有刮削信息时不该写番剧级文件: %q %q", res.TVNFOPath, res.Poster)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Errorf("正片应当已入库: %v", err)
	}
}

func writeBytes(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 简介/评分比入库晚到时的补救：老库里的半成品 NFO 必须能被对齐，
// 否则用户只能删了 NFO 等下一集——没人会那么干。
func TestRefreshSeriesFillsLateMetadata(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "downloads", "ep.mkv")
	writeBytes(t, src, "假正片")
	cover := filepath.Join(dir, "covers", "3141.jpg")
	writeBytes(t, cover, "假封面")

	cfg := config.Default().Library
	cfg.Root = filepath.Join(dir, "library")
	cfg.NameRule = "{group}/{title}/第 {season:02d} 季/{title} - S{season:02d}E{episode:02d}"
	o := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 第一步：入库时只有蜜柑的信息（有封面、有日期，没有简介与评分）。
	res, err := o.Organize(context.Background(), Input{
		Source: src,
		Parsed: matcher.Parsed{Title: "葬送的芙莉莲", Season: 1, Episode: 1, Kind: matcher.KindTV},
		Meta:   &Meta{ID: "3141", Title: "Sousou no Frieren", TitleCN: "葬送的芙莉莲", Date: "2023-09-29"},
		Group:  "2024秋",
	})
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}
	before, err := os.ReadFile(res.TVNFOPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "<plot>") || strings.Contains(string(before), "<rating>") {
		t.Fatalf("入库时还没有简介/评分，不该写出这些标签:\n%s", before)
	}
	if res.Poster != "" {
		t.Fatalf("入库时没给封面，不该有海报文件: %s", res.Poster)
	}

	// 第二步：Bangumi 补到了简介与评分，用库里存的路径对齐。
	tv, poster, err := o.RefreshSeries(res.Path, &Meta{
		ID: "3141", Title: "Sousou no Frieren", TitleCN: "葬送的芙莉莲",
		Date: "2023-09-29", Summary: "勇者一行打倒魔王之后的故事。", Score: 8.5, Poster: cover,
	})
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if tv != res.TVNFOPath {
		t.Errorf("刷新的 NFO 路径不对: %s", tv)
	}
	after, err := os.ReadFile(tv)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<plot>勇者一行打倒魔王之后的故事。</plot>", "<rating>8.5</rating>", "<thumb>poster.jpg</thumb>", "3141"} {
		if !strings.Contains(string(after), want) {
			t.Errorf("刷新后的 tvshow.nfo 缺少 %q:\n%s", want, after)
		}
	}
	if poster != filepath.Join(filepath.Dir(res.TVNFOPath), "poster.jpg") {
		t.Errorf("海报应当补回番剧根目录: %s", poster)
	}
	if b, _ := os.ReadFile(poster); string(b) != "假封面" {
		t.Errorf("补回来的海报内容不对: %q", b)
	}

	// 第三步：把海报删掉再刷一次，图片能补回来，但 NFO 内容没变、不该重写。
	if err := os.Remove(poster); err != nil {
		t.Fatal(err)
	}

	epoch := time.Unix(1000000000, 0)
	if err := os.Chtimes(tv, epoch, epoch); err != nil {
		t.Fatal(err)
	}
	if _, p2, err := o.RefreshSeries(res.Path, &Meta{
		ID: "3141", Title: "Sousou no Frieren", TitleCN: "葬送的芙莉莲",
		Date: "2023-09-29", Summary: "勇者一行打倒魔王之后的故事。", Score: 8.5, Poster: cover,
	}); err != nil {
		t.Fatalf("二次刷新失败: %v", err)
	} else if b, err := os.ReadFile(p2); err != nil || string(b) != "假封面" {
		t.Errorf("被删掉的海报应当补回来: %q %v", b, err)
	}
	st, err := os.Stat(tv)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(epoch) {
		t.Error("元数据没变却又写了一遍 NFO")
	}

	// 落地路径为空（还没入库）时安静返回，不建空目录。
	if tv2, _, err := o.RefreshSeries("", &Meta{Title: "谁"}); err != nil || tv2 != "" {
		t.Errorf("空路径应当安静返回: tv=%q err=%v", tv2, err)
	}
}

// 海报源丢了（封面没抓到、缓存被清）时，NFO 里不能留下一条指向不存在文件的
// <thumb>poster.jpg</thumb>：媒体库会一直去找那张图，而它永远不存在。
func TestMissingPosterSourceLeavesNoDanglingThumb(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mkv")
	writeBytes(t, src, "video")

	cfg := config.Default().Library
	cfg.Root = filepath.Join(dir, "library")
	o := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := o.Organize(context.Background(), Input{
		Source: src,
		Parsed: matcher.Parsed{Title: "有简介没海报", Season: 1, Episode: 1, Kind: matcher.KindTV},
		Meta: &Meta{
			Title:   "有简介没海报",
			Summary: "简介在，海报源不在。",
			Poster:  filepath.Join(dir, "已经被删掉的封面.jpg"),
		},
	})
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}
	if res.Poster != "" {
		t.Errorf("海报没落盘就不该报出路径: %q", res.Poster)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(res.TVNFOPath), "poster.jpg")); err == nil {
		t.Errorf("不该凭空出现 poster.jpg")
	}
	nfo, err := os.ReadFile(res.TVNFOPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nfo), "poster.jpg") {
		t.Errorf("海报没落盘，NFO 不该提 poster.jpg:\n%s", nfo)
	}
	if !strings.Contains(string(nfo), "<plot>简介在，海报源不在。</plot>") {
		t.Errorf("简介该照写:\n%s", nfo)
	}
}

// 海报落了盘的正常路径：NFO 指向的就是那个文件。
func TestPosterPresentMeansThumbPointsAtIt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ep.mkv")
	writeBytes(t, src, "video")
	cover := filepath.Join(dir, "cover.jpg")
	writeBytes(t, cover, "jpegbytes")

	cfg := config.Default().Library
	cfg.Root = filepath.Join(dir, "library")
	o := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := o.Organize(context.Background(), Input{
		Source: src,
		Parsed: matcher.Parsed{Title: "有海报", Season: 1, Episode: 1, Kind: matcher.KindTV},
		Meta:   &Meta{Title: "有海报", Poster: cover},
	})
	if err != nil {
		t.Fatalf("整理失败: %v", err)
	}
	if res.Poster == "" {
		t.Fatal("海报应当落盘")
	}
	if _, err := os.Stat(res.Poster); err != nil {
		t.Fatalf("海报文件不在: %v", err)
	}
	nfo, _ := os.ReadFile(res.TVNFOPath)
	if !strings.Contains(string(nfo), "<thumb>poster.jpg</thumb>") {
		t.Errorf("NFO 该指向本地海报:\n%s", nfo)
	}
}
