package logstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/CedarHuang/Namesplit/internal/event"
)

const (
	// MaxRows 是查询记录的行数上限。
	MaxRows = 200000
	// MaxBytes 是查询日志库文件（含 WAL/SHM）的字节上限。
	MaxBytes = 100 << 20

	persistQueueSize  = 8192
	persistBatchSize  = 500
	persistRetryInit  = 10 * time.Millisecond
	persistRetryLimit = time.Second

	// incrementalAutoVacuum 是 PRAGMA auto_vacuum=INCREMENTAL 的取值。
	incrementalAutoVacuum = 2
)

// limits 是查询记录的容量上限；以参数传入，便于按场景使用不同阈值。
type limits struct {
	maxRows  int64
	maxBytes int64
}

// defaultLimits 返回默认容量上限。
func defaultLimits() limits {
	return limits{maxRows: MaxRows, maxBytes: MaxBytes}
}

// minuteSource 提供待落盘的分钟聚合。
type minuteSource interface {
	takeDirtyMinutes() []MinuteStat
	// restoreDirtyMinutes 把写入失败的分钟聚合重新标记为待落盘。落盘值按
	// lastFlushed 计算增量，重试不会重复计数。
	restoreDirtyMinutes(stats []MinuteStat)
	// minuteCumulative 返回内存中全部分钟聚合的累计值，用于建立落盘基线。
	minuteCumulative() []MinuteStat
}

// persister 负责把事件与分钟聚合写入 SQLite。
type persister struct {
	dir           string
	logger        *slog.Logger
	db            *sql.DB
	queue         chan event.Event
	done          chan struct{}
	minutes       minuteSource
	persistErrors *atomic.Uint64
	limits        limits
	rowCount      atomic.Int64

	// lastFlushed 记录每个分钟已落盘的累计值；落盘时只写增量，从而支持跨重启
	// 累加与关闭持久化期间的"不补录"。
	lastFlushed map[string]MinuteStat

	closed    atomic.Bool
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// newPersister 打开数据库并启动写协程。
func newPersister(
	dir string,
	logger *slog.Logger,
	persistErrors *atomic.Uint64,
	minutes minuteSource,
	lim limits,
) (*persister, error) {
	if lim.maxRows <= 0 {
		lim.maxRows = MaxRows
	}
	if lim.maxBytes <= 0 {
		lim.maxBytes = MaxBytes
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "querylog.db"))
	if err != nil {
		return nil, fmt.Errorf("打开查询日志库: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := initSchema(db); err != nil {
		_ = db.Close()

		return nil, err
	}

	p := &persister{
		dir:           dir,
		logger:        logger,
		db:            db,
		queue:         make(chan event.Event, persistQueueSize),
		done:          make(chan struct{}),
		minutes:       minutes,
		persistErrors: persistErrors,
		limits:        lim,
		lastFlushed:   make(map[string]MinuteStat),
	}
	for _, stat := range minutes.minuteCumulative() {
		p.lastFlushed[stat.Minute] = stat
	}

	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM queries").Scan(&count); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("读取查询记录数: %w", err)
	}
	p.rowCount.Store(count)

	p.wg.Add(1)
	go p.run()

	return p, nil
}

// initSchema 创建表、索引并设置连接参数。
func initSchema(db *sql.DB) error {
	// auto_vacuum 必须在任何表创建之前设置；已有数据库只能通过 VACUUM 迁移。
	// 启用增量回收后，DELETE 释放的页才能经 incremental_vacuum 归还文件系统。
	if _, err := db.Exec(`PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return fmt.Errorf("设置查询日志库 auto_vacuum: %w", err)
	}

	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS queries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id INTEGER NOT NULL,
			time_ns INTEGER NOT NULL,
			time_text TEXT NOT NULL,
			domain TEXT NOT NULL,
			qtype TEXT NOT NULL,
			source TEXT NOT NULL,
			group_id TEXT NOT NULL,
			rule TEXT NOT NULL,
			upstream TEXT NOT NULL,
			rcode INTEGER NOT NULL,
			duration_ms REAL NOT NULL,
			cache_hit INTEGER NOT NULL,
			payload TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_queries_time_ns ON queries(time_ns)`,
		`CREATE INDEX IF NOT EXISTS idx_queries_domain ON queries(domain)`,
		`CREATE INDEX IF NOT EXISTS idx_queries_source ON queries(source)`,
		`CREATE TABLE IF NOT EXISTS minute_stats (
			minute TEXT PRIMARY KEY,
			queries INTEGER NOT NULL,
			failures INTEGER NOT NULL,
			cache_hits INTEGER NOT NULL,
			latency_avg_ms REAL NOT NULL,
			latency_max_ms REAL NOT NULL
		)`,
	}

	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("初始化查询日志库: %w", err)
		}
	}

	return migrateAutoVacuum(db)
}

// migrateAutoVacuum 确保已有数据库也启用增量回收。
//
// 旧库（auto_vacuum=NONE）上 PRAGMA 只记录目标值，必须执行一次 VACUUM 重写
// 数据库才会真正生效；新库在 initSchema 设置时已经生效，不产生额外开销。
func migrateAutoVacuum(db *sql.DB) error {
	mode, err := autoVacuumMode(db)
	if err != nil {
		return err
	}
	if mode == incrementalAutoVacuum {
		return nil
	}

	if _, err := db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("迁移查询日志库 auto_vacuum: %w", err)
	}

	mode, err = autoVacuumMode(db)
	if err != nil {
		return err
	}
	if mode != incrementalAutoVacuum {
		return fmt.Errorf("查询日志库 auto_vacuum 迁移失败: 当前值 %d", mode)
	}

	return nil
}

// autoVacuumMode 返回当前的 auto_vacuum 模式。
func autoVacuumMode(db *sql.DB) (int, error) {
	var mode int
	if err := db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return 0, fmt.Errorf("读取查询日志库 auto_vacuum: %w", err)
	}

	return mode, nil
}

// enqueue 把事件放入持久化队列；存储关闭后直接返回。
//
// 队列满时阻塞形成反压，只有磁盘长期异常时才会触发。关闭信号与入队用 select
// 竞争，因此 Close 不会被阻塞的入队者卡住。
func (p *persister) enqueue(ev event.Event) {
	if p.closed.Load() {
		return
	}

	select {
	case p.queue <- ev:
	case <-p.done:
	}
}

// run 批量写事件并周期落盘分钟聚合。
func (p *persister) run() {
	defer p.wg.Done()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	batch := make([]event.Event, 0, persistBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}

		p.writeBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case ev := <-p.queue:
			batch = append(batch, ev)
			if len(batch) >= persistBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
			p.flushMinutes()
		case <-p.done:
			for {
				select {
				case ev := <-p.queue:
					batch = append(batch, ev)
					if len(batch) >= persistBatchSize {
						flush()
					}
				default:
					flush()
					p.flushMinutes()
					p.checkpoint()
					_ = p.db.Close()

					return
				}
			}
		}
	}
}

// writeBatch 写入一批事件。
//
// 写入失败只记录 persist_errors 并按指数退避重试，不设丢弃路径；队列满后由
// enqueue 反压上游，因此只有磁盘长期异常才会反压查询。
func (p *persister) writeBatch(batch []event.Event) {
	backoff := persistRetryInit

	for {
		err := p.insertBatch(batch)
		if err == nil {
			p.rowCount.Add(int64(len(batch)))
			p.enforceLimits()

			return
		}

		p.persistErrors.Add(1)
		p.logger.Error("查询日志写入失败，将重试", "error", err, "backoff", backoff)

		time.Sleep(backoff)
		if backoff < persistRetryLimit {
			backoff *= 2
			if backoff > persistRetryLimit {
				backoff = persistRetryLimit
			}
		}
	}
}

// insertBatch 在单个事务内插入一批事件。
func (p *persister) insertBatch(batch []event.Event) (err error) {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	statement, err := tx.Prepare(`INSERT INTO queries (
		event_id, time_ns, time_text, domain, qtype, source, group_id, rule,
		upstream, rcode, duration_ms, cache_hit, payload
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = statement.Close() }()

	for _, ev := range batch {
		payload, err := json.Marshal(ev)
		if err != nil {
			return err
		}

		cacheHit := 0
		if ev.CacheHit {
			cacheHit = 1
		}

		if _, err = statement.Exec(
			ev.EventID,
			ev.Time.UTC().UnixNano(),
			ev.Time.UTC().Format(time.RFC3339Nano),
			ev.Domain,
			ev.QType,
			ev.Source,
			ev.Group,
			ev.Rule,
			ev.Upstream,
			ev.Rcode,
			ev.DurationMS,
			cacheHit,
			string(payload),
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// enforceLimits 按行数与文件字节上限滚动删除最旧记录。
//
// 行数上限按行删除即可；字节上限则必须以物理文件体积为判据：SQLite 的 DELETE
// 只把页放入 freelist，必须执行 incremental_vacuum 才能真正归还磁盘空间。
func (p *persister) enforceLimits() {
	if count := p.rowCount.Load(); count > p.limits.maxRows {
		if deleted := p.deleteOldest(count - p.limits.maxRows); deleted > 0 {
			p.rowCount.Add(-deleted)
		}
		p.reclaimSpace()
	}

	for p.totalBytes() > p.limits.maxBytes {
		remaining := p.rowCount.Load()
		if remaining == 0 {
			// 记录已删空仍超限：只剩空闲页或其它表占用，做一次完整收缩。
			p.reclaimSpace()
			p.vacuum()

			return
		}

		target := remaining / 10
		if target < 1 {
			target = 1
		}
		deleted := p.deleteOldest(target)
		if deleted == 0 {
			p.vacuum()

			return
		}
		p.rowCount.Add(-deleted)
		p.reclaimSpace()
	}
}

// reclaimSpace 归还删除产生的空闲页并截断 WAL。
func (p *persister) reclaimSpace() {
	if _, err := p.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
		p.logger.Error("回收查询日志空闲页失败", "error", err)
	}
	p.checkpoint()
}

// vacuum 重写数据库文件，用于兜底收缩无法通过增量回收释放的空间。
func (p *persister) vacuum() {
	if _, err := p.db.Exec(`VACUUM`); err != nil {
		p.logger.Error("收缩查询日志库失败", "error", err)

		return
	}
	p.checkpoint()
}

// deleteOldest 删除最旧的 n 条记录。
func (p *persister) deleteOldest(n int64) int64 {
	if n <= 0 {
		return 0
	}

	result, err := p.db.Exec(
		`DELETE FROM queries WHERE id IN (SELECT id FROM queries ORDER BY id LIMIT ?)`,
		n,
	)
	if err != nil {
		p.persistErrors.Add(1)
		p.logger.Error("滚动删除查询记录失败", "error", err)

		return 0
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0
	}

	return deleted
}

// totalBytes 返回数据库文件与 WAL/SHM 的总字节数。
func (p *persister) totalBytes() int64 {
	path := filepath.Join(p.dir, "querylog.db")

	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err == nil {
			total += info.Size()
		}
	}

	return total
}

// flushMinutes 把待落盘的分钟聚合写入数据库。
func (p *persister) flushMinutes() {
	p.pruneMinutes()

	stats := p.minutes.takeDirtyMinutes()
	if len(stats) == 0 {
		return
	}

	failed := make([]MinuteStat, 0)
	for _, stat := range stats {
		delta, changed := minuteDelta(p.lastFlushed[stat.Minute], stat)
		if !changed {
			p.lastFlushed[stat.Minute] = stat

			continue
		}

		if _, err := p.db.Exec(
			`INSERT INTO minute_stats (minute, queries, failures, cache_hits, latency_avg_ms, latency_max_ms)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(minute) DO UPDATE SET
				queries = minute_stats.queries + excluded.queries,
				failures = minute_stats.failures + excluded.failures,
				cache_hits = minute_stats.cache_hits + excluded.cache_hits,
				latency_avg_ms = CASE
					WHEN minute_stats.queries + excluded.queries <= 0 THEN 0
					ELSE (
						minute_stats.latency_avg_ms * minute_stats.queries +
						excluded.latency_avg_ms * excluded.queries
					) / (minute_stats.queries + excluded.queries)
				END,
				latency_max_ms = MAX(minute_stats.latency_max_ms, excluded.latency_max_ms)`,
			delta.Minute, delta.Queries, delta.Failures, delta.CacheHits,
			delta.LatencyAvgMS, delta.LatencyMaxMS,
		); err != nil {
			p.persistErrors.Add(1)
			p.logger.Error("分钟聚合写入失败，将在下个周期重试", "error", err)
			failed = append(failed, stat)

			continue
		}

		p.lastFlushed[stat.Minute] = stat
	}

	if len(failed) > 0 {
		p.minutes.restoreDirtyMinutes(failed)
	}
}

// minuteDelta 计算相对上次落盘值的增量。
//
// 分钟聚合在内存中是累计值；写增量而不是绝对值，才能让跨重启的同一分钟
// 正确累加。平均延迟按事件数加权，最大延迟取本次累计最大值。
func minuteDelta(prev, cur MinuteStat) (MinuteStat, bool) {
	queries := cur.Queries - prev.Queries
	failures := cur.Failures - prev.Failures
	cacheHits := cur.CacheHits - prev.CacheHits
	if queries < 0 || failures < 0 || cacheHits < 0 {
		// 桶被重置（时间回拨/裁剪后重建）时以当前累计值为增量。
		queries, failures, cacheHits = cur.Queries, cur.Failures, cur.CacheHits
		prev = MinuteStat{}
	}
	if queries == 0 && failures == 0 && cacheHits == 0 {
		return MinuteStat{}, false
	}

	avg := 0.0
	if queries > 0 {
		prevSum := prev.LatencyAvgMS * float64(prev.Queries)
		curSum := cur.LatencyAvgMS * float64(cur.Queries)
		avg = (curSum - prevSum) / float64(queries)
	}

	return MinuteStat{
		Minute:       cur.Minute,
		Queries:      queries,
		Failures:     failures,
		CacheHits:    cacheHits,
		LatencyAvgMS: avg,
		LatencyMaxMS: cur.LatencyMaxMS,
	}, true
}

// pruneMinutes 删除数据库中超出保留期的分钟聚合与对应落盘基线。
func (p *persister) pruneMinutes() {
	cutoff := time.Now().UTC().Add(-minuteRetention).Format(time.RFC3339)
	if _, err := p.db.Exec(`DELETE FROM minute_stats WHERE minute < ?`, cutoff); err != nil {
		p.persistErrors.Add(1)
		p.logger.Error("清理过期分钟聚合失败", "error", err)
	}

	for minute := range p.lastFlushed {
		if minute < cutoff {
			delete(p.lastFlushed, minute)
		}
	}
}

// checkpoint 把 WAL 合并回主库。
func (p *persister) checkpoint() {
	if _, err := p.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		p.logger.Error("查询日志 checkpoint 失败", "error", err)
	}
}

// query 执行历史查询。
func (p *persister) query(ctx context.Context, filter Filter) (Page, error) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 8)

	if filter.Cursor != "" {
		cursor, err := strconv.ParseInt(filter.Cursor, 10, 64)
		if err != nil {
			return Page{}, fmt.Errorf("cursor %q 不是十进制整数", filter.Cursor)
		}
		where = append(where, "id < ?")
		args = append(args, cursor)
	}
	if filter.Domain != "" {
		where = append(where, "instr(lower(domain), lower(?)) > 0")
		args = append(args, filter.Domain)
	}
	if filter.QType != "" {
		where = append(where, "qtype = ?")
		args = append(args, filter.QType)
	}
	if filter.Source != "" {
		where = append(where, "source = ?")
		args = append(args, filter.Source)
	}
	if filter.Group != "" {
		where = append(where, "group_id = ?")
		args = append(args, filter.Group)
	}
	if filter.Rule != "" {
		where = append(where, "rule = ?")
		args = append(args, filter.Rule)
	}
	if filter.Upstream != "" {
		where = append(where, "upstream = ?")
		args = append(args, filter.Upstream)
	}
	if filter.Rcode != nil {
		where = append(where, "rcode = ?")
		args = append(args, *filter.Rcode)
	}
	if filter.MinDurationMS != nil {
		where = append(where, "duration_ms >= ?")
		args = append(args, *filter.MinDurationMS)
	}
	if filter.MaxDurationMS != nil {
		where = append(where, "duration_ms <= ?")
		args = append(args, *filter.MaxDurationMS)
	}
	if filter.From != nil {
		where = append(where, "time_ns >= ?")
		args = append(args, filter.From.UTC().UnixNano())
	}
	if filter.To != nil {
		where = append(where, "time_ns < ?")
		args = append(args, filter.To.UTC().UnixNano())
	}

	query := strings.Join([]string{
		"SELECT id, payload FROM queries WHERE",
		strings.Join(where, " AND "),
		"ORDER BY id DESC LIMIT ?",
	}, " ")

	args = append(args, filter.Limit+1)

	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]HistoryItem, 0, filter.Limit+1)
	for rows.Next() {
		var (
			id      int64
			payload string
		)
		if err := rows.Scan(&id, &payload); err != nil {
			return Page{}, err
		}

		var ev event.Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return Page{}, err
		}

		items = append(items, HistoryItem{ID: id, Event: ev})
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}

	page := Page{Items: items, PersistenceEnabled: true}
	if len(items) > filter.Limit {
		page.Items = items[:filter.Limit]
		next := strconv.FormatInt(page.Items[len(page.Items)-1].ID, 10)
		page.NextCursor = &next
	}

	return page, nil
}

// clear 删除全部查询记录。
func (p *persister) clear(ctx context.Context) (int64, error) {
	result, err := p.db.ExecContext(ctx, `DELETE FROM queries`)
	if err != nil {
		return 0, err
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	p.rowCount.Store(0)
	p.reclaimSpace()

	return deleted, nil
}

// minute 读取分钟聚合。
func (p *persister) minute(from, to time.Time) []MinuteStat {
	rows, err := p.db.Query(
		`SELECT minute, queries, failures, cache_hits, latency_avg_ms, latency_max_ms
		FROM minute_stats WHERE minute >= ? AND minute < ? ORDER BY minute`,
		from.UTC().Format(time.RFC3339),
		to.UTC().Format(time.RFC3339),
	)
	if err != nil {
		p.persistErrors.Add(1)
		p.logger.Error("读取分钟聚合失败", "error", err)

		return []MinuteStat{}
	}
	defer func() { _ = rows.Close() }()

	stats := make([]MinuteStat, 0)
	for rows.Next() {
		var stat MinuteStat
		if err := rows.Scan(
			&stat.Minute, &stat.Queries, &stat.Failures, &stat.CacheHits,
			&stat.LatencyAvgMS, &stat.LatencyMaxMS,
		); err != nil {
			p.logger.Error("解析分钟聚合失败", "error", err)

			return stats
		}
		stats = append(stats, stat)
	}

	return stats
}

// top 从查询记录计算 Top。
func (p *persister) top(by string, since time.Time, limit int) []TopItem {
	var column string
	switch by {
	case "domain":
		column = "domain"
	case "upstream":
		column = "upstream"
	default:
		return []TopItem{}
	}

	rows, err := p.db.Query(
		fmt.Sprintf(
			`SELECT %s, COUNT(*) FROM queries WHERE time_ns >= ? AND %s <> ''
			GROUP BY %s ORDER BY COUNT(*) DESC, %s ASC LIMIT ?`,
			column, column, column, column,
		),
		since.UTC().UnixNano(),
		limit,
	)
	if err != nil {
		p.persistErrors.Add(1)
		p.logger.Error("读取 Top 统计失败", "error", err)

		return []TopItem{}
	}
	defer func() { _ = rows.Close() }()

	items := make([]TopItem, 0, limit)
	for rows.Next() {
		var item TopItem
		if err := rows.Scan(&item.Key, &item.Count); err != nil {
			p.logger.Error("解析 Top 统计失败", "error", err)

			return items
		}
		items = append(items, item)
	}

	return items
}

// close 停止写协程并关闭数据库。
//
// 关闭信号通过 closeOnce 单独发布，不等待正在阻塞的入队者：入队用 select 监听
// done，会立即返回，因此关闭不会与反压互相等待。
func (p *persister) close(ctx context.Context) error {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		close(p.done)
	})

	finished := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(finished)
	}()

	if ctx == nil {
		<-finished

		return nil
	}

	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
