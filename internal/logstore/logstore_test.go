package logstore

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CedarHuang/Namesplit/internal/event"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func testEvent(id uint64, when time.Time, domain, source string, rcode int) event.Event {
	return event.Event{
		EventID:      id,
		Time:         when,
		Domain:       domain,
		QType:        "A",
		Rule:         "",
		RuleIndex:    -1,
		RuleKind:     "default",
		Source:       source,
		Group:        "default",
		Upstream:     "udp://1.1.1.1:53",
		UpstreamName: "Cloudflare",
		DurationMS:   1.5,
		Rcode:        rcode,
		RcodeName:    event.RcodeName(rcode),
		Protocol:     "udp",
		Answers:      []string{},
		Authority:    []string{},
		Additional:   []string{},
		CacheHit:     source == "cache",
	}
}

func TestIngestCountersAndSnapshot(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir(), Options{RingSize: MinRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	now := time.Now()
	store.Ingest(testEvent(1, now, "a.test.", "upstream", 0))
	store.Ingest(testEvent(2, now, "b.test.", "cache", 0))
	store.Ingest(testEvent(3, now, "c.test.", "upstream", 2))

	stats := store.Stats()
	if stats.Counters.Queries != 3 || stats.Counters.CacheHits != 1 || stats.Counters.Failures != 1 {
		t.Fatalf("计数器 = %+v", stats.Counters)
	}
	if stats.PersistenceEnabled {
		t.Fatal("未开启持久化时代码应为 false")
	}

	snapshot, updates, cancel := store.Subscribe(context.Background())
	defer cancel()

	if len(snapshot) != 3 {
		t.Fatalf("快照数量 = %d", len(snapshot))
	}
	if snapshot[0].EventID != 1 || snapshot[2].EventID != 3 {
		t.Fatalf("快照顺序错误: %+v", snapshot)
	}

	store.Ingest(testEvent(4, now, "d.test.", "upstream", 0))

	select {
	case ev := <-updates:
		if ev.EventID != 4 {
			t.Fatalf("实时事件 = %d", ev.EventID)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到实时事件")
	}
}

func TestRingResizeKeepsNewest(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir(), Options{RingSize: MaxRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	now := time.Now()
	for i := 0; i < MaxRingSize; i++ {
		store.Ingest(testEvent(uint64(i+1), now, "a.test.", "upstream", 0))
	}

	if err := store.Reconfigure(context.Background(), MinRingSize, false); err != nil {
		t.Fatalf("调整环形缓冲: %v", err)
	}

	snapshot, _, cancel := store.Subscribe(context.Background())
	defer cancel()

	if len(snapshot) != MinRingSize {
		t.Fatalf("快照数量 = %d", len(snapshot))
	}
	if snapshot[0].EventID != uint64(MaxRingSize-MinRingSize+1) {
		t.Fatalf("最旧事件 = %d", snapshot[0].EventID)
	}
}

func TestSlowSubscriberDropsAreCounted(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir(), Options{RingSize: MinRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	_, _, cancel := store.Subscribe(context.Background())
	defer cancel()

	now := time.Now()
	for i := 0; i < defaultSubscriberBuffer+50; i++ {
		store.Ingest(testEvent(uint64(i+1), now, "a.test.", "upstream", 0))
	}

	if got := store.Stats().Counters.DroppedStream; got == 0 {
		t.Fatal("慢消费者应有丢弃计数")
	}
}

func TestPersistenceAndHistory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}

	base := time.Date(2026, 10, 18, 8, 12, 0, 0, time.UTC)
	store.Ingest(testEvent(1, base, "one.example.", "upstream", 0))
	store.Ingest(testEvent(2, base.Add(time.Second), "two.example.", "cache", 0))
	store.Ingest(testEvent(3, base.Add(2*time.Second), "three.example.", "upstream", 2))

	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("关闭存储: %v", err)
	}

	reopened, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("重新打开存储: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })

	page, err := reopened.Queries(context.Background(), Filter{Limit: 10})
	if err != nil {
		t.Fatalf("查询历史: %v", err)
	}
	if len(page.Items) != 3 || !page.PersistenceEnabled {
		t.Fatalf("历史页 = %+v", page)
	}
	if page.Items[0].ID <= page.Items[1].ID {
		t.Fatalf("历史应按 id 降序: %+v", page.Items)
	}
	if page.Items[0].Domain != "three.example." {
		t.Fatalf("最新记录 = %q", page.Items[0].Domain)
	}

	filtered, err := reopened.Queries(context.Background(), Filter{Limit: 10, Domain: "EXAMPLE"})
	if err != nil {
		t.Fatalf("过滤查询: %v", err)
	}
	if len(filtered.Items) != 3 {
		t.Fatalf("域名子串过滤命中 %d 条", len(filtered.Items))
	}

	first, err := reopened.Queries(context.Background(), Filter{Limit: 2})
	if err != nil {
		t.Fatalf("分页查询: %v", err)
	}
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("第一页 = %+v", first)
	}

	second, err := reopened.Queries(context.Background(), Filter{Limit: 2, Cursor: *first.NextCursor})
	if err != nil {
		t.Fatalf("第二页查询: %v", err)
	}
	if len(second.Items) != 1 || second.NextCursor != nil {
		t.Fatalf("第二页 = %+v", second)
	}

	deleted, err := reopened.ClearQueries(context.Background())
	if err != nil || deleted != 3 {
		t.Fatalf("清空 = %d, %v", deleted, err)
	}

	empty, err := reopened.Queries(context.Background(), Filter{Limit: 10})
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("清空后查询 = %+v, %v", empty, err)
	}
}

func TestPersistTogglesAtRuntime(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir(), Options{RingSize: MinRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	if err := store.Reconfigure(context.Background(), MinRingSize, true); err != nil {
		t.Fatalf("开启持久化: %v", err)
	}
	if !store.PersistenceEnabled() {
		t.Fatal("持久化未开启")
	}

	store.Ingest(testEvent(1, time.Now(), "a.test.", "upstream", 0))

	waitFor(t, 3*time.Second, func() bool {
		page, err := store.Queries(context.Background(), Filter{Limit: 10})
		return err == nil && len(page.Items) == 1
	})

	if err := store.Reconfigure(context.Background(), MinRingSize, false); err != nil {
		t.Fatalf("关闭持久化: %v", err)
	}
	page, err := store.Queries(context.Background(), Filter{Limit: 10})
	if err != nil {
		t.Fatalf("关闭后查询: %v", err)
	}
	if page.PersistenceEnabled || len(page.Items) != 0 {
		t.Fatalf("关闭持久化后应返回空页: %+v", page)
	}
}

func TestMinuteAggregation(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir(), Options{RingSize: MinRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	now := time.Now()
	store.Ingest(testEvent(1, now, "a.test.", "upstream", 0))
	store.Ingest(testEvent(2, now, "b.test.", "cache", 0))

	stats, persistent := store.Minute(now.Add(-time.Hour), now.Add(time.Hour))
	if persistent {
		t.Fatal("未开启持久化时不应跨重启")
	}
	if len(stats) != 1 {
		t.Fatalf("分钟聚合数量 = %d", len(stats))
	}
	if stats[0].Queries != 2 || stats[0].CacheHits != 1 || stats[0].Failures != 0 {
		t.Fatalf("分钟聚合 = %+v", stats[0])
	}
}

func TestTopFromMemory(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir(), Options{RingSize: MinRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	now := time.Now()
	store.Ingest(testEvent(1, now, "a.test.", "upstream", 0))
	store.Ingest(testEvent(2, now, "a.test.", "upstream", 0))
	store.Ingest(testEvent(3, now, "b.test.", "upstream", 0))

	items, persistent := store.Top("domain", now.Add(-time.Minute), 10)
	if persistent {
		t.Fatal("未开启持久化时 Top 不应跨重启")
	}
	if len(items) != 2 || items[0].Key != "a.test." || items[0].Count != 2 {
		t.Fatalf("Top = %+v", items)
	}
}

func TestHighLoad(t *testing.T) {
	if os.Getenv("NAMESPLIT_LOADTEST") == "" {
		t.Skip("设置 NAMESPLIT_LOADTEST=1 运行 1,000 QPS ×5min 压测")
	}

	dir := t.TempDir()
	store, err := Open(dir, Options{RingSize: DefaultRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}

	const (
		qps      = 1000
		duration = 5 * time.Minute
	)
	events := qps * int(duration/time.Second)

	start := time.Now()
	for i := 0; i < events; i++ {
		store.Ingest(testEvent(uint64(i+1), time.Now(), "load.test.", "upstream", 0))
		if i%qps == 0 {
			next := start.Add(time.Duration(i/qps+1) * time.Second)
			time.Sleep(time.Until(next))
		}
	}

	beforeClose := store.Stats()
	if beforeClose.Counters.Queries != uint64(events) {
		t.Fatalf("查询计数 = %d, 期望 %d", beforeClose.Counters.Queries, events)
	}
	if beforeClose.Counters.PersistErrors != 0 {
		t.Fatalf("持久化错误 = %d", beforeClose.Counters.PersistErrors)
	}

	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("关闭存储: %v", err)
	}

	reopened, err := Open(dir, Options{RingSize: DefaultRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("重新打开存储: %v", err)
	}
	defer func() { _ = reopened.Close(context.Background()) }()

	page, err := reopened.Queries(context.Background(), Filter{Limit: 500})
	if err != nil || len(page.Items) != 500 {
		t.Fatalf("持久化数据读取失败: %d 条, %v", len(page.Items), err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("等待条件超时")
}

// 写入失败必须重试到成功，事件不能在运行期丢失。
func TestPersistenceRetriesWithoutDroppingEvents(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	p := store.currentPersister()
	if p == nil {
		t.Fatal("持久化未开启")
	}

	// 连接级只读会稳定地让写入失败，模拟磁盘异常。
	if _, err = p.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatalf("设置只读: %v", err)
	}

	now := time.Now()
	const events = 10
	for i := 0; i < events; i++ {
		store.Ingest(testEvent(uint64(i+1), now, "retry.test.", "upstream", 0))
	}

	waitFor(t, 10*time.Second, func() bool {
		return store.Stats().Counters.PersistErrors > 0
	})

	if _, err = p.db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatalf("恢复可写: %v", err)
	}

	waitFor(t, 15*time.Second, func() bool {
		page, queryErr := store.Queries(context.Background(), Filter{Limit: 100})

		return queryErr == nil && len(page.Items) == events
	})
}

// 字节上限以物理文件体积为判据：删除行后必须回收空闲页，否则文件体积不会下降。
func TestEnforceLimitsShrinksDatabase(t *testing.T) {
	const (
		maxBytes = 256 << 10
		batch    = 20
		batches  = 30
	)

	dir := t.TempDir()
	persistErrors := &atomic.Uint64{}
	p, err := newPersister(dir, discardLogger(), persistErrors, &stubMinutes{}, limits{
		maxRows:  MaxRows,
		maxBytes: maxBytes,
	})
	if err != nil {
		t.Fatalf("打开持久化: %v", err)
	}
	defer func() { _ = p.close(context.Background()) }()

	payload := strings.Repeat("x", 4<<10)
	var lastID uint64
	for i := 0; i < batches; i++ {
		events := make([]event.Event, 0, batch)
		for j := 0; j < batch; j++ {
			lastID++
			ev := testEvent(lastID, time.Now(), "limit.test.", "upstream", 0)
			ev.Answers = []string{payload}
			events = append(events, ev)
		}
		p.writeBatch(events)
	}

	if total := p.totalBytes(); total > maxBytes {
		t.Fatalf("数据库体积 = %d, 超过上限 %d", total, maxBytes)
	}
	if p.rowCount.Load() == 0 {
		t.Fatal("滚动删除不应删空全部记录")
	}

	page, err := p.query(context.Background(), Filter{Limit: 1})
	if err != nil {
		t.Fatalf("读取最新记录: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].EventID != lastID {
		t.Fatalf("滚动删除应保留最新记录，实际 %+v", page.Items)
	}
}

// 清空查询记录也必须归还磁盘空间：DELETE 之后要执行 incremental_vacuum。
func TestClearQueriesReclaimsSpace(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	const events = 300
	payload := strings.Repeat("x", 4<<10)
	now := time.Now()
	for i := 0; i < events; i++ {
		ev := testEvent(uint64(i+1), now, "clear.test.", "upstream", 0)
		ev.Answers = []string{payload}
		store.Ingest(ev)
	}

	waitFor(t, 15*time.Second, func() bool {
		page, queryErr := store.Queries(context.Background(), Filter{Limit: 500})

		return queryErr == nil && len(page.Items) == events
	})

	dbPath := filepath.Join(dir, "querylog.db")
	totalBytes := func() (sum int64) {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if info, statErr := os.Stat(dbPath + suffix); statErr == nil {
				sum += info.Size()
			}
		}

		return sum
	}

	before := totalBytes()
	deleted, err := store.ClearQueries(context.Background())
	if err != nil {
		t.Fatalf("清空查询记录: %v", err)
	}
	if deleted != events {
		t.Fatalf("删除行数 = %d, 期望 %d", deleted, events)
	}

	if after := totalBytes(); after > before/2 {
		t.Fatalf("清空后体积 %d 未回收（清空前 %d）", after, before)
	}
}

// qps 窗口必须覆盖最近 1 秒。
func TestQPSWindowSliding(t *testing.T) {
	w := newQPSWindow()
	for slot := int64(100); slot < 110; slot++ {
		for i := 0; i < 100; i++ {
			w.observeAt(slot)
		}
	}

	if got := w.rateAt(109); got != 1000 {
		t.Fatalf("1 秒窗口速率 = %v, 期望 1000", got)
	}
	if got := w.rateAt(110); got != 900 {
		t.Fatalf("滑出最旧桶后的速率 = %v, 期望 900", got)
	}
	if got := w.rateAt(118); got != 100 {
		t.Fatalf("只剩最新桶时的速率 = %v, 期望 100", got)
	}
	if got := w.rateAt(120); got != 0 {
		t.Fatalf("超过 1 秒后的速率 = %v, 期望 0", got)
	}
}

// 分钟聚合的落盘值按增量累加，跨重启不能覆盖已有数据。
func TestMinuteAggregationAccumulatesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	when := time.Now().UTC().Truncate(time.Minute)

	first, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	first.Ingest(testEvent(1, when, "minute.test.", "upstream", 0))
	first.Ingest(testEvent(2, when, "minute.test.", "upstream", 0))
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("关闭存储: %v", err)
	}

	second, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("重新打开存储: %v", err)
	}
	second.Ingest(testEvent(3, when, "minute.test.", "upstream", 0))
	if err := second.Close(context.Background()); err != nil {
		t.Fatalf("关闭存储: %v", err)
	}

	third, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("再次打开存储: %v", err)
	}
	defer func() { _ = third.Close(context.Background()) }()

	stats, persistent := third.Minute(when, when.Add(time.Minute))
	if !persistent {
		t.Fatal("持久化应处于开启状态")
	}
	if len(stats) != 1 || stats[0].Queries != 3 {
		t.Fatalf("分钟聚合 = %+v, 期望 queries=3", stats)
	}
}

// 重新开启持久化时，关闭期间累积的数据不应补写。
func TestMinutePersistenceDoesNotBackfillAfterEnable(t *testing.T) {
	dir := t.TempDir()
	when := time.Now().UTC().Truncate(time.Minute)

	store, err := Open(dir, Options{RingSize: MinRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	for i := 0; i < 3; i++ {
		store.Ingest(testEvent(uint64(i+1), when, "backfill.test.", "upstream", 0))
	}
	if err = store.Reconfigure(context.Background(), MinRingSize, true); err != nil {
		t.Fatalf("开启持久化: %v", err)
	}

	// 等至少一个落盘周期，确认关闭期间的数据未被补写。
	time.Sleep(1200 * time.Millisecond)
	if stats, _ := store.Minute(when, when.Add(time.Minute)); len(stats) != 0 {
		t.Fatalf("关闭持久化期间的数据不应补录: %+v", stats)
	}

	store.Ingest(testEvent(4, when, "backfill.test.", "upstream", 0))
	store.Ingest(testEvent(5, when, "backfill.test.", "upstream", 0))
	waitFor(t, 5*time.Second, func() bool {
		stats, _ := store.Minute(when, when.Add(time.Minute))

		return len(stats) == 1 && stats[0].Queries == 2
	})
}

// 内存分钟聚合只保留 24 小时。
func TestMinuteRetentionPrunesMemory(t *testing.T) {
	store, err := Open(t.TempDir(), Options{RingSize: MinRingSize}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	now := time.Now().UTC().Truncate(time.Minute)
	stale := now.Add(-25 * time.Hour)

	store.minuteMu.Lock()
	for i := 0; i < maxMinuteBuckets+2; i++ {
		minute := now.Add(-time.Duration(i) * time.Minute)
		store.minutes[minute] = &minuteBucket{queries: 1}
		store.dirty[minute] = struct{}{}
	}
	store.minutes[stale] = &minuteBucket{queries: 1}
	store.dirty[stale] = struct{}{}
	store.pruneMinutesLocked(now)
	store.minuteMu.Unlock()

	if _, ok := store.minutes[stale]; ok {
		t.Fatal("超过 24 小时的分钟聚合应被清理")
	}
	if _, ok := store.minutes[now]; !ok {
		t.Fatal("保留期内的分钟聚合不应被清理")
	}
}

// 数据库中的分钟聚合同样只保留 24 小时。
func TestMinuteRetentionPrunesDatabase(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	p := store.currentPersister()
	stale := time.Now().UTC().Add(-25 * time.Hour).Truncate(time.Minute)
	if _, err = p.db.Exec(
		`INSERT INTO minute_stats (minute, queries, failures, cache_hits, latency_avg_ms, latency_max_ms)
		VALUES (?, 1, 0, 0, 0, 0)`,
		stale.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("写入过期分钟聚合: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		var count int
		if queryErr := p.db.QueryRow(
			`SELECT COUNT(*) FROM minute_stats WHERE minute = ?`,
			stale.Format(time.RFC3339),
		).Scan(&count); queryErr != nil {
			return false
		}

		return count == 0
	})
}

// stubMinutes 是 minuteSource 的测试替身。
type stubMinutes struct {
	stats []MinuteStat
}

func (s *stubMinutes) takeDirtyMinutes() []MinuteStat {
	stats := s.stats
	s.stats = nil

	return stats
}

func (s *stubMinutes) restoreDirtyMinutes(stats []MinuteStat) {
	s.stats = append(s.stats, stats...)
}

func (s *stubMinutes) minuteCumulative() []MinuteStat {
	return nil
}

// 旧数据库（auto_vacuum=NONE）必须在打开时迁移为增量回收，否则删除不会归还磁盘空间。
func TestOpenMigratesLegacyAutoVacuum(t *testing.T) {
	dir := t.TempDir()

	store, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	store.Ingest(testEvent(1, time.Now(), "legacy.test.", "upstream", 0))
	if err = store.Close(context.Background()); err != nil {
		t.Fatalf("关闭存储: %v", err)
	}

	// 把数据库改回 NONE，模拟迁移前的库。
	raw, err := sql.Open("sqlite", filepath.Join(dir, "querylog.db"))
	if err != nil {
		t.Fatalf("打开原始数据库: %v", err)
	}
	raw.SetMaxOpenConns(1)
	for _, statement := range []string{`PRAGMA auto_vacuum=NONE`, `VACUUM`} {
		if _, err = raw.Exec(statement); err != nil {
			t.Fatalf("构造旧库状态: %v", err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatalf("关闭原始数据库: %v", err)
	}

	migrated, err := Open(dir, Options{RingSize: MinRingSize, Persist: true}, discardLogger())
	if err != nil {
		t.Fatalf("重新打开存储: %v", err)
	}
	defer func() { _ = migrated.Close(context.Background()) }()

	mode, err := autoVacuumMode(migrated.currentPersister().db)
	if err != nil {
		t.Fatalf("读取 auto_vacuum: %v", err)
	}
	if mode != incrementalAutoVacuum {
		t.Fatalf("auto_vacuum = %d, 期望 %d", mode, incrementalAutoVacuum)
	}
}
