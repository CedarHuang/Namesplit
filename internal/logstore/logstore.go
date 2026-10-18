// Package logstore 保存逐查询事件：内存环形缓冲、SQLite 持久化、计数器、
// 分钟聚合与历史查询。
//
// 事件先更新内存状态再进入持久化队列；持久化不丢弃事件，队列满时对调用方
// 形成反压；实时订阅者消费过慢时丢弃并计入 dropped_stream。
package logstore

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/event"
)

const (
	// DefaultRingSize 是内存环形缓冲的默认容量。
	DefaultRingSize = 5000
	// MinRingSize 是环形缓冲的最小容量。
	MinRingSize = 1000
	// MaxRingSize 是环形缓冲的最大容量。
	MaxRingSize = 20000

	defaultSubscriberBuffer = 256
	defaultQPSBuckets       = 20
	qpsBucketWidth          = 100 * time.Millisecond

	// qpsWindowSlots 是 1 秒速率窗口包含的桶数。
	qpsWindowSlots = int(time.Second / qpsBucketWidth)

	// minuteRetention 是分钟聚合的保留时长。
	minuteRetention = 24 * time.Hour
	// maxMinuteBuckets 是保留期内可能出现的分钟数。
	maxMinuteBuckets = int(minuteRetention / time.Minute)
)

// Options 是日志存储的容量与持久化设置。
type Options struct {
	RingSize int
	Persist  bool
}

// Counters 是本次运行的进程计数器。
type Counters struct {
	Queries       uint64 `json:"queries"`
	Failures      uint64 `json:"failures"`
	CacheHits     uint64 `json:"cache_hits"`
	DroppedStream uint64 `json:"dropped_stream"`
	PersistErrors uint64 `json:"persist_errors"`
}

// Stats 是 GET /api/stats 的响应载荷。
type Stats struct {
	UptimeMS           int64    `json:"uptime_ms"`
	Counters           Counters `json:"counters"`
	QPS1s              float64  `json:"qps_1s"`
	PersistenceEnabled bool     `json:"persistence_enabled"`
}

// Store 保存事件与统计数据。
type Store struct {
	dir       string
	logger    *slog.Logger
	startedAt time.Time

	streamMu sync.Mutex
	ring     *ringBuffer
	subs     map[uint64]*subscriber
	nextSub  uint64

	queries       atomic.Uint64
	failures      atomic.Uint64
	cacheHits     atomic.Uint64
	droppedStream atomic.Uint64
	persistErrors atomic.Uint64

	qps *qpsWindow

	minuteMu sync.Mutex
	minutes  map[time.Time]*minuteBucket
	dirty    map[time.Time]struct{}

	persistMu sync.RWMutex
	persister *persister

	closed atomic.Bool
}

// Open 打开日志存储。dir 为数据目录；持久化开启时在其中创建 querylog.db。
func Open(dir string, opts Options, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}

	ringSize, err := normalizeRingSize(opts.RingSize)
	if err != nil {
		return nil, err
	}

	store := &Store{
		dir:       dir,
		logger:    logger,
		startedAt: time.Now(),
		ring:      newRingBuffer(ringSize),
		subs:      make(map[uint64]*subscriber),
		qps:       newQPSWindow(),
		minutes:   make(map[time.Time]*minuteBucket),
		dirty:     make(map[time.Time]struct{}),
	}

	if opts.Persist {
		p, err := newPersister(dir, logger, &store.persistErrors, store, defaultLimits())
		if err != nil {
			return nil, err
		}
		store.persister = p
	}

	return store, nil
}

// Ingest 记录一条已完成的查询事件。
func (s *Store) Ingest(ev event.Event) {
	if s.closed.Load() {
		return
	}

	s.queries.Add(1)
	if ev.Source == "cache" {
		s.cacheHits.Add(1)
	}
	if ev.Rcode == dns.RcodeServerFailure {
		s.failures.Add(1)
	}
	s.qps.observe()
	s.recordMinute(ev)

	s.streamMu.Lock()
	s.ring.add(ev)
	dropped := false
	for _, sub := range s.subs {
		if !sub.send(ev) {
			dropped = true
		}
	}
	s.streamMu.Unlock()

	if dropped {
		s.droppedStream.Add(1)
	}

	// 只在取指针期间持有读锁：enqueue 可能因队列满而阻塞，不能让它拖住
	// Reconfigure/Close 需要的写锁。
	if p := s.currentPersister(); p != nil {
		p.enqueue(ev)
	}
}

// currentPersister 返回当前持久化实例；未开启时返回 nil。
func (s *Store) currentPersister() *persister {
	s.persistMu.RLock()
	defer s.persistMu.RUnlock()

	return s.persister
}

// Subscribe 返回订阅时的环形缓冲快照与后续事件流。
//
// 快照与订阅在同一临界区完成，因此快照与实时流之间不会重复也不会丢事件。
func (s *Store) Subscribe(ctx context.Context) (snapshot []event.Event, updates <-chan event.Event, cancel func()) {
	if s.closed.Load() {
		closedCh := make(chan event.Event)

		return nil, closedCh, func() {}
	}

	sub := newSubscriber()

	s.streamMu.Lock()
	snapshot = s.ring.snapshot()
	s.nextSub++
	id := s.nextSub
	s.subs[id] = sub
	s.streamMu.Unlock()

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			s.streamMu.Lock()
			delete(s.subs, id)
			s.streamMu.Unlock()
			sub.close()
		})
	}

	if ctx != nil {
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}

	return snapshot, sub.ch, cancel
}

// Stats 返回本次运行计数器。
func (s *Store) Stats() Stats {
	return Stats{
		UptimeMS: time.Since(s.startedAt).Milliseconds(),
		Counters: Counters{
			Queries:       s.queries.Load(),
			Failures:      s.failures.Load(),
			CacheHits:     s.cacheHits.Load(),
			DroppedStream: s.droppedStream.Load(),
			PersistErrors: s.persistErrors.Load(),
		},
		QPS1s:              s.qps.rate(),
		PersistenceEnabled: s.PersistenceEnabled(),
	}
}

// PersistenceEnabled 返回持久化当前是否开启。
func (s *Store) PersistenceEnabled() bool {
	return s.currentPersister() != nil
}

// Reconfigure 应用新的环形缓冲容量与持久化开关。
func (s *Store) Reconfigure(ctx context.Context, ringSize int, persist bool) error {
	size, err := normalizeRingSize(ringSize)
	if err != nil {
		return err
	}

	s.streamMu.Lock()
	s.ring.resize(size)
	s.streamMu.Unlock()

	s.persistMu.Lock()
	current := s.persister
	switch {
	case persist && current == nil:
		p, err := newPersister(s.dir, s.logger, &s.persistErrors, s, defaultLimits())
		if err != nil {
			s.persistMu.Unlock()

			return err
		}
		s.persister = p
		s.persistMu.Unlock()
	case !persist && current != nil:
		s.persister = nil
		s.persistMu.Unlock()

		return current.close(ctx)
	default:
		s.persistMu.Unlock()
	}

	return nil
}

// Close 停止订阅、落盘并关闭存储。
func (s *Store) Close(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	s.streamMu.Lock()
	for _, sub := range s.subs {
		sub.close()
	}
	s.subs = make(map[uint64]*subscriber)
	s.streamMu.Unlock()

	s.persistMu.Lock()
	p := s.persister
	s.persister = nil
	s.persistMu.Unlock()

	if p != nil {
		return p.close(ctx)
	}

	return nil
}

// recordMinute 更新分钟聚合并标记为待落盘。
func (s *Store) recordMinute(ev event.Event) {
	minute := ev.Time.UTC().Truncate(time.Minute)

	s.minuteMu.Lock()
	bucket := s.minutes[minute]
	if bucket == nil {
		bucket = &minuteBucket{}
		s.minutes[minute] = bucket
	}

	bucket.queries++
	if ev.Rcode == dns.RcodeServerFailure {
		bucket.failures++
	}
	if ev.Source == "cache" {
		bucket.cacheHits++
	}
	bucket.latencySumMS += ev.DurationMS
	bucket.latencyCount++
	if ev.DurationMS > bucket.latencyMaxMS {
		bucket.latencyMaxMS = ev.DurationMS
	}
	s.dirty[minute] = struct{}{}

	s.pruneMinutesLocked(minute)
	s.minuteMu.Unlock()
}

// pruneMinutesLocked 清理超过保留期的分钟聚合。
func (s *Store) pruneMinutesLocked(now time.Time) {
	if len(s.minutes) <= maxMinuteBuckets+1 {
		return
	}

	cutoff := now.Add(-minuteRetention)
	for minute := range s.minutes {
		if minute.Before(cutoff) {
			delete(s.minutes, minute)
			delete(s.dirty, minute)
		}
	}
}

// takeDirtyMinutes 取出待落盘的分钟聚合。
func (s *Store) takeDirtyMinutes() []MinuteStat {
	s.minuteMu.Lock()
	defer s.minuteMu.Unlock()

	if len(s.dirty) == 0 {
		return nil
	}

	out := make([]MinuteStat, 0, len(s.dirty))
	for minute := range s.dirty {
		bucket := s.minutes[minute]
		if bucket == nil {
			// 桶已被保留期裁剪，脏标记失去意义。
			delete(s.dirty, minute)

			continue
		}
		out = append(out, bucket.stat(minute))
		delete(s.dirty, minute)
	}

	return out
}

// restoreDirtyMinutes 把写入失败的分钟聚合重新标记为待落盘。
//
// 落盘值按相对上次落盘基线的增量计算，重试写同一增量不会重复计数。
func (s *Store) restoreDirtyMinutes(stats []MinuteStat) {
	s.minuteMu.Lock()
	defer s.minuteMu.Unlock()

	for _, stat := range stats {
		minute, err := time.Parse(time.RFC3339, stat.Minute)
		if err != nil {
			continue
		}
		if s.minutes[minute] == nil {
			continue
		}
		s.dirty[minute] = struct{}{}
	}
}

// minuteCumulative 返回内存中全部分钟聚合的累计值。
//
// 持久化开启时用它给新 persister 建立"已落盘基线"，避免把关闭持久化期间
// 累积的数据补写到磁盘。
func (s *Store) minuteCumulative() []MinuteStat {
	s.minuteMu.Lock()
	defer s.minuteMu.Unlock()

	out := make([]MinuteStat, 0, len(s.minutes))
	for minute, bucket := range s.minutes {
		out = append(out, bucket.stat(minute))
	}

	return out
}

// minuteFromMemory 返回内存中的分钟聚合。
func (s *Store) minuteFromMemory(from, to time.Time) []MinuteStat {
	s.minuteMu.Lock()
	defer s.minuteMu.Unlock()

	out := make([]MinuteStat, 0)
	for minute, bucket := range s.minutes {
		if minute.Before(from) || !minute.Before(to) {
			continue
		}
		out = append(out, bucket.stat(minute))
	}

	sortMinuteStats(out)

	return out
}

// normalizeRingSize 校验并归一环形缓冲容量。
func normalizeRingSize(size int) (int, error) {
	if size == 0 {
		return DefaultRingSize, nil
	}
	if size < MinRingSize || size > MaxRingSize {
		return 0, fmt.Errorf("环形缓冲容量 %d 不在 %d–%d", size, MinRingSize, MaxRingSize)
	}

	return size, nil
}

// ringBuffer 是定长环形缓冲。
type ringBuffer struct {
	buf   []event.Event
	start int
	count int
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{buf: make([]event.Event, size)}
}

func (r *ringBuffer) add(ev event.Event) {
	if len(r.buf) == 0 {
		return
	}

	if r.count < len(r.buf) {
		r.buf[(r.start+r.count)%len(r.buf)] = ev
		r.count++

		return
	}

	r.buf[r.start] = ev
	r.start = (r.start + 1) % len(r.buf)
}

func (r *ringBuffer) snapshot() []event.Event {
	out := make([]event.Event, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.buf[(r.start+i)%len(r.buf)])
	}

	return out
}

func (r *ringBuffer) resize(size int) {
	events := r.snapshot()
	if len(events) > size {
		events = events[len(events)-size:]
	}

	r.buf = make([]event.Event, size)
	r.start = 0
	r.count = 0
	for _, ev := range events {
		r.add(ev)
	}
}

// subscriber 是单个事件流订阅者。
type subscriber struct {
	ch     chan event.Event
	mu     sync.Mutex
	closed bool
}

func newSubscriber() *subscriber {
	return &subscriber{ch: make(chan event.Event, defaultSubscriberBuffer)}
}

// send 非阻塞发送；缓冲满时返回 false。
func (s *subscriber) send(ev event.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return true
	}

	select {
	case s.ch <- ev:
		return true
	default:
		return false
	}
}

func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
}

// qpsWindow 统计最近 1 秒内完成的查询数。
type qpsWindow struct {
	mu      sync.Mutex
	buckets [defaultQPSBuckets]uint64
	slots   [defaultQPSBuckets]int64
}

func newQPSWindow() *qpsWindow {
	return &qpsWindow{}
}

// observe 记录一次查询完成。
func (w *qpsWindow) observe() {
	w.observeAt(time.Now().UnixMilli() / qpsBucketWidth.Milliseconds())
}

// observeAt 在指定时间槽记录一次查询完成。
func (w *qpsWindow) observeAt(slot int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i := range w.slots {
		// 丢弃滑出窗口的旧槽；时钟回退时同样作废，避免把未来槽计入。
		if w.slots[i] < slot-int64(qpsWindowSlots)+1 || w.slots[i] > slot {
			w.slots[i] = -1
			w.buckets[i] = 0
		}
	}

	index := int(slot % int64(len(w.buckets)))
	if w.slots[index] != slot {
		w.slots[index] = slot
		w.buckets[index] = 0
	}
	w.buckets[index]++
}

// rate 返回最近 1 秒的查询数。
func (w *qpsWindow) rate() float64 {
	return w.rateAt(time.Now().UnixMilli() / qpsBucketWidth.Milliseconds())
}

// rateAt 返回以 current 为结束时间槽的最近 1 秒查询数。
func (w *qpsWindow) rateAt(current int64) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	var total uint64
	for i := range w.slots {
		if w.slots[i] > current-int64(qpsWindowSlots) && w.slots[i] <= current {
			total += w.buckets[i]
		}
	}

	return float64(total)
}

// minuteBucket 是单分钟的聚合值。
type minuteBucket struct {
	queries      int64
	failures     int64
	cacheHits    int64
	latencySumMS float64
	latencyMaxMS float64
	latencyCount int64
}

func (b *minuteBucket) stat(minute time.Time) MinuteStat {
	avg := 0.0
	if b.latencyCount > 0 {
		avg = b.latencySumMS / float64(b.latencyCount)
	}

	return MinuteStat{
		Minute:       minute.UTC().Format(time.RFC3339),
		Queries:      b.queries,
		Failures:     b.failures,
		CacheHits:    b.cacheHits,
		LatencyAvgMS: avg,
		LatencyMaxMS: b.latencyMaxMS,
	}
}
