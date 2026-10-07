// Package library 负责把下载完成的文件整理成媒体库认得的名字与结构。
//
// 目标：Jellyfin / Emby / Kodi 直接扫得到，且尽量不占额外空间。
package library

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/matcher"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// Vars 是命名模板可用的变量集合。
type Vars struct {
	Title      string // 番剧名
	Group      string // 订阅分组
	Fansub     string // 字幕组
	Resolution string
	Codec      string
	Source     string
	Kind       string
	Season     int
	Episode    float64
	EpisodeTo  float64
	Batch      bool
}

// tokenRe 匹配 {name} 或 {name:03d}；数字部分是补零宽度。
var tokenRe = regexp.MustCompile(`\{([a-z_]+)(?::([0-9]+)[a-z]?)?\}`)

// Render 展开命名模板。
//
// 模板示例（默认值）：
//
//	{group}/{title}/第 {season:02d} 季/{title} - S{season:02d}E{span}
//
// 占位符：{group} {title} {fansub} {resolution} {codec} {source} {kind}
//
//	{season} {episode} {episode_to} {span} {ep} {sxxexx}
//
// 合集与单集用 {span}：单集渲染成 03，合集渲染成 01-28，
// 于是合集不会和真正的第一集同名（{episode} 只给单集用）。
func Render(rule string, v Vars) string {
	out := tokenRe.ReplaceAllStringFunc(rule, func(tok string) string {
		m := tokenRe.FindStringSubmatch(tok)
		name := m[1]
		width := 0
		if m[2] != "" {
			width, _ = strconv.Atoi(m[2])
		}
		switch name {
		case "title":
			return v.Title
		case "group":
			return v.Group
		case "fansub":
			return v.Fansub
		case "resolution":
			return v.Resolution
		case "codec":
			return v.Codec
		case "source":
			return v.Source
		case "kind":
			return v.Kind
		case "season":
			return padNum(float64(v.Season), width)
		case "episode":
			return padNum(v.Episode, width)
		case "episode_to":
			return padNum(v.EpisodeTo, width)
		case "ep":
			// 简写：SP/OVA/剧场版这类没有集号时退化为 "SP"
			if v.Episode == 0 && v.Kind != string(matcher.KindTV) {
				return strings.ToUpper(v.Kind)
			}
			return padNum(v.Episode, width)
		case "span":
			// 单集就是「03」，合集是「01-28」：合集叫 S01E01 会和真正的第一集撞名。
			if v.Batch && v.EpisodeTo > v.Episode {
				return padNum(v.Episode, width) + "-" + padNum(v.EpisodeTo, width)
			}
			if v.Episode == 0 && v.Kind != string(matcher.KindTV) {
				return strings.ToUpper(v.Kind)
			}
			return padNum(v.Episode, width)
		case "sxxexx":
			if v.Episode == 0 && v.Kind != string(matcher.KindTV) {
				return strings.ToUpper(v.Kind)
			}
			return fmt.Sprintf("S%02dE%s", v.Season, padNum(v.Episode, 2))
		}
		return "" // 未知变量直接抹掉，避免在磁盘上留下 {foo}
	})
	return SanitizePath(out)
}

// padNum 补零。集数允许 .5 这种半集，所以不能直接当整数格式化。
func padNum(v float64, width int) string {
	if width <= 0 {
		width = 2
	}
	neg := v < 0
	if neg {
		v = -v
	}
	intPart := int64(v)
	frac := v - float64(intPart)
	s := strconv.FormatInt(intPart, 10)
	for len(s) < width {
		s = "0" + s
	}
	if frac > 0.001 {
		s += strings.TrimRight(fmt.Sprintf("%.2f", frac)[1:], "0")
	}
	if neg {
		return "-" + s
	}
	return s
}

// SanitizePath 去掉文件系统非法字符，并压掉超长路径。
func SanitizePath(p string) string {
	// 逐段处理，保留分隔符。
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i, seg := range parts {
		seg = strings.Map(func(r rune) rune {
			switch r {
			case '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
				return -1
			}
			return r
		}, seg)
		seg = strings.Join(strings.Fields(seg), " ")
		seg = strings.Trim(seg, " .")
		if seg == "" {
			seg = "_"
		}
		// ext4 单段上限 255 字节，中文按 3 字节算，留出余量取 80 个字符。
		if len([]rune(seg)) > 80 {
			seg = string([]rune(seg)[:80])
		}
		parts[i] = seg
	}
	return strings.Join(parts, string(filepath.Separator))
}

// videoExts 是入库时认可的媒体扩展名。
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".ts": true, ".rmvb": true,
	".flv": true, ".wmv": true, ".webm": true, ".mov": true, ".m4v": true, ".m2ts": true,
}

// pickVideo 在下载目录里挑出正片：递归两层以内，取体积最大的媒体文件。
//
// 为什么不按文件名匹配：字幕组给的目录里常混着 SP、PV、样片、字体包，
// 体积最大者几乎总是正片，这个启发式比正则便宜也更稳。
func pickVideo(root string) (string, error) {
	var bestPath string
	var bestSize int64
	err := walkLimited(root, 0, 2, func(path string, size int64) {
		if !videoExts[strings.ToLower(filepath.Ext(path))] {
			return
		}
		if size > bestSize {
			bestPath, bestSize = path, size
		}
	})
	if err != nil {
		return "", err
	}
	if bestPath == "" {
		return "", os.ErrNotExist
	}
	return bestPath, nil
}

func walkLimited(root string, depth, maxDepth int, fn func(path string, size int64)) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		fn(root, info.Size())
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(root, name)
		if e.IsDir() {
			if depth >= maxDepth {
				continue
			}
			if err := walkLimited(full, depth+1, maxDepth, fn); err != nil {
				continue
			}
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		fn(full, fi.Size())
	}
	return nil
}

// ---- NFO ----

type nfoEpisode struct {
	XMLName   xml.Name `xml:"episodedetails"`
	Title     string   `xml:"title"`
	ShowTitle string   `xml:"showtitle,omitempty"`
	Season    int      `xml:"season"`
	Episode   float64  `xml:"episode"`
	Plot      string   `xml:"plot,omitempty"`
	Aired     string   `xml:"aired,omitempty"`
	Rating    float64  `xml:"rating,omitempty"`
	UniqueID  *nfoUID  `xml:"uniqueid,omitempty"`
}

type nfoUID struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr"`
	Value   string `xml:",chardata"`
}

// nfoTVShow 是番剧级元数据（tvshow.nfo），放在番剧根目录。
//
// 为什么值得写：Jellyfin/Emby/Kodi 扫到 tvshow.nfo + poster.jpg 就能直接
// 显示中文标题和海报；不写的话用户得在媒体库里一部一部手动改封面和标题。
type nfoTVShow struct {
	XMLName   xml.Name `xml:"tvshow"`
	Title     string   `xml:"title"`
	Original  string   `xml:"originaltitle,omitempty"`
	Plot      string   `xml:"plot,omitempty"`
	Year      int      `xml:"year,omitempty"`
	Premiered string   `xml:"premiered,omitempty"`
	Rating    float64  `xml:"rating,omitempty"`
	// Thumb 指同目录下的 poster.jpg：Kodi 认这个标签，Jellyfin 认文件名，
	// 两个都写上，省得用户装完还得手动贴海报。
	Thumb    string  `xml:"thumb,omitempty"`
	UniqueID *nfoUID `xml:"uniqueid,omitempty"`
}

// renderTVShowNFO 把番剧级元数据渲染成字节，方便先比对再决定要不要落盘。
func renderTVShowNFO(title, original, plot, premiered, metaID string, score float64, poster string) ([]byte, error) {
	n := nfoTVShow{
		Title:     title,
		Original:  original,
		Plot:      plot,
		Year:      yearOf(premiered),
		Premiered: isoDate(premiered),
		Rating:    score,
		Thumb:     poster,
	}
	if metaID != "" {
		n.UniqueID = &nfoUID{Type: "mikan", Default: true, Value: metaID}
	}
	data, err := xml.MarshalIndent(n, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), data...), nil
}

// writeTVShowNFO 写番剧级元数据。字段全部来自刮削，缺什么就省略什么，
// 不编造空标签——媒体库对空 plot 的处理比缺 plot 更难看。
func writeTVShowNFO(path, title, original, plot, premiered, metaID string, score float64, poster string) (string, error) {
	data, err := renderTVShowNFO(title, original, plot, premiered, metaID, score, poster)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// yearOf 从「4/1/2023」「2023-04-01」这类日期里抠出年份，抠不到返回 0。
func yearOf(s string) int {
	for i := 0; i+4 <= len(s); i++ {
		if !isDigit(s[i]) || !isDigit(s[i+1]) || !isDigit(s[i+2]) || !isDigit(s[i+3]) {
			continue
		}
		y := int(s[i]-'0')*1000 + int(s[i+1]-'0')*100 + int(s[i+2]-'0')*10 + int(s[i+3]-'0')
		if y >= 1950 && y <= 2100 {
			return y
		}
	}
	return 0
}

// isoDate 把蜜柑的 4/1/2023 转成媒体库认的 2023-04-01；已经是 ISO 的原样返回。
func isoDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 10 && s[4] == '-' {
		return s
	}
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return ""
	}
	d, m, y := yearOf(parts[2]), 0, parts[2]
	if len(parts[0]) > 2 || len(parts[1]) > 2 {
		return ""
	}
	m = atoiSafe(parts[0])
	d = atoiSafe(parts[1])
	if m == 0 || d == 0 || yearOf(y) == 0 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", yearOf(y), m, d)
}

func atoiSafe(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return 0
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// writeNFO 写出 Jellyfin/Emby/Kodi 都能读的剧集侧挂元数据。
func writeNFO(path string, e model.LibraryEntry, showTitle string, season int, plot, aired string, rating float64, metaID string) (string, error) {
	n := nfoEpisode{
		Title:     e.Title,
		ShowTitle: showTitle,
		Season:    season,
		Episode:   e.Episode,
		Plot:      plot,
		Aired:     aired,
		Rating:    rating,
	}
	if metaID != "" {
		n.UniqueID = &nfoUID{Type: "bangumi", Default: true, Value: metaID}
	}
	data, err := xml.MarshalIndent(n, "", "  ")
	if err != nil {
		return "", err
	}
	body := append([]byte(xml.Header), data...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
