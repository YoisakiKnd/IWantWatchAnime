// suzu —— 轻量的番剧订阅归档器。
//
// 目标平台是玩客云这类 armv7 小机器（四核 A5 + 1GB 内存）：
// 单二进制、无 CGO、无 Node、无数据库服务，常驻内存目标 30MB 以内。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/downloader"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/httpx"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/pipeline"
	"github.com/YoisakiKnd/IWantWatchAnime/internal/store"
)

// version 由构建脚本注入：-ldflags "-X main.version=..."
var version = "dev"

func main() {
	var (
		cfgPath     = flag.String("config", "config.toml", "配置文件路径")
		showVersion = flag.Bool("version", false, "打印版本后退出")
		listenAddr  = flag.String("listen", "", "覆盖监听地址，例如 0.0.0.0:8637")
		logLevel    = flag.String("log-level", "", "覆盖日志级别：debug/info/warn/error")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("suzu %s (%s)\n", version, buildTarget())
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(2)
	}
	if *listenAddr != "" {
		cfg.Server.Listen = *listenAddr
	}
	if *logLevel != "" {
		cfg.Runtime.LogLevel = *logLevel
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(2)
	}
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "创建目录失败:", err)
		os.Exit(2)
	}

	log := newLogger(cfg.Runtime.LogLevel)
	pipeline.Version = version

	// 内存硬闸：GOMEMLIMIT 让 GC 在接近上限时提前回收，
	// 配合更低的 GOGC，可以把稳态 RSS 压在 1GB 机器能长期承受的范围内。
	debug.SetMemoryLimit(int64(cfg.Runtime.MemLimitMiB) << 20)
	debug.SetGCPercent(50)

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		log.Error("打开数据库失败", "path", cfg.DBPath(), "err", err)
		os.Exit(1)
	}
	defer st.Close()

	hc := newHTTPClient(cfg)
	dl, err := downloader.New(cfg, hc)
	if err != nil {
		log.Error("初始化下载内核失败", "err", err)
		os.Exit(1)
	}

	p := pipeline.New(cfg, st, dl, hc, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := p.Bootstrap(ctx); err != nil {
		log.Error("启动失败", "err", err)
		os.Exit(1)
	}

	srv, err := httpx.New(cfg, st, p, log)
	if err != nil {
		log.Error("初始化面板失败", "err", err)
		os.Exit(1)
	}
	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go p.Run(ctx)
	go func() {
		log.Info("面板已启动",
			"地址", "http://"+cfg.Server.Listen,
			"数据目录", cfg.Storage.DataDir,
			"下载目录", cfg.Engine.Aria2.Dir,
			"媒体库", cfg.Library.Root,
			"版本", version,
			"目标", buildTarget())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务退出", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("收到退出信号，正在收尾")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 关闭超时", "err", err)
	}
	// 退出前把 WAL 落盘，避免非正常断电后 SQLite 需要恢复。
	if err := st.Checkpoint(shutdownCtx); err != nil {
		log.Warn("退出前落盘失败", "err", err)
	}
	log.Info("已退出")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// newHTTPClient 统一出网策略：一份连接池给 RSS、aria2 RPC、qBittorrent、
// 通知和刮削共用，避免每处各建一个 Transport 把 armv7 的 fd 耗光。
func newHTTPClient(cfg config.Config) *http.Client {
	return &http.Client{
		Timeout: cfg.HTTPTimeout(),
		Transport: &http.Transport{
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 2, // 番剧站通常只有一两个源，2 条足够
			MaxConnsPerHost:     4,
			IdleConnTimeout:     60 * time.Second,
			ForceAttemptHTTP2:   true,
			Proxy:               http.ProxyFromEnvironment,
		},
	}
}

func buildTarget() string {
	return fmt.Sprintf("%s/%s", goos(), goarch())
}
