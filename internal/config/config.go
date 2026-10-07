// Package config 负责加载与校验配置文件。
//
// 为什么用 TOML 而不是 YAML：TOML 解析器（BurntSushi/toml）约 100KB 编译后，
// 无反射型标签解析开销，且不会因为缩进写错而在玩客云上静默使用默认值。
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config 是完整配置树。
type Config struct {
	Server  Server  `toml:"server"`
	Storage Storage `toml:"storage"`
	Engine  Engine  `toml:"engine"`
	Library Library `toml:"library"`
	Meta    Meta    `toml:"metadata"`
	Notify  Notify  `toml:"notify"`
	Relink  Relink  `toml:"relink"`
	Runtime Runtime `toml:"runtime"`
	Feeds   []Feed  `toml:"feeds"` // 首启动种子订阅，之后以数据库为准
}

// Server 是面板与 API 的监听配置。
type Server struct {
	Listen      string `toml:"listen"`
	User        string `toml:"user"`
	Password    string `toml:"password"`
	Token       string `toml:"token"`        // 脚本调用用的长期令牌
	AcceptToken bool   `toml:"accept_token"` // 允许 X-Api-Token 头
}

// Storage 是数据落盘位置。eMMC 写寿命敏感，所以默认全部放数据盘。
type Storage struct {
	DataDir        string `toml:"data_dir"`
	SnapshotMin    int    `toml:"snapshot_min"`     // 0 表示关闭定时快照
	KeepEventsRows int    `toml:"keep_events_rows"` // 活动流保留行数上限
}

// Engine 选择下载内核。
type Engine struct {
	Kind  string `toml:"kind"` // aria2 | qbittorrent | none
	Aria2 Aria2  `toml:"aria2"`
	QBit  QBit   `toml:"qbittorrent"`
}

// Aria2 是内嵌优先的 BT 内核：aria2 在 armv7/OpenWrt/Entware 上到处都有包，
// 内存约 20MB、CPU 只在哈希时抬升，比把 BT 栈编进主程序更划算。
type Aria2 struct {
	Endpoint    string `toml:"endpoint"`
	Secret      string `toml:"secret"`
	Dir         string `toml:"dir"`           // 下载目录
	MaxPeers    int    `toml:"max_peers"`     // bt-max-peers：armv7 上 30 足够
	SeedTimeMin int    `toml:"seed_time_min"` // 完成后做种分钟数，0 = 不做种
}

// QBit 是可选的内网已有下载器。
type QBit struct {
	Endpoint string `toml:"endpoint"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	Category string `toml:"category"`
}

// Library 是整理入库策略。
type Library struct {
	Root       string `toml:"root"`
	NameRule   string `toml:"name_rule"`
	LinkMode   string `toml:"link_mode"` // hardlink | copy | move
	WriteNFO   bool   `toml:"write_nfo"`
	SeasonDir  bool   `toml:"season_dir"`
	MinFreeGiB int64  `toml:"min_free_gib"`
}

// Meta 是番剧信息刮削配置。
//
// 默认走蜜柑计划（mikanani.me）：项目里所有订阅链接本来就来自蜜柑，
// 用它当唯一信息源，标题、封面、字幕组、RSS 就能全部对齐，
// 不会出现「番剧名来自 A 站、字幕组来自 B 站」的错位。
type Meta struct {
	Enabled   bool     `toml:"enabled"`
	Provider  string   `toml:"provider"`     // mikan | none
	MikanBase string   `toml:"mikan_base"`   // 可换镜像站，如 https://mikanime.tv
	MinGapMs  int      `toml:"min_gap_ms"`   // 两次抓取之间至少间隔多少毫秒
	CacheH    int      `toml:"cache_hours"`  // 番剧详情的内存缓存时长
	CoverDir  string   `toml:"cover_dir"`    // 封面缓存目录，空 = data_dir/covers
	Prefer    []string `toml:"prefer"`       // 全局字幕组偏好，一键订阅时按这个挑
	FetchPost bool     `toml:"fetch_poster"` // 把封面写进媒体库（poster.jpg）
	TimeoutS  int      `toml:"timeout_s"`
	BGMBase   string   `toml:"bgm_base"`  // 可换镜像/自建 Bangumi，空 = https://api.bgm.tv
	BGMToken  string   `toml:"bgm_token"` // 可选：带上就是登录态，接口限额更宽
}

// Notify 是通知配置。
type Notify struct {
	Webhooks    []string `toml:"webhooks"`
	Telegram    Telegram `toml:"telegram"`
	OnStart     bool     `toml:"on_start"` // 启动时通知一次（含版本与目录）
	ThrottleMin int      `toml:"throttle_min"`
}

// Telegram 是最常用的推送通道。
type Telegram struct {
	BotToken string `toml:"bot_token"`
	ChatID   string `toml:"chat_id"`
}

// Relink 是「断种自动换链」：一集卡着下不动时，自动改投另一个字幕组的版本。
//
// 为什么需要：老番的种子种子里没人做种是常态（尤其是两三年前的老番），
// 任务会永远停在 0%，既不报错也不完成，夜里没人看就得等到第二天。
type Relink struct {
	Enabled    bool `toml:"enabled"`
	StuckHours int  `toml:"stuck_hours"`     // 卡在 0% 超过这么久就认定断种
	MaxPerEp   int  `toml:"max_per_episode"` // 同一集最多换几次，防止来回投递
}

// Runtime 是运行时调优开关，直接对应玩客云的硬件约束。
type Runtime struct {
	MemLimitMiB  int    `toml:"mem_limit_mib"` // GOMEMLIMIT，硬上限
	PollWorkers  int    `toml:"poll_workers"`  // 并发拉 feed 的数量
	ReconcileSec int    `toml:"reconcile_sec"` // 下载进度对账间隔
	HTTPTimeoutS int    `toml:"http_timeout_s"`
	FeedMaxBytes int64  `toml:"feed_max_bytes"` // 单次 feed 读取上限，防坏源打爆内存
	LogLevel     string `toml:"log_level"`
}

// Feed 是首启动种子订阅。
type Feed struct {
	Name     string   `toml:"name"`
	URL      string   `toml:"url"`
	Group    string   `toml:"group"`
	Interval int      `toml:"interval_min"`
	Include  []string `toml:"include"`
	Exclude  []string `toml:"exclude"`
	Prefer   []string `toml:"prefer"`
	StartEp  float64  `toml:"start_ep"`
}

// Default 返回一份对玩客云友好的默认配置。
func Default() Config {
	return Config{
		// 默认只监听本机：这个面板能删订阅、能删文件，默认摊在局域网上不合适。
		// 想在手机/电视上看面板，再显式改成 0.0.0.0:8637 并设上账号密码。
		Server:  Server{Listen: "127.0.0.1:8637", User: "admin", AcceptToken: true},
		Storage: Storage{DataDir: "data", SnapshotMin: 360, KeepEventsRows: 2000},
		Engine: Engine{
			Kind:  "aria2",
			Aria2: Aria2{Endpoint: "http://127.0.0.1:6800/jsonrpc", Dir: "downloads", MaxPeers: 30},
		},
		Library: Library{
			Root: "library", NameRule: "{group}/{title}/第 {season:02d} 季/{title} - S{season:02d}E{span}",
			LinkMode: "hardlink", WriteNFO: true, SeasonDir: true, MinFreeGiB: 2,
		},
		// 刮削默认开着：面板的「搜番剧 → 一键订阅」全靠它，关掉就没有入口了。
		// 所有抓取都有节流与缓存，不会在空闲时产生任何请求。
		Meta: Meta{
			Enabled: true, Provider: "mikan", MikanBase: "https://mikanani.me",
			MinGapMs: 1200, CacheH: 12, FetchPost: true, TimeoutS: 12,
		},
		Notify:  Notify{OnStart: false, ThrottleMin: 0},
		Relink:  Relink{Enabled: true, StuckHours: 6, MaxPerEp: 2},
		Runtime: Runtime{MemLimitMiB: 64, PollWorkers: 2, ReconcileSec: 15, HTTPTimeoutS: 25, FeedMaxBytes: 8 << 20, LogLevel: "info"},
	}
}

// Load 读取配置文件；文件不存在时返回默认配置（首启动体验：直接跑起来）。
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("解析 %s: %w", path, err)
	}
	return cfg, nil
}

// Validate 在启动阶段就把致命配置错误报出来，而不是等到半夜下载失败。
// isLoopbackListen 判断监听地址是否只对本机开放。
//
// ":8637" 与 "0.0.0.0:8637" 都算对所有网卡开放；"localhost:8637" 算本机。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // 没带端口的写法（"localhost"）直接当主机名看
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		return errors.New("server.listen 不能为空")
	}
	// 没有账号密码还监听非本机地址，等于把"删订阅、删文件、暂停任务"的按钮
	// 摆到局域网上——家里任何一台设备、任何一个连上 WiFi 的人都能操作。
	// 这种情况宁可起不来，也不要"先用着再说"。
	if c.Server.User == "" && c.Server.Password == "" && !isLoopbackListen(c.Server.Listen) {
		return fmt.Errorf("server.listen = %q 会让面板暴露到局域网，但 server.user / server.password 都是空的；"+
			"请设上账号密码，或改回只监听本机（127.0.0.1:8637）", c.Server.Listen)
	}
	switch c.Engine.Kind {
	case "aria2":
		if c.Engine.Aria2.Endpoint == "" {
			return errors.New("engine.aria2.endpoint 不能为空")
		}
		if c.Engine.Aria2.Dir == "" {
			return errors.New("engine.aria2.dir 不能为空")
		}
	case "qbittorrent":
		if c.Engine.QBit.Endpoint == "" {
			return errors.New("engine.qbittorrent.endpoint 不能为空")
		}
	case "none":
		// 只做订阅与匹配、不下载，用于调试规则
	default:
		return fmt.Errorf("engine.kind 只能是 aria2 / qbittorrent / none，当前为 %q", c.Engine.Kind)
	}
	switch c.Meta.Provider {
	case "", "mikan", "none":
	default:
		return fmt.Errorf("metadata.provider 只能是 mikan / none，当前为 %q", c.Meta.Provider)
	}
	if c.Meta.BGMBase != "" && !strings.HasPrefix(c.Meta.BGMBase, "http") {
		c.Meta.BGMBase = "https://" + c.Meta.BGMBase
	}
	if c.Meta.Enabled && c.Meta.Provider == "mikan" && c.Meta.MikanBase == "" {
		return errors.New("metadata.mikan_base 不能为空")
	}
	if c.Meta.MinGapMs < 300 {
		c.Meta.MinGapMs = 300 // 低于这个值就是给蜜柑添堵了
	}
	if c.Meta.CacheH < 1 {
		c.Meta.CacheH = 1
	}
	switch c.Library.LinkMode {
	case "", "hardlink", "copy", "move":
	default:
		return fmt.Errorf("library.link_mode 只能是 hardlink / copy / move，当前为 %q", c.Library.LinkMode)
	}
	if c.Relink.StuckHours < 1 {
		c.Relink.StuckHours = 1
	}
	if c.Relink.StuckHours > 72 {
		c.Relink.StuckHours = 72
	}
	if c.Relink.MaxPerEp < 1 {
		c.Relink.MaxPerEp = 1
	}
	if c.Relink.MaxPerEp > 5 {
		c.Relink.MaxPerEp = 5
	}
	if c.Runtime.PollWorkers < 1 {
		c.Runtime.PollWorkers = 1
	}
	if c.Runtime.PollWorkers > 4 {
		c.Runtime.PollWorkers = 4 // 单核 A5 上再多只是互相抢时间片
	}
	if c.Runtime.ReconcileSec < 5 {
		c.Runtime.ReconcileSec = 5
	}
	if c.Runtime.MemLimitMiB < 24 {
		c.Runtime.MemLimitMiB = 24
	}
	return nil
}

// HTTPTimeout 返回统一的出网超时。
func (c *Config) HTTPTimeout() time.Duration {
	return time.Duration(c.Runtime.HTTPTimeoutS) * time.Second
}

// DBPath 返回数据库绝对路径。
func (c *Config) DBPath() string { return filepath.Join(c.Storage.DataDir, "suzu.db") }

// CoverDir 返回封面缓存目录。抓一次用两处：面板缩略图 + 媒体库 poster.jpg。
func (c *Config) CoverDir() string {
	if c.Meta.CoverDir != "" {
		return c.Meta.CoverDir
	}
	return filepath.Join(c.Storage.DataDir, "covers")
}

// SnapshotPath 返回快照路径。
func (c *Config) SnapshotPath() string { return filepath.Join(c.Storage.DataDir, "suzu.snapshot.db") }

// EnsureDirs 创建运行所需目录。
func (c *Config) EnsureDirs() error {
	dirs := []string{c.Storage.DataDir, c.Engine.Aria2.Dir, c.Library.Root, c.CoverDir()}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// SplitTokens 把 "简日双语,喵萌奶茶屋" 这类配置写成切片，兼容中英文逗号。
func SplitTokens(s string) []string {
	s = strings.ReplaceAll(s, "，", ",")
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
