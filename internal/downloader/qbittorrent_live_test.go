package downloader

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// 真机联调：对着一个真的 qBittorrent Web API 走一遍投递 / 认领 / 状态 /
// 暂停 / 继续 / 删除。默认跳过，要用的时候：
//
//	qbittorrent-nox --profile=/tmp/qbt --webui-port=8080
//	QBT_LIVE=1 QBT_PASS=<首页打印的临时密码> go test ./internal/downloader/ -run TestQBitLive -v
//
// 单元测试用的是假 Web API（自己写的假服务器只能证明"我以为的协议"），
// 这个用例证明的是"qBittorrent 实际怎么回"。
func TestQBitLiveEndToEnd(t *testing.T) {
	if os.Getenv("QBT_LIVE") == "" {
		t.Skip("需要真机 qBittorrent：置 QBT_LIVE=1 并提供 QBT_PASS")
	}
	endpoint := envOr("QBT_ENDPOINT", "http://127.0.0.1:8080")
	user := envOr("QBT_USER", "admin")
	pass := os.Getenv("QBT_PASS")
	if pass == "" {
		t.Fatal("必须给 QBT_PASS（qbittorrent-nox 首次启动会在日志里打印临时密码）")
	}

	// 现造一个合法的单文件 .torrent：qbittorrent 得能解析出 infohash，
	// 我们才能拿它跟 Add 返回的句柄逐字节核对。
	payload := []byte("suzu live test payload ")
	for len(payload) < 4096 {
		payload = append(payload, payload...)
	}
	payload = payload[:4096]
	piece := sha1.Sum(payload)
	torrent, infoHash := buildTorrent(t, payload, piece[:], "suzu-demo-ep01.mkv")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(torrent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	q := NewQBit(endpoint, user, pass, "suzu-live", &http.Client{Timeout: 8 * time.Second})
	if err := q.Ping(ctx); err != nil {
		t.Fatalf("登录真机失败: %v", err)
	}

	// 1) .torrent 直链（蜜柑订阅走的就是这条）：返回的必须是真实 infohash。
	gid, err := q.Add(ctx, AddRequest{
		URI:     srv.URL + "/demo.torrent",
		SaveDir: t.TempDir(),
		Name:    "[喵萌奶茶屋&LoliHouse] 葬送的芙莉莲 - 01 [1080p]",
	})
	if err != nil {
		t.Fatalf("投递 .torrent 失败: %v", err)
	}
	if gid != infoHash {
		t.Fatalf("认领的句柄不对: 拿到 %q，种子里是 %q", gid, infoHash)
	}
	t.Logf("真机投递 .torrent 成功，infohash=%s", gid)

	// 2) 状态：没 peer 的种子会停在 stalledDL，映射成「下载中」。
	st, err := q.Status(ctx, gid)
	if err != nil {
		t.Fatalf("查询状态失败: %v", err)
	}
	t.Logf("真机状态: state=%v progress=%.1f%% total=%d done=%d", st.State, st.Progress, st.TotalBytes, st.DoneBytes)
	if st.TotalBytes != int64(len(payload)) {
		t.Errorf("总字节数应当来自种子本身（%d），实际 %d", len(payload), st.TotalBytes)
	}
	if st.State != model.TaskQueued && st.State != model.TaskDownloading {
		t.Errorf("没有 peer 的种子应当是排队/下载中，实际 %v（err=%s）", st.State, st.Error)
	}

	// 3) 暂停 / 继续：真机上这两个调用不报错即可（状态由 qBittorrent 决定）。
	if err := q.Pause(ctx, gid); err != nil {
		t.Errorf("暂停失败: %v", err)
	}
	if err := q.Resume(ctx, gid); err != nil {
		t.Errorf("继续失败: %v", err)
	}

	// 4) 磁力链：btih 就是句柄，投递后应当原样返回（不再依赖反查）。
	magnet := "magnet:?xt=urn:btih:" + infoHash + "&dn=suzu-demo-ep01.mkv"
	m, err := q.Add(ctx, AddRequest{URI: magnet, SaveDir: t.TempDir(), Name: "magnet-probe"})
	if err != nil {
		t.Fatalf("投递磁力链失败: %v", err)
	}
	if m != infoHash {
		t.Errorf("磁力链应直接返回 btih 作句柄: %q", m)
	}

	// 5) 清理：删掉任务。qBittorrent 的删除是异步的（真机上会晚几十毫秒
	// 才从列表里消失），所以这里等它生效，而不是删完立刻断言。
	if err := q.Remove(ctx, gid); err != nil {
		t.Errorf("删除任务失败: %v", err)
	}
	// 任务被删掉后，Status 不报错，而是明确说「已不存在」（对账据此提示用户）。
	gone := false
	for i := 0; i < 20; i++ {
		st, err := q.Status(ctx, gid)
		if err != nil {
			t.Errorf("删除后的查询不该报错（应当由状态说明）: %v", err)
			break
		}
		if st.State == model.TaskError && strings.Contains(st.Error, "不存在") {
			gone = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !gone {
		t.Error("删掉的任务应当在真机上被识别为「已不存在」")
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// buildTorrent 拼一个单文件种子，并算出 infohash（info 字典的 sha1）。
// 不引第三方 bencode 库：这里只需要一个固定形状的字典。
func buildTorrent(t *testing.T, payload, pieces []byte, name string) (torrent []byte, infoHash string) {
	t.Helper()
	var info []byte
	info = append(info, 'd')
	info = append(info, "6:lengthi"...)
	info = append(info, []byte(itoa(len(payload)))...)
	info = append(info, 'e')
	info = append(info, []byte("4:name"+itoa(len(name))+":"+name)...)
	info = append(info, []byte("12:piece lengthi32768e")...)
	info = append(info, []byte("6:pieces"+itoa(len(pieces))+":")...)
	info = append(info, pieces...)
	info = append(info, 'e')

	sum := sha1.Sum(info)
	announce := "http://127.0.0.1:9/announce"
	var out []byte
	out = append(out, []byte("d8:announce"+itoa(len(announce))+":"+announce)...)
	out = append(out, []byte("4:info")...)
	out = append(out, info...)
	out = append(out, 'e')

	// 顺手回读一次：种子文件必须能被解析出同一个 hash，否则测试自己就是错的。
	if got := parseInfoHashForTest(out); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("自造种子不合法: %s != %s", got, hex.EncodeToString(sum[:]))
	}
	return out, hex.EncodeToString(sum[:])
}

// parseInfoHashForTest 从 bencode 里切出 info 字典并算 sha1。
//
// 不能靠数 'd'/'e' 找边界：pieces 是二进制，里面出现 'e' 是家常便饭。
// 老老实实按 bencode 规则跳值（整数看到 e、字符串先读长度）。
func parseInfoHashForTest(data []byte) string {
	if len(data) == 0 || data[0] != 'd' {
		return ""
	}
	i := 1
	for i < len(data) && data[i] != 'e' {
		keyEnd, ok := bencodeSkip(data, i)
		if !ok {
			return ""
		}
		key := bencodeKey(data[i:keyEnd])
		valEnd, ok := bencodeSkip(data, keyEnd)
		if !ok {
			return ""
		}
		if key == "info" {
			sum := sha1.Sum(data[keyEnd:valEnd])
			return hex.EncodeToString(sum[:])
		}
		i = valEnd
	}
	return ""
}

// bencodeSkip 返回从 i 开始的那个 bencode 值结束后的下标。
func bencodeSkip(data []byte, i int) (int, bool) {
	if i >= len(data) {
		return 0, false
	}
	switch data[i] {
	case 'i':
		j := bytes.IndexByte(data[i:], 'e')
		if j < 0 {
			return 0, false
		}
		return i + j + 1, true
	case 'l':
		i++
		for i < len(data) && data[i] != 'e' {
			n, ok := bencodeSkip(data, i)
			if !ok {
				return 0, false
			}
			i = n
		}
		return i + 1, true
	case 'd':
		i++
		for i < len(data) && data[i] != 'e' {
			n, ok := bencodeSkip(data, i) // 键
			if !ok {
				return 0, false
			}
			n, ok = bencodeSkip(data, n) // 值
			if !ok {
				return 0, false
			}
			i = n
		}
		return i + 1, true
	default:
		j := bytes.IndexByte(data[i:], ':')
		if j < 0 {
			return 0, false
		}
		l, err := strconv.Atoi(string(data[i : i+j]))
		if err != nil {
			return 0, false
		}
		end := i + j + 1 + l
		if end > len(data) {
			return 0, false
		}
		return end, true
	}
}

// bencodeKey 把 "<长度>:<字节>" 形式的键还原成字符串。
func bencodeKey(raw []byte) string {
	j := bytes.IndexByte(raw, ':')
	if j < 0 {
		return ""
	}
	l, err := strconv.Atoi(string(raw[:j]))
	if err != nil || j+1+l > len(raw) {
		return ""
	}
	return string(raw[j+1 : j+1+l])
}

func itoa(n int) string { return strconv.Itoa(n) }
