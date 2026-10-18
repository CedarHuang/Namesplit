package upstream

import (
	"errors"
	"fmt"
	"log/slog"
)

// Manager 持有一组上游组，供解析链路按组 ID 取用。
type Manager struct {
	groups map[string]*Group
}

// NewManager 构建全部上游组。specs 以 ID 索引，groups 以组 ID 索引上游 ID 列表。
func NewManager(
	specs map[string]Spec,
	groups map[string][]string,
	logger *slog.Logger,
) (*Manager, error) {
	if len(groups) == 0 {
		return nil, errors.New("至少需要一个上游组")
	}
	if logger == nil {
		logger = slog.Default()
	}

	manager := &Manager{groups: make(map[string]*Group, len(groups))}

	for id, members := range groups {
		groupMembers := make([]Spec, 0, len(members))
		for _, specID := range members {
			spec, ok := specs[specID]
			if !ok {
				manager.closeAll()

				return nil, fmt.Errorf("上游组 %q 引用了不存在的上游 %q", id, specID)
			}

			groupMembers = append(groupMembers, spec)
		}

		group, err := NewGroup(id, groupMembers, logger)
		if err != nil {
			manager.closeAll()

			return nil, err
		}

		manager.groups[id] = group
	}

	return manager, nil
}

// Group 返回指定 ID 的上游组；不存在时返回 nil。
func (m *Manager) Group(id string) *Group {
	return m.groups[id]
}

// ClearCaches 清空所有组的缓存。
func (m *Manager) ClearCaches() {
	for _, group := range m.groups {
		group.ClearCache()
	}
}

// Close 关闭全部上游组。
func (m *Manager) Close() error {
	var errs []error
	for _, group := range m.groups {
		errs = append(errs, group.Close())
	}

	return errors.Join(errs...)
}

// closeAll 关闭已构建的组，用于构建失败时清理。
func (m *Manager) closeAll() {
	for _, group := range m.groups {
		_ = group.Close()
	}
}
