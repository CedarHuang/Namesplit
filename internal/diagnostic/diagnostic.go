// Package diagnostic 定义配置校验明细与稳定的错误码常量。
//
// code 是稳定标识，message 仅用于调试，界面应按 code 本地化。
package diagnostic

// Severity 是校验明细级别。
type Severity string

const (
	// SeverityError 阻止保存或应用。
	SeverityError Severity = "error"
	// SeverityWarning 允许保存，仅提示。
	SeverityWarning Severity = "warning"
)

// Diagnostic 是一条配置校验明细。
type Diagnostic struct {
	Path     string   `json:"path"`
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Line     int      `json:"line,omitempty"`
}

// 配置校验错误码。
const (
	CodeInvalidTOML              = "invalid_toml"
	CodeUnknownField             = "unknown_field"
	CodeMissingField             = "missing_field"
	CodeInvalidType              = "invalid_type"
	CodeInvalidValue             = "invalid_value"
	CodeUnsupportedSchemaVersion = "unsupported_schema_version"
	CodeDuplicateGroupID         = "duplicate_group_id"
	CodeDuplicateUpstreamID      = "duplicate_upstream_id"
	CodeInvalidGroupID           = "invalid_group_id"
	CodeInvalidUpstreamID        = "invalid_upstream_id"
	CodeMissingGroup             = "missing_group"
	CodeMissingUpstream          = "missing_upstream"
	CodeInvalidUpstreamCount     = "invalid_upstream_count"
	CodeInvalidUpstreamURL       = "invalid_upstream_url"
	CodeInvalidBootstrap         = "invalid_bootstrap"
	CodeInvalidTimeout           = "invalid_timeout"
	CodeInvalidName              = "invalid_name"
	CodeInvalidRuleLine          = "invalid_rule_line"
	CodeUnsupportedRuleType      = "unsupported_rule_type"
	CodeWildcardNotSupported     = "wildcard_not_supported"
	CodeInvalidDomain            = "invalid_domain"
	CodeDuplicateRule            = "duplicate_rule"
	CodeInvalidRecordLine        = "invalid_record_line"
	CodeInvalidTTL               = "invalid_ttl"
	CodeInvalidRecordValue       = "invalid_record_value"
	CodeCNAMEConflict            = "cname_conflict"
	CodeInvalidRingSize          = "invalid_ring_size"
	CodeInvalidSettingValue      = "invalid_setting_value"
	CodeConfigTooLarge           = "config_too_large"
)

// 配置校验警告码。
const (
	CodeShadowedRule    = "shadowed_rule"
	CodeDuplicateRecord = "duplicate_record"
)

// 进程级配置错误码。
const (
	CodeConfigReadFailed      = "config_read_failed"
	CodeConfigMigrationFailed = "config_migration_failed"
)

// HasError 判断明细中是否包含 error 级别项。
func HasError(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == SeverityError {
			return true
		}
	}

	return false
}

// Filter 返回指定级别的明细；severity 为空时返回全部。
func Filter(diags []Diagnostic, severity Severity) []Diagnostic {
	if severity == "" {
		return append([]Diagnostic(nil), diags...)
	}

	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		if d.Severity == severity {
			out = append(out, d)
		}
	}

	return out
}
