// Package httpx 是面板与 HTTP API 层。
//
// 前端刻意不引入任何构建链：服务端模板 + 一个 60 行的原生 JS 轮询片段，
// 静态资源全部 embed 进二进制。理由是目标机器上没有 Node，也不该为了
// 一个管理面板在 armv7 上跑 vite。
package httpx

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/pipeline"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

//go:embed web/templates/*.html web/static/*
var assets embed.FS

// Server 持有面板所需的依赖。
type Server struct {
	cfg config.Config
	st  *store.Store
	p   *pipeline.Pipeline
	log *slog.Logger
	tpl *template.Template
}

// New 构造 HTTP 层。
func New(cfg config.Config, st *store.Store, p *pipeline.Pipeline, log *slog.Logger) (*Server, error) {
	tpl, err := template.New("").Funcs(funcMap).ParseFS(assets, "web/templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("解析模板: %w", err)
	}
	return &Server{cfg: cfg, st: st, p: p, log: log, tpl: tpl}, nil
}

// Handler 返回挂好中间件的根 handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(assets, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok %s uptime=%s\n", pipeline.Version, time.Since(started).Round(time.Second))
	})

	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /partials/schedule", s.partial("schedule"))
	mux.HandleFunc("GET /partials/queue", s.partial("queue"))
	mux.HandleFunc("GET /partials/activity", s.partial("activity"))
	mux.HandleFunc("GET /partials/library", s.partial("library"))
	mux.HandleFunc("GET /partials/verdict", s.partial("verdict"))

	// 番剧库：搜蜜柑 → 挑字幕组 → 一键订阅。
	mux.HandleFunc("GET /subscribe", s.browse)
	mux.HandleFunc("POST /api/v1/bangumi/subscribe", s.apiBrowseSubscribe)
	mux.HandleFunc("GET /api/v1/bangumi/search", s.apiBangumiSearch)
	mux.HandleFunc("GET /api/v1/covers/{name}", s.apiCover)

	mux.HandleFunc("GET /api/v1/state", s.apiState)
	mux.HandleFunc("POST /api/v1/subscriptions", s.createSub)
	mux.HandleFunc("POST /api/v1/subscriptions/{id}/toggle", s.toggleSub)
	mux.HandleFunc("POST /api/v1/subscriptions/{id}/delete", s.deleteSub)
	mux.HandleFunc("POST /api/v1/subscriptions/{id}/poll", s.pollSub)
	mux.HandleFunc("POST /api/v1/subscriptions/{id}/backfill", s.backfillSub)
	mux.HandleFunc("POST /api/v1/tasks/{id}/{action}", s.controlTask)

	return s.withAuth(mux)
}

// withAuth 用 Basic Auth 保护一切（/healthz 与静态资源除外）。
// 这是一个能删文件、能写盘的接口，暴露在局域网也必须带认证。
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.Server.User == "" && s.cfg.Server.Password == "" {
		// 能走到这里说明只监听了本机（config.Validate 已经拦下"敞开还没密码"的情况），
		// 所以这不是"暴露在公网"，只是提醒一句顺手设个密码。
		s.log.Warn("面板无认证（按配置只监听本机，局域网访问不到）。设上 server.user / server.password 会更稳妥")
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.Server.AcceptToken && s.cfg.Server.Token != "" && r.Header.Get("X-Api-Token") == s.cfg.Server.Token {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.cfg.Server.User || pass != s.cfg.Server.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="suzu"`)
			http.Error(w, "需要认证", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var started = time.Now()

// ---- 视图数据 ----

type viewData struct {
	OV      *pipeline.Overview
	Rows    []scheduleRow
	Cursor  int // 「现在」光标插入位置
	Filters []string
}

type scheduleRow struct {
	Sub        pipeline.SubView
	Clock      string
	Next       string // 相对时间：3 分后
	Past       bool
	StateClass string
}

// buildView 计算放送轴：
// 行按「下次轮询时刻」排序，并在第一个未来时刻前插入「现在」光标——
// 于是每一行在页面上的纵向位置本身就编码了它的放送时刻。
func (s *Server) buildView(ctx context.Context) (*viewData, error) {
	ov, err := s.p.Overview(ctx)
	if err != nil {
		return nil, err
	}
	vd := &viewData{OV: ov, Cursor: -1}
	now := time.Now()
	for i, sub := range ov.Subs {
		row := scheduleRow{Sub: sub}
		if sub.NextPoll.IsZero() {
			row.Clock = "--:--"
			row.Next = "未排期"
			row.StateClass = "off"
		} else {
			row.Clock = sub.NextPoll.Format("15:04")
			d := sub.NextPoll.Sub(now)
			row.Past = d < 0
			row.Next = humanDur(d)
			switch {
			case !sub.Enabled:
				row.StateClass = "off"
			case sub.LastError != "":
				row.StateClass = "err"
			case row.Past:
				row.StateClass = "due"
			default:
				row.StateClass = "wait"
			}
		}
		if vd.Cursor < 0 && !row.Past && sub.Enabled {
			vd.Cursor = i
		}
		vd.Rows = append(vd.Rows, row)
	}
	if vd.Cursor < 0 {
		vd.Cursor = len(vd.Rows)
	}
	return vd, nil
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	vd, err := s.buildView(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "dashboard.html", vd)
}

func (s *Server) partial(name string) http.HandlerFunc {
	frag := "frag-" + name
	return func(w http.ResponseWriter, r *http.Request) {
		vd, err := s.buildView(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.render(w, http.StatusOK, frag, vd)
	}
}

func (s *Server) render(w http.ResponseWriter, code int, name string, data any) {
	// 模板在启动时已解析完成，运行期只做执行；
	// 缓冲到内存再写出，避免半个页面的状态泄漏给浏览器。
	var buf strings.Builder
	if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("渲染模板失败", "template", name, "err", err)
		http.Error(w, "模板渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(buf.String()))
}

// ---- API ----

func (s *Server) apiState(w http.ResponseWriter, r *http.Request) {
	ov, err := s.p.Overview(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(ov)
}

func (s *Server) createSub(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	url := strings.TrimSpace(r.FormValue("url"))
	if url == "" {
		writeErr(w, http.StatusBadRequest, errors.New("订阅地址不能为空"))
		return
	}
	if name == "" {
		name = url
	}
	interval, _ := strconv.Atoi(r.FormValue("interval_min"))
	if interval <= 0 {
		interval = 30
	}
	startEp, _ := strconv.ParseFloat(r.FormValue("start_ep"), 64)

	sub := &model.Subscription{
		Name:        name,
		FeedURL:     url,
		Group:       strings.TrimSpace(r.FormValue("group")),
		SavePath:    strings.TrimSpace(r.FormValue("save_path")),
		Include:     config.SplitTokens(r.FormValue("include")),
		Exclude:     config.SplitTokens(r.FormValue("exclude")),
		Prefer:      config.SplitTokens(r.FormValue("prefer")),
		StartEp:     startEp,
		IntervalMin: interval,
		Enabled:     true,
	}
	id, err := s.st.CreateSubscription(r.Context(), sub)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sub.ID = id
	s.p.UpsertSchedule(sub)
	_ = s.st.Log(r.Context(), "poll", "新增订阅："+name)
	s.p.PollNow(id) // 立刻试跑一次，用户马上能看到规则对不对
	redirectBack(w, r, "/")
}

func (s *Server) toggleSub(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	sub, err := s.st.GetSubscription(r.Context(), id)
	if err != nil || sub == nil {
		writeErr(w, http.StatusNotFound, errors.New("订阅不存在"))
		return
	}
	sub.Enabled = !sub.Enabled
	if err := s.st.UpdateSubscription(r.Context(), sub); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.p.UpsertSchedule(sub)
	redirectBack(w, r, "/")
}

func (s *Server) deleteSub(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if err := s.st.DeleteSubscription(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.p.RemoveSchedule(id)
	_ = s.st.Log(r.Context(), "poll", fmt.Sprintf("删除订阅 #%d", id))
	redirectBack(w, r, "/")
}

func (s *Server) pollSub(w http.ResponseWriter, r *http.Request) {
	s.p.PollNow(pathID(r))
	redirectBack(w, r, "/")
}

// backfillSub 把一条已有订阅的起点退到第 1 集，补下往期。
//
// 为什么不能只改起始集数了事：被「集数低于起始集」挡掉的条目已经以
// rejected 状态写进库了，而条目是按 guid 去重的 —— 光把集数改小，
// 下一轮轮询会把它们当成「已处理过」跳过，用户改了设置却什么都没发生。
// 所以这里先把这批记录清掉，再立刻拉一次。
func (s *Server) backfillSub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := pathID(r)
	sub, err := s.st.GetSubscription(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if sub == nil {
		writeErr(w, http.StatusNotFound, errors.New("订阅不存在"))
		return
	}

	sub.StartEp = 0
	if err := s.st.UpdateSubscription(ctx, sub); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	cleared, err := s.st.ClearEpBlocked(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.p.UpsertSchedule(sub) // 调度里存的是订阅副本，同步一份
	_ = s.st.Log(ctx, "poll", fmt.Sprintf("%s：补下往期（起点退到第 1 集，清掉 %d 条被挡记录）",
		sub.Name, cleared))
	s.p.PollNow(id)
	redirectBack(w, r, "/")
}

func (s *Server) controlTask(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	action := r.PathValue("action")
	if err := s.p.ControlTask(r.Context(), id, action); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// redirectBack 优先回到表单指定的页面：同一个操作按钮会出现在
// 面板和番剧库两处，回跳目标由页面上的隐藏字段决定。
func redirectBack(w http.ResponseWriter, r *http.Request, fallback string) {
	to := fallback
	if n := r.FormValue("next"); strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "//") {
		to = n
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
