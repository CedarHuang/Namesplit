// Package rules 编译域名分流规则并执行顺序首命中匹配。
//
// 规则有 DOMAIN 与 DOMAIN-SUFFIX；列表顺序是唯一优先级，
// 不存在精确优先或最长后缀优先。
package rules

import (
	"fmt"
	"strings"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/dnsname"
)

// Kind 是规则的匹配类型。
type Kind string

const (
	// KindDomain 只匹配域名本身。
	KindDomain Kind = "DOMAIN"
	// KindSuffix 匹配域名本身及其任意深度子域（包含 apex）。
	KindSuffix Kind = "DOMAIN-SUFFIX"
)

// Rule 是一条规则；Domain 保留用户原文，Group 是目标上游组 ID。
type Rule struct {
	Kind   Kind
	Domain string
	Group  string
	// Line 是规则在配置文件中的行号；来自 API 时为 0。
	Line int
	// Index 由 Compile 按输入顺序写入，调用方无需设置。
	Index int
}

// Hit 是一次匹配结果。
type Hit struct {
	// Index 是命中规则在规则列表中的下标（0 起）。
	Index int
	// Kind 是命中类型；事件中映射为 exact/suffix。
	Kind Kind
	// Domain 是规则的 domain 配置原文。
	Domain string
	// Group 是目标上游组 ID。
	Group string
}

// Table 是编译后的规则表。
type Table struct {
	rules  []Rule
	exact  map[string]int
	suffix map[string]int
}

// Compile 编译规则并返回规则内语义校验明细。
//
// 只检查规则自身（类型、域名、重复、遮蔽），不检查组是否存在。
// 明细中出现 error 级别项时调用方不得应用该配置；Table 仍返回已成功编译的
// 前序规则，便于规则测试器展示。
func Compile(in []Rule) (*Table, []diagnostic.Diagnostic) {
	table := &Table{
		rules:  make([]Rule, 0, len(in)),
		exact:  make(map[string]int, len(in)),
		suffix: make(map[string]int, len(in)),
	}

	var diags []diagnostic.Diagnostic
	seen := make(map[ruleKey]int, len(in))

	for i, rule := range in {
		path := fmt.Sprintf("rules[%d]", i)

		kind := normalizeKind(rule.Kind)
		if kind != KindDomain && kind != KindSuffix {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".match",
				Code:     diagnostic.CodeUnsupportedRuleType,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("不支持的规则类型 %q，只支持 DOMAIN 与 DOMAIN-SUFFIX", string(rule.Kind)),
				Line:     rule.Line,
			})

			continue
		}

		domain := strings.TrimSpace(rule.Domain)
		if domain == "" {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".domain",
				Code:     diagnostic.CodeInvalidDomain,
				Severity: diagnostic.SeverityError,
				Message:  "规则域名为空",
				Line:     rule.Line,
			})

			continue
		}
		if strings.Contains(domain, "*") {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".domain",
				Code:     diagnostic.CodeWildcardNotSupported,
				Severity: diagnostic.SeverityError,
				Message:  "不支持通配规则，请用「DOMAIN apex + DOMAIN-SUFFIX」两行表达",
				Line:     rule.Line,
			})

			continue
		}

		canon, err := dnsname.Canon(domain)
		if err != nil {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".domain",
				Code:     diagnostic.CodeInvalidDomain,
				Severity: diagnostic.SeverityError,
				Message:  err.Error(),
				Line:     rule.Line,
			})

			continue
		}

		if rule.Group == "" {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".group",
				Code:     diagnostic.CodeInvalidRuleLine,
				Severity: diagnostic.SeverityError,
				Message:  "规则未指定上游组",
				Line:     rule.Line,
			})

			continue
		}

		key := ruleKey{kind: kind, canon: canon}
		if _, ok := seen[key]; ok {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path,
				Code:     diagnostic.CodeDuplicateRule,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("重复规则：%s %s", kind, domain),
				Line:     rule.Line,
			})

			continue
		}

		if coveredBy, ok := table.coveredBy(canon, kind); ok {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path,
				Code:     diagnostic.CodeShadowedRule,
				Severity: diagnostic.SeverityWarning,
				Message:  fmt.Sprintf("规则被第 %d 条规则完全遮蔽，永不生效", coveredBy),
				Line:     rule.Line,
			})
		}

		seen[key] = i
		table.rules = append(table.rules, Rule{
			Kind:   kind,
			Domain: domain,
			Group:  rule.Group,
			Line:   rule.Line,
			Index:  i,
		})

		switch kind {
		case KindDomain:
			if _, ok := table.exact[canon]; !ok {
				table.exact[canon] = i
			}
		case KindSuffix:
			if _, ok := table.suffix[canon]; !ok {
				table.suffix[canon] = i
			}
		}
	}

	return table, diags
}

// Match 返回 qname 命中的规则；未命中返回 ok=false（由调用方使用默认组）。
func (t *Table) Match(qname string) (hit Hit, ok bool) {
	canon, err := dnsname.CanonQuery(qname)
	if err != nil {
		return Hit{}, false
	}

	best := -1
	if idx, found := t.exact[canon]; found {
		best = idx
	}

	for _, suffix := range dnsname.Suffixes(canon) {
		if idx, found := t.suffix[suffix]; found && (best < 0 || idx < best) {
			best = idx
		}
	}

	if best < 0 {
		return Hit{}, false
	}

	rule := t.rules[best]

	return Hit{
		Index:  rule.Index,
		Kind:   rule.Kind,
		Domain: rule.Domain,
		Group:  rule.Group,
	}, true
}

// Rules 返回编译后的规则快照，供诊断与测试使用。
func (t *Table) Rules() []Rule {
	return append([]Rule(nil), t.rules...)
}

// coveredBy 判断 canon 是否为已有规则的匹配子集，返回覆盖它的规则下标。
func (t *Table) coveredBy(canon string, kind Kind) (int, bool) {
	if kind == KindDomain {
		for _, suffix := range dnsname.Suffixes(canon) {
			if idx, ok := t.suffix[suffix]; ok {
				return idx, true
			}
		}

		return 0, false
	}

	for _, suffix := range dnsname.Suffixes(canon) {
		if idx, ok := t.suffix[suffix]; ok {
			return idx, true
		}
	}

	return 0, false
}

// normalizeKind 把大小写不敏感的类型文本规范为常量。
func normalizeKind(kind Kind) Kind {
	return Kind(strings.ToUpper(strings.TrimSpace(string(kind))))
}

// ruleKey 是规则唯一性判定的键。
type ruleKey struct {
	kind  Kind
	canon string
}
