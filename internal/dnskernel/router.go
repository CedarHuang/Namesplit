package dnskernel

import (
	"fmt"
	"strings"
)

// RuleKind 是规则匹配类型：
//   - exact：仅匹配该域名本身；
//   - wildcard：`*.example.com` 匹配任意深度子域，不匹配 apex。
type RuleKind string

const (
	// RuleExact 为精确域名规则。
	RuleExact RuleKind = "exact"
	// RuleWildcard 为子域通配规则。
	RuleWildcard RuleKind = "wildcard"
)

// DefaultGroup 是默认上游组的保留名。
const DefaultGroup = "default"

// Rule 是一条「匹配条件 → 上游组」规则。
type Rule struct {
	// Kind 是匹配类型。
	Kind RuleKind
	// Pattern 是规则原文：exact 为域名，wildcard 形如 "*.example.com"。
	Pattern string
	// Group 是目标上游组名。
	Group string
	// QTypes 是可选查询类型条件；为空表示不限类型。
	QTypes []uint16
}

// Route 是一次路由结果。
type Route struct {
	// Group 是目标上游组名。
	Group string
	// Rule 是命中的规则原文；命中默认组时为空。
	Rule string
	// Kind 是命中类型；命中默认组时为 "default"。
	Kind string
}

// Router 按「精确 > 通配 > 列表顺序 > 默认组」做域名分流。
//
// 当前实现精确与通配两类规则，尚未实现正则、冲突检测与大规模规则的匹配优化。
type Router struct {
	defaultGroup string
	rules        []Rule
}

// NewRouter 构造路由表。rules 允许为空；defaultGroup 不能为空。
func NewRouter(defaultGroup string, rules []Rule) (r *Router, err error) {
	if defaultGroup == "" {
		return nil, fmt.Errorf("默认上游组不能为空")
	}

	normalized := make([]Rule, 0, len(rules))
	for i, rule := range rules {
		rule.Pattern = normalizeName(rule.Pattern)
		switch rule.Kind {
		case RuleExact:
			rule.Pattern = strings.TrimSuffix(rule.Pattern, ".")
		case RuleWildcard:
			if !strings.HasPrefix(rule.Pattern, "*.") {
				return nil, fmt.Errorf("第 %d 条通配规则 %q 必须形如 *.example.com", i, rule.Pattern)
			}
			rule.Pattern = strings.TrimSuffix(rule.Pattern, ".")
		default:
			return nil, fmt.Errorf("第 %d 条规则的匹配类型 %q 不受支持", i, rule.Kind)
		}

		if rule.Group == "" {
			return nil, fmt.Errorf("第 %d 条规则 %q 未指定上游组", i, rule.Pattern)
		}

		normalized = append(normalized, rule)
	}

	return &Router{defaultGroup: defaultGroup, rules: normalized}, nil
}

// DefaultGroup 返回默认上游组名。
func (r *Router) DefaultGroup() string {
	return r.defaultGroup
}

// Rules 返回规则快照，供诊断与测试使用。
func (r *Router) Rules() []Rule {
	return append([]Rule(nil), r.rules...)
}

// Match 返回 name/qtype 的路由结果。name 会归一为小写 FQDN。
//
// 优先级：精确（按列表顺序）> 通配（按列表顺序，更长的后缀先）> 默认组。
func (r *Router) Match(name string, qtype uint16) (route Route) {
	fqdn := normalizeName(name)
	host := strings.TrimSuffix(fqdn, ".")

	var wildcard Rule
	var wildcardLen int

	for _, rule := range r.rules {
		if !matchQType(rule.QTypes, qtype) {
			continue
		}

		switch rule.Kind {
		case RuleExact:
			if host == rule.Pattern {
				return Route{Group: rule.Group, Rule: rule.Pattern, Kind: string(rule.Kind)}
			}
		case RuleWildcard:
			if !matchWildcard(host, rule.Pattern) {
				continue
			}

			// 通配规则取后缀最长者，避免规则顺序影响「更具体优先」。
			if suffixLen := len(rule.Pattern); suffixLen > wildcardLen {
				wildcard = rule
				wildcardLen = suffixLen
			}
		}
	}

	if wildcardLen > 0 {
		return Route{Group: wildcard.Group, Rule: wildcard.Pattern, Kind: string(wildcard.Kind)}
	}

	return Route{Group: r.defaultGroup, Kind: "default"}
}

// matchWildcard 判断 host 是否匹配 "*.suffix"：至少多一级标签，且不匹配 apex。
func matchWildcard(host, pattern string) bool {
	suffix := strings.TrimPrefix(pattern, "*")
	if !strings.HasSuffix(host, suffix) {
		return false
	}

	// host 必须比 "suffix 去掉开头的点" 更长，即至少存在一级额外标签。
	apex := strings.TrimPrefix(suffix, ".")
	return len(host) > len(apex)
}

// matchQType 判断查询类型是否满足规则条件。
func matchQType(qtypes []uint16, qtype uint16) bool {
	if len(qtypes) == 0 {
		return true
	}

	for _, t := range qtypes {
		if t == qtype {
			return true
		}
	}

	return false
}
