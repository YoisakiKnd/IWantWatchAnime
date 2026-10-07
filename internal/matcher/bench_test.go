package matcher

import "testing"

// 解析是每次轮询都要跑几十上百次的路径，所以在弱 CPU 上它的成本直接决定
// 一轮轮询的耗时。这个基准是「armv7 上能不能跑得动」的量化依据。
var benchTitles = []string{
	"[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 - 01 [WebRip 1080p HEVC-10bit AAC][简繁内封字幕].mkv",
	"【星空字幕组】【10月新番】药屋少女的呢喃 第24话 [1080P][简日双语]",
	"[Lilith-Raws] 我推的孩子 - 05 [Baha][WEB-DL][1080p][AVC AAC][CHT][MP4]",
	"孤独摇滚!.S01E08.1080p.WEB-DL.HEVC.AAC",
	"[SubGroup] 夏日重现 [01-25][合集][1080p][简繁]",
	"【喵萌奶茶屋】★10月新番★[葬送的芙莉莲][03][1080p][简日双语][招募翻译]",
}

func BenchmarkParseTitle(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ParseTitle(benchTitles[i%len(benchTitles)])
	}
}

func BenchmarkRuleJudge(b *testing.B) {
	r := Rule{Include: []string{"1080"}, Exclude: []string{"招募"}, Prefer: []string{"喵萌奶茶屋"}, StartEp: 1}
	p := ParseTitle(benchTitles[0])
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Judge(p, p.Raw)
	}
}
