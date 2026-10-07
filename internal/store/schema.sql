-- 玩客云上的取舍：全部字段用整数/文本，不用 BLOB；
-- 时间统一存 Unix 秒；列表查询都走索引，不给 armv7 留全表扫描。

CREATE TABLE IF NOT EXISTS subscriptions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    feed_url     TEXT    NOT NULL UNIQUE,
    grp          TEXT    NOT NULL DEFAULT '',
    save_path    TEXT    NOT NULL DEFAULT '',
    include      TEXT    NOT NULL DEFAULT '',
    exclude      TEXT    NOT NULL DEFAULT '',
    prefer       TEXT    NOT NULL DEFAULT '',
    start_ep     REAL    NOT NULL DEFAULT 0,
    interval_min INTEGER NOT NULL DEFAULT 30,
    enabled      INTEGER NOT NULL DEFAULT 1,
    mikan_id     INTEGER NOT NULL DEFAULT 0,
    etag         TEXT    NOT NULL DEFAULT '',
    last_mod     TEXT    NOT NULL DEFAULT '',
    last_poll_at INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS items (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    sub_id     INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    guid       TEXT    NOT NULL,
    title      TEXT    NOT NULL,
    link       TEXT    NOT NULL DEFAULT '',
    uri        TEXT    NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    pub_date   INTEGER NOT NULL DEFAULT 0,
    episode    REAL    NOT NULL DEFAULT 0,
    episode_to REAL    NOT NULL DEFAULT 0,
    batch      INTEGER NOT NULL DEFAULT 0,
    fansub     TEXT    NOT NULL DEFAULT '',
    status     TEXT    NOT NULL DEFAULT 'new',
    reason     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0
);

-- 同一条目重复出现在 feed 里是常态，靠联合唯一键去重，避免重复下载。
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_sub_guid ON items(sub_id, guid);
CREATE INDEX IF NOT EXISTS idx_items_status ON items(status, id DESC);

CREATE TABLE IF NOT EXISTS tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    title       TEXT    NOT NULL DEFAULT '',
    downloader  TEXT    NOT NULL DEFAULT '',
    gid         TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL DEFAULT 'queued',
    progress    REAL    NOT NULL DEFAULT 0,
    total_bytes INTEGER NOT NULL DEFAULT 0,
    done_bytes  INTEGER NOT NULL DEFAULT 0,
    save_path   TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL DEFAULT 0,
    progress_at INTEGER NOT NULL DEFAULT 0
);

-- 对账循环只关心未终结的任务。
CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state, id DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_gid ON tasks(gid);

CREATE TABLE IF NOT EXISTS library (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    series_id INTEGER NOT NULL DEFAULT 0,
    task_id  INTEGER NOT NULL DEFAULT 0,
    title    TEXT    NOT NULL DEFAULT '',
    path     TEXT    NOT NULL DEFAULT '',
    linked   INTEGER NOT NULL DEFAULT 0,
    nfo_path TEXT    NOT NULL DEFAULT '',
    episode  REAL    NOT NULL DEFAULT 0,
    added_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_library_added ON library(added_at DESC);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    kind    TEXT    NOT NULL DEFAULT '',
    message TEXT    NOT NULL DEFAULT '',
    at      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_events_at ON events(at DESC);

-- 番剧信息刮削结果。字数不多，整表也就几十行，但省掉了每次开面板都去爬蜜柑。
CREATE TABLE IF NOT EXISTS bangumi (
    id         INTEGER PRIMARY KEY,          -- 蜜柑番剧 id
    title      TEXT    NOT NULL DEFAULT '',
    cover_path TEXT    NOT NULL DEFAULT '',
    air_text   TEXT    NOT NULL DEFAULT '',
    episodes   INTEGER NOT NULL DEFAULT 0,
    start_at   TEXT    NOT NULL DEFAULT '',
    official   TEXT    NOT NULL DEFAULT '',
    subgroups  TEXT    NOT NULL DEFAULT '',  -- JSON: [{id,name,rss}]
    summary    TEXT    NOT NULL DEFAULT '',  -- 简介：来自 Bangumi
    score      REAL    NOT NULL DEFAULT 0,   -- Bangumi 评分
    fetched_at INTEGER NOT NULL DEFAULT 0
);

-- 面板按刮削时间倒排：太久没更新的排前面，方便提示刷新。
CREATE INDEX IF NOT EXISTS idx_bangumi_fetched ON bangumi(fetched_at);

CREATE TABLE IF NOT EXISTS meta (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL DEFAULT ''
);