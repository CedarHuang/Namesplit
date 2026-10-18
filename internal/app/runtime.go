package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CedarHuang/Namesplit/internal/config"
	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/dnskernel"
	"github.com/CedarHuang/Namesplit/internal/localrecords"
	"github.com/CedarHuang/Namesplit/internal/rules"
	"github.com/CedarHuang/Namesplit/internal/upstream"
)

// snapshot 是一次配置应用的运行时视图。
type snapshot struct {
	cfg    *config.Config
	rules  *rules.Table
	local  *localrecords.Table
	groups *upstream.Manager

	ownsGroups atomic.Bool
	refs       atomic.Int64
	retired    atomic.Bool
	closeOnce  sync.Once
}

func (s *snapshot) Rules() *rules.Table        { return s.rules }
func (s *snapshot) Local() *localrecords.Table { return s.local }
func (s *snapshot) Groups() *upstream.Manager  { return s.groups }
func (s *snapshot) DefaultGroup() string       { return s.cfg.Settings.DefaultGroup }

// close 释放快照拥有的上游组。
func (s *snapshot) close() {
	s.closeOnce.Do(func() {
		if s.ownsGroups.Load() && s.groups != nil {
			_ = s.groups.Close()
		}
	})
}

// runtimeStore 持有当前快照；在途查询通过引用计数延迟释放旧快照。
type runtimeStore struct {
	mu  sync.Mutex
	cur *snapshot
}

// Acquire 实现 dnskernel.Store。
func (s *runtimeStore) Acquire() (dnskernel.View, func()) {
	s.mu.Lock()
	snap := s.cur
	if snap != nil {
		snap.refs.Add(1)
	}
	s.mu.Unlock()

	release := func() {
		if snap == nil {
			return
		}

		if snap.refs.Add(-1) == 0 && snap.retired.Load() {
			snap.close()
		}
	}

	return snap, release
}

// current 返回当前快照。
func (s *runtimeStore) current() *snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cur
}

// swap 安装新快照并返回旧快照。
func (s *runtimeStore) swap(next *snapshot) (old *snapshot) {
	s.mu.Lock()
	old = s.cur
	s.cur = next
	s.mu.Unlock()

	if old != nil {
		old.retired.Store(true)
		if old.refs.Load() == 0 {
			old.close()
		}
	}

	return old
}

// buildSnapshot 根据配置构建运行时视图；第二个返回值表示是否复用旧上游组。
func (a *App) buildSnapshot(cfg *config.Config) (*snapshot, bool, error) {
	ruleInputs := make([]rules.Rule, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		ruleInputs = append(ruleInputs, rules.Rule{
			Kind:   rules.Kind(rule.Match),
			Domain: rule.Domain,
			Group:  rule.Group,
		})
	}
	rulesTable, ruleDiags := rules.Compile(ruleInputs)
	if diagnostic.HasError(ruleDiags) {
		return nil, false, fmt.Errorf("规则编译失败")
	}

	recordInputs := make([]localrecords.Record, 0, len(cfg.LocalRecords))
	for _, record := range cfg.LocalRecords {
		recordInputs = append(recordInputs, localrecords.Record{
			Type:    localrecords.Type(record.Type),
			Name:    record.Name,
			Value:   record.Value,
			TTL:     record.TTL,
			Enabled: record.Enabled,
		})
	}
	localTable, recordDiags := localrecords.Compile(recordInputs, cfg.Settings.PtrAuto)
	if diagnostic.HasError(recordDiags) {
		return nil, false, fmt.Errorf("本地记录编译失败")
	}

	old := a.store.current()
	reuseGroups := old != nil &&
		upstreamsSemanticallyEqual(old.cfg.Upstreams, cfg.Upstreams) &&
		groupsSemanticallyEqual(old.cfg.Groups, cfg.Groups)

	var groups *upstream.Manager
	if reuseGroups {
		groups = old.groups
	} else {
		manager, err := buildUpstreamManager(cfg, a.logger)
		if err != nil {
			return nil, false, err
		}
		groups = manager
	}

	snap := &snapshot{
		cfg:    cfg.Clone(),
		rules:  rulesTable,
		local:  localTable,
		groups: groups,
	}
	snap.ownsGroups.Store(!reuseGroups)

	if reuseGroups && groups != nil && localRecordsChanged(old.cfg, cfg) {
		groups.ClearCaches()
	}

	return snap, reuseGroups, nil
}

// upstreamsSemanticallyEqual 比较两份上游配置；bootstrap 的 nil 与空切片等价。
func upstreamsSemanticallyEqual(left, right map[string]config.Upstream) bool {
	if len(left) != len(right) {
		return false
	}

	for id, item := range left {
		other, ok := right[id]
		if !ok ||
			item.Name != other.Name ||
			item.URL != other.URL ||
			item.TimeoutMS != other.TimeoutMS ||
			item.Remark != other.Remark ||
			!slices.Equal(item.Bootstrap, other.Bootstrap) {
			return false
		}
	}

	return true
}

// groupsSemanticallyEqual 比较两份组配置；成员切片的 nil 与空切片等价。
func groupsSemanticallyEqual(left, right map[string]config.Group) bool {
	if len(left) != len(right) {
		return false
	}

	for id, group := range left {
		other, ok := right[id]
		if !ok || group.Name != other.Name || !slices.Equal(group.Upstreams, other.Upstreams) {
			return false
		}
	}

	return true
}

// localRecordsChanged 判断本地记录或其编译开关是否变化。
//
// 本地记录不影响上游组本身，复用上游组时只需清空其缓存，避免新旧记录交替
// 期间返回过期答案。
func localRecordsChanged(left, right *config.Config) bool {
	return !slices.Equal(left.LocalRecords, right.LocalRecords) ||
		left.Settings.PtrAuto != right.Settings.PtrAuto
}

// buildUpstreamManager 根据配置构建上游组。
func buildUpstreamManager(cfg *config.Config, logger *slog.Logger) (*upstream.Manager, error) {
	specs := make(map[string]upstream.Spec, len(cfg.Upstreams))
	for id, item := range cfg.Upstreams {
		specs[id] = upstream.Spec{
			ID:        id,
			Name:      item.Name,
			URL:       item.URL,
			Bootstrap: append([]string(nil), item.Bootstrap...),
			Timeout:   time.Duration(item.TimeoutMS) * time.Millisecond,
		}
	}

	groups := make(map[string][]string, len(cfg.Groups))
	for id, group := range cfg.Groups {
		groups[id] = group.Upstreams
	}

	return upstream.NewManager(specs, groups, logger)
}

// pendingRuntime 是配置管理器在写盘前构建、写盘后提交的运行时。
type pendingRuntime struct {
	app   *App
	snap  *snapshot
	reuse bool
}

// Commit 切换运行时。
func (p *pendingRuntime) Commit() {
	p.app.installSnapshot(p.snap, p.reuse)
}

// Close 释放未提交的运行时。
func (p *pendingRuntime) Close() error {
	if !p.reuse && p.snap != nil {
		p.snap.ownsGroups.Store(true)
		p.snap.close()
	}

	return nil
}

// installSnapshot 安装新快照并应用日志设置。
func (a *App) installSnapshot(snap *snapshot, reuseGroups bool) {
	if reuseGroups {
		if old := a.store.current(); old != nil {
			old.ownsGroups.Store(false)
		}
		snap.ownsGroups.Store(true)
	}

	a.store.swap(snap)

	if a.logs != nil {
		if err := a.logs.Reconfigure(
			context.Background(),
			snap.cfg.Settings.LogRingSize,
			snap.cfg.Settings.LogPersist,
		); err != nil {
			a.logger.Error("应用日志设置失败", "error", err)
		}
	}
}
