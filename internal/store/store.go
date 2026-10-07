// Package store 是唯一接触数据库的地方。
//
// 选型说明：用 modernc.org/sqlite（纯 Go 转译版 SQLite），不引入 CGO，
// 于是 x86 机器上一条命令就能交叉编译出 armv7 静态二进制，
// 目标机不需要装 GCC、不需要动态库。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// Store 包装 *sql.DB。开放连接数固定为 1：
// 这个应用的写量极小（每分钟几次），串行化反而彻底消灭 SQLITE_BUSY，
// 同时把 SQLite 的连接级内存从 N 份降到 1 份。
type Store struct {
	db   *sql.DB
	path string
}

// Open 打开（必要时创建）数据库并执行建表。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	// cache_size 取负数是 KiB：-4096 = 4MiB 页缓存，够用且可控。
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=cache_size(-4096)" +
		"&_pragma=mmap_size(0)" +
		"&_pragma=journal_size_limit(1048576)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表: %w", err)
	}
	st := &Store{db: db, path: path}
	// 老库补列：升级不丢数据。CREATE TABLE IF NOT EXISTS 不会给已存在的表加字段。
	if err := st.ensureColumn("bangumi", "summary", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, err
	}
	if err := st.ensureColumn("bangumi", "score", "REAL NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := st.ensureColumn("subscriptions", "mikan_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级表结构: %w", err)
	}
	if err := st.ensureColumn("tasks", "progress_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级表结构: %w", err)
	}
	if err := st.ensureColumn("library", "series_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级表结构: %w", err)
	}
	// 老库的入库记录没有 series_id（补出来是 0）。回填一次，两条线索：
	//   1. 入库记录里的标题就是番剧名（手动订的源是这样）；
	//   2. 文件路径里有番剧目录（媒体库命名规则里 {title} 就是番剧名）。
	// 都对不上的保持 0，之后按标题兜底查询，或者等这部番下一次入库时自动写上。
	if _, err := st.db.Exec(`UPDATE library SET series_id = COALESCE(
		(SELECT b.id FROM bangumi b WHERE b.title = library.title), 0)
		WHERE series_id = 0`); err != nil {
		db.Close()
		return nil, fmt.Errorf("回填 series_id: %w", err)
	}
	if _, err := st.db.Exec(`UPDATE library SET series_id = COALESCE(
		(SELECT b.id FROM bangumi b WHERE library.path LIKE '%/' || b.title || '/%'), 0)
		WHERE series_id = 0`); err != nil {
		db.Close()
		return nil, fmt.Errorf("按路径回填 series_id: %w", err)
	}
	// 老库的 tasks 没有 progress_at（补出来是 0）。直接按 0 算会得出
	// 「几万小时没动」→ 一升级就把所有在跑的任务判成断种。
	// 拿创建时间兜底，等价于「从投递那一刻起算」。
	if _, err := st.db.Exec(`UPDATE tasks SET progress_at = created_at WHERE progress_at = 0`); err != nil {
		db.Close()
		return nil, fmt.Errorf("回填 progress_at: %w", err)
	}
	return st, nil
}

// ensureColumn 检查表里有没有某一列，没有就补上（幂等）。
func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('` + table + `')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + ddl)
	return err
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Checkpoint 把 WAL 落盘并截断，配合定时快照可显著降低 eMMC / 硬盘的写放大。
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Snapshot 用 SQLite 原生的 VACUUM INTO 生成一致性快照。
// 相比写备份脚本，这条语句不需要停服、不需要拷贝 WAL。
func (s *Store) Snapshot(ctx context.Context, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	// VACUUM INTO 的目标是表达式而非占位符，这里手工转义单引号。
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(tmp, "'", "''")+"'"); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func now() int64 { return time.Now().Unix() }

func ts(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func unts(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// joinList 把字符串切片压成单字段存储；分隔符用 \x1f（单元分隔符），
// 正常文件名里不会出现，且比 JSON 省一次编解码。
const listSep = "\x1f"

func joinList(v []string) string { return strings.Join(v, listSep) }

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, listSep)
}

// ---------- subscriptions ----------

const subCols = `id, name, feed_url, grp, save_path, include, exclude, prefer,
	start_ep, interval_min, enabled, mikan_id, etag, last_mod, last_poll_at, last_error, created_at`

func scanSub(sc interface{ Scan(...any) error }) (model.Subscription, error) {
	var s model.Subscription
	var include, exclude, prefer string
	var enabled int
	var lastPoll, created int64
	err := sc.Scan(&s.ID, &s.Name, &s.FeedURL, &s.Group, &s.SavePath, &include, &exclude, &prefer,
		&s.StartEp, &s.IntervalMin, &enabled, &s.MikanID, &s.ETag, &s.LastMod, &lastPoll, &s.LastError, &created)
	if err != nil {
		return s, err
	}
	s.Include, s.Exclude, s.Prefer = splitList(include), splitList(exclude), splitList(prefer)
	s.Enabled = enabled != 0
	s.LastPollAt, s.CreatedAt = unts(lastPoll), unts(created)
	return s, nil
}

// ListSubscriptions 返回全部订阅。
func (s *Store) ListSubscriptions(ctx context.Context) ([]model.Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subCols+` FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Subscription
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// GetSubscription 按 id 取订阅。
func (s *Store) GetSubscription(ctx context.Context, id int64) (*model.Subscription, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+subCols+` FROM subscriptions WHERE id = ?`, id)
	sub, err := scanSub(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// CreateSubscription 新增订阅，返回自增 id。
func (s *Store) CreateSubscription(ctx context.Context, sub *model.Subscription) (int64, error) {
	if sub.IntervalMin <= 0 {
		sub.IntervalMin = 30
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO subscriptions (name, feed_url, grp, save_path, include, exclude, prefer,
		 start_ep, interval_min, enabled, mikan_id, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		sub.Name, sub.FeedURL, sub.Group, sub.SavePath, joinList(sub.Include), joinList(sub.Exclude),
		joinList(sub.Prefer), sub.StartEp, sub.IntervalMin, boolInt(sub.Enabled), sub.MikanID, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSubscription 更新可变字段（不含轮询游标）。
func (s *Store) UpdateSubscription(ctx context.Context, sub *model.Subscription) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET name=?, grp=?, save_path=?, include=?, exclude=?, prefer=?,
		 start_ep=?, interval_min=?, enabled=?, mikan_id=? WHERE id=?`,
		sub.Name, sub.Group, sub.SavePath, joinList(sub.Include), joinList(sub.Exclude),
		joinList(sub.Prefer), sub.StartEp, sub.IntervalMin, boolInt(sub.Enabled), sub.MikanID, sub.ID)
	return err
}

// DeleteSubscription 删除订阅；条目与任务由外键级联清理。
func (s *Store) DeleteSubscription(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	return err
}

// SetPollResult 记录条件请求游标。
func (s *Store) SetPollResult(ctx context.Context, id int64, etag, lastMod string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET etag=?, last_mod=?, last_poll_at=?, last_error='' WHERE id=?`,
		etag, lastMod, now(), id)
	return err
}

// SetSubscriptionError 记录最近一次失败原因。
func (s *Store) SetSubscriptionError(ctx context.Context, id int64, msg string) error {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET last_error=?, last_poll_at=? WHERE id=?`, msg, now(), id)
	return err
}

// Touched 更新最近轮询时间（304 分支用）。
func (s *Store) Touched(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET last_poll_at=? WHERE id=?`, now(), id)
	return err
}

// ---------- items ----------

// InsertItemIfNew 在 (sub_id, guid) 上做幂等插入，返回条目 id 与是否新建。
// created=false 表示这条已经在库里了——这是防止重复下载的第一道闸门。
func (s *Store) InsertItemIfNew(ctx context.Context, it *model.Item) (int64, bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO items (sub_id, guid, title, link, uri, size_bytes, pub_date, episode,
		 episode_to, batch, fansub, status, reason, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(sub_id, guid) DO NOTHING`,
		it.SubID, it.GUID, it.Title, it.Link, it.URI, it.SizeBytes, ts(it.PubDate),
		it.Episode, it.EpisodeTo, boolInt(it.Batch), it.Fansub, string(it.Status), it.Reason, now())
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		id, _ := res.LastInsertId()
		it.ID = id
		return id, true, nil
	}
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM items WHERE sub_id = ? AND guid = ?`, it.SubID, it.GUID).Scan(&id); err != nil {
		return 0, false, err
	}
	it.ID = id
	return id, false, nil
}

// GetItem 按 id 取条目。
func (s *Store) GetItem(ctx context.Context, id int64) (*model.Item, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id = ?`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// AltItemsForEpisode 找同一集「被择优刷掉但还没下过」的其它版本，用于断种换链。
//
// 只挑 rejected 与 matched 的候选：done 表示已经下好了，failed 表示这条也已经
// 试过并失败，pending 表示还没轮到判定。按体积倒序——同一集里更大的通常码率更高。
// 排除 excludeItemID（刚失败的那条），避免换链后原地再投一次。
func (s *Store) AltItemsForEpisode(ctx context.Context, subID int64, episode float64, excludeItemID int64, limit int) ([]model.Item, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+itemCols+` FROM items
		WHERE sub_id = ? AND ABS(episode - ?) < 0.01 AND id <> ?
		  AND status IN ('rejected', 'matched')
		ORDER BY size_bytes DESC, id DESC LIMIT ?`,
		subID, episode, excludeItemID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Item, 0, limit)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// CountRelinks 数一条条目已经换过几次链（同订阅同一集）。
func (s *Store) CountRelinks(ctx context.Context, subID int64, episode float64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM items
		WHERE sub_id = ? AND ABS(episode - ?) < 0.01 AND reason LIKE '断种换链%'`,
		subID, episode).Scan(&n)
	return n, err
}

// EpisodeTaken 判断某订阅的某一集是否已经有任务或已入库。
// 这是「同一集被多个字幕组同时发布」时的第二道闸门（第一道是 guid 去重）。
func (s *Store) EpisodeTaken(ctx context.Context, subID int64, episode float64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks t JOIN items i ON i.id = t.item_id
		WHERE i.sub_id = ? AND ABS(i.episode - ?) < 0.01 AND t.state <> 'error'`,
		subID, episode).Scan(&n)
	return n > 0, err
}

const itemCols = `id, sub_id, guid, title, link, uri, size_bytes, pub_date, episode,
	episode_to, batch, fansub, status, reason, created_at`

func scanItem(sc interface{ Scan(...any) error }) (model.Item, error) {
	var it model.Item
	var pub, created int64
	var batch int
	var status string
	err := sc.Scan(&it.ID, &it.SubID, &it.GUID, &it.Title, &it.Link, &it.URI, &it.SizeBytes, &pub,
		&it.Episode, &it.EpisodeTo, &batch, &it.Fansub, &status, &it.Reason, &created)
	it.PubDate, it.CreatedAt, it.Batch, it.Status = unts(pub), unts(created), batch != 0, model.ItemStatus(status)
	return it, err
}

// ListItems 返回最近条目。
func (s *Store) ListItems(ctx context.Context, limit int) ([]model.Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+itemCols+` FROM items ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetItemStatus 更新条目状态与原因。
func (s *Store) SetItemStatus(ctx context.Context, id int64, st model.ItemStatus, reason string) error {
	if len(reason) > 200 {
		reason = reason[:200]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE items SET status=?, reason=? WHERE id=?`, string(st), reason, id)
	return err
}

// CountItemsByStatus 返回各状态条目数，供面板概览使用。
func (s *Store) CountItemsByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM items GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// ---------- tasks ----------

const taskCols = `id, item_id, title, downloader, gid, state, progress, total_bytes,
	done_bytes, save_path, error, created_at, updated_at, progress_at`

func scanTask(sc interface{ Scan(...any) error }) (model.Task, error) {
	var t model.Task
	var created, updated, changed int64
	var state string
	err := sc.Scan(&t.ID, &t.ItemID, &t.Title, &t.Downloader, &t.GID, &state, &t.Progress,
		&t.TotalBytes, &t.DoneBytes, &t.SavePath, &t.Error, &created, &updated, &changed)
	t.State = model.TaskState(state)
	t.CreatedAt, t.UpdatedAt, t.ProgressAt = unts(created), unts(updated), unts(changed)
	return t, err
}

// CreateTask 建任务。
func (s *Store) CreateTask(ctx context.Context, t *model.Task) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (item_id, title, downloader, gid, state, progress, total_bytes,
		 done_bytes, save_path, error, created_at, updated_at, progress_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ItemID, t.Title, t.Downloader, t.GID, string(t.State), t.Progress, t.TotalBytes,
		t.DoneBytes, t.SavePath, t.Error, now(), now(), now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListTasks 返回最近任务。
func (s *Store) ListTasks(ctx context.Context, limit int) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskCols+` FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListActiveTasks 返回未终结的任务，对账循环每轮只查这些。
func (s *Store) ListActiveTasks(ctx context.Context) ([]model.Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE state NOT IN ('completed','error') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTask 取单个任务。
func (s *Store) GetTask(ctx context.Context, id int64) (*model.Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UpdateTaskProgress 只写对账循环真正会变的那几列。
func (s *Store) UpdateTaskProgress(ctx context.Context, id int64, gid string, st model.TaskState, progress float64, total, done int64, errMsg string) error {
	if len(errMsg) > 300 {
		errMsg = errMsg[:300]
	}
	// progress_at 只在进度真的变了才推进：断种判断要的是「多久没动」，
	// 而不是「多久没查」——对账每轮都查，后者永远是刚刚。
	n := now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET gid=?, state=?, progress=?, total_bytes=?, done_bytes=?, error=?,
		 updated_at=?,
		 progress_at = CASE WHEN progress <> ? OR done_bytes <> ? THEN ? ELSE progress_at END
		 WHERE id=?`, gid, string(st), progress, total, done, errMsg, n,
		progress, done, n, id)
	return err
}

// CountActiveTasks 返回进行中任务数，用于给面板做限流提示。
func (s *Store) CountActiveTasks(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE state NOT IN ('completed','error')`).Scan(&n)
	return n, err
}

// ---------- library ----------

// AddLibraryEntry 记录入库结果。
func (s *Store) AddLibraryEntry(ctx context.Context, e *model.LibraryEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO library (series_id, task_id, title, path, linked, nfo_path, episode, added_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		e.SeriesID, e.TaskID, e.Title, e.Path, boolInt(e.Linked), e.NFOPath, e.Episode, now())
	return err
}

// ListLibrary 返回最近入库记录。
func (s *Store) ListLibrary(ctx context.Context, limit int) ([]model.LibraryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, series_id, task_id, title, path, linked, nfo_path, episode, added_at
		 FROM library ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LibraryEntry
	for rows.Next() {
		var e model.LibraryEntry
		var linked int
		var added int64
		if err := rows.Scan(&e.ID, &e.SeriesID, &e.TaskID, &e.Title, &e.Path, &linked, &e.NFOPath, &e.Episode, &added); err != nil {
			return nil, err
		}
		e.Linked, e.AddedAt = linked != 0, unts(added)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountLibrary 返回入库总数。
// LibraryPathForSeries 找出这部番剧已经入库的某个文件路径（没有就返回空串）。
//
// 番剧级元数据（tvshow.nfo / poster.jpg）落在「番剧根目录」下，
// 而根目录是从文件路径反推出来的，所以刷新元数据前要先拿到一条真实路径。
//
// 优先用番剧 id 找：番剧标题会因为刮削来源不同而变（蜜柑叫「葬送的芙莉莲」、
// Bangumi 叫「葬送のフリーレン」，带不带日文原名也变），拿标题当键迟早对不上。
// seriesID 为 0 或没命中时退回按标题匹配，照顾加这个列之前入库的老记录。
func (s *Store) LibraryPathForSeries(ctx context.Context, seriesID int64, title string) (string, error) {
	var path string
	if seriesID > 0 {
		err := s.db.QueryRowContext(ctx,
			`SELECT path FROM library WHERE series_id = ? ORDER BY id DESC LIMIT 1`, seriesID).Scan(&path)
		switch {
		case err == nil:
			return path, nil
		case err != sql.ErrNoRows:
			return "", err
		}
	}
	if title == "" {
		return "", nil
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT path FROM library WHERE title = ? ORDER BY id DESC LIMIT 1`, title).Scan(&path)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) CountLibrary(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM library`).Scan(&n)
	return n, err
}

// ---------- events ----------

// Log 写一条活动记录。刻意只保留最近若干条，见 TrimEvents。
func (s *Store) Log(ctx context.Context, kind, msg string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (kind, message, at) VALUES (?,?,?)`,
		kind, msg, now())
	return err
}

// RecentEvents 返回最近活动。
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]model.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, message, at FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		var e model.Event
		var at int64
		if err := rows.Scan(&e.ID, &e.Kind, &e.Message, &at); err != nil {
			return nil, err
		}
		e.At = unts(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrimEvents 裁剪活动流，长时间运行的设备上不能让这张表无限长。
func (s *Store) TrimEvents(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── 番剧信息刮削 ────────────────────────────────────────────────────────

const bangumiCols = `id, title, cover_path, air_text, episodes, start_at, official, subgroups, fetched_at, summary, score`

// UpsertBangumi 写入或覆盖一条番剧信息。
//
// 搜索页拿到的卡片只有标题和封面，详情页才带字幕组与集数；
// 所以这里的新值一律「空则不覆盖」：搜一次番剧不会把已刮到的
// 字幕组、总集数、放送时间洗成空的。幂等，面板连点两次订阅也只留一条。
func (s *Store) UpsertBangumi(ctx context.Context, b *model.Bangumi) error {
	// 空列表写空串而不是 "null"：SQL 里的「空则不覆盖」才判得准。
	sg := ""
	if len(b.Subgroups) > 0 {
		raw, err := json.Marshal(b.Subgroups)
		if err != nil {
			return err
		}
		sg = string(raw)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bangumi (id, title, cover_path, air_text, episodes, start_at, official, subgroups, fetched_at, summary, score)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   title=excluded.title,
		   cover_path=CASE WHEN excluded.cover_path <> '' THEN excluded.cover_path ELSE bangumi.cover_path END,
		   air_text=CASE WHEN excluded.air_text <> '' THEN excluded.air_text ELSE bangumi.air_text END,
		   episodes=CASE WHEN excluded.episodes > 0 THEN excluded.episodes ELSE bangumi.episodes END,
		   start_at=CASE WHEN excluded.start_at <> '' THEN excluded.start_at ELSE bangumi.start_at END,
		   official=CASE WHEN excluded.official <> '' THEN excluded.official ELSE bangumi.official END,
		   subgroups=CASE WHEN excluded.subgroups NOT IN ('', '[]') THEN excluded.subgroups ELSE bangumi.subgroups END,
		   summary=CASE WHEN excluded.summary <> '' THEN excluded.summary ELSE bangumi.summary END,
		   score=CASE WHEN excluded.score > 0 THEN excluded.score ELSE bangumi.score END,
		   fetched_at=excluded.fetched_at`,
		b.ID, b.Title, b.CoverPath, b.AirText, b.Episodes, b.StartAt, b.Official, sg,
		ts(b.FetchedAt), b.Summary, b.Score)
	return err
}

// GetBangumi 读一条番剧信息；不存在时返回 nil, nil。
func (s *Store) GetBangumi(ctx context.Context, id int64) (*model.Bangumi, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+bangumiCols+` FROM bangumi WHERE id = ?`, id)
	b, err := scanBangumi(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ListBangumi 按刮削时间倒序取最近的若干条，供面板做封面缓存与展示。
func (s *Store) ListBangumi(ctx context.Context, limit int) ([]model.Bangumi, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+bangumiCols+` FROM bangumi ORDER BY fetched_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Bangumi, 0, 16)
	for rows.Next() {
		b, err := scanBangumi(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanBangumi(sc interface{ Scan(...any) error }) (model.Bangumi, error) {
	var b model.Bangumi
	var sg string
	var fetched int64
	if err := sc.Scan(&b.ID, &b.Title, &b.CoverPath, &b.AirText, &b.Episodes,
		&b.StartAt, &b.Official, &sg, &fetched, &b.Summary, &b.Score); err != nil {
		return b, err
	}
	if sg != "" {
		// 字幕组列表坏掉不该让整条记录读不出来，忽略解析错误即可。
		_ = json.Unmarshal([]byte(sg), &b.Subgroups)
	}
	b.FetchedAt = unts(fetched)
	return b, nil
}
