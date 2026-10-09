package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/pipeline"
)

// 番剧库：搜索蜜柑上的番剧、看放送时间与字幕组、一键订阅。
//
// 为什么单独开一页而不是塞进面板：
// 面板回答「现在在干什么」，这一页回答「我要把什么加进来」。
// 两件事的节奏完全不同——前者一直在轮询刷新，后者是一次性的动作。
//
// 交付方式沿用无构建链的思路：GET 表单搜索 + POST 表单订阅 + 302 回跳，
// 没有 fetch、没有前端状态，断网也只影响刮削本身。

// browseData 是番剧库页面的数据。
type browseData struct {
	OV      *pipeline.Overview
	Query   string        // 搜索词
	Results []browseRow   // 搜索结果
	Detail  *browseDetail // 指定 id 时的番剧详情（含字幕组选择器）
	OK      string        // 操作成功提示
	Err     string        // 错误提示
	MikanOn bool          // 刮削是否可用
	Prefers []string      // 全局字幕组偏好
}

type browseRow struct {
	B        model.Bangumi
	Day      string
	MikanURL string
	Subbed   int  // 已经订阅了几个字幕组
	Finished bool // 已经播完：订阅时会默认连往期一起下
}

type browseDetail struct {
	B         model.Bangumi
	Day       string
	MikanURL  string
	Options   []subgroupOption
	Auto      int64 // 按偏好会自动挑中的字幕组 id（0 表示没有偏好命中）
	HasSub    bool
	AutoLabel string
	Finished  bool // 这部番已经播完：页面上把「连往期一起下」默认勾上并说明原因
}

type subgroupOption struct {
	ID     int64
	Name   string
	Picked bool // 命中全局偏好
	Subbed bool // 已经订阅过
}

// browse 处理 GET /subscribe：没有 q 也没有 id 时是空状态。
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	vd := &browseData{
		Query:   strings.TrimSpace(q.Get("q")),
		OK:      strings.TrimSpace(q.Get("ok")),
		Err:     strings.TrimSpace(q.Get("err")),
		MikanOn: s.p.Mikan() != nil && s.p.Mikan().Enabled(),
	}
	ov, err := s.p.Overview(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	vd.OV = ov
	vd.Prefers = s.p.Mikan().Prefer()

	// 已经订阅过的 RSS，用来在字幕组列表上打勾，避免重复订阅。
	subbed := make(map[string]bool, 32)
	subbedByShow := make(map[int64]int, 16)
	for _, sub := range ov.Subs {
		subbed[sub.FeedURL] = true
		if sub.MikanID > 0 {
			subbedByShow[sub.MikanID]++
		}
	}

	if id := int64(atoiSafe(q.Get("id"))); id > 0 {
		b, err := s.p.BangumiInfo(ctx, id)
		if err != nil {
			vd.Err = err.Error()
		} else if b != nil {
			vd.Detail = s.buildDetail(b, subbed)
		}
		s.render(w, http.StatusOK, "subscribe.html", vd)
		return
	}

	if vd.Query != "" {
		list, err := s.p.SearchBangumi(ctx, vd.Query)
		if err != nil {
			vd.Err = err.Error()
		}
		for _, b := range list {
			vd.Results = append(vd.Results, browseRow{
				B:        b,
				Day:      dayGlyph(b.AirText),
				MikanURL: s.p.Mikan().BangumiURL(b.ID),
				Subbed:   subbedByShow[b.ID],
				Finished: b.Finished(time.Now()),
			})
		}
	}
	s.render(w, http.StatusOK, "subscribe.html", vd)
}

func (s *Server) buildDetail(b *model.Bangumi, subbed map[string]bool) *browseDetail {
	d := &browseDetail{
		B:        *b,
		Day:      dayGlyph(b.AirText),
		MikanURL: s.p.Mikan().BangumiURL(b.ID),
		Finished: b.Finished(time.Now()),
	}
	// 按偏好排序：用户最想要的那个排第一，一眼看到。
	for _, sg := range s.p.Mikan().PickSubgroups(b, nil) {
		opt := subgroupOption{
			ID:     sg.ID,
			Name:   sg.Name,
			Picked: s.p.Mikan().Matches(sg.Name, nil),
			Subbed: subbed[s.p.Mikan().RSSURL(sg)],
		}
		d.Options = append(d.Options, opt)
		if d.Auto == 0 && opt.Picked {
			d.Auto = sg.ID
			d.AutoLabel = sg.Name
		}
	}
	if d.Auto == 0 && len(d.Options) > 0 {
		d.Auto, d.AutoLabel = d.Options[0].ID, d.Options[0].Name
	}
	for _, o := range d.Options {
		if o.Subbed {
			d.HasSub = true
		}
	}
	return d
}

// apiBrowseSubscribe 处理 POST /api/v1/bangumi/subscribe：一键订阅。
func (s *Server) apiBrowseSubscribe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req := pipeline.SubscribeRequest{
		MikanID:     atoi64(r.FormValue("mikan_id")),
		Keyword:     strings.TrimSpace(r.FormValue("keyword")),
		Group:       strings.TrimSpace(r.FormValue("group")),
		Prefer:      configSplit(r.FormValue("prefer")),
		IntervalMin: aToi(r.FormValue("interval_min")),
		StartEp:     aToFloat(r.FormValue("start_ep")),
		Backfill:    r.FormValue("backfill") == "1",
		// 勾选框所在的页面总会带上这个字段，所以「没勾」能被当成明确选择；
		// 而直接打接口的调用方不带它 —— 那就由后端按番剧是否播完自动决定。
		BackfillChosen: r.FormValue("backfill_chosen") == "1",
		AutoPick:       1,
	}
	for _, v := range r.Form["sg"] {
		if id := atoi64(v); id > 0 {
			req.SubgroupIDs = append(req.SubgroupIDs, id)
		}
	}
	if r.FormValue("auto") == "1" {
		req.SubgroupIDs = nil
	}

	res, err := s.p.SubscribeBangumi(r.Context(), req)
	back := "/subscribe"
	if req.MikanID > 0 {
		back = fmt.Sprintf("/subscribe?id=%d", req.MikanID)
	} else if req.Keyword != "" {
		back = "/subscribe?q=" + urlQueryEscape(req.Keyword)
	}
	if err != nil {
		http.Redirect(w, r, back+sep(back)+"err="+urlQueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	msg := fmt.Sprintf("已订阅《%s》", res.Bangumi.Title)
	if res.Backfill {
		msg += "（连往期一起下）"
	}
	if n := len(res.Created); n > 0 {
		names := make([]string, 0, n)
		for _, sub := range res.Created {
			names = append(names, trimGroupName(sub.Name, res.Bangumi.Title))
		}
		msg += "：" + strings.Join(names, "、")
	} else {
		msg = fmt.Sprintf("《%s》没有新增订阅", res.Bangumi.Title)
	}
	if len(res.Skipped) > 0 {
		msg += "；跳过 " + strings.Join(res.Skipped, "、")
	}
	http.Redirect(w, r, back+sep(back)+"ok="+urlQueryEscape(msg), http.StatusSeeOther)
}

// apiCover 处理 GET /api/v1/covers/{id}.jpg。
//
// 由本进程提供封面而不是热链接蜜柑：断网也能看，
// 不给人家站点添第二次流量，而且同一张图正好拿去当媒体库海报。
func (s *Server) apiCover(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	name = strings.TrimSuffix(name, ".jpg")
	id := atoi64(name)
	if id <= 0 {
		http.NotFound(w, r)
		return
	}
	path := s.p.CachedCover(id)
	if path == "" {
		b, err := s.st.GetBangumi(r.Context(), id)
		if err != nil || b == nil || b.CoverPath == "" {
			http.NotFound(w, r)
			return
		}
		p, err := s.p.EnsureCover(r.Context(), b)
		if err != nil || p == "" {
			s.log.Debug("封面抓取失败", "id", id, "err", err)
			http.NotFound(w, r)
			return
		}
		path = p
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 封面基本不变，让浏览器自己缓存一天，省掉面板反复刷新的请求。
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, path, st.ModTime(), f)
}

// apiBangumiSearch 是给脚本用的 JSON 搜索接口。
func (s *Server) apiBangumiSearch(w http.ResponseWriter, r *http.Request) {
	kw := strings.TrimSpace(r.URL.Query().Get("q"))
	if kw == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少查询参数 q"))
		return
	}
	list, err := s.p.SearchBangumi(r.Context(), kw)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	// 字幕组按偏好排序，和面板上看到的顺序一致，脚本调用不用自己再排一遍。
	for i := range list {
		if len(list[i].Subgroups) > 0 {
			list[i].Subgroups = s.p.Mikan().PickSubgroups(&list[i], nil)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": kw, "count": len(list), "results": list})
}

// dayGlyph 取放送日里的单字：星期六 → 六。面板用一个大字竖排显示。
func dayGlyph(air string) string {
	for _, r := range air {
		switch r {
		case '一', '二', '三', '四', '五', '六', '日', '天':
			return string(r)
		}
	}
	return ""
}

// trimGroupName 把「葬送的芙莉莲 [喵萌奶茶屋]」还原成「喵萌奶茶屋」，用于回执文案。
func trimGroupName(name, show string) string {
	name = strings.TrimPrefix(name, show)
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "[")
	name = strings.TrimSuffix(name, "]")
	if name == "" {
		return show
	}
	return name
}

func sep(base string) string {
	if strings.Contains(base, "?") {
		return "&"
	}
	return "?"
}

// configSplit 兼容中英文逗号，和配置文件里的列表写法一致。
func configSplit(s string) []string { return config.SplitTokens(s) }

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func aToi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func aToFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// atoiSafe 尽量从字符串里取整数：'3141'、'3141.jpg' 都能拿到 3141。
func atoiSafe(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
