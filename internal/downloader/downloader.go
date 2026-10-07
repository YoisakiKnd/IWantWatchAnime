// Package downloader 把「下载」这件事抽象成一个很小很薄的接口。
//
// 架构取舍（这是与 ANI-RSS 类项目最大的分歧点）：
// 不在主程序里内嵌 BT 栈，而是把下载委托给外部 aria2 / qBittorrent。
// 理由：玩客云是四核 Cortex-A5 + 1GB 内存，BT 哈希与 peer 管理是 CPU 密集的，
// 让 aria2（armv7 上到处都有包，常驻约 20MB）独立承担，主程序可以一直保持
// 「单二进制、<30MB 常驻、空闲 0% CPU」，并且崩溃互不影响。
package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

// ErrDisabled 表示下载内核被关闭（engine.kind = none，只跑匹配方便调规则）。
var ErrDisabled = errors.New("下载内核已关闭")

// AddRequest 是一次投递请求。
type AddRequest struct {
	URI     string // magnet 或 .torrent 地址
	SaveDir string // 下载目录（引擎侧）
	Name    string // 建议名称，仅用于日志
}

// Status 是统一的下载状态快照。
type Status struct {
	GID        string
	FollowedBy string // aria2 的 BT 元数据跟进任务：非空时应切换到新 gid
	State      model.TaskState
	Progress   float64 // 0..100
	TotalBytes int64
	DoneBytes  int64
	SavePath   string // 完成后用于整理入库的落点
	Error      string
}

// Downloader 是下载内核需要实现的全部能力。
type Downloader interface {
	// Kind 返回内核名，写入任务记录，便于排查「是谁下的」。
	Kind() string
	// Add 投递任务并返回引擎侧句柄（gid / infohash）。
	Add(ctx context.Context, req AddRequest) (string, error)
	// Status 查询单个任务状态。对账循环每 ReconcileSec 秒调一次。
	Status(ctx context.Context, gid string) (Status, error)
	// Pause / Resume / Remove 供面板手动干预。
	Pause(ctx context.Context, gid string) error
	Resume(ctx context.Context, gid string) error
	Remove(ctx context.Context, gid string) error
	// Ping 由启动阶段调用，未就绪时给出可执行的错误提示。
	Ping(ctx context.Context) error
}

// New 按配置挑选内核。http.Client 由调用方注入，这样超时、代理、
// 连接池策略只有一处定义，各内核共用一份 keep-alive 连接。
func New(cfg config.Config, hc *http.Client) (Downloader, error) {
	switch cfg.Engine.Kind {
	case "aria2":
		return NewAria2(cfg.Engine.Aria2, hc), nil
	case "qbittorrent":
		q := cfg.Engine.QBit
		return NewQBit(q.Endpoint, q.User, q.Password, q.Category, hc), nil
	case "none":
		return nil, nil
	default:
		return nil, fmt.Errorf("未知下载内核 %q", cfg.Engine.Kind)
	}
}

// trimGID 归一化句柄：qBittorrent 只有小写 hash，用户可能贴大写的。
func trimGID(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func clamp100(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
