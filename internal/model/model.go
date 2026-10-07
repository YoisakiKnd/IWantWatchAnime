// Package model 定义贯穿整条流水线的核心数据结构。
//
// 所有时间字段统一用 time.Time，存储层落库为 Unix 秒，避免不同驱动的
// 时间格式差异（在 armv7 上少一次 time.Parse，也少一次内存分配）。
package model

import "time"

// ItemStatus 表示抓到的条目在流水线中的状态。
type ItemStatus string

const (
	ItemNew      ItemStatus = "new"      // 刚抓到，等待规则匹配
	ItemRejected ItemStatus = "rejected" // 被规则排除，保留记录便于排查
	ItemMatched  ItemStatus = "matched"  // 命中规则，已投递给下载器
	ItemDone     ItemStatus = "done"     // 已下载并整理入库
	ItemFailed   ItemStatus = "failed"   // 投递或下载失败
)

// TaskState 表示下载任务状态，统一映射各下载器的原生状态。
type TaskState string

const (
	TaskQueued      TaskState = "queued"
	TaskDownloading TaskState = "downloading"
	TaskPaused      TaskState = "paused"
	TaskSeeding     TaskState = "seeding"
	TaskCompleted   TaskState = "completed"
	TaskError       TaskState = "error"
)

// Finished 报告任务是否已到达终态，终态任务不再进入对账循环。
func (t TaskState) Finished() bool {
	return t == TaskCompleted || t == TaskError
}

// Subscription 是一条 RSS 订阅（对应一部番剧或一个字幕组发布页）。
type Subscription struct {
	ID          int64
	Name        string
	FeedURL     string
	Group       string   // 番剧分组，决定入库子目录
	SavePath    string   // 覆盖全局下载目录；留空则用默认值
	Include     []string // 命中任一即采纳；为空表示不限制
	Exclude     []string // 命中任一即排除
	Prefer      []string // 字幕组优先级，靠前者得分高
	StartEp     float64  // 只收 >= 该集数的条目
	IntervalMin int      // 轮询间隔（分钟）
	Enabled     bool
	MikanID     int64  // 关联的蜜柑番剧 id，0 = 纯手动订阅
	ETag        string // 条件请求，命中 304 时零解析开销
	LastMod     string
	LastPollAt  time.Time
	LastError   string
	CreatedAt   time.Time
}

// Bangumi 是从蜜柑计划刮下来的番剧信息。
//
// 它是「订阅」的上游：面板上搜番剧、看封面与放送时间、挑字幕组，
// 挑完生成的就是一条 Subscription。字段刻意只保留面板和媒体库
// 真正要用的东西，其余一律不存——数据库在 eMMC 上，能省则省。
type Bangumi struct {
	ID        int64      // 蜜柑番剧 id，也是详情页 /Home/Bangumi/{id} 的 id
	Title     string     // 中文标题
	CoverPath string     // 站内相对路径 /images/Bangumi/...
	AirText   string     // 放送日期：星期六
	Episodes  int        // 总集数，0 表示未知（蜜柑没有时由 Bangumi 补）
	Summary   string     // 简介：蜜柑没有这个字段，只有 Bangumi 有
	Score     float64    // Bangumi 评分，0 表示没查到
	StartAt   string     // 放送开始：4/1/2023
	Official  string     // 官方网站
	Subgroups []Subgroup // 字幕组（含各自的 RSS）
	FetchedAt time.Time  // 刮削时间，用于决定要不要刷新
}

// Subgroup 是一个字幕组及其在蜜柑上的 RSS 地址。
type Subgroup struct {
	ID   int64
	Name string
	RSS  string // 站内相对路径 /RSS/Bangumi?bangumiId=..&subgroupid=..
}

// Item 是 RSS 里的一条发布记录。
type Item struct {
	ID        int64
	SubID     int64
	GUID      string
	Title     string
	Link      string // 详情页
	URI       string // 实际交给下载器的东西：magnet 或 .torrent 地址
	SizeBytes int64
	PubDate   time.Time
	Episode   float64
	EpisodeTo float64 // 合集时表示结束集
	Batch     bool
	Fansub    string
	Status    ItemStatus
	Reason    string
	CreatedAt time.Time
}

// Task 是一次下载任务。
type Task struct {
	ID         int64
	ItemID     int64
	Title      string // 冗余存一份标题，UI 列表不必再 JOIN
	Downloader string
	GID        string // aria2 gid 或 qBittorrent infohash
	State      TaskState
	Progress   float64 // 0..100
	TotalBytes int64
	DoneBytes  int64
	SavePath   string
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	// ProgressAt 是「进度最后一次变化」的时刻。
	// 断种判断靠它而不是 UpdatedAt：对账每轮都会刷新 UpdatedAt，
	// 拿它当"多久没动"会永远算出"刚刚动过"。
	ProgressAt time.Time
}

// LibraryEntry 是一条入库记录。
type LibraryEntry struct {
	SeriesID int64 // 所属番剧（蜜柑 id）；0 = 纯手动订阅，没有番剧记录
	ID       int64
	TaskID   int64
	Title    string
	Path     string
	Linked   bool // true 表示硬链接（不占额外空间），false 表示复制/移动
	NFOPath  string
	Episode  float64
	AddedAt  time.Time
}

// Event 是给面板用的活动流，也是无人值守设备上唯一的排查线索。
type Event struct {
	ID      int64
	Kind    string // poll / match / download / library / notify / error
	Message string
	At      time.Time
}
