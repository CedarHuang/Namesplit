package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
)

// Runtime 是配置对应的运行时。Commit 在配置文件写盘成功后切换运行态；
// Close 释放尚未提交的运行时。
type Runtime interface {
	Commit()
	Close() error
}

// BuildFunc 根据新配置构建运行时。
type BuildFunc func(*Config) (Runtime, error)

// Error 是配置加载/保存失败的结构化错误。
type Error struct {
	Code            string                  `json:"code"`
	Message         string                  `json:"message"`
	Line            int                     `json:"line,omitempty"`
	Path            string                  `json:"path,omitempty"`
	Details         []diagnostic.Diagnostic `json:"details,omitempty"`
	CurrentRevision *uint64                 `json:"current_revision,omitempty"`
}

// Error 实现 error。
func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}

	return e.Code
}

// Manager 持有当前配置与 revision。
type Manager struct {
	path   string
	logger *slog.Logger

	mu       sync.Mutex
	cfg      *Config
	revision uint64
	warnings []diagnostic.Diagnostic
	loadErr  *Error
}

// Open 加载配置文件；文件缺失时创建内置默认配置。
func Open(path string, build BuildFunc, logger *slog.Logger) (*Manager, *Error) {
	if logger == nil {
		logger = slog.Default()
	}

	info, err := os.Stat(path)
	switch {
	case err == nil:
		if info.Size() > MaxFileBytes {
			return nil, &Error{
				Code:    diagnostic.CodeConfigTooLarge,
				Message: fmt.Sprintf("配置文件超过 %d 字节", MaxFileBytes),
			}
		}

		return loadExisting(path, build, logger)
	case errors.Is(err, os.ErrNotExist):
		return createDefault(path, build, logger)
	default:
		return nil, &Error{Code: diagnostic.CodeConfigReadFailed, Message: err.Error()}
	}
}

// loadExisting 加载已有配置文件。
func loadExisting(path string, build BuildFunc, logger *slog.Logger) (*Manager, *Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: diagnostic.CodeConfigReadFailed, Message: err.Error()}
	}

	cfg, diags := ParseFile(data)
	if diagnostic.HasError(diags) {
		return nil, errorFromDiagnostics(diags)
	}

	runtime, err := build(cfg)
	if err != nil {
		return nil, &Error{Code: "apply_failed", Message: err.Error()}
	}
	runtime.Commit()

	return &Manager{
		path:     path,
		logger:   logger,
		cfg:      cfg,
		revision: 1,
		warnings: diagnostic.Filter(diags, diagnostic.SeverityWarning),
	}, nil
}

// createDefault 在文件缺失时创建内置默认配置并应用。
func createDefault(path string, build BuildFunc, logger *slog.Logger) (*Manager, *Error) {
	cfg := Default()

	diags := Validate(cfg)
	if diagnostic.HasError(diags) {
		return nil, errorFromDiagnostics(diags)
	}

	data, err := MarshalTOML(cfg)
	if err != nil {
		return nil, &Error{Code: "apply_failed", Message: err.Error()}
	}
	if err := WriteAtomic(path, data); err != nil {
		return nil, &Error{Code: "config_write_failed", Message: err.Error()}
	}

	runtime, err := build(cfg)
	if err != nil {
		return nil, &Error{Code: "apply_failed", Message: err.Error()}
	}
	runtime.Commit()

	return &Manager{
		path:     path,
		logger:   logger,
		cfg:      cfg,
		revision: 1,
		warnings: diagnostic.Filter(diags, diagnostic.SeverityWarning),
	}, nil
}

// Current 返回当前配置、revision、警告与最近一次加载错误。
//
// 警告切片始终非 nil，调用方直接序列化时会得到 [] 而不是 null。
func (m *Manager) Current() (*Config, uint64, []diagnostic.Diagnostic, *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.cfg.Clone(), m.revision, append([]diagnostic.Diagnostic{}, m.warnings...), m.loadErr
}

// Validate 校验配置，不产生副作用。
func (m *Manager) Validate(cfg *Config) []diagnostic.Diagnostic {
	return Validate(cfg)
}

// Save 校验、构建、写盘并切换运行时。
func (m *Manager) Save(ctx context.Context, baseRevision uint64, cfg *Config, build BuildFunc) (uint64, []diagnostic.Diagnostic, *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.saveLocked(ctx, baseRevision, cfg, build)
}

// Reset 恢复内置默认配置。
func (m *Manager) Reset(ctx context.Context, baseRevision uint64, build BuildFunc) (uint64, []diagnostic.Diagnostic, *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.saveLocked(ctx, baseRevision, Default(), build)
}

// MarkLoadError 记录一次热加载失败，运行中的配置保持不变。
func (m *Manager) MarkLoadError(loadErr *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.loadErr = loadErr
}

// saveLocked 执行完整的保存流程。
func (m *Manager) saveLocked(
	_ context.Context,
	baseRevision uint64,
	cfg *Config,
	build BuildFunc,
) (uint64, []diagnostic.Diagnostic, *Error) {
	if baseRevision != m.revision {
		current := m.revision

		return 0, nil, &Error{Code: "revision_conflict", CurrentRevision: &current}
	}

	diags := Validate(cfg)
	validationErrors := diagnostic.Filter(diags, diagnostic.SeverityError)
	if len(validationErrors) > 0 {
		return 0, nil, &Error{Code: "invalid_config", Details: validationErrors}
	}

	data, err := MarshalTOML(cfg)
	if err != nil {
		return 0, nil, &Error{Code: "apply_failed", Message: err.Error()}
	}

	runtime, err := build(cfg)
	if err != nil {
		return 0, nil, &Error{Code: "apply_failed", Message: err.Error()}
	}

	if err := WriteAtomic(m.path, data); err != nil {
		_ = runtime.Close()

		return 0, nil, &Error{Code: "config_write_failed", Message: err.Error()}
	}

	runtime.Commit()
	m.cfg = cfg.Clone()
	m.revision++
	m.warnings = diagnostic.Filter(diags, diagnostic.SeverityWarning)
	m.loadErr = nil

	return m.revision, append([]diagnostic.Diagnostic{}, m.warnings...), nil
}

// errorFromDiagnostics 把诊断明细转换为配置错误。
func errorFromDiagnostics(diags []diagnostic.Diagnostic) *Error {
	errorsOnly := diagnostic.Filter(diags, diagnostic.SeverityError)
	if len(errorsOnly) == 0 {
		return nil
	}

	first := errorsOnly[0]

	return &Error{
		Code:    first.Code,
		Message: first.Message,
		Line:    first.Line,
		Path:    first.Path,
		Details: errorsOnly,
	}
}
