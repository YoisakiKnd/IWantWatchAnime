package library

import (
	"strings"
	"testing"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
)

func TestRender(t *testing.T) {
	v := Vars{
		Title:      "葬送的芙莉莲",
		Group:      "2024秋",
		Fansub:     "喵萌奶茶屋",
		Resolution: "1080p",
		Kind:       string(matcher.KindTV),
		Season:     1,
		Episode:    3,
	}

	cases := []struct {
		rule string
		want string
	}{
		{
			"{group}/{title}/第 {season:02d} 季/{title} - S{season:02d}E{episode:02d}",
			"2024秋/葬送的芙莉莲/第 01 季/葬送的芙莉莲 - S01E03",
		},
		{
			// SxxExx 简写
			"{group}/{title}/{title} [{sxxexx}][{resolution}][{fansub}]",
			"2024秋/葬送的芙莉莲/葬送的芙莉莲 [S01E03][1080p][喵萌奶茶屋]",
		},
		{
			// 未知变量必须被抹掉，不能把 {foo} 写进磁盘
			"{title}/{foo}",
			"葬送的芙莉莲/_",
		},
	}

	for _, c := range cases {
		if got := Render(c.rule, v); got != c.want {
			t.Errorf("规则 %q\n 期望 %q\n 实际 %q", c.rule, c.want, got)
		}
	}
}

// 半集（11.5）不能被格式化成整数，否则会跟第 11 集撞名互相覆盖。
func TestRenderHalfEpisode(t *testing.T) {
	got := Render("{title} - S{season:02d}E{episode:02d}", Vars{
		Title: "胆大党", Kind: string(matcher.KindTV), Season: 2, Episode: 11.5,
	})
	want := "胆大党 - S02E11.5"
	if got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}
}

func TestRenderMovieAndOVA(t *testing.T) {
	got := Render("{title}/{title} [{ep}]", Vars{Title: "铃芽之旅", Kind: string(matcher.KindMovie)})
	if got != "铃芽之旅/铃芽之旅 [MOVIE]" {
		t.Fatalf("剧场版命名不符: %q", got)
	}
	got = Render("{title}/{title} [{ep}]", Vars{Title: "某OVA", Kind: string(matcher.KindOVA)})
	if got != "某OVA/某OVA [OVA]" {
		t.Fatalf("OVA 命名不符: %q", got)
	}
}

// {span} 是给合集用的：单集跟 {episode} 一样是 03，
// 合集渲染成 01-28，于是 S01E01-28 不会和真正的第一集撞名。
func TestRenderSpan(t *testing.T) {
	rule := "{title} - S{season:02d}E{span}"
	single := Render(rule, Vars{Title: "某番", Season: 1, Episode: 3, EpisodeTo: 3, Kind: string(matcher.KindTV)})
	if single != "某番 - S01E03" {
		t.Fatalf("单集命名不符: %q", single)
	}
	batch := Render(rule, Vars{Title: "某番", Season: 1, Episode: 1, EpisodeTo: 28, Batch: true, Kind: string(matcher.KindTV)})
	if batch != "某番 - S01E01-28" {
		t.Fatalf("合集命名不符: %q", batch)
	}
	// 没写 {span} 的老规则行为不变。
	old := Render("{title}/{title} [{episode:02d}]", Vars{Title: "某番", Episode: 1, EpisodeTo: 28, Batch: true})
	if old != "某番/某番 [01]" {
		t.Fatalf("旧规则不该被改动: %q", old)
	}
}

func TestSanitizePath(t *testing.T) {
	cases := map[string]string{
		`Fate/stay night: 无限剑制?`: "Fate/stay night 无限剑制",
		`标题 <with> "quotes"`:     "标题 with quotes",
		`带 空格  的 名字`:             "带 空格 的 名字",
		`尾点.`:                    "尾点",
	}
	for in, want := range cases {
		if got := SanitizePath(in); got != want {
			t.Errorf("SanitizePath(%q)\n 期望 %q\n 实际 %q", in, want, got)
		}
	}

	long := strings.Repeat("字", 200)
	out := SanitizePath(long)
	if len([]rune(out)) > 80 {
		t.Errorf("超长路径段未被截断：%d 个字符", len([]rune(out)))
	}
}

func TestPadNum(t *testing.T) {
	cases := []struct {
		v     float64
		width int
		want  string
	}{
		{1, 2, "01"}, {12, 2, "12"}, {123, 2, "123"}, {0, 2, "00"},
		{11.5, 2, "11.5"}, {3.5, 3, "003.5"},
	}
	for _, c := range cases {
		if got := padNum(c.v, c.width); got != c.want {
			t.Errorf("padNum(%v, %d) = %q，期望 %q", c.v, c.width, got, c.want)
		}
	}
}
