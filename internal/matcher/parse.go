// Package matcher 负责「这条 RSS 是什么番、第几集」以及「要不要下」。
//
// 这是整个项目唯一真正有难度的部分：RSS 标题是各字幕组自己拼的字符串，
// 没有标准。这里用「括号分词 + 有序集数正则 + 元数据剥离」三步处理，
// 不依赖任何外网服务，因此断网也能判定，且单次解析在 armv7 上是微秒级。
package matcher

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Kind 是发布内容形态，决定入库时的命名分支。
type Kind string

const (
	KindTV    Kind = "tv"
	KindMovie Kind = "movie"
	KindOVA   Kind = "ova"
	KindSP    Kind = "sp"
)

// Parsed 是解析结果。
type Parsed struct {
	Raw        string
	Fansub     string
	Title      string
	Episode    float64
	EpisodeTo  float64
	Season     int
	Resolution string
	Source     string
	Codec      string
	Kind       Kind
	Batch      bool
	Ext        string
}

// 第一集判定：集数区间，超过则视为合集。
const batchThreshold = 1.5

var (
	// 括号分词：中英文方括号与圆括号。
	braceRe = regexp.MustCompile(`\[([^\]]*)\]|【([^】]*)】|\(([^)]*)\)|（([^）]*)）|「([^」]*)」|『([^』]*)』`)
	// 装饰性星号包裹，如 ★10月新番★。
	starRe = regexp.MustCompile(`★[^★]*★`)

	// 单独一个 token 是否属于技术元数据（分辨率/编码/字幕语言/来源）。
	// 允许 "HEVC_AAC"、"AVC AAC"、"1080p WEB-DL" 这类组合 token。
	metaWholeRe = regexp.MustCompile(`(?i)^(1080p|720p|480p|2160p|1440p|4k|8k|hdr10?|dv|10bit|8bit|12bit|hevc|h265|h264|avc|x265|x264|aac|flac|opus|ac3|eac3|dts(-hd)?|mp3|web-?dl|web-?rip|web|dl|hd|blu-?ray|bd(rip|box)?|baha|b-?global|cr|netflix|nf|amzn|at-?x|hdtv|tvrip|dvdrip|mp4|mkv|avi|rmvb|flv|ass|srt|ssa|sub|chs|cht|jpsc|jptc|简|繁|簡|简体|繁体|簡體|简日|繁日|简繁|中日|日语|双语|中字|内嵌|外挂|无修|合集|全集|fin|end|movie|剧场版|劇場版|svfi|v\d+)$`)
	metaPartRe  = regexp.MustCompile(`[\s_+/&·・,，-]+`)
	// 整串里的元数据词，用于把 "Title.S01E03.1080p.WEB-DL" 洗干净。
	metaInlineRe = regexp.MustCompile(`(?i)\b(2160p|1440p|1080p|720p|480p|4320p|4k|8k|hdr10|hdr|dv|10bit|8bit|hevc|h265|h264|avc|x265|x264|aac|flac|opus|eac3|ac3|dts|web-?dl|web-?rip|webrip|webdl|blu-?ray|bdrip|bdbox|baha|b-global|netflix|amzn|hdtv|tvrip|dvdrip|mp4|mkv|avi|rmvb|flv|ass|srt|ssa|chs|cht|jpsc|jptc)\b`)

	// 明显不是字幕组名的装饰 token。
	junkRe = regexp.MustCompile(`★|新番|月新|番剧|番組|组务|招募|招人|合购|搬运|发布组|生肉|熟肉|禁转|自压|自购|汉化|预告|PV|CM|版权|仅供|交流|学习|下载后|请于|删除|声明|发布|更新`)
	// 纯集数 token：[01] [12] [01v2] [3.5]
	pureEpRe = regexp.MustCompile(`^(\d{1,4}(?:\.\d)?)(?:v\d+)?$`)
	// 区间 token：[01-12]
	pureRangeRe = regexp.MustCompile(`^(\d{1,3})\s*[-~]\s*(\d{1,3})(?:v\d+)?$`)
	// rangeAnyRe 不锚定：用来从「01-28 修正合集」这种带后缀的 token 里抠区间。
	rangeAnyRe = regexp.MustCompile(`(\d{1,3})\s*[-~～〜－]\s*(\d{1,3})`)
	// totalEpsRe 匹配「全28话 / 全12集」这类只给总集数的写法。
	totalEpsRe = regexp.MustCompile(`(?i)全\s*(\d{1,3})\s*[话話集]`)

	resRe       = regexp.MustCompile(`(?i)\b(4320p|2160p|1440p|1080p|720p|480p|360p|4k|8k)\b`)
	sourceRe    = regexp.MustCompile(`(?i)\b(web-?dl|web-?rip|web|baha|b-global|cr|netflix|blu-?ray|bdrip|bdbox|hdtv|tvrip|dvdrip)\b`)
	codecRe     = regexp.MustCompile(`(?i)\b(hevc|avc|x265|x264|h265|h264|vvc|av1)\b`)
	seasonCnRe  = regexp.MustCompile(`第\s*([一二三四五六七八九十]{1,3})\s*[季期部]`)
	seasonNumRe = regexp.MustCompile(`(?i)第\s*(\d{1,2})\s*[季期部]|\bseason\s*(\d{1,2})\b|(?:\b|^)s(\d{1,2})e\d`)
	kindRe      = regexp.MustCompile(`(?i)剧场版|劇場版|剧场|movie|the movie`)
	ovaRe       = regexp.MustCompile(`(?i)\bova\b|\boad\b|番外|外传`)
	spRe        = regexp.MustCompile(`(?i)\bsp\d{0,3}\b|特别篇|特番|特別篇`)
	batchRe     = regexp.MustCompile(`(?i)合集|全集|全\d{1,3}\s*[话話集]|\bbatch\b|\bfin\b|完结|完結|完成`)
	spaceRe     = regexp.MustCompile(`\s{2,}`)
	yearRe      = regexp.MustCompile(`^\d{4}$`)
	// 2024-10-06 / 10.06 这类 token 是日期不是组名
	dateRe = regexp.MustCompile(`^\d{4}[-/.]\d{1,2}([-/.]\d{1,2})?$|^\d{1,2}[-/.]\d{1,2}$`)
)

// epRule 是一条集数识别规则。按数组顺序生效，先命中先返回。
type epRule struct {
	name   string
	re     *regexp.Regexp
	ep     int // 集数所在捕获组
	end    int // 结束集捕获组，0 表示没有
	season int // 季数捕获组，0 表示没有
}

var epRules = []epRule{
	// Title.S01E03 / Title S01E03-E05
	{"season-episode", regexp.MustCompile(`(?i)\bs(\d{1,2})\s*e(\d{1,4})(?:[-~](\d{1,4}))?\b`), 2, 3, 1},
	// 第03话 / 第 3.5 話
	{"cn-episode", regexp.MustCompile(`第\s*(\d{1,4}(?:\.\d)?)\s*[话話集]`), 1, 0, 0},
	// [01v2] 形式的纯集数 token 已在上一步优先处理，这里兜底不带括号的情况
	{"ep-prefix", regexp.MustCompile(`(?i)\b(?:ep|e)\s*\.?\s*(\d{1,4})\b`), 1, 0, 0},
	// 01-12 形式：需要词边界，否则 2024-10-06 这类日期会被误判
	{"episode-range", regexp.MustCompile(`\b(\d{1,3})\s*[-~]\s*(\d{1,3})\b`), 1, 2, 0},
	// Title - 03 / Title — 03
	{"dash-episode", regexp.MustCompile(`\s[-–—_]\s*(\d{1,4}(?:\.\d)?)\s*(?:v\d+)?(?:\s|$|\[|\()`), 1, 0, 0},
	// 兜底：标题里的裸数字
	{"bare-number", regexp.MustCompile(`\b(\d{1,4}(?:\.\d)?)\s*(?:v\d+)?\s*(?:$|\s|\[|\()`), 1, 0, 0},
}

var knownExt = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".ts": true, ".rmvb": true,
	".flv": true, ".wmv": true, ".webm": true, ".mov": true, ".m4v": true, ".torrent": true,
}

// ParseTitle 把一条原始 RSS 标题解析成结构化信息。
// 任何一步失败都退化为「标题=原文、集数=0」，绝不返回错误——
// 订阅里混着广播剧、音乐、漫画是常态，不能因此中断整轮轮询。
func ParseTitle(raw string) Parsed {
	p := Parsed{Raw: raw, Season: 1, Kind: KindTV}
	work := strings.TrimSpace(raw)
	if ext := strings.ToLower(filepath.Ext(work)); knownExt[ext] {
		p.Ext = ext
		work = strings.TrimSuffix(work, filepath.Ext(work))
	}

	// 1) 括号分词，同时把 token 从正文里摘掉。
	var tokens []string
	core := braceRe.ReplaceAllStringFunc(work, func(m string) string {
		tokens = append(tokens, strings.Trim(m, "[]【】()（）「」『』"))
		return " "
	})
	core = starRe.ReplaceAllString(core, " ")

	// 2) 挑字幕组名 + 优先取纯集数 token。
	// contentTokens 记录「像标题」的 token：部分源（如蜜柑）把番剧名也写在方括号里。
	var contentTokens []string
	for _, tk := range tokens {
		tk = strings.TrimSpace(tk)
		if tk == "" {
			continue
		}
		if m := pureRangeRe.FindStringSubmatch(tk); m != nil {
			from, _ := strconv.ParseFloat(m[1], 64)
			to, _ := strconv.ParseFloat(m[2], 64)
			if p.Episode == 0 && epPlausible(from) && to > from {
				p.Episode, p.EpisodeTo, p.Batch = from, to, true
			}
			continue
		}
		if m := pureEpRe.FindStringSubmatch(tk); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			if p.Episode == 0 && epPlausible(v) {
				p.Episode = v
			}
			continue
		}
		if p.Episode == 0 && batchRe.MatchString(tk) {
			// 方括号里带后缀的合集：「[01-28 修正合集]」「[全28话]」。
			// 抠不出区间时 Episode 会停在 0，入库名字就成了 S01E00。
			if from, to, ok := rangeInToken(tk); ok {
				p.Episode, p.EpisodeTo, p.Batch = from, to, true
				continue
			}
			if m := totalEpsRe.FindStringSubmatch(tk); m != nil {
				if n, _ := strconv.ParseFloat(m[1], 64); epPlausible(n) {
					p.Episode, p.EpisodeTo, p.Batch = 1, n, true
					continue
				}
			}
		}
		if isMetaToken(tk) || junkRe.MatchString(tk) {
			continue
		}
		if isLikelyGroup(tk) {
			contentTokens = append(contentTokens, tk)
			if p.Fansub == "" {
				p.Fansub = tk
			}
		}
	}

	// 3) 洗正文：去掉分辨率/编码等噪音，统一分隔符。
	core = metaInlineRe.ReplaceAllString(core, " ")
	core = strings.ReplaceAll(core, "1920x1080", " ")
	core = strings.ReplaceAll(core, "1280x720", " ")
	core = strings.ReplaceAll(core, "3840x2160", " ")
	core = deseparate(core)
	core = strings.ReplaceAll(core, "_", " ")
	core = spaceRe.ReplaceAllString(core, " ")

	// 4) 正文里找集数（括号集数已拿到则不覆盖），命中后从正文里删掉该片段。
	if p.Episode == 0 || p.EpisodeTo == 0 {
		if span, val, to, season, ok := findEpisode(core); ok {
			if p.Episode == 0 {
				p.Episode, p.EpisodeTo = val, to
			}
			if season > 0 {
				p.Season = season
			}
			core = core[:span[0]] + " " + core[span[1]:]
		}
	}

	// 5) 形态与季数。
	if kindRe.MatchString(work) {
		p.Kind = KindMovie
	} else if ovaRe.MatchString(work) {
		p.Kind = KindOVA
	} else if spRe.MatchString(work) {
		p.Kind = KindSP
	}
	if p.Kind != KindTV {
		p.Episode, p.EpisodeTo, p.Batch = 0, 0, false
	}
	if n := findSeason(work); n > 0 {
		p.Season = n
	}
	if !p.Batch {
		if p.EpisodeTo > p.Episode+batchThreshold || batchRe.MatchString(work) {
			p.Batch = true
			if p.EpisodeTo == 0 {
				p.EpisodeTo = p.Episode
			}
		}
	}

	// 6) 技术属性。
	if m := resRe.FindString(work); m != "" {
		p.Resolution = strings.ToLower(m)
	}
	if m := sourceRe.FindString(work); m != "" {
		p.Source = strings.ToLower(m)
	}
	if m := codecRe.FindString(work); m != "" {
		p.Codec = strings.ToLower(m)
	}

	// 7) 标题。
	p.Title = cleanTitle(core)
	if len([]rune(p.Title)) < 2 {
		// 番剧名被写在方括号里的源（蜜柑/Nyaa 常见）走这条兜底路径。
		if best := longestToken(contentTokens, p.Fansub); best != "" {
			p.Title = cleanTitle(best)
		} else {
			p.Title = cleanTitle(work)
		}
	}
	if p.Title == "" {
		p.Title = "未命名"
	}
	if p.Fansub == "" {
		p.Fansub = "未知字幕组"
	}
	return p
}

// findEpisode 按规则表找集数，返回命中区间、集数、结束集与季数。
func findEpisode(s string) (span [2]int, ep, to float64, season int, ok bool) {
	for _, r := range epRules {
		loc := r.re.FindStringSubmatchIndex(s)
		if loc == nil {
			continue
		}
		v, err := strconv.ParseFloat(s[loc[2*r.ep]:loc[2*r.ep+1]], 64)
		if err != nil || !epPlausible(v) {
			continue
		}
		out := [2]int{loc[0], loc[1]}
		end := 0.0
		if r.end > 0 && loc[2*r.end] >= 0 {
			if e, err := strconv.ParseFloat(s[loc[2*r.end]:loc[2*r.end+1]], 64); err == nil && e >= v {
				end = e
			}
		}
		// 季数只在 SxxExx 规则里可信；S01 这种在综艺/剧场版里也常见。
		if r.season > 0 && loc[2*r.season] >= 0 {
			if n, err := strconv.Atoi(s[loc[2*r.season]:loc[2*r.season+1]]); err == nil {
				season = n
			}
		}
		return out, v, end, season, true
	}
	return span, 0, 0, 0, false
}

// rangeInToken 从「01-28 修正合集」这类 token 里抠出集数区间。
// 逐个候选试：日期（2023-10）和分辨率会被 epPlausible 挡掉。
func rangeInToken(tk string) (from, to float64, ok bool) {
	for _, m := range rangeAnyRe.FindAllStringSubmatch(tk, -1) {
		f, _ := strconv.ParseFloat(m[1], 64)
		t, _ := strconv.ParseFloat(m[2], 64)
		if epPlausible(f) && epPlausible(t) && t > f {
			return f, t, true
		}
	}
	return 0, 0, false
}

// epPlausible 挡掉分辨率（1080/720/480）和年份（2024）这类高仿集数。
func epPlausible(v float64) bool {
	if v <= 0 || v > 1900 {
		return false
	}
	switch int(v) {
	case 264, 265, 360, 480, 540, 576, 720, 1080, 1440, 2160, 4320:
		return false
	}
	return true
}

// isMetaToken 判断一个括号 token 是否整体由技术元数据构成。
func isMetaToken(s string) bool {
	if metaWholeRe.MatchString(s) {
		return true
	}
	parts := metaPartRe.Split(s, -1)
	seen := false
	for _, p := range parts {
		if p == "" {
			continue
		}
		if !metaWholeRe.MatchString(p) {
			return false
		}
		seen = true
	}
	return seen
}

// isLikelyGroup 排除明显不是组名的 token（日期、年份、极短噪声）。
func isLikelyGroup(s string) bool {
	if len(s) < 2 || len(s) > 40 {
		return false
	}
	if yearRe.MatchString(s) || dateRe.MatchString(s) {
		return false
	}
	if strings.Contains(s, "月") && strings.ContainsAny(s, "0123456789") {
		return false
	}
	if allDigits(s) {
		return false
	}
	return true
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// cleanTitle 收敛空白并去掉首尾分隔符；清洗后为空则返回空串，
// 由调用方决定兜底策略（不要在这里塞默认值，否则兜底判断会失效）。
func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " -–—_·.。~|/\\\"'*")
	return strings.Join(strings.Fields(s), " ")
}

// deseparate 把文件名式分隔符换成空格，但保留 11.5 这类小数集数的点。
func deseparate(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range rs {
		if r != '.' && r != '\u3000' {
			b.WriteRune(r)
			continue
		}
		prevDigit := i > 0 && rs[i-1] >= '0' && rs[i-1] <= '9'
		nextDigit := i+1 < len(rs) && rs[i+1] >= '0' && rs[i+1] <= '9'
		if r == '.' && prevDigit && nextDigit {
			b.WriteRune(r)
			continue
		}
		b.WriteRune(' ')
	}
	return b.String()
}

// findSeason 从标题里取季/期/部序号，兼容中文数字与 SxxExx。
func findSeason(s string) int {
	if m := seasonCnRe.FindStringSubmatch(s); m != nil {
		if n := parseCnNum(m[1]); n > 0 {
			return n
		}
	}
	if m := seasonNumRe.FindStringSubmatch(s); m != nil {
		for i := 1; i < len(m); i++ {
			if m[i] == "" {
				continue
			}
			if n, err := strconv.Atoi(m[i]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

var cnDigits = map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

// parseCnNum 处理 十二 / 二十 / 二十三 这类中文数字。
func parseCnNum(s string) int {
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	total, last := 0, 0
	for _, r := range s {
		if r == '十' {
			if last == 0 {
				last = 1
			}
			total += last * 10
			last = 0
			continue
		}
		d, ok := cnDigits[r]
		if !ok {
			return 0
		}
		last = d
	}
	return total + last
}

// longestToken 取最长的内容 token，并跳过已被认作字幕组的那个。
func longestToken(tokens []string, skip string) string {
	best, bestLen := "", 0
	for _, tk := range tokens {
		if tk == skip {
			continue
		}
		if n := len([]rune(tk)); n > bestLen {
			best, bestLen = tk, n
		}
	}
	return best
}
