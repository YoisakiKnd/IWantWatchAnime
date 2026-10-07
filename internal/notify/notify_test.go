package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
)

// recorder 是一个真的 HTTP 服务：通知必须"发出去了"，而不只是调了函数。
type recorder struct {
	mu   sync.Mutex
	got  []map[string]any
	code int
}

func (r *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		payload["_ctype"] = req.Header.Get("Content-Type")
		r.mu.Lock()
		r.got = append(r.got, payload)
		code := r.code
		r.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *recorder) bodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.got))
	for _, p := range r.got {
		out = append(out, p["title"].(string)+" | "+p["body"].(string))
	}
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWebhookDeliversRealRequest(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	m := New(config.Notify{Webhooks: []string{srv.URL}}, srv.Client(), quietLogger())
	if !m.Enabled() {
		t.Fatal("配了 webhook 就该是启用的")
	}
	m.Send(context.Background(), Message{Title: "入库完成：葬送的芙莉莲", Body: "第 28 集\n硬链接"})

	if rec.count() != 1 {
		t.Fatalf("期望收到 1 条，实际 %d", rec.count())
	}
	got := rec.got[0]
	if got["source"] != "suzu" {
		t.Errorf("source = %v，期望 suzu（接收端要能分辨是谁发的）", got["source"])
	}
	if !strings.Contains(got["body"].(string), "第 28 集") {
		t.Errorf("正文丢了: %v", got["body"])
	}
	if ts, ok := got["ts"].(float64); !ok || ts < 1e9 {
		t.Errorf("缺少 unix 时间戳: %v", got["ts"])
	}
	if ct := got["_ctype"].(string); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望 JSON", ct)
	}
}

func TestFanOutToEveryWebhook(t *testing.T) {
	a, b := &recorder{}, &recorder{}
	srvA := httptest.NewServer(a.handler())
	defer srvA.Close()
	srvB := httptest.NewServer(b.handler())
	defer srvB.Close()

	m := New(config.Notify{Webhooks: []string{srvA.URL, srvB.URL}}, srvA.Client(), quietLogger())
	m.Send(context.Background(), Message{Title: "下载失败", Body: "断种"})
	if a.count() != 1 || b.count() != 1 {
		t.Errorf("两个通道都要收到，实际 %d/%d", a.count(), b.count())
	}
}

func TestThrottleSameTitle(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	m := New(config.Notify{Webhooks: []string{srv.URL}, ThrottleMin: 5}, srv.Client(), quietLogger())
	for i := 0; i < 4; i++ {
		m.Send(context.Background(), Message{Title: "下载失败", Body: "同一个错误不该刷屏"})
	}
	if rec.count() != 1 {
		t.Errorf("同一个标题 4 次只应发 1 条，实际 %d", rec.count())
	}
	// 不同标题不受影响：节流按标题去重，不是全局静音。
	m.Send(context.Background(), Message{Title: "入库完成：某番剧", Body: "第 1 集"})
	if rec.count() != 2 {
		t.Errorf("不同标题应当照常发，实际 %d", rec.count())
	}
	// 过了窗口期要能继续发。
	m.mu.Lock()
	m.last["下载失败"] = time.Now().Add(-time.Hour)
	m.mu.Unlock()
	m.Send(context.Background(), Message{Title: "下载失败", Body: "窗口过后再发一次"})
	if rec.count() != 3 {
		t.Errorf("过了节流窗口应当重发，实际 %d", rec.count())
	}
}

func TestThrottleDisabledWhenZero(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	m := New(config.Notify{Webhooks: []string{srv.URL}}, srv.Client(), quietLogger())
	m.Send(context.Background(), Message{Title: "同一个标题", Body: "a"})
	m.Send(context.Background(), Message{Title: "同一个标题", Body: "b"})
	if rec.count() != 2 {
		t.Errorf("throttle_min=0 表示不节流，实际只发了 %d 条", rec.count())
	}
}

// 通道返回 500 时只能记日志：通知是旁路，绝不能把下载主流程拖挂。
func TestWebhookFailureIsSwallowed(t *testing.T) {
	rec := &recorder{code: http.StatusInternalServerError}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	m := New(config.Notify{Webhooks: []string{"http://127.0.0.1:1/closed", srv.URL}}, srv.Client(), quietLogger())
	m.Send(context.Background(), Message{Title: "投递失败", Body: "x"}) // 不该 panic，也不该阻塞
	if rec.count() != 1 {
		t.Errorf("坏通道之后的通道仍应收到，实际 %d", rec.count())
	}
}

func TestTelegramRequestShape(t *testing.T) {
	var mu sync.Mutex
	var path, ctype, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		path, ctype, body = r.URL.Path, r.Header.Get("Content-Type"), string(raw)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	m := New(config.Notify{Telegram: config.Telegram{BotToken: "123:ABC", ChatID: "-10099"}}, srv.Client(), quietLogger())
	m.tgBase = srv.URL
	m.Send(context.Background(), Message{Title: "入库完成：葬送的芙莉莲", Body: "第 28 集"})

	mu.Lock()
	defer mu.Unlock()
	if path != "/bot123:ABC/sendMessage" {
		t.Errorf("请求路径 = %q，期望 /bot{token}/sendMessage", path)
	}
	if !strings.Contains(ctype, "x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q", ctype)
	}
	if !strings.Contains(body, "chat_id=-10099") {
		t.Errorf("chat_id 丢了: %q", body)
	}
	if !strings.Contains(body, "text=") || !strings.Contains(body, "%E5%85%A5%E5%BA%93") { // 入库 的 URL 编码
		t.Errorf("正文没有正确编码: %q", body)
	}
}

func TestTelegramErrorSurfacesDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer srv.Close()
	m := New(config.Notify{Telegram: config.Telegram{BotToken: "1:x", ChatID: "1"}}, srv.Client(), quietLogger())
	m.tgBase = srv.URL
	err := m.postTelegram(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("应当把 Telegram 的描述带出来，实际 %v", err)
	}
}

func TestNoChannelMeansNil(t *testing.T) {
	if m := New(config.Notify{}, http.DefaultClient, quietLogger()); m.Enabled() {
		t.Error("没有配置任何通道时应当返回 nil，调用方靠判空跳过")
	}
	m := New(config.Notify{Webhooks: []string{"http://example.invalid/x"}}, http.DefaultClient, quietLogger())
	m.Send(context.Background(), Message{Title: "t", Body: "b"}) // 发不出去也只是日志
	if !m.Enabled() {
		t.Error("配了通道就该启用")
	}
}
