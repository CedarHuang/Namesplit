package rules

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
)

func mustCompile(t *testing.T, in []Rule) (*Table, []diagnostic.Diagnostic) {
	t.Helper()

	table, diags := Compile(in)
	if table == nil {
		t.Fatal("Compile 返回空表")
	}

	return table, diags
}

func codes(diags []diagnostic.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}

	return out
}

func hasCode(diags []diagnostic.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}

	return false
}

func TestRuleOrderAndSuffixMatching(t *testing.T) {
	t.Parallel()

	table, diags := mustCompile(t, []Rule{
		{Kind: KindDomain, Domain: "api.corp.local", Group: "corp"},
		{Kind: KindSuffix, Domain: "corp.local", Group: "corp"},
		{Kind: KindDomain, Domain: "example.com", Group: "public"},
		{Kind: KindSuffix, Domain: "example.com", Group: "adfilter"},
	})
	if diagnostic.HasError(diags) {
		t.Fatalf("规则顺序用例不应有错误: %+v", diags)
	}

	tests := []struct {
		qname  string
		want   int
		group  string
		kind   Kind
		missed bool
	}{
		{qname: "api.corp.local", want: 0, group: "corp", kind: KindDomain},
		{qname: "www.corp.local", want: 1, group: "corp", kind: KindSuffix},
		{qname: "corp.local", want: 1, group: "corp", kind: KindSuffix},
		{qname: "api.corp.local.", want: 0, group: "corp", kind: KindDomain},
		{qname: "API.CORP.LOCAL", want: 0, group: "corp", kind: KindDomain},
		{qname: "example.com", want: 2, group: "public", kind: KindDomain},
		{qname: "www.example.com", want: 3, group: "adfilter", kind: KindSuffix},
		{qname: "example.com.cn", missed: true},
		{qname: "notexample.com", missed: true},
	}

	for _, tt := range tests {
		t.Run(tt.qname, func(t *testing.T) {
			t.Parallel()

			hit, ok := table.Match(tt.qname)
			if tt.missed {
				if ok {
					t.Fatalf("Match(%q) 命中 %+v，期望未命中", tt.qname, hit)
				}

				return
			}

			if !ok {
				t.Fatalf("Match(%q) 未命中", tt.qname)
			}
			if hit.Index != tt.want || hit.Group != tt.group || hit.Kind != tt.kind {
				t.Fatalf("Match(%q) = %+v，期望 index=%d group=%s kind=%s", tt.qname, hit, tt.want, tt.group, tt.kind)
			}
		})
	}
}

func TestDomainAndSuffixCombination(t *testing.T) {
	t.Parallel()

	table, _ := mustCompile(t, []Rule{
		{Kind: KindDomain, Domain: "example.com", Group: "default"},
		{Kind: KindSuffix, Domain: "example.com", Group: "adfilter"},
	})

	tests := []struct {
		qname string
		want  int
		group string
	}{
		{qname: "example.com", want: 0, group: "default"},
		{qname: "www.example.com", want: 1, group: "adfilter"},
		{qname: "a.b.example.com", want: 1, group: "adfilter"},
	}

	for _, tt := range tests {
		hit, ok := table.Match(tt.qname)
		if !ok || hit.Index != tt.want || hit.Group != tt.group {
			t.Fatalf("Match(%q) = %+v/%v，期望 index=%d group=%s", tt.qname, hit, ok, tt.want, tt.group)
		}
	}

	if hit, ok := table.Match("example.com.cn"); ok {
		t.Fatalf("Match(example.com.cn) 命中 %+v，期望未命中", hit)
	}
}

func TestShadowedRule(t *testing.T) {
	t.Parallel()

	table, diags := mustCompile(t, []Rule{
		{Kind: KindSuffix, Domain: "example.com", Group: "adfilter"},
		{Kind: KindDomain, Domain: "api.example.com", Group: "corp", Line: 2},
	})

	if !hasCode(diags, diagnostic.CodeShadowedRule) {
		t.Fatalf("期望 shadowed_rule 警告，实际: %v", codes(diags))
	}
	if diagnostic.HasError(diags) {
		t.Fatalf("遮蔽不应是错误: %+v", diags)
	}

	hit, ok := table.Match("api.example.com")
	if !ok || hit.Index != 0 || hit.Group != "adfilter" {
		t.Fatalf("Match(api.example.com) = %+v/%v，期望命中第 0 条", hit, ok)
	}
}

func TestRuleCompileValidation(t *testing.T) {
	t.Parallel()

	t.Run("重复规则", func(t *testing.T) {
		t.Parallel()

		_, diags := mustCompile(t, []Rule{
			{Kind: KindSuffix, Domain: "Example.COM", Group: "a"},
			{Kind: KindSuffix, Domain: "example.com", Group: "b"},
		})
		if !hasCode(diags, diagnostic.CodeDuplicateRule) {
			t.Fatalf("期望 duplicate_rule，实际: %v", codes(diags))
		}
	})

	t.Run("通配", func(t *testing.T) {
		t.Parallel()

		_, diags := mustCompile(t, []Rule{
			{Kind: KindDomain, Domain: "*.example.com", Group: "a"},
		})
		if !hasCode(diags, diagnostic.CodeWildcardNotSupported) {
			t.Fatalf("期望 wildcard_not_supported，实际: %v", codes(diags))
		}
	})

	t.Run("未知类型", func(t *testing.T) {
		t.Parallel()

		_, diags := mustCompile(t, []Rule{
			{Kind: "DOMAIN-REGEX", Domain: "^x$", Group: "a"},
		})
		if !hasCode(diags, diagnostic.CodeUnsupportedRuleType) {
			t.Fatalf("期望 unsupported_rule_type，实际: %v", codes(diags))
		}
	})

	t.Run("空规则合法", func(t *testing.T) {
		t.Parallel()

		table, diags := mustCompile(t, nil)
		if diagnostic.HasError(diags) {
			t.Fatalf("空规则不应有错误: %+v", diags)
		}
		if _, ok := table.Match("example.com"); ok {
			t.Fatal("空规则不应命中")
		}
	})
}

func TestIDNRule(t *testing.T) {
	t.Parallel()

	table, _ := mustCompile(t, []Rule{
		{Kind: KindSuffix, Domain: "bücher.example", Group: "corp"},
	})

	hit, ok := table.Match("xn--bcher-kva.example")
	if !ok || hit.Index != 0 || hit.Domain != "bücher.example" {
		t.Fatalf("Match(xn--bcher-kva.example) = %+v/%v", hit, ok)
	}
}

// IDNA 会把全角句号/句点映射为「.」并保留结尾点，规则归一必须去掉它，
// 否则规则能通过校验但永不命中，重复检测也会漏判。
func TestRuleDomainWithFullWidthDot(t *testing.T) {
	t.Parallel()

	table, diags := mustCompile(t, []Rule{
		{Kind: KindDomain, Domain: "api.example.com。", Group: "corp"},
		{Kind: KindSuffix, Domain: "corp.local．", Group: "corp"},
	})
	if diagnostic.HasError(diags) {
		t.Fatalf("全角句号规则不应有错误: %+v", diags)
	}

	for _, qname := range []string{"api.example.com", "sub.corp.local"} {
		if _, ok := table.Match(qname); !ok {
			t.Fatalf("Match(%q) 未命中全角句号规则", qname)
		}
	}

	if _, diags := mustCompile(t, []Rule{
		{Kind: KindDomain, Domain: "example.com", Group: "a"},
		{Kind: KindDomain, Domain: "example.com。", Group: "b"},
	}); !hasCode(diags, diagnostic.CodeDuplicateRule) {
		t.Fatalf("全角句号规则应被判为重复: %+v", diags)
	}
}

func TestPTRAndCNAMERouteByQueryName(t *testing.T) {
	t.Parallel()

	table, _ := mustCompile(t, []Rule{
		{Kind: KindSuffix, Domain: "in-addr.arpa", Group: "corp"},
		{Kind: KindSuffix, Domain: "corp.local", Group: "corp"},
	})

	for _, qname := range []string{"10.1.168.192.in-addr.arpa", "api.corp.local"} {
		if _, ok := table.Match(qname); !ok {
			t.Fatalf("Match(%q) 未命中", qname)
		}
	}
}

func TestMatchRejectsMalformedQuery(t *testing.T) {
	t.Parallel()

	table, _ := mustCompile(t, []Rule{{Kind: KindSuffix, Domain: "example.com", Group: "a"}})

	for _, qname := range []string{"", ".", "a..b"} {
		if hit, ok := table.Match(qname); ok {
			t.Fatalf("Match(%q) = %+v, 期望未命中", qname, hit)
		}
	}
}

// buildTable 构造 n 条互不遮蔽的规则：偶数条精确、奇数条后缀。
func buildTable(n int) (*Table, []string) {
	in := make([]Rule, 0, n)
	queries := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			domain := fmt.Sprintf("host-%d.example-%d.test", i, i)
			in = append(in, Rule{Kind: KindDomain, Domain: domain, Group: "g"})
			queries = append(queries, domain)
		} else {
			domain := fmt.Sprintf("suffix-%d.example-%d.test", i, i)
			in = append(in, Rule{Kind: KindSuffix, Domain: domain, Group: "g"})
			queries = append(queries, "sub."+domain)
		}
	}

	table, _ := Compile(in)

	return table, queries
}

func BenchmarkMatch10000(b *testing.B) {
	table, queries := buildTable(10000)
	miss := make([]string, 0, len(queries))
	for i := range queries {
		miss = append(miss, fmt.Sprintf("miss-%d.unknown.test", i))
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		table.Match(queries[i%len(queries)])
		table.Match(miss[i%len(miss)])
	}
}

func TestMatchP99(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过 p99 基准")
	}

	table, queries := buildTable(10000)
	miss := make([]string, 0, len(queries))
	for i := range queries {
		miss = append(miss, fmt.Sprintf("miss-%d.unknown.test", i))
	}

	const (
		samples   = 2000
		perSample = 100
	)

	durations := make([]time.Duration, 0, samples)
	hits := 0

	for s := 0; s < samples; s++ {
		start := time.Now()
		for j := 0; j < perSample; j++ {
			i := s*perSample + j
			qname := queries[i%len(queries)]
			if i%2 == 1 {
				qname = miss[i%len(miss)]
			}

			if hit, ok := table.Match(qname); ok {
				hits += hit.Index + 1
			}
		}

		durations = append(durations, time.Since(start)/perSample)
	}

	if hits == 0 {
		t.Fatal("匹配结果被优化掉，基准无效")
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p99 := durations[len(durations)*99/100]

	if p99 > time.Millisecond {
		t.Fatalf("1 万规则 p99 = %v，超过 1ms", p99)
	}

	t.Logf("1 万规则 p99 = %v", p99)
}
