package config

import (
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/localrecords"
	"github.com/CedarHuang/Namesplit/internal/rules"
	"github.com/CedarHuang/Namesplit/internal/upstream"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Validate 返回配置的诊断明细，error 级别阻止应用，warning 级别仅提示。
func Validate(cfg *Config) []diagnostic.Diagnostic {
	if cfg == nil {
		return []diagnostic.Diagnostic{{
			Code:     diagnostic.CodeInvalidValue,
			Severity: diagnostic.SeverityError,
			Message:  "配置为空",
		}}
	}

	var diags []diagnostic.Diagnostic

	add := func(path, code, message string, severity diagnostic.Severity) {
		diags = append(diags, diagnostic.Diagnostic{
			Path:     path,
			Code:     code,
			Severity: severity,
			Message:  message,
		})
	}

	if cfg.SchemaVersion != DefaultSchemaVersion {
		add("schema_version", diagnostic.CodeUnsupportedSchemaVersion,
			fmt.Sprintf("不支持的配置版本 %d", cfg.SchemaVersion), diagnostic.SeverityError)
	}

	if cfg.Settings.DefaultGroup == "" {
		add("settings.default_group", diagnostic.CodeMissingField, "缺少默认上游组", diagnostic.SeverityError)
	}
	if cfg.Settings.LogRingSize < 1000 || cfg.Settings.LogRingSize > 20000 {
		add("settings.log_ring_size", diagnostic.CodeInvalidRingSize,
			fmt.Sprintf("日志环形缓冲容量 %d 不在 1000–20000", cfg.Settings.LogRingSize), diagnostic.SeverityError)
	}

	diags = append(diags, validateUpstreams(cfg)...)
	diags = append(diags, validateGroups(cfg)...)

	if cfg.Settings.DefaultGroup != "" {
		if _, ok := cfg.Groups[cfg.Settings.DefaultGroup]; !ok {
			add("settings.default_group", diagnostic.CodeMissingGroup,
				fmt.Sprintf("默认组 %q 不存在", cfg.Settings.DefaultGroup), diagnostic.SeverityError)
		}
	}

	diags = append(diags, validateRules(cfg)...)
	diags = append(diags, validateRecords(cfg)...)

	if data, err := MarshalTOML(cfg); err == nil && len(data) > MaxFileBytes {
		add("", diagnostic.CodeConfigTooLarge,
			fmt.Sprintf("规范化配置超过 %d 字节", MaxFileBytes), diagnostic.SeverityError)
	}

	return diags
}

// validateUpstreams 校验上游定义。
func validateUpstreams(cfg *Config) []diagnostic.Diagnostic {
	var diags []diagnostic.Diagnostic

	for _, id := range slices.Sorted(maps.Keys(cfg.Upstreams)) {
		upstreamCfg := cfg.Upstreams[id]
		path := "upstreams." + id

		if !idPattern.MatchString(id) {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path,
				Code:     diagnostic.CodeInvalidUpstreamID,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("上游 ID %q 不合法", id),
			})
		}

		if utf8.RuneCountInString(upstreamCfg.Name) > 64 {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".name",
				Code:     diagnostic.CodeInvalidName,
				Severity: diagnostic.SeverityError,
				Message:  "上游显示名超过 64 字符",
			})
		}
		if utf8.RuneCountInString(upstreamCfg.Remark) > 256 {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".remark",
				Code:     diagnostic.CodeInvalidName,
				Severity: diagnostic.SeverityError,
				Message:  "上游备注超过 256 字符",
			})
		}
		if upstreamCfg.TimeoutMS < 100 || upstreamCfg.TimeoutMS > 30000 {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".timeout_ms",
				Code:     diagnostic.CodeInvalidTimeout,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("超时 %d 不在 100–30000ms", upstreamCfg.TimeoutMS),
			})
		}

		parsed, err := upstream.ParseURL(upstreamCfg.URL)
		if err != nil {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".url",
				Code:     diagnostic.CodeInvalidUpstreamURL,
				Severity: diagnostic.SeverityError,
				Message:  err.Error(),
			})

			continue
		}

		if !parsed.HostIsName {
			continue
		}

		if len(upstreamCfg.Bootstrap) == 0 {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".bootstrap",
				Code:     diagnostic.CodeInvalidBootstrap,
				Severity: diagnostic.SeverityError,
				Message:  "上游主机是域名时必须配置 bootstrap",
			})

			continue
		}

		for index, text := range upstreamCfg.Bootstrap {
			if _, err := netip.ParseAddr(text); err != nil {
				diags = append(diags, diagnostic.Diagnostic{
					Path:     fmt.Sprintf("%s.bootstrap[%d]", path, index),
					Code:     diagnostic.CodeInvalidBootstrap,
					Severity: diagnostic.SeverityError,
					Message:  "bootstrap 元素必须是 IP 字面量",
				})
			}
		}
	}

	return diags
}

// validateGroups 校验组定义与引用。
func validateGroups(cfg *Config) []diagnostic.Diagnostic {
	var diags []diagnostic.Diagnostic

	for _, id := range slices.Sorted(maps.Keys(cfg.Groups)) {
		group := cfg.Groups[id]
		path := "groups." + id

		if !idPattern.MatchString(id) {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path,
				Code:     diagnostic.CodeInvalidGroupID,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("组 ID %q 不合法", id),
			})
		}
		if utf8.RuneCountInString(group.Name) > 64 {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".name",
				Code:     diagnostic.CodeInvalidName,
				Severity: diagnostic.SeverityError,
				Message:  "组显示名超过 64 字符",
			})
		}

		count := len(group.Upstreams)
		if count < 1 || count > 32 {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".upstreams",
				Code:     diagnostic.CodeInvalidUpstreamCount,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("组内上游数量 %d 不在 1–32", count),
			})
		}

		for index, upstreamID := range group.Upstreams {
			if _, ok := cfg.Upstreams[upstreamID]; !ok {
				diags = append(diags, diagnostic.Diagnostic{
					Path:     fmt.Sprintf("%s.upstreams[%d]", path, index),
					Code:     diagnostic.CodeMissingUpstream,
					Severity: diagnostic.SeverityError,
					Message:  fmt.Sprintf("组引用的上游 %q 不存在", upstreamID),
				})
			}
		}
	}

	return diags
}

// validateRules 校验规则语义。
func validateRules(cfg *Config) []diagnostic.Diagnostic {
	var diags []diagnostic.Diagnostic

	inputs := make([]rules.Rule, 0, len(cfg.Rules))
	for index, rule := range cfg.Rules {
		if _, ok := cfg.Groups[rule.Group]; !ok {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     fmt.Sprintf("rules[%d].group", index),
				Code:     diagnostic.CodeMissingGroup,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("规则引用的组 %q 不存在", rule.Group),
			})
		}

		inputs = append(inputs, rules.Rule{
			Kind:   rules.Kind(strings.ToUpper(strings.TrimSpace(rule.Match))),
			Domain: rule.Domain,
			Group:  rule.Group,
		})
	}

	_, ruleDiags := rules.Compile(inputs)
	diags = append(diags, ruleDiags...)

	return diags
}

// validateRecords 校验本地记录语义。
func validateRecords(cfg *Config) []diagnostic.Diagnostic {
	inputs := make([]localrecords.Record, 0, len(cfg.LocalRecords))
	for _, record := range cfg.LocalRecords {
		inputs = append(inputs, localrecords.Record{
			Type:    localrecords.Type(strings.ToUpper(strings.TrimSpace(record.Type))),
			Name:    record.Name,
			Value:   record.Value,
			TTL:     record.TTL,
			Enabled: record.Enabled,
		})
	}

	_, diags := localrecords.Compile(inputs, cfg.Settings.PtrAuto)

	return diags
}
