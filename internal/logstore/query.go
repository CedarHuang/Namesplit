package logstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/CedarHuang/Namesplit/internal/event"
)

// Filter 是历史查询的过滤条件。
type Filter struct {
	// Limit 为返回条数，1–500，默认 100。
	Limit int
	// Cursor 是上一页最后一条记录的 ID（十进制）；空表示第一页。
	Cursor string
	// Domain 是域名子串（大小写不敏感）。
	Domain string
	// QType、Source、Group、Rule、Upstream 为精确匹配。
	QType    string
	Source   string
	Group    string
	Rule     string
	Upstream string
	// Rcode 为精确匹配的 RCODE。
	Rcode *int
	// MinDurationMS/MaxDurationMS 为耗时区间（闭区间）。
	MinDurationMS *float64
	MaxDurationMS *float64
	// From 包含、To 不包含。
	From *time.Time
	To   *time.Time
}

// HistoryItem 是一条历史查询。
type HistoryItem struct {
	ID int64 `json:"id"`
	event.Event
}

// Page 是历史查询的一页。
type Page struct {
	Items              []HistoryItem `json:"items"`
	NextCursor         *string       `json:"next_cursor"`
	PersistenceEnabled bool          `json:"persistence_enabled"`
}

// MinuteStat 是一分钟的聚合值。
type MinuteStat struct {
	Minute       string  `json:"minute"`
	Queries      int64   `json:"queries"`
	Failures     int64   `json:"failures"`
	CacheHits    int64   `json:"cache_hits"`
	LatencyAvgMS float64 `json:"latency_avg_ms"`
	LatencyMaxMS float64 `json:"latency_max_ms"`
}

// TopItem 是 Top 统计的一项。
type TopItem struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// Queries 返回历史查询。持久化关闭时返回空页。
func (s *Store) Queries(ctx context.Context, filter Filter) (Page, error) {
	filter, err := normalizeFilter(filter)
	if err != nil {
		return Page{}, err
	}

	s.persistMu.RLock()
	defer s.persistMu.RUnlock()

	if s.persister == nil {
		return Page{Items: []HistoryItem{}, PersistenceEnabled: false}, nil
	}

	return s.persister.query(ctx, filter)
}

// ClearQueries 清空持久化查询记录，返回删除条数。
func (s *Store) ClearQueries(ctx context.Context) (int64, error) {
	s.persistMu.RLock()
	defer s.persistMu.RUnlock()

	if s.persister == nil {
		return 0, nil
	}

	return s.persister.clear(ctx)
}

// Minute 返回分钟聚合。第二个返回值表示数据是否跨重启保留。
func (s *Store) Minute(from, to time.Time) ([]MinuteStat, bool) {
	from, to = normalizeRange(from, to)

	s.persistMu.RLock()
	defer s.persistMu.RUnlock()

	if s.persister == nil {
		return s.minuteFromMemory(from, to), false
	}

	return s.persister.minute(from, to), true
}

// Top 返回 Top 域名或上游。by 取值 domain/upstream。
func (s *Store) Top(by string, since time.Time, limit int) ([]TopItem, bool) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}

	s.persistMu.RLock()
	defer s.persistMu.RUnlock()

	if s.persister != nil {
		return s.persister.top(by, since, limit), true
	}

	return s.topFromMemory(by, since, limit), false
}

// topFromMemory 在持久化关闭时用环形缓冲计算 Top。
func (s *Store) topFromMemory(by string, since time.Time, limit int) []TopItem {
	s.streamMu.Lock()
	events := s.ring.snapshot()
	s.streamMu.Unlock()

	counts := make(map[string]int64)
	for _, ev := range events {
		if ev.Time.Before(since) {
			continue
		}

		var key string
		switch by {
		case "domain":
			key = ev.Domain
		case "upstream":
			key = ev.Upstream
		default:
			return []TopItem{}
		}
		if key == "" {
			continue
		}
		counts[key]++
	}

	return sortedTop(counts, limit)
}

// sortedTop 把计数转换为按数量降序、键升序的列表。
func sortedTop(counts map[string]int64, limit int) []TopItem {
	items := make([]TopItem, 0, len(counts))
	for key, count := range counts {
		items = append(items, TopItem{Key: key, Count: count})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}

		return items[i].Key < items[j].Key
	})

	if len(items) > limit {
		items = items[:limit]
	}

	return items
}

// sortMinuteStats 按分钟升序排序。
func sortMinuteStats(stats []MinuteStat) {
	sort.Slice(stats, func(i, j int) bool { return stats[i].Minute < stats[j].Minute })
}

// normalizeFilter 校验并归一历史查询参数。
func normalizeFilter(filter Filter) (Filter, error) {
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 1 || filter.Limit > 500 {
		return Filter{}, fmt.Errorf("limit %d 不在 1–500", filter.Limit)
	}

	return filter, nil
}

// normalizeRange 为分钟聚合填充默认时间窗。
func normalizeRange(from, to time.Time) (time.Time, time.Time) {
	now := time.Now()
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}

	return from, to
}
