package mikan

import (
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// 蜜柑的页面很大：搜索页 3.9MB，番剧页 330KB，而需要的数据全都在前 250KB。
//
// 用 html.Parse 建一棵几万节点的树再挑字段，在 1GB 内存的机器上是纯粹的浪费；
// 这里用流式 tokenizer：边读边取，读完即弃，常驻内存只有几十 KB 的缓冲区。
// 上限是硬保险——万一蜜柑改版把结构挪到页面末尾，也不会把内存吃爆。
const (
	maxSearchBytes = 512 << 10
	// 番剧页会随字幕组数量涨：23 个字幕组的番剧实测 1.04MB，
	// 因为每个字幕组块后面都跟着它自己的剧集表，必须扫到最后一块。
	// 4MB 是防站点异常的保护线，不是预期用量（流式解析，内存与页长无关）。
	maxDetailBytes = 4 << 20
)

var (
	// 番剧链接：卡片是 href="/Home/Bangumi/3141"，用户也可能直接贴整条 URL，
	// 所以不锚定开头，能匹配上就行。
	hrefBangumiRe = regexp.MustCompile(`/Home/Bangumi/(\d+)`)
	// 字幕组 id 只从 RSS 链接里取，不依赖具体排版。
	subgroupIDRe = regexp.MustCompile(`subgroupid=(\d+)`)
	// 封面藏在 style="background-image: url('...')" 里。
	coverStyleRe = regexp.MustCompile(`url\('([^']+)'\)`)
	// 站内图片路径：去掉 ?width=400&height=560&format=webp 这类缩放参数，
	// 缩放参数由我们自己按用途拼（面板要小图、媒体库要海报尺寸）。
	coverPathRe = regexp.MustCompile(`/images/Bangumi/[^"')?#\s]+`)
)

// errBudget 表示读到了本次解析愿意读的上限；对解析来说等价于 EOF。
var errBudget = errors.New("mikan: 到达读取上限")

// budgetReader 给响应体加硬上限，避免异常页面把内存读爆。
type budgetReader struct {
	r    io.Reader
	left int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errBudget
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if err == io.EOF && b.left <= 0 {
		return n, errBudget
	}
	return n, err
}

// advance 推进一个 token。
//
// 返回 ok=false 表示正常结束（EOF、到达上限、或者被截断），
// 这时已经解析出来的部分照常返回——常见于蜜柑在页面中途断开连接。
func advance(z *html.Tokenizer) (html.TokenType, html.Token, bool, error) {
	tt := z.Next()
	if tt != html.ErrorToken {
		return tt, z.Token(), true, nil
	}
	err := z.Err()
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, errBudget),
		errors.Is(err, io.ErrUnexpectedEOF):
		return 0, html.Token{}, false, nil
	default:
		return 0, html.Token{}, false, err
	}
}

// scanSearch 从搜索页里只取番剧卡片，跳过后面 3.8MB 的剧集列表。
//
// 结构（2026-10 实测）：
//
//	<ul class="list-inline an-ul">
//	  <li><a href="/Home/Bangumi/3141">
//	    <span data-src="/images/Bangumi/202309/5ce9fed1.jpg" class="b-lazy"></span>
//	    <div class="an-info"><div class="an-info-group">
//	      <div class="an-text" title="葬送的芙莉莲">葬送的芙莉莲</div>
//	    </div></div>
//	  </a></li>
//	</ul>
//
// 卡片块结束后立即返回：搜索页剩下的内容一行都不读，省流量也省时间。
func scanSearch(r io.Reader) ([]model.Bangumi, error) {
	z := html.NewTokenizer(&budgetReader{r: r, left: maxSearchBytes})
	var (
		out      []model.Bangumi
		seen     = make(map[int64]bool, 8)
		inCards  bool
		depth    int
		cur      *model.Bangumi
		grabName bool
		buf      strings.Builder
	)

	flush := func() {
		if cur == nil || cur.ID == 0 || seen[cur.ID] {
			cur = nil
			return
		}
		seen[cur.ID] = true
		out = append(out, *cur)
		cur = nil
	}

	for {
		tt, t, ok, err := advance(z)
		if err != nil {
			return out, err
		}
		if !ok {
			flush()
			return out, nil
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			if t.Data == "ul" && hasClass(t, "an-ul") {
				inCards, depth = true, 1
				continue
			}
			if !inCards {
				continue
			}
			switch t.Data {
			case "ul":
				depth++
			case "a":
				if id := bangumiID(attr(t, "href")); id != 0 {
					flush()
					cur = &model.Bangumi{ID: id}
				}
			case "span":
				if cur != nil && cur.CoverPath == "" {
					cur.CoverPath = cleanCover(attr(t, "data-src"))
				}
			case "div":
				if cur != nil && cur.Title == "" && hasClass(t, "an-text") {
					if ti := strings.TrimSpace(attr(t, "title")); ti != "" {
						cur.Title = ti
					} else {
						// 没有 title 属性时退化为取文本内容。
						grabName = true
						buf.Reset()
					}
				}
			}
		case html.TextToken:
			if inCards && grabName {
				buf.WriteString(t.Data)
			}
		case html.EndTagToken:
			if !inCards {
				continue
			}
			switch t.Data {
			case "div":
				if grabName {
					grabName = false
					if cur != nil && cur.Title == "" {
						cur.Title = strings.TrimSpace(buf.String())
					}
				}
			case "a":
				flush()
			case "ul":
				if depth--; depth <= 0 {
					return out, nil
				}
			}
		}
	}
}

// scanDetail 解析番剧详情页：封面、标题、放送信息、字幕组与各自的 RSS。
//
// 结构（2026-10 实测）：
//
//	<div class="bangumi-poster" style="background-image: url('/images/Bangumi/202304/ba3dc557.jpg?...')"></div>
//	<p class="bangumi-title">地狱乐 <a class="mikan-rss" href="/RSS/Bangumi?bangumiId=2996">…</a></p>
//	<p class="bangumi-info">放送日期：星期六</p>
//	<p class="bangumi-info">总集数：13</p>
//	<p class="bangumi-info">放送开始：4/1/2023</p>
//	<div class="subgroup-text" id="1254">
//	  <a href="/Home/PublishGroup/1025">7³ACG</a>
//	  <a class="mikan-rss" href="/RSS/Bangumi?bangumiId=2996&subgroupid=1254">…</a>
//	  <div id="subscription-popover-…">…</div>   ← 嵌套 div，块结束要靠深度计数
//	</div>
//	…每个字幕组块后面跟着它自己的剧集表，交替出现，所以必须扫到最后一块。
func scanDetail(r io.Reader) (*model.Bangumi, error) {
	z := html.NewTokenizer(&budgetReader{r: r, left: maxDetailBytes})
	b := &model.Bangumi{}
	var (
		inTitle bool
		inInfo  bool
		inSG    bool
		sgDepth int
		sg      *model.Subgroup
		inGroup bool
		buf     strings.Builder
	)

	for {
		tt, t, ok, err := advance(z)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			// 「回到顶部」按钮出现在整个番剧内容区之后，
			// 见到它就说明字幕组列表已经收完，后面全是脚本与广告，不必再读。
			if hasClass(t, "cd-top-btn") {
				closeSubgroup(b, sg)
				return b, nil
			}
			if inSG {
				switch t.Data {
				case "div":
					sgDepth++
				case "a":
					href := attr(t, "href")
					if strings.HasPrefix(href, "/Home/PublishGroup/") {
						inGroup = true
						buf.Reset()
					} else if sg != nil && sg.RSS == "" && subgroupID(href) != 0 {
						sg.RSS = href
					}
				}
				continue
			}
			switch t.Data {
			case "div":
				if hasClass(t, "bangumi-poster") && b.CoverPath == "" {
					if m := coverStyleRe.FindStringSubmatch(attr(t, "style")); m != nil {
						b.CoverPath = cleanCover(m[1])
					}
				} else if hasClass(t, "subgroup-text") {
					inSG, sgDepth = true, 1
					sg = &model.Subgroup{ID: parseInt(attr(t, "id"))}
				}
			case "p":
				switch {
				case hasClass(t, "bangumi-title"):
					inTitle = true
					buf.Reset()
				case hasClass(t, "bangumi-info"):
					inInfo = true
					buf.Reset()
				}
			case "a":
				// 官网链接（在 bangumi-info 块里）直接取 href，比解析文本可靠。
				if inInfo && b.Official == "" {
					if href := attr(t, "href"); strings.HasPrefix(href, "http") {
						b.Official = href
					}
				}
			}
		case html.TextToken:
			switch {
			case inSG && inGroup:
				buf.WriteString(t.Data)
			case inTitle, inInfo:
				buf.WriteString(t.Data)
			}
		case html.EndTagToken:
			if inSG {
				switch t.Data {
				case "a":
					if inGroup {
						inGroup = false
						if sg != nil && sg.Name == "" {
							sg.Name = strings.TrimSpace(buf.String())
						}
					}
				case "div":
					if sgDepth--; sgDepth <= 0 {
						closeSubgroup(b, sg)
						sg, inSG = nil, false
					}
				}
				continue
			}
			switch t.Data {
			case "p":
				text := strings.TrimSpace(buf.String())
				switch {
				case inTitle:
					inTitle = false
					if b.Title == "" {
						b.Title = text
					}
				case inInfo:
					inInfo = false
					applyInfo(b, text)
				}
			}
		}
	}
	// 页面被截断在最后一个字幕组块中间时，把已经拿到的部分也收下。
	closeSubgroup(b, sg)
	return b, nil
}

// closeSubgroup 把字幕组收进列表：没有名字或没有 RSS 的都丢掉，
// 宁缺毋滥——面板上列一个点不动的字幕组比少列一个更糟。
func closeSubgroup(b *model.Bangumi, sg *model.Subgroup) {
	if b == nil || sg == nil || sg.ID == 0 || sg.RSS == "" {
		return
	}
	if sg.Name == "" {
		sg.Name = "字幕组 " + strconv.FormatInt(sg.ID, 10)
	}
	for _, e := range b.Subgroups {
		if e.ID == sg.ID {
			return
		}
	}
	b.Subgroups = append(b.Subgroups, *sg)
}

// applyInfo 处理一行「放送日期：星期六」。
func applyInfo(b *model.Bangumi, text string) {
	key, val := splitInfo(text)
	switch key {
	case "放送日期", "放送时间":
		b.AirText = val
	case "总集数":
		b.Episodes = int(parseInt(val))
	case "放送开始", "首播":
		b.StartAt = val
	case "官方网站":
		if b.Official == "" {
			b.Official = val
		}
	}
}

func splitInfo(s string) (string, string) {
	for _, sep := range []string{"：", ":"} {
		if i := strings.Index(s, sep); i > 0 {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(sep):])
		}
	}
	return strings.TrimSpace(s), ""
}

// cleanCover 把 /images/Bangumi/202304/ba3dc557.jpg?width=400… 缩成纯路径。
func cleanCover(u string) string {
	if m := coverPathRe.FindString(u); m != "" {
		return m
	}
	return ""
}

func bangumiID(href string) int64 {
	if m := hrefBangumiRe.FindStringSubmatch(href); m != nil {
		return parseInt(m[1])
	}
	return 0
}

func subgroupID(href string) int64 {
	if m := subgroupIDRe.FindStringSubmatch(href); m != nil {
		return parseInt(m[1])
	}
	return 0
}

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func attr(t html.Token, key string) string {
	for _, a := range t.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(t html.Token, name string) bool {
	for _, f := range strings.Fields(attr(t, "class")) {
		if f == name {
			return true
		}
	}
	return false
}
