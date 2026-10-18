// Package api 提供 sidecar 的内部 HTTP API。
//
// 所有请求都要求 X-Namesplit-Token；响应字段使用 snake_case。接口只在回环
// 地址监听，不处理 CORS 与浏览器来源校验。
package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/event"
	"github.com/CedarHuang/Namesplit/internal/logstore"
	"github.com/CedarHuang/Namesplit/internal/upstream"
)

// Backend 是 API 依赖的业务能力，由组合根实现。
type Backend interface {
	Health() Health
	Status() Status
	ConfigState() (ConfigState, error)
	ConfigValidate(cfg json.RawMessage) (Validation, error)
	ConfigSave(ctx context.Context, baseRevision uint64, cfg json.RawMessage) (SaveResult, error)
	ConfigReset(ctx context.Context, baseRevision uint64) (SaveResult, error)
	RulesTest(ctx context.Context, domain, qtype string) (RuleTest, error)
	SubscribeEvents(ctx context.Context) (snapshot []event.Event, updates <-chan event.Event, cancel func(), err error)
	Queries(ctx context.Context, filter logstore.Filter) (logstore.Page, error)
	ClearQueries(ctx context.Context) (int64, error)
	Stats() logstore.Stats
	Minute(from, to time.Time) ([]logstore.MinuteStat, bool)
	Top(by string, since time.Time, limit int) ([]logstore.TopItem, bool)
	UpstreamTest(ctx context.Context, request UpstreamTestRequest) ([]upstream.TestResult, error)
	Shutdown()
}

// BindError 描述 DNS 监听失败的目标。
type BindError struct {
	Code    string        `json:"code"`
	Message string        `json:"message"`
	Targets []BindFailure `json:"targets"`
}

// BindFailure 是单个监听目标的失败信息。
type BindFailure struct {
	Address  string `json:"address"`
	Protocol string `json:"protocol"`
	Error    string `json:"error,omitempty"`
}

// DNSStatus 是 DNS 监听状态。
type DNSStatus struct {
	Ready     bool       `json:"ready"`
	UDP       []string   `json:"udp"`
	TCP       []string   `json:"tcp"`
	BindError *BindError `json:"bind_error"`
}

// Health 是 GET /api/health 的响应载荷。
type Health struct {
	Ready          bool            `json:"ready"`
	PID            int             `json:"pid"`
	Version        string          `json:"version"`
	Commit         string          `json:"commit"`
	StartedAt      time.Time       `json:"started_at"`
	UptimeMS       int64           `json:"uptime_ms"`
	ConfigRevision uint64          `json:"config_revision"`
	ConfigError    json.RawMessage `json:"config_error"`
	DNS            DNSStatus       `json:"dns"`
}

// Status 是 GET /api/status 的响应载荷。
type Status struct {
	Version        string          `json:"version"`
	Commit         string          `json:"commit"`
	UptimeMS       int64           `json:"uptime_ms"`
	ConfigRevision uint64          `json:"config_revision"`
	ConfigError    json.RawMessage `json:"config_error"`
	DNS            DNSStatus       `json:"dns"`
}

// ConfigState 是 GET /api/config 的响应载荷。
type ConfigState struct {
	Revision    uint64                  `json:"revision"`
	Config      json.RawMessage         `json:"config"`
	ConfigError json.RawMessage         `json:"config_error"`
	Warnings    []diagnostic.Diagnostic `json:"warnings"`
}

// Validation 是配置校验结果。
type Validation struct {
	Valid    bool                    `json:"valid"`
	Errors   []diagnostic.Diagnostic `json:"errors"`
	Warnings []diagnostic.Diagnostic `json:"warnings"`
}

// SaveResult 是配置保存/恢复默认的响应载荷。
type SaveResult struct {
	Revision uint64                  `json:"revision"`
	Warnings []diagnostic.Diagnostic `json:"warnings"`
}

// RuleTestLocal 是规则测试的本地记录结果。
type RuleTestLocal struct {
	Hit     bool     `json:"hit"`
	Name    string   `json:"name,omitempty"`
	QType   string   `json:"qtype,omitempty"`
	NODATA  bool     `json:"nodata,omitempty"`
	Answers []string `json:"answers,omitempty"`
	TTL     uint32   `json:"ttl,omitempty"`
}

// MarshalJSON 固定本地结果的响应形状：未命中只有 hit 字段；命中时
// name/qtype/nodata/answers/ttl 必须始终出现，answers 空集合输出 []。
//
// 不能依赖 omitempty：消费方需要区分「字段缺失」与「无答案/NODATA/ttl 为 0」，
// 否则命中结果会因零值被误读为未命中。
func (l RuleTestLocal) MarshalJSON() ([]byte, error) {
	if !l.Hit {
		return []byte(`{"hit":false}`), nil
	}

	answers := l.Answers
	if answers == nil {
		answers = []string{}
	}

	return json.Marshal(struct {
		Hit     bool     `json:"hit"`
		Name    string   `json:"name"`
		QType   string   `json:"qtype"`
		NODATA  bool     `json:"nodata"`
		Answers []string `json:"answers"`
		TTL     uint32   `json:"ttl"`
	}{
		Hit:     true,
		Name:    l.Name,
		QType:   l.QType,
		NODATA:  l.NODATA,
		Answers: answers,
		TTL:     l.TTL,
	})
}

// RuleTestRule 是规则测试命中的规则。
type RuleTestRule struct {
	Index  int    `json:"index"`
	Match  string `json:"match"`
	Domain string `json:"domain"`
	Group  string `json:"group"`
}

// RuleTest 是规则测试结果。
type RuleTest struct {
	Canonical    string                  `json:"canonical"`
	Local        RuleTestLocal           `json:"local"`
	Rule         *RuleTestRule           `json:"rule"`
	TargetGroup  string                  `json:"target_group"`
	DefaultGroup string                  `json:"default_group"`
	Warnings     []diagnostic.Diagnostic `json:"warnings"`
}

// UpstreamTestRequest 是连通性测试请求；三种来源互斥。
type UpstreamTestRequest struct {
	UpstreamID string         `json:"upstream_id,omitempty"`
	GroupID    string         `json:"group_id,omitempty"`
	Upstream   *DraftUpstream `json:"upstream,omitempty"`
}

// DraftUpstream 是未保存的上游草稿。
type DraftUpstream struct {
	URL       string   `json:"url"`
	Bootstrap []string `json:"bootstrap,omitempty"`
	TimeoutMS *int     `json:"timeout_ms,omitempty"`
}

// Error 是 API 错误；Handler 根据 Code 映射 HTTP 状态码。
type Error struct {
	Code            string
	Message         string
	Details         []diagnostic.Diagnostic
	CurrentRevision *uint64
}

// Error 实现 error。
func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}

	return e.Code
}
