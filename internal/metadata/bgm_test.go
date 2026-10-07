package metadata

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// bgmJSON 是 api.bgm.tv /v0/search/subjects 的真实响应切片（2026-10-06 实测）。
//
// 特意保留两个坑：
//   - id 是数字，不是字符串；
//   - 评分在 rating.score 里，不在顶层。
//
// 这两点以前把 Info 的解析整条打挂（json 解不动就整个 Decode 失败），
// 而调用方只看到 "err != nil 就跳过"，于是刮削静悄悄地永远没生效。
const bgmJSON = `{"total":5,"data":[{
  "id":400602,"name":"葬送のフリーレン","name_cn":"葬送的芙莉莲",
  "summary":"电视动画片《葬送的芙莉莲》改编自山田钟人负责故事……",
  "date":"2023-09-29","eps":28,"image":"https://lain.bgm.tv/pic/cover/l/13/c5/400602_ZI8Y9.jpg",
  "images":{"large":"https://lain.bgm.tv/pic/cover/l/13/c5/400602_ZI8Y9.jpg","common":"https://lain.bgm.tv/r/400/pic/cover/l/13/c5/400602_ZI8Y9.jpg"},
  "rating":{"score":8.5,"total":12345}}]}`

func fakeBGM(t *testing.T, hits *int32) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/search/subjects" {
			t.Errorf("意外的路径 %s", r.URL.Path)
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("Bangumi 要求带 UA，这里没带")
		}
		var body struct {
			Keyword string `json:"keyword"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		atomic.AddInt32(hits, 1)
		if body.Keyword == "不存在的番" {
			_, _ = w.Write([]byte(`{"total":0,"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(bgmJSON))
	}))
	t.Cleanup(srv.Close)
	c := New(true, "", srv.Client())
	c.SetBase(srv.URL)
	return c
}

func TestSearchDecodesRealShape(t *testing.T) {
	var hits int32
	c := fakeBGM(t, &hits)
	got, err := c.Search(context.Background(), "葬送的芙莉莲")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got == nil {
		t.Fatal("应当拿到条目")
	}
	if got.ID != 400602 {
		t.Errorf("ID = %d，期望 400602（数字字段不能按字符串解）", got.ID)
	}
	if got.Name() != "葬送的芙莉莲" {
		t.Errorf("Name() = %q，期望中文名", got.Name())
	}
	if got.Score != 8.5 {
		t.Errorf("Score = %v，期望 8.5（来自 rating.score）", got.Score)
	}
	if got.Episodes != 28 {
		t.Errorf("Episodes = %d，期望 28：蜜柑不写总集数，这是唯一来源", got.Episodes)
	}
	if got.Summary == "" || got.Date != "2023-09-29" {
		t.Errorf("简介/日期没解出来：%q %q", got.Summary, got.Date)
	}
	if want := "large"; !hasSuffix(got.Better(), "/large/") && !contains(got.Better(), "/cover/l/") {
		t.Errorf("Better() = %q，期望优先大图（%s）", got.Better(), want)
	}
}

func TestSearchCachesAndSkipsSecondCall(t *testing.T) {
	var hits int32
	c := fakeBGM(t, &hits)
	for i := 0; i < 3; i++ {
		if _, err := c.Search(context.Background(), "葬送的芙莉莲"); err != nil {
			t.Fatalf("第 %d 次 Search: %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("外网请求 %d 次，期望 1 次（进程内缓存）", hits)
	}
}

func TestSearchEmptyResultCached(t *testing.T) {
	var hits int32
	c := fakeBGM(t, &hits)
	for i := 0; i < 2; i++ {
		got, err := c.Search(context.Background(), "不存在的番")
		if err != nil || got != nil {
			t.Fatalf("期望 nil,nil，实际 %v,%v", got, err)
		}
	}
	if hits != 1 {
		t.Errorf("查空也算结果，应当只问一次，实际 %d 次", hits)
	}
}

func TestDisabledReturnsNil(t *testing.T) {
	c := New(false, "", http.DefaultClient)
	got, err := c.Search(context.Background(), "葬送的芙莉莲")
	if got != nil || err != nil {
		t.Fatalf("关掉刮削不该有任何动作，实际 %v,%v", got, err)
	}
	if c.Enabled() {
		t.Error("Enabled() 应当为 false")
	}
}

func TestBreakerAfterThreeFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests) // 匿名查询被限流的样子
	}))
	defer srv.Close()
	c := New(true, "", srv.Client())
	c.SetBase(srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.Search(context.Background(), "番剧"+string(rune('a'+i))); err == nil {
			t.Fatal("限流应当报错")
		}
	}
	if _, err := c.Search(context.Background(), "第四个"); err != nil {
		t.Fatalf("熔断期间应当安静地返回 nil，而不是继续报错：%v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 3 {
		t.Errorf("熔断后不该再发请求，实际 %d 次", n)
	}
	c.mu.Lock()
	c.until = time.Now().Add(-time.Second) // 手动解除，验证能恢复
	c.mu.Unlock()
	if _, err := c.Search(context.Background(), "第五个"); err == nil {
		t.Error("解除熔断后应当重新尝试")
	}
}

func TestCleanKeyword(t *testing.T) {
	cases := []struct{ in, want string }{
		{"葬送的芙莉莲 / Sousou no Frieren [01-28 修正合集]", "葬送的芙莉莲"},
		{"【我推的孩子】", "【我推的孩子】"}, // 开头的括号是名字的一部分，原样保留
		{"胆大党 第二季", "胆大党 第二季"},
		{"药屋少女的呢喃（第二季）", "药屋少女的呢喃"},
		{"Sousou no Frieren [1080p]", "Sousou no Frieren"},
	}
	for _, c := range cases {
		if got := CleanKeyword(c.in); got != c.want {
			t.Errorf("CleanKeyword(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestBetterPrefersLarge(t *testing.T) {
	i := &Info{Poster: "small.jpg"}
	i.Images.Large = "large.jpg"
	i.Images.Common = "common.jpg"
	if i.Better() != "large.jpg" {
		t.Errorf("Better() = %q", i.Better())
	}
	i.Images.Large = ""
	if i.Better() != "common.jpg" {
		t.Errorf("大图缺失时应当退回中图，实际 %q", i.Better())
	}
	i.Images.Common = ""
	if i.Better() != "small.jpg" {
		t.Errorf("都没有时应当用 image，实际 %q", i.Better())
	}
}

// TestLiveSmokeBGM 打真实 api.bgm.tv。默认跳过，BGM_LIVE=1 才跑：
// 匿名查询也能用，但配额有限，不适合放进常规 CI。
func TestLiveSmokeBGM(t *testing.T) {
	if os.Getenv("BGM_LIVE") != "1" {
		t.Skip("设置 BGM_LIVE=1 才跑真实抓取")
	}
	c := New(true, "", &http.Client{Timeout: 12 * time.Second})
	info, err := c.Search(context.Background(), "葬送的芙莉莲")
	if err != nil {
		t.Fatalf("真实抓取失败：%v", err)
	}
	if info == nil || info.Summary == "" || info.Episodes == 0 {
		t.Fatalf("真实响应解析不完整：%+v", info)
	}
	// 真实接口的封面在 images.large 里：这里必须能拿到地址，
	// 否则媒体库那一步没图可下（曾经就是这样，海报永远是空的）。
	if !hasPrefix(info.Poster, "http") {
		t.Fatalf("真实响应没给出封面地址：%q", info.Poster)
	}
	t.Logf("id=%d 中文名=%s 集数=%d 评分=%.1f 简介=%d 字 封面=%s",
		info.ID, info.Name(), info.Episodes, info.Score, len(info.Summary), info.Poster)

	// 顺手把封面下下来，确认那条地址真能用（不是死链）。
	resp, err := http.Get(info.Poster)
	if err != nil {
		t.Fatalf("封面下载失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("封面下载失败：HTTP %d", resp.StatusCode)
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(buf) < 1024 || !hasPrefix(resp.Header.Get("Content-Type"), "image/") {
		t.Fatalf("封面不是图片：%d 字节 %s", len(buf), resp.Header.Get("Content-Type"))
	}
	t.Logf("封面可下：%d 字节 %s", len(buf), resp.Header.Get("Content-Type"))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasPrefix(s, pre string) bool { return len(s) >= len(pre) && s[:len(pre)] == pre }

func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
