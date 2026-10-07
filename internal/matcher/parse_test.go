package matcher

import "testing"

// 这些标题来自 mikan / dmhy / nyaa 真实发布习惯，改动解析逻辑必须保证它们全绿。
func TestParseTitle(t *testing.T) {
	cases := []struct {
		raw        string
		title      string
		episode    float64
		episodeTo  float64
		season     int
		fansub     string
		kind       Kind
		batch      bool
		resolution string
	}{
		{
			raw:        "[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 - 01 [WebRip 1080p HEVC-10bit AAC][简繁内封字幕].mkv",
			title:      "葬送的芙莉莲",
			episode:    1,
			season:     1,
			fansub:     "喵萌奶茶屋&LoliHouse",
			kind:       KindTV,
			resolution: "1080p",
		},
		{
			raw:     "【星空字幕组】【10月新番】药屋少女的呢喃 第24话 [1080P][简日双语]",
			season:  1,
			title:   "药屋少女的呢喃",
			episode: 24,
			fansub:  "星空字幕组",
			kind:    KindTV,
		},
		{
			raw:        "[Lilith-Raws] 我推的孩子 - 05 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4]",
			season:     1,
			title:      "我推的孩子",
			episode:    5,
			fansub:     "Lilith-Raws",
			kind:       KindTV,
			resolution: "1080p",
		},
		{
			raw:     "孤独摇滚!.S01E08.1080p.WEB-DL.HEVC.AAC",
			title:   "孤独摇滚!",
			episode: 8,
			season:  1,
			kind:    KindTV,
		},
		{
			raw:     "[ANi] 转生成为史莱姆的那件事 第三季 - 12 [1080P][Baha][WEB-DL][AAC AVC][CHT][MP4]",
			title:   "转生成为史莱姆的那件事 第三季",
			episode: 12,
			season:  3,
			fansub:  "ANi",
			kind:    KindTV,
		},
		{
			// 蜜柑上真实存在的写法：区间后面还跟着「修正合集」。
			raw:       "[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 / Sousou no Frieren [01-28 修正合集][WebRip 1080p HEVC-10bit AAC][简繁日内封字幕]",
			season:    1,
			title:     "葬送的芙莉莲 / Sousou no Frieren",
			episode:   1,
			episodeTo: 28,
			fansub:    "喵萌奶茶屋&LoliHouse",
			batch:     true,
			kind:      KindTV,
		},
		{
			// 只给总集数的合集：能抠出区间，才不会被命名成 S01E00。
			raw:       "[LoliHouse] 葬送的芙莉莲 / Sousou no Frieren [全28话][WebRip 1080p HEVC-10bit AAC]",
			season:    1,
			title:     "葬送的芙莉莲 / Sousou no Frieren",
			episode:   1,
			episodeTo: 28,
			fansub:    "LoliHouse",
			batch:     true,
			kind:      KindTV,
		},
		{
			raw:       "[SubGroup] 夏日重现 [01-25][合集][1080p][简繁]",
			season:    1,
			title:     "夏日重现",
			episode:   1,
			episodeTo: 25,
			fansub:    "SubGroup",
			batch:     true,
			kind:      KindTV,
		},
		{
			raw:    "[桜都字幕组] 铃芽之旅 剧场版 [1080p][简繁内封]",
			season: 1,
			title:  "铃芽之旅 剧场版",
			fansub: "桜都字幕组",
			kind:   KindMovie,
		},
		{
			raw:     "【喵萌奶茶屋】★10月新番★[葬送的芙莉莲][03][1080p][简日双语][招募翻译]",
			season:  1,
			title:   "葬送的芙莉莲",
			episode: 3,
			fansub:  "喵萌奶茶屋",
			kind:    KindTV,
		},
		{
			raw:     "「番剧」某科学的超电磁炮T - 第07话 [720p][CHS]",
			season:  1,
			title:   "某科学的超电磁炮T",
			episode: 7,
			kind:    KindTV,
		},
		{
			raw:     "[LoliHouse] 间谍过家家 - 02 [1080p][HEVC_AAC]",
			season:  1,
			title:   "间谍过家家",
			episode: 2,
			fansub:  "LoliHouse",
			kind:    KindTV,
		},
		{
			// 日期不能当成集数；发布公告类条目标题应当被兜底保留。
			raw:    "[组务][2024-10-06] 本组本月停更公告",
			season: 1,
			title:  "本组本月停更公告",
			kind:   KindTV,
		},
		{
			raw:     "[ANi] 胆大党 第二季 - 11.5 [1080P][Baha]",
			title:   "胆大党 第二季",
			episode: 11.5,
			season:  2,
			fansub:  "ANi",
			kind:    KindTV,
		},
	}

	for _, c := range cases {
		got := ParseTitle(c.raw)
		if got.Title != c.title {
			t.Errorf("标题不符\n 原始: %s\n 期望: %q\n 实际: %q", c.raw, c.title, got.Title)
		}
		if got.Episode != c.episode {
			t.Errorf("集数不符\n 原始: %s\n 期望: %v\n 实际: %v", c.raw, c.episode, got.Episode)
		}
		if got.EpisodeTo != c.episodeTo {
			t.Errorf("结束集不符\n 原始: %s\n 期望: %v\n 实际: %v", c.raw, c.episodeTo, got.EpisodeTo)
		}
		if got.Season != c.season {
			t.Errorf("季数不符\n 原始: %s\n 期望: %v\n 实际: %v", c.raw, c.season, got.Season)
		}
		if c.fansub != "" && got.Fansub != c.fansub {
			t.Errorf("字幕组不符\n 原始: %s\n 期望: %q\n 实际: %q", c.raw, c.fansub, got.Fansub)
		}
		if got.Kind != c.kind {
			t.Errorf("形态不符\n 原始: %s\n 期望: %q\n 实际: %q", c.raw, c.kind, got.Kind)
		}
		if got.Batch != c.batch {
			t.Errorf("合集标记不符\n 原始: %s\n 期望: %v\n 实际: %v", c.raw, c.batch, got.Batch)
		}
		if c.resolution != "" && got.Resolution != c.resolution {
			t.Errorf("分辨率不符\n 原始: %s\n 期望: %q\n 实际: %q", c.raw, c.resolution, got.Resolution)
		}
	}
}

func TestRuleJudge(t *testing.T) {
	r := Rule{
		Exclude: []string{"招募", "re:生肉|无字幕"},
		Include: []string{"1080", "简"},
		Prefer:  []string{"喵萌奶茶屋", "LoliHouse"},
		StartEp: 2,
	}

	ok := ParseTitle("[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 - 03 [WebRip 1080p HEVC-10bit AAC][简繁内封字幕].mkv")
	if d := r.Judge(ok, ok.Raw); !d.OK || d.Score < 100 {
		t.Fatalf("期望采纳且偏好加分，实际 %+v", d)
	}

	early := ParseTitle("[喵萌奶茶屋] 葬送的芙莉莲 - 01 [1080p]")
	if d := r.Judge(early, early.Raw); d.OK {
		t.Fatalf("起始集过滤失效: %+v", d)
	}

	// 合集也要吃起始集过滤，否则「只跟新的」会被一个 [01-28] 合集整季拉下来。
	batch := ParseTitle("[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 / Sousou no Frieren [01-28 修正合集][WebRip 1080p HEVC-10bit AAC]")
	if d := r.Judge(batch, batch.Raw); d.OK {
		t.Fatalf("起始集应当挡掉往期合集: %+v（解析 ep=%v to=%v batch=%v）", d, batch.Episode, batch.EpisodeTo, batch.Batch)
	}

	excluded := ParseTitle("[喵萌奶茶屋] 葬送的芙莉莲 - 04 [1080p][招募翻译]")
	if d := r.Judge(excluded, excluded.Raw); d.OK {
		t.Fatalf("排除规则失效: %+v", d)
	}

	noInclude := ParseTitle("[其它组] 某番 - 05 [720p][繁中]")
	if d := r.Judge(noInclude, noInclude.Raw); d.OK {
		t.Fatalf("包含规则失效: %+v", d)
	}

	// 空规则集应当全放行。
	if d := (Rule{}).Judge(noInclude, noInclude.Raw); !d.OK {
		t.Fatalf("空规则应放行: %+v", d)
	}
}
