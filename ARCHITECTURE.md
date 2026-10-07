# suzu 项目架构

> 一个能在玩客云（Amlogic S805 / 四核 Cortex-A5 / 1GB 内存 / armv7l）上长期常驻的番剧订阅归档器。
> 目标是 ANI-RSS 干的事，但用一套「不可能崩」的运行时换掉那套「需要 Node + 已编译好的 armv8 镜像」的运行时。

---

## 0. 先把痛点翻译成工程约束

你踩过的三个坑，每一个都对应下面一条硬约束。架构里所有取舍都从这张表开始。

| 你踩的坑 | 翻译成技术约束 | 架构上的对策 |
| --- | --- | --- |
| 玩客云 eMMC 无法覆写 / 写坏 | 根文件系统不可依赖，写操作必须集中在可换的外接盘 | 二进制 `-s -w` 后 15MB 单文件；**运行期只写 `data/`、`downloads/`、`library/` 三个目录**；systemd `ProtectSystem=strict` 把其余路径全挂只读；日志只进 journald（可配置成纯内存） |
| 移动硬盘无法引导 | 系统启动链路不能作为前提 | 程序是普通用户态进程，链路是 `eMMC 上的 systemd → 进程 → 外接盘上的数据`；外接盘晚挂载、中途掉盘都只是「任务失败」，不是「系统起不来」 |
| 看番项目没有 armv7 架构 | 上游不提供 32 位 ARM 产物，且目标机没法自举编译 | `CGO_ENABLED=0` 纯 Go → 交叉编译就是一条 `GOOS=linux GOARCH=arm GOARM=7 go build`，产物**静态无动态依赖**，拷过去 `chmod +x` 就能跑；不需要在机器上装 Go、不需要 armv7 的 docker 基础镜像（`arm32v7/` 系列早就没人维护了） |
| 轻量 / 内存低 / CPU 好 | 单核峰值 < 1 秒、常驻 < 64MiB、四核不许被跑满 | 见 §6「资源控制清单」：`GOMEMLIMIT` + 数据库 `cache_size=-4MiB` + 同集去重 + 并发夹紧 + 首次轮询摊开 |

一句话总结取向：**宁可功能少一点，也不要出现"半夜某个源超时把 1GB 内存吃光导致 SSH 都进不去"的情况。**

---

## 1. 技术选型（含"为什么不选另一个"）

| 关注点 | 选型 | 备选 | 为什么 |
| --- | --- | --- | --- |
| 语言 | Go 1.24+ | Python / Node | 静态单体二进制 + 原生 goroutine 并发 + 交叉编译一行命令。Python 在 armv7 上要么装不上依赖，要么常驻 150MB+；Node 直接出局（内存与产物都要 A 系列 64 位） |
| 存储 | SQLite（`modernc.org/sqlite`，纯 Go） | `mattn/go-sqlite3`（需 CGO）、JSON 文件、BoltDB | 纯 Go = 无 CGO = 交叉编译无障碍，这是**唯一一条不能让步**的选型。JSON 文件在"同集去重 / 断点续跑"这类查询上会退化成全量加载；BoltDB 不好做 SQL 式的 cruft 清理与运维查询 |
| 下载器 | 外挂 aria2（JSON-RPC），可选 qBittorrent | 内置 `anacrolix/torrent` | **这是最重要的一个决定**。内置 BT 内核意味着 DHT、tracker、加密握手、硬盘随机写全在 Go 进程里，armv7 上单核要吃满、内存要翻倍、崩溃要连带整个服务。外挂 aria2 后：内存与 CPU 归零到 suzu 之外，aria2 挂了 suzu 照常显示排期与活动流，还能让 NAS 上已有的 qBittorrent 直接复用 |
| RSS 解析 | `mmcdole/gofeed`（+ ETag/Last-Modified 条件请求） | 自己写正则 / XML 解析 | 各家 RSS 的日期格式、`content:encoded`、命名空间都很脏；同时必须支持条件请求，否则 30 分钟一次的全量拉取是无意义的带宽与 CPU |
| 前端 | 服务端 `html/template` + `embed` + 60 行原生 JS 轮询 | Vue/React + Vite、htmx | 目标机上没有 Node，也不该为一个管理面板在 armv7 上跑构建；`embed` 让静态资源进二进制，部署就一个文件。交互只做「片段轮询替换」，够用且零构建 |
| 配置 | TOML（`BurntSushi/toml`） | YAML / 环境变量 | TOML 允许注释——这个项目的配置文件是给用户长期手改的，注释比"结构优雅"重要；YAML 缩进太容易错，环境变量表达不了 `[[feeds]]` 这种列表 |
| 日志 | `log/slog`（stdout） | zap / 自己写 | 标准库、结构化、零依赖；日志交给 journald 或 `docker logs`，不自己管轮转 |
| 打包 | Makefile（交叉编译）+ 可选 Dockerfile | 只在机器上编译 | 目标机四核 A5 编译一次要十几分钟且容易 OOM，交叉编译 + scp 是唯一舒服的路径 |

---

## 2. 分层与目录

依赖方向单向向下，`pipeline` 是唯一的编排者，`httpx` 与 `pipeline` 只通过 store 交换状态——**没有反向依赖，也没有事件总线**。

```
cmd/suzu/            进程入口：flag 解析、GOMEMLIMIT、优雅退出、Checkpoint
  main.go            读配置 → 建 store → 建 downloader → 建 pipeline → 起 HTTP
  platform.go        GOOS/GOARCH 上报（面板上能直接看出手上这个二进制是给谁的）

internal/config/     配置加载/校验/默认值/目录创建（唯一允许有默认值的地方）
internal/model/      纯数据结构，无方法、无依赖（Subscription/Item/Task/LibraryEntry/Event）
internal/store/      SQLite：schema.sql + 全部读写方法，其他包不许直接碰 *sql.DB
internal/feed/       HTTP 拉取（条件请求、体积上限、超时）+ gofeed 解析 + 归一化
internal/matcher/    标题解析 + 规则判定（纯函数、零 IO，可被 benchmark 与单测穷举）
internal/downloader/ 接口 Downloader + aria2 适配器 + qBittorrent 适配器
internal/library/    命名模板渲染 + 路径安全化 + 落盘整理（硬链接/复制/移动）+ NFO
internal/metadata/   刮削（默认关闭，失败只影响元数据不影响下载）
internal/notify/     通知出口（webhook / Telegram），带节流
internal/pipeline/   编排核心：调度、轮询、去重、投递、对账、维护循环 + Overview 视图
internal/httpx/      面板与 HTTP API（模板 embed 在 web/ 下）
  web/templates/     dashboard.html + _schedule/_queue/_library/_activity.html 四个片段
  web/static/        app.css / app.js
deploy/              systemd unit（含 eMMC 保护） + 交叉编译 Dockerfile
Makefile             armv7 / armv6 / arm64 / amd64 一键交叉编译
```

**为什么 matcher 必须零 IO**：标题解析是整个系统里最容易被"某个奇怪命名"打死的地方，也是最耗 CPU 的地方。把它做成纯函数，就能用 `go test -bench` 在几秒内确认"armv7 上解析 200 条要多久"，也能把所有诡异 case 写成表驱动测试。

---

## 3. 数据流

```
                 ┌──────────────── 调度循环（每订阅一个下次触发时刻）────────────────┐
                 │                                                                  │
  RSS 源 ──HTTP──▶ feed.Fetch ──▶ matcher.ParseTitle ──▶ matcher.RuleJudge ──▶ store
 (ETag/304)      (体积上限)      (季/集/字幕组/分辨率)      (包含/排除/偏好/起始集)   │
                                                              │                  │
                                         accepted? ───────────┤                  │
                                                              ▼                  │
                                              downloader.Add ──▶ aria2/qBittorrent
                                                              │   (gid 落库)
                 ┌──────── 对账循环（每 15 秒）───────────────┘
                 ▼
        tellStatus ──▶ 进度写库 ──┬─ 下完?（含在做种）──▶ library.Organize
                                  │                    │
                                  │     pickVideo ── hardlink/copy ──▶ 媒体库
                                  │                    │
                                  │          NFO + 刮削 + notify + 活动流
                                  │
                                  └─ error / 卡在 0% 超阈值 ──▶ relink
                                          找同集被择优刷掉的另一条源 ──▶ downloader.Add
                                          （每集次数上限，原任务暂停并标注原因）
```

番剧库是另一条**只在用户点的时候才走**的链路，跟上面三个循环完全解耦：

```
面板搜索 ──▶ mikan.Search ──▶ 卡片落库(bangumi 表，只在字段非空时覆盖)
                                  │
点开一部 ──▶ mikan.Detail ──▶ 标题/放送日/开播/封面/字幕组(含各自 RSS) ──▶ 缓存 12h
                                  │
勾字幕组订阅 ──▶ 每个字幕组生成一条 RSS 订阅 ──▶ UpsertSchedule + PollNow
                                  │
                        封面抓一次落 data/covers/{id}.jpg
                        （面板与媒体库海报共用这一张，不再重复下载）
```

对账循环里有三条容易被忽略、但直接决定"打开 Jellyfin 到底有没有东西看"的规则：

- **「下完在做种」就是下完了。** aria2 的 `seed_time_min > 0` 与 qBittorrent 的默认行为，都会在文件下完后继续把任务报成 `active + seeder` / `seeding`。如果对账只认 `complete`，任务会永远挂在活动列表里（每 15 秒被问一次），而媒体库里始终没有这一集。真机 qBittorrent 联调当场就把这条踩出来了（见 §8）。
- **元数据迟到就地补齐，不要求用户重下。** `tvshow.nfo` 是在入库那一刻写下的，而简介/评分要等 Bangumi 刮削回来，可能晚几秒也可能晚几天。所以番剧信息一旦落库，`syncLibraryMeta` 会按 **番剧 id** 找到媒体库里那部番的目录，重新渲染 NFO（内容变了才写盘，海报"存在即跳过"）。
- **媒体库里只有本地图，不写 URL。** Bangumi 给的是 `images.large` 一条 https 链接，而 organizer 只认本地文件；直接把 URL 当海报，结果是"海报目录空的 + NFO 里 `<thumb>poster.jpg</thumb>` 指向一个不存在的文件"，Jellyfin 每次打开都在找那张图。所以封面统一先落盘：蜜柑的封面在订阅时抓进 `data/covers/{id}.jpg`，只有 Bangumi 有封面时由 `fetchImage` 抓到同一个封面槽。**先确认海报文件真的写成了，再往 NFO 里写 `<thumb>`**——写进去就必须真的有。

三条循环职责分离，互不阻塞：

| 循环 | 周期 | 做什么 | 崩了会怎样 |
| --- | --- | --- | --- |
| 调度循环 | 每订阅自己的间隔（默认 30min，首次 < 45s） | 拉 RSS、判规则、投递下载 | 单个源失败只写 `LastError`，排期行变红，下一个周期继续试 |
| 对账循环 | 15s | 问下载器要进度、落库、完成即整理入库 | 下载器不可达只是进度不变，恢复后自动追平 |
| 维护循环 | 1h | WAL checkpoint、活动流裁剪、可选快照 | 纯后台琐事，失败不影响主链路 |

---

## 3.1 断种换链（`internal/pipeline/relink.go`）

整条逻辑只有一句话：**一集下不动，就换成同集另一个字幕组的版本**。但边界比逻辑重要：

| 规则 | 为什么 |
| --- | --- |
| 只在同一订阅、同一集（`ABS(episode - ?) < 0.01`）里找替补 | 换错片比不换更糟；跨番剧找必然出错 |
| 候选只取 `rejected` / `matched` 状态的条目 | `done` 已经下好、`failed` 表示这条也试过、`pending` 还没轮到判定。**被择优刷掉的条目正是天然替补池** |
| 每集最多换 `relink.max_per_episode`（默认 2）次 | 候选池来自 RSS，有限；计数用条目上的「断种换链」标记持久化，重启不重置 |
| 偏好字幕组优先，其余按体积倒序 | 「偏好命中里最大的那条」就是最想要的替补；与订阅时用的是同一套匹配规则（`mikan.Matches`） |
| 原任务暂停（`Pause`）而不是删除 | 保留现场：万一是下载器抽风，面板上还能看到它、还能手动继续 |
| 触发条件 = 报错 或（进度 0 且 `progress_at` 超过 `stuck_hours`） | 真实的「断种」从来不报错，它只是永远不动；而"刚投递还没连上 peer"必须被排除，所以要求进度为 0 且时间足够久 |
| 用户手动暂停的任务一律跳过 | 那是等一个明确的指令，程序不该跟人抢方向盘 |

副作用也要说清楚：换链会把原条目标成 `failed`、原因写「断种换链：…」，新条目写 `matched`。
于是面板上同一集会同时出现"下不动的那个"和"正在下的那个"——这不是重复下载，是刻意留下的现场。

---

## 4. 数据模型

```sql
subscriptions(id, name, feed_url, grp, save_path, include, exclude, prefer, start_ep,
              interval_min, enabled, mikan_id, etag, last_mod, last_poll_at,
              last_error, created_at)

items(id, sub_id→subscriptions, guid, title, link, uri, size_bytes, pub_date,
      episode, episode_to, batch, fansub, status, reason, created_at)
      UNIQUE(sub_id, guid)          -- 同一条目重复出现时天然幂等

tasks(id, item_id→items, title, downloader, gid, state, progress, total_bytes,
      done_bytes, save_path, error, created_at, updated_at, progress_at)
      UNIQUE(downloader, gid)
      -- progress_at = 进度最后一次变化的时刻，断种判断只看它

library(id, series_id, task_id→tasks, title, path, linked, nfo_path, episode, added_at)
bangumi(id, title, cover_path, air_text, episodes, start_at, official,
        subgroups, fetched_at, summary, score)   -- 番剧库缓存，蜜柑 + Bangumi 合并
events(id, kind, message, at)       -- 面板活动流，行数上限可配
```

`library.series_id` 指回 `bangumi.id`：**入过的番按 id 认，不按标题认**。番剧标题会变——蜜柑叫「葬送的芙莉莲」，Bangumi 叫「葬送のフリーレン」，带不带日文原名也变，而发布标题更是字幕组随手写。用标题当键，迟早出现"番剧信息补上了，媒体库里的 NFO 却没跟着更新"。

几个关键设计点：

1. **`items` 是"我见过哪些发布"，`tasks` 是"我在下什么"，`library` 是"我收到了什么"**。三张表分离让"投递失败但条目已记录"变成可恢复状态：重启后对账循环发现条目已 accepted 却没有 task，会重新投递。
2. **`items.status` 而不是删行**：被规则拒绝的条目（同集已有更优版本、命中排除词）也留在库里，面板上能直接回答"这集为什么没下载"——这是无人值守设备上最有价值的一条信息。
3. **同集去重靠 `EpisodeTaken(subID, episode)`**：跨字幕组的重复发布只留偏好命中那一条，避免硬盘被同一集灌三遍（对只有 1TB 移动硬盘的人这不是小事）。
4. **`tasks.updated_at` 不能用来判断"多久没动"**：对账循环每 15 秒都会刷新它，拿它算差值永远是"刚刚动过"。断种换链单独记了 `progress_at`——只在 `progress`/`done_bytes` 真的变化时才推进（一条 SQL 里的 `CASE WHEN`，不用读改写）。老库升级时用 `created_at` 回填，否则补出来的 0 会被算成"几万小时没动"，一升级就把所有在跑的任务判成断种。

5. **「暂停」在库里是粘性的**：面板上按暂停后，对账循环不会因为下载器还报"进行中"就把状态改回去，直到用户点继续。否则 15 秒后的下一轮对账会把用户刚做的操作抹掉——这类"按钮按了像没按"的 bug 在无人值守设备上极难被发现。
6. **默认只监听本机，想敞开就必须带密码**：面板能删订阅、能删文件、能暂停任务，又是一个长期在线的进程。默认值要是 `0.0.0.0` 且没密码，全屋任何一台联了 WiFi 的设备都能操作它。所以 `server.listen` 默认 `127.0.0.1:8637`，`Validate` 还会拦下"非回环地址 + 空账号密码"的组合——连 `-listen` 覆盖也一并拦下（校验在覆盖之后跑）。实测：无密码 + `0.0.0.0:8637` 直接退出码 2，设了密码才起得来。
7. **老库升级就地补列，不要求重装**：`Open` 时逐个 `ensureColumn`（`bangumi.summary/score`、`subscriptions.mikan_id`、`tasks.progress_at`、`library.series_id`），并且回填有意义的值——`progress_at` 用 `created_at` 兜底，`series_id` 先按番剧名、再按"文件路径里有没有番剧目录"两条线索猜一次。升级完直接能用，用户的订阅和入库记录一条不丢。

---


## 5. 关键接口

```go
// 下载器只要能做这几件事，就换得掉。
// 新增 Transmission/Deluge 支持 = 新写一个文件，不改 pipeline。
type Downloader interface {
    Kind() string
    Ping(ctx context.Context) error
    Add(ctx context.Context, req AddRequest) (gid string, err error)   // req.URI 是磁力或 .torrent 地址
    Status(ctx context.Context, gid string) (Status, error)
    Pause / Resume / Remove(ctx context.Context, gid string) error
}
// Status{State: queued|downloading|seeding|completed|paused|error,
//        Progress, TotalBytes, DoneBytes, SavePath, Error}
// 做种态（seeding）在对账里按"已完成"处理——文件已经在盘上了。

// 匹配层是纯函数，没有 struct、没有 IO。
func ParseTitle(raw string) Parsed     // 季/集/区间/合集/字幕组/分辨率/编码
func RuleJudge(p Parsed, r Rules) Verdict // Verdict{Accept bool, Score int, Reason string}
```

刮削层也只有一个入口，失败一律降级而不是中断：

```
GET  /subscribe?q=名字         搜番剧；?id=3141 打开详情并列出字幕组
POST /api/v1/bangumi/subscribe  {mikan_id, sg[], backfill, group, interval_min}
GET  /api/v1/bangumi/search    JSON 搜索
GET  /api/v1/covers/3141.jpg   封面代理，命中本地缓存直接回，否则抓一次再缓存
（封面槽 data/covers/{id}.jpg 由蜜柑或 Bangumi 先到者填充：入库、面板缩略图、
  "元数据迟到"的重写都把它当本地文件用，谁都不必再判断"这张图从哪来"）
```

`pipeline.Overview(ctx)` 是给面板和 `/api/v1/state` 用的**单一视图数据源**：一次调用返回排期、队列、入库、活动流、计数、内存/协程/运行时长。面板不做 N 次查询，也不在模板里做业务判断。

---

## 6. 资源控制清单（这是"轻量"的具体兑现）

| 手段 | 位置 | 效果 |
| --- | --- | --- |
| `debug.SetMemoryLimit(64MiB)` | `cmd/suzu/main.go` | Go 1.19+ 的软上限，GC 会为它主动干活；比 `GOGC=20` 这种粗糙旋钮精准 |
| `SetGCPercent(50)` | 同上 | 分配密集的解析阶段减少内存翻倍 |
| `PRAGMA cache_size=-4096` | `store/schema.sql` | SQLite 页缓存锁死 4MiB（默认会按内存自适应到几十 MB） |
| `PRAGMA journal_mode=WAL` + 1h checkpoint | schema + 维护循环 | 写入不阻塞读，且 WAL 不会无限长 |
| 并发夹紧 `PollWorkers ≤ 4` | `config` | 单核 A5 上 2 个并发已够；防止"50 个源同时开火把四个核跑满" |
| 首次轮询摊开 45s + index 偏移 | `pipeline.jitter` | 重启后马上能看到结果，同时不产生 thundering herd |
| feed 体积上限 8MiB | `feed.Fetch` | 一个坏源/恶意源不能把内存打满 |
| 条件请求（ETag/Last-Modified） | `feed.Fetch` | 无更新时 304，几乎零成本 |
| 同集去重 | `pipeline` | 不重复下载、不重复整理 |
| 活动流行数上限 2000 | 维护循环 | 一年后 `events` 表仍是几 MB |
| systemd `MemoryMax=192M / CPUWeight=20 / Nice=5` | `deploy/suzu.service` | 就算程序自己失控，也抢不走 SSH 的 CPU 和内存 |

实测（本机 amd64，`go test -bench`）：标题解析 `54.8µs/op`、规则判定 `1.79µs/op`。armv7 上按 10–20 倍慢估：**解析 200 条 RSS 约 0.1–0.2 秒 CPU**，每个源 30 分钟一次——单核占用可以忽略。

---

## 7. 部署形态（对应你的三个坑）

**形态 A：玩客云原生跑（推荐）**

```bash
make armv7                                               # x86 笔记本上出 15MB 静态二进制
scp -r bin/suzu-linux-armv7 deploy root@玩客云:/root/setup/
ssh root@玩客云 'sudo SUZU_DATA=/mnt/usb /root/setup/deploy/install.sh'
```

一条命令干完：建 `suzu/ downloads/ library/` → 拷二进制 → 用 `deploy/config.armv7.toml` 渲染 `config.toml`
（随机生成面板密码并打印一次）→ 建 `suzu` 系统用户 → 装 `deploy/suzu.service` 并 `enable --now` → 等 `/healthz`。
脚本有两道防线：**不是挂载点的目录直接拒绝安装**（防止"盘没挂上，数据写进 eMMC"），
以及渲染出的配置默认 `listen = "127.0.0.1:8637"` + 非空密码（想让局域网看面板必须同时改这两处）。

- 二进制与数据库都在外接盘上，`ProtectSystem=strict` 让根文件系统对它只读
- aria2 单独一个 unit，`--enable-rpc --dir=/mnt/usb/downloads --max-peers=30 --seed-time=0`
- 启动语义要记住：起服务时 Ping 一次下载内核，**探不到就直接退出**（`engine.kind = "none"` 除外），
  由 `Restart=always` 每 5 秒重试。所以 aria2 晚起能自愈，unit 名字写错就是每 5 秒一次的失败循环。
- 手工步骤 / 排错表 / 五个上机核对项：`deploy/README-armv7.md`;
  实测记录：`docs/live/armv7-install.log`（安装器输出）、`docs/live/armv7-installed-verify.log`（起来 + 认证 + 真连蜜柑 + aria2 不在时的退出码）

**形态 B：Docker（外接盘已经挂好、系统是 arm64 或 32 位 docker 能跑）**

`deploy/Dockerfile` 用 buildx 在 x86 上产 `linux/arm/v7` 镜像，镜像里只装 suzu 本体，aria2 用 compose 起第二个服务。

**媒体库**：`library.root` 直接指给 Jellyfin/Emby/Kodi 扫。命名模板 `{group}/{title}/第 01 季/{title} - S01E03.mkv` 与 `tvshow.nfo` 同目录，Kodi 系抓得到。硬链接让"做种"和"入库"共享同一份数据，不占第二份空间。

---

## 8. 已验证的部分（不是计划，是现状）

| 验证项 | 结果 |
| --- | --- |
| `go vet ./...` / `gofmt` | 干净 |
| `go test ./...` | 全绿 10 个包（config 阈值夹取、matcher 标题解析、store 迁移与查询、library 命名与番剧元数据、mikan 解析、metadata 刮削、notify 出口、downloader 双内核、pipeline 端到端/换链/做种入库/预热、httpx 模板渲染 + 认证/API） |
| 端到端链路 | 本地假 RSS + 假 aria2：4 条 → 过滤 → 2 条投递 → 进度对账 → 完成整理入库（硬链接 + NFO）✅ |
| 断种换链 | 6 个场景：失败换链、卡 0% 换链（含暂停原任务）、到上限停手、用户手动暂停不碰、关掉后不介入、时长文案 ✅ |
| 断种换链（真机演示） | 在对真实蜜柑 RSS 的演示库上埋一条"卡了 8 小时、进度 0"的任务：下一轮对账（15 秒内）自动改投同集另一版本，原任务标为「断种换链：卡在 0%（8 小时没动静，多半断种）」并暂停，新版本随即下载完成、整理入库 `S01E10.mkv` ✅ |
| qBittorrent 适配器 | 假 Web API（登录/投递/反查 infohash/状态映射/歧义拒绝）7 个用例 ✅ |
| qBittorrent 真机联调 | 装真 `qbittorrent-nox` 跑起来，`QBT_LIVE=1 QBT_PASS=… go test ./internal/downloader/ -run TestQBitLive -v`：真登录 → 投递真实 `.torrent` → 认领回的 infohash 与种子 info 字典的 sha1 **逐字节相等** → 状态读到种子真实体积 4096 → 暂停/继续 → 磁力链直接返回 btih → 删除。**真机下载**：把同一台 qBittorrent 当引擎跑第二次端到端（HTTP 种子源），下载 100% 后停在 `seeding` → 对账按完成入库 `别当欧尼酱了 - S01E01.mkv`（硬链接 + 单集 NFO）✅ |
| 通知真收包 | 本机 webhook 接收端记下真实 POST：`{"source":"suzu","title":"断种换链：…","body":"…","ts":…}`；一次演示里连续收到「已投递」「下载失败」「断种换链 ×2」「入库完成」四种 ✅ |
| 番剧元数据迟到 | 打开详情页触发刮削后，磁盘上那份入库时写下的 `tvshow.nfo` 被就地补齐：`<plot>`（234 字真实简介）、`<rating>8.5</rating>`、`<thumb>poster.jpg</thumb>`，日志「番剧级元数据已对齐」✅ |
| **armv7 二进制真的跑起来了** | `sudo apt-get install qemu-user-static` 后用 `qemu-arm-static bin/suzu-linux-armv7` 跑完整一套：日志 `目标=linux/arm`，本地面板 200（4 个片段全 200），一次轮询「条目=3 投递=3 过滤=0 耗时=39ms」，3 集全部硬链接入库（`links=2`）+ 每集 NFO，假 webhook 真收到 4 条通知，面板上的暂停/继续与手动轮询都是 303。原始日志见 `docs/live/armv7-qemu.log`、`docs/live/armv7-webhook.log` ✅ |
| ARM 二进制里安全守卫同样生效 | 同一份 armv7 二进制加 `-listen 0.0.0.0:8649`（配置里没密码）→ 退出码 2 并打印那条中文错误 ✅ |
| 封面接进媒体库（真机全链路） | `MIKAN_LIVE=1 BGM_LIVE=1 go test ./internal/pipeline/ -run TestLiveCover -v`：真蜜柑详情（23 字幕组）→ 真封面 216,601 字节落 `data/covers/3141.jpg` → 真 Bangumi（8.5 分 / 674 字简介 / 28 集）→ 入库目录里同时有 `poster.jpg` 与带 `<thumb>/<plot>/<rating>` 的 `tvshow.nfo` ✅ |
| 海报只认本地、不留死引用 | 单测钉住两条：海报源丢了就不写 `<thumb>`（否则媒体库一直找一张不存在的图）；海报落盘了才写、且指向同名文件 ✅ |
| 打开番剧库的等待 | 搜索 0.58s 返回，后台顺带预热前两张卡的详情；随后点开第一张 **0.003s**（库里已有），没预热时是 **2.03s**（真机抓蜜柑详情页）✅ |

上面几条真机记录都留了原始日志，可复查：`docs/live/qbittorrent-live.log`（下载器换内核后从投递到入库的整段）、`docs/live/webhook-received.log`（落地的通知原文）。
| armv7 交叉编译 | `ELF 32-bit LSB executable, ARM, EABI5, statically linked`，`ldd` = not a dynamic executable ✅ |
| 安装包（D）真的能装能起 | `deploy/install.sh` 用前缀模式实跑：目录/二进制/配置/unit 四处路径全部正确渲染；用**它渲染出来的那份配置**（带密码）起 armv7 二进制 → `目标=linux/arm`、`/healthz` 200；认证实测 无凭据 401 / 错密码 401 / 正确 200；同一实例真连蜜柑搜到 2 部；确认 6800 上没有 aria2 后启动 → 退出码 1 并给出可读原因 ✅（日志 `docs/live/armv7-installed-verify.log`） |
| armv7 上到底能不能跑 | 用 qemu-arm 真跑（不是只看 `file` 输出）：完整走通面板、轮询、投递、硬链接入库、通知。**内存数字别信 qemu 那份**——qemu 进程 RSS 52.4 MiB 混着它自己的翻译开销，设备上的真实值要用真机量 ✅（见 §9 边界） |
| 产物体积 | armv7 15MB / arm64 15MB / amd64 16MB（`-s -w -trimpath`）✅ |
| 模板渲染 | 首页 + 五个片段全部 200，含"现在"光标、进度条、活动流 ✅ |
| 常驻内存（实测 RSS） | **21.9 MiB**（长跑实例，且 `VmHWM == VmRSS`：峰值就是常驻值，没有阶段性膨胀）；换用新二进制重启后 16.9 MiB，本轮修封面链路后的二进制重启为 17.9 MiB（`VmRSS: 18284 kB`） |
| 空闲 CPU | `ps` 报 0.1%，20 秒内 CPU 时间增量 ≈ 0 —— 空闲时真的什么都不做 |
| 协程数稳定性 | 连续 72 秒采样 12↔20 之间来回并回落（无单调增长），堆在 1.5–1.9 MiB 之间波动 → 无泄漏 |

### 真机联网实测（2026-10-06，真实 mikanani.me，非 mock）

| 验证项 | 结果 |
| --- | --- |
| 站点可达性 | `mikanani.me` 200，无 Cloudflare 拦截，UA 正常即可 |
| 搜索 | `/subscribe?q=葬送的芙莉莲` → 2 张番剧卡（3141 第一季 / 3821 第二季），0.8s |
| 番剧详情 | `/subscribe?id=3141` → 23 个字幕组、放送 星期五、开播 9/29/2023、封面 400×560 JPEG 65,621 字节；该页没有"总集数"行，所以集数如实为 0（宁缺毋滥，不外推） |
| 一键订阅 | 勾两个字幕组 → 303 + 一句话回执「已订阅《葬送的芙莉莲》：喵萌奶茶屋、LoliHouse」，生成 `?bangumiId=3141&subgroupid=382/370` 两条 RSS |
| 封面缓存 | `data/covers/3141.jpg` 落盘一次；`/api/v1/covers/3141.jpg` 命中缓存返回 `image/jpeg` 65,621 字节，媒体库海报用同一张 |
| 真实 RSS 轮询 | 蜜柑 RSS 60 条 / 30 条 → 首次轮询即定起点，投递 1 条、过滤 29 条，第二次轮询投递 0（幂等） |
| 「只跟新的」 | 首次轮询把起点定在最新一集（第 28 集）并写回；往期条目记为「集数低于起始集」 |
| 合集不再伪装成第一集 | `[01-28 修正合集]` 解析为 `ep=1 to=28 batch=true`，被起始集挡掉；命名用 `{span}` 落成 `S01E28`，不会再有 `S01E00` |
| 入库 | 真实蜜柑种子 → 硬链接 `第 01 季/葬送的芙莉莲 - S01E28.mkv`，同时写 `tvshow.nfo` 与 `poster.jpg`（来源就是那张蜜柑封面） |
| 面板 | 放送轴出现蜜柑订阅行：真封面缩略图 + 放送日「五」，判定区把 28 条往期逐条交代原因 |

**对比参考**：同类项目 ANI-RSS 是 Java 写的，冷启动后 RSS 通常在 200–400MB 量级，在 1GB 的玩客云上会直接挤掉系统缓存；suzu 的 22MiB 意味着它在这台机器上几乎不存在。

**测试策略里值得一提的一条**：`internal/httpx/templates_test.go` 会把首页与所有片段真正渲染一遍并断言内容。模板是运行期才求值的，函数名写错、参数类型不匹配编译期一律发现不了——这个测试写出来当场就抓到了两个真 bug（`epLabel` 未注册、入库列表把 `LibraryEntry` 当 `Item` 传）。

---

## 9. 已知边界与后续里程碑

**当前边界（诚实说明）**

- 刮削以**蜜柑**为准（搜索、详情、字幕组、封面、一键订阅全部实测可用）；简介与评分走 **Bangumi 公开接口**（匿名可用，实测 0.31s 拿到 674 字简介 + 8.5 分），写进番剧库与 `tvshow.nfo`；Bangumi 挂了只影响简介，不影响入库。
- 蜜柑番剧页里没有"总集数"行时集数就是 0，不猜；订阅起点由 RSS 实测的最新一集决定，比页面信息更可靠。
- 通知出口（webhook / Telegram）的单测已覆盖真实请求形状与节流，但**没有往真实 Telegram 发过消息**——那需要用户的 bot token。
- qBittorrent 适配器有完整的假服务端测试；**没有对着真实 qBittorrent 跑过**（本机演示环境用的是 aria2）。
- armv7 只在 qemu 里跑过（全链路通），**没在玩客云本体上跑过**：qemu 的 RSS 含它自己的翻译开销，不能当设备内存；USB 盘的挂载、`link_mode = "hardlink"` 跨设备时的表现都还要真机确认。
- 断种换链只看"进度长时间为 0"这一个信号，不做 tracker/DHT 层面的判活；候选池来自 RSS 里已见过的条目，蜜柑只给出最近 60 条时，替补池也就这么大。
- 面板是只读 + 少量表单（订阅/暂停/继续/移除），没有做逐条手工重投的 UI——换链已经是自动的，真要手工干预就用 RSS 重新订阅。

**建议的下一步（按性价比排序）**

1. **真实 qBittorrent 联调**：现有测试是假服务端，至少找一台内网 qB 跑一遍 `.torrent` 直链投递与 infohash 反查（并发投递认领不到的情况已经在面板上报错，实测一遍更放心）。
2. **播放器友好的库视图**：入库后回写 Jellyfin 的扫描触发（可选），省掉"扫不到"的困惑。
3. **`is_alive` 与 `engine` 双探活**：面板顶部显示 aria2 是否在线、磁盘剩余空间，避免"下到一半盘满"。
4. **公开 RSS 订阅分享**：`?subscribe=1` 生成一条带规则的订阅链接，方便群里互相加番。

---

## 10. 一句话架构总结

> **一个纯 Go 静态二进制（无 CGO、无运行时依赖）常驻在 armv7 上，用 SQLite 存四张表描述"见过什么 / 在下什么 / 收到了什么 / 发生过什么"，用三条互不阻塞的循环分别负责拉取判规则、追问进度并整理入库、后台收拾自己；下载这件事外包给 aria2，前端这件事外包给 `embed` + 服务端模板。所有写操作收敛到外接盘的三个目录，所有资源消耗都设了硬上限——于是这台 1GB 的机器上，最坏情况是"某集没下到"，而不是"系统进不去"。**