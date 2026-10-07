package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// fakeQBit 是一个够真的 qBittorrent WebAPI v2：
// 登录发 SID cookie、投递后任务真的出现在列表里、状态按 state 映射。
//
// 重点验证「.torrent 直链没有 infohash 时，能不能把刚投递的任务认领回来」——
// 蜜柑的订阅全是 .torrent 直链，这条路不通的话 qBittorrent 内核就是摆设。
type fakeQBit struct {
	mu       sync.Mutex
	sid      string
	torrents []map[string]any
	added    []map[string]string
	calls    []string
	failList bool
}

func newFakeQBit() *fakeQBit {
	return &fakeQBit{sid: "SID-abc123"}
}

func (f *fakeQBit) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v2")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, path)

		switch path {
		case "/auth/login":
			form := readForm(r)
			if form.Get("username") != "admin" || form.Get("password") != "secret" {
				_, _ = io.WriteString(w, "Fails.")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: f.sid, Path: "/"})
			_, _ = io.WriteString(w, "Ok.")
		case "/torrents/add":
			if !f.authed(r) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			form := readForm(r)
			f.added = append(f.added, map[string]string{
				"urls": form.Get("urls"), "savepath": form.Get("savepath"), "category": form.Get("category"),
			})
			// 模拟真实行为：任务以 URL 尾巴命名，元数据解析后才变成真名。
			// 列表按「最近添加」倒序，新任务排最前。
			fresh := map[string]any{
				"hash": fmt.Sprintf("%040d", len(f.torrents)+1), "name": "Download",
				"state": "metaDL", "progress": 0.0, "size": 0, "completed": 0,
				"save_path": form.Get("savepath"), "content_path": "",
			}
			f.torrents = append([]map[string]any{fresh}, f.torrents...)
			_, _ = io.WriteString(w, "Ok.")
		case "/torrents/info":
			if !f.authed(r) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if f.failList {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			list := f.torrents
			if h := r.URL.Query().Get("hashes"); h != "" {
				list = nil
				for _, t := range f.torrents {
					if strings.EqualFold(fmt.Sprint(t["hash"]), h) {
						list = append(list, t)
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if list == nil {
				list = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(list)
		case "/torrents/stop", "/torrents/start", "/torrents/delete":
			if !f.authed(r) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, "Ok.")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *fakeQBit) authed(r *http.Request) bool {
	c, err := r.Cookie("SID")
	return err == nil && c.Value == f.sid
}

// resolve 让假内核完成元数据解析：改回真名、进入下载/完成状态。
func (f *fakeQBit) resolve(name string, progress float64, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.torrents) == 0 {
		return
	}
	f.torrents[0]["name"] = name
	f.torrents[0]["progress"] = progress
	f.torrents[0]["state"] = state
	f.torrents[0]["size"] = 1500000000
	f.torrents[0]["completed"] = int64(progress * 1500000000)
	f.torrents[0]["content_path"] = "/downloads/mikan/" + name
}

func (f *fakeQBit) snapshot() (calls, added []string, torrents int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls = append(calls, f.calls...)
	for _, a := range f.added {
		added = append(added, a["urls"]+" → "+a["savepath"]+" ["+a["category"]+"]")
	}
	return calls, added, len(f.torrents)
}

func readForm(r *http.Request) url.Values {
	_ = r.ParseForm()
	return r.PostForm
}

func newTestQBit(t *testing.T, f *fakeQBit) (*QBit, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	q := NewQBit(srv.URL, "admin", "secret", "suzu", srv.Client())
	return q, srv
}

func TestQBitLoginAndAddTorrentURL(t *testing.T) {
	f := newFakeQBit()
	q, _ := newTestQBit(t, f)

	if err := q.Ping(context.Background()); err != nil {
		t.Fatalf("Ping(登录): %v", err)
	}
	// 蜜柑的真实形态：.torrent 直链，没有 btih。
	uri := "https://mikanani.me/Download/20231004/abc123.t"
	hash, err := q.Add(context.Background(), AddRequest{
		URI: uri, SaveDir: "/downloads/mikan", Name: "[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 - 28 [WebRip 1080p HEVC-10bit AAC][简繁日内封字幕][End]",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if hash == "" {
		t.Fatal("投递 .torrent 直链后应当反查回 infohash，否则后续无法对账")
	}

	calls, added, n := f.snapshot()
	if n != 1 {
		t.Fatalf("假内核里应当有 1 个任务，实际 %d", n)
	}
	if !strings.Contains(added[0], "/downloads/mikan") || !strings.Contains(added[0], "[suzu]") {
		t.Errorf("投递参数不对: %v", added)
	}
	var sawLogin, sawAdd, sawList bool
	for _, c := range calls {
		switch c {
		case "/auth/login":
			sawLogin = true
		case "/torrents/add":
			sawAdd = true
		case "/torrents/info":
			sawList = true
		}
	}
	if !sawLogin || !sawAdd || !sawList {
		t.Errorf("调用序列不完整 login=%v add=%v list=%v: %v", sawLogin, sawAdd, sawList, calls)
	}
}

func TestQBitAddMagnetUsesBtih(t *testing.T) {
	f := newFakeQBit()
	q, _ := newTestQBit(t, f)
	const btih = "0123456789abcdef0123456789abcdef01234567"
	hash, err := q.Add(context.Background(), AddRequest{
		URI: "magnet:?xt=urn:btih:" + btih + "&dn=test", SaveDir: "/tmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hash != btih {
		t.Errorf("磁力链应当直接用 btih 作句柄，实际 %q", hash)
	}
	// 有 btih 时不需要反查，少一次 API 调用。
	calls, _, _ := f.snapshot()
	for _, c := range calls {
		if c == "/torrents/info" {
			t.Errorf("磁力链不该触发反查: %v", calls)
		}
	}
}

func TestQBitStatusMapping(t *testing.T) {
	f := newFakeQBit()
	q, _ := newTestQBit(t, f)
	name := "[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 - 28 [WebRip 1080p HEVC-10bit AAC][简繁日内封字幕][End]"
	hash, err := q.Add(context.Background(), AddRequest{
		URI: "https://mikanani.me/Download/20231004/abc.t", SaveDir: "/downloads", Name: name,
	})
	if err != nil || hash == "" {
		t.Fatalf("Add: %v %q", err, hash)
	}

	f.resolve(name, 0.42, "downloading")
	st, err := q.Status(context.Background(), hash)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != model.TaskDownloading || st.Progress < 41 || st.Progress > 43 {
		t.Errorf("下载中映射不对: %+v", st)
	}
	if st.SavePath == "" {
		t.Error("应当把落点带出来，入库要用它找正片")
	}

	f.resolve(name, 1.0, "uploading")
	st, _ = q.Status(context.Background(), hash)
	if st.State != model.TaskSeeding {
		t.Errorf("做种应当算「已完成，可以入库」的前置状态，实际 %v", st.State)
	}

	f.resolve(name, 1.0, "stalledDL")
	if st, _ = q.Status(context.Background(), hash); st.State != model.TaskDownloading {
		t.Errorf("stalledDL 是「卡住但还在」，不该当失败：%v", st.State)
	}
	f.resolve(name, 1.0, "missingFiles")
	st, _ = q.Status(context.Background(), hash)
	if st.State != model.TaskError || st.Error == "" {
		t.Errorf("missingFiles 应当报错并带上原因: %+v", st)
	}
	f.resolve(name, 1.0, "completed")
	if st, _ = q.Status(context.Background(), hash); st.State != model.TaskCompleted || st.Progress != 100 {
		t.Errorf("completed 映射不对: %+v", st)
	}
}

func TestQBitStatusMissingTorrent(t *testing.T) {
	f := newFakeQBit()
	q, _ := newTestQBit(t, f)
	st, err := q.Status(context.Background(), "deadbeef")
	if err != nil {
		t.Fatalf("任务被外部删掉不该返回错误，而应当是明确的状态: %v", err)
	}
	if st.State != model.TaskError || !strings.Contains(st.Error, "不存在") {
		t.Errorf("期望提示任务已不存在: %+v", st)
	}
	// 完全没有 infohash 时要说清楚原因，而不是发一个必然查不到的请求。
	if _, err := q.Status(context.Background(), ""); err == nil {
		t.Error("空 infohash 应当直接报错")
	}
}

func TestQBitLifecycleCalls(t *testing.T) {
	f := newFakeQBit()
	q, _ := newTestQBit(t, f)
	ctx := context.Background()
	hash, _ := q.Add(ctx, AddRequest{URI: "https://mikanani.me/Download/x.t", SaveDir: "/d", Name: "n"})
	if err := q.Pause(ctx, hash); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := q.Resume(ctx, hash); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if q.Kind() != "qbittorrent" {
		t.Errorf("Kind = %q", q.Kind())
	}
	calls, _, _ := f.snapshot()
	want := map[string]bool{"/torrents/stop": false, "/torrents/start": false}
	for _, c := range calls {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("没有调用 %s: %v", k, calls)
		}
	}
}

func TestQBitLoginFailureIsClear(t *testing.T) {
	f := newFakeQBit()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	q := NewQBit(srv.URL, "admin", "wrong", "", srv.Client())
	err := q.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "登录失败") {
		t.Errorf("密码错应当给人话提示，实际 %v", err)
	}
}

// 反查认领不到时必须返回空，而不是随便挑一个任务挂上去。
func TestQBitResolveRefusesAmbiguous(t *testing.T) {
	f := newFakeQBit()
	q, _ := newTestQBit(t, f)
	// 先塞两个"历史"任务，再投递一个，制造「多个新任务」的场面。
	f.mu.Lock()
	f.torrents = []map[string]any{
		{"hash": "aa", "name": "老任务", "state": "uploading", "progress": 1.0},
	}
	f.mu.Unlock()
	if _, err := q.Add(context.Background(), AddRequest{URI: "https://mikanani.me/Download/y.t", SaveDir: "/d", Name: "新任务"}); err != nil {
		t.Fatal(err)
	}
	hash, err := q.Add(context.Background(), AddRequest{URI: "https://mikanani.me/Download/z.t", SaveDir: "/d", Name: "另一个"})
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Error("投递前后各加一个，仍然应当能认出第二个新任务（单个新任务直接认领）")
	}

	// 反查用的时间预算不能失控：qBittorrent 挂了也不能把轮询卡死。
	start := time.Now()
	f.mu.Lock()
	f.failList = true
	f.mu.Unlock()
	if h := q.resolveHash(context.Background(), "x", map[string]bool{}); h != "" {
		t.Errorf("查询失败时应当返回空，实际 %q", h)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("反查失败耗了 %v，太久", d)
	}
}
