package localrecords

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
)

func newLocalTable(t *testing.T) *Table {
	t.Helper()

	table, diags := Compile([]Record{
		{Type: TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 60, Enabled: true},
		{Type: TypeA, Name: "nas.home", Value: "192.168.1.11", TTL: 120, Enabled: true},
		{Type: TypeTXT, Name: "nas.home", Value: "hello", TTL: 60, Enabled: true},
		{Type: TypeA, Name: "*.dev.local", Value: "192.168.1.20", TTL: 60, Enabled: true},
		{Type: TypeCNAME, Name: "cname.dev.local", Value: "nas.home", TTL: 60, Enabled: true},
	}, true)
	if diagnostic.HasError(diags) {
		t.Fatalf("本地应答用例编译出错: %+v", diags)
	}

	return table
}

func lookup(t *testing.T, table *Table, qname string, qtype uint16) (Result, bool) {
	t.Helper()

	result, ok := table.Lookup(qname, qtype)
	if ok && !result.NODATA && len(result.Answers) == 0 {
		t.Fatalf("Lookup(%q) 返回 ok 但无应答且非 NODATA", qname)
	}

	return result, ok
}

func aValues(t *testing.T, result Result) []string {
	t.Helper()

	out := make([]string, 0, len(result.Answers))
	for _, rr := range result.Answers {
		a, ok := rr.(*dns.A)
		if !ok {
			t.Fatalf("期望 A 记录，实际 %T", rr)
		}

		out = append(out, a.A.String())
	}

	return out
}

func TestLocalAnswers(t *testing.T) {
	t.Parallel()

	table := newLocalTable(t)

	t.Run("多值 A 且 TTL 取最小", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "nas.home", dns.TypeA)
		if !ok {
			t.Fatal("nas.home A 未命中原生记录")
		}
		if got := aValues(t, result); len(got) != 2 || got[0] != "192.168.1.10" || got[1] != "192.168.1.11" {
			t.Fatalf("A 记录 = %v", got)
		}
		for _, rr := range result.Answers {
			if rr.Header().Ttl != 60 {
				t.Fatalf("TTL = %d, 期望 60", rr.Header().Ttl)
			}
			if rr.Header().Name != "nas.home." {
				t.Fatalf("owner = %q, 期望 nas.home.", rr.Header().Name)
			}
		}
	})

	t.Run("未知类型返回 NODATA", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "nas.home", dns.TypeAAAA)
		if !ok || !result.NODATA || len(result.Answers) != 0 {
			t.Fatalf("nas.home AAAA = %+v/%v", result, ok)
		}
	})

	t.Run("ANY 返回全部启用记录", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "nas.home", dns.TypeANY)
		if !ok || result.NODATA {
			t.Fatalf("nas.home ANY = %+v/%v", result, ok)
		}
		if len(result.Answers) != 3 {
			t.Fatalf("ANY 记录数 = %d, 期望 3", len(result.Answers))
		}
	})

	t.Run("通配子域", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "app.dev.local", dns.TypeA)
		if !ok || len(result.Answers) != 1 {
			t.Fatalf("app.dev.local A = %+v/%v", result, ok)
		}
		if got := aValues(t, result); got[0] != "192.168.1.20" {
			t.Fatalf("A 值 = %v", got)
		}
		if result.Answers[0].Header().Name != "app.dev.local." {
			t.Fatalf("通配应答 owner = %q, 期望查询名", result.Answers[0].Header().Name)
		}
	})

	t.Run("通配不含 apex", func(t *testing.T) {
		t.Parallel()

		if _, ok := table.Lookup("dev.local", dns.TypeA); ok {
			t.Fatal("dev.local 不应命中通配记录")
		}
	})

	t.Run("已有名字的未知类型 NODATA", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "nas.home", dns.TypeMX)
		if !ok || !result.NODATA {
			t.Fatalf("nas.home MX = %+v/%v", result, ok)
		}
	})

	t.Run("CNAME 原样返回", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "cname.dev.local", dns.TypeCNAME)
		if !ok || len(result.Answers) != 1 {
			t.Fatalf("cname.dev.local CNAME = %+v/%v", result, ok)
		}
		cname := result.Answers[0].(*dns.CNAME)
		if cname.Target != "nas.home." {
			t.Fatalf("CNAME target = %q", cname.Target)
		}
	})

	t.Run("自动生成 PTR", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "10.1.168.192.in-addr.arpa", dns.TypePTR)
		if !ok || len(result.Answers) != 1 {
			t.Fatalf("PTR = %+v/%v", result, ok)
		}
		ptr := result.Answers[0].(*dns.PTR)
		if ptr.Ptr != "nas.home." {
			t.Fatalf("PTR target = %q, 期望 nas.home.", ptr.Ptr)
		}
		if ptr.Hdr.Ttl != 60 {
			t.Fatalf("PTR TTL = %d, 期望源记录 TTL 60", ptr.Hdr.Ttl)
		}
	})

	t.Run("CNAME 名字的 A 查询返回 CNAME（标准行为）", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "cname.dev.local", dns.TypeA)
		if !ok || result.NODATA || len(result.Answers) != 1 {
			t.Fatalf("cname.dev.local A = %+v/%v", result, ok)
		}
		if _, isCNAME := result.Answers[0].(*dns.CNAME); !isCNAME {
			t.Fatalf("期望 CNAME，实际 %T", result.Answers[0])
		}
	})

	t.Run("CNAME 名字的 ANY 返回 CNAME", func(t *testing.T) {
		t.Parallel()

		result, ok := lookup(t, table, "cname.dev.local", dns.TypeANY)
		if !ok || len(result.Answers) != 1 {
			t.Fatalf("cname.dev.local ANY = %+v/%v", result, ok)
		}
	})
}

func TestLocalValidation(t *testing.T) {
	t.Parallel()

	t.Run("CNAME 冲突", func(t *testing.T) {
		t.Parallel()

		_, diags := Compile([]Record{
			{Type: TypeA, Name: "x.dev.local", Value: "192.168.1.30", TTL: 60, Enabled: true},
			{Type: TypeCNAME, Name: "x.dev.local", Value: "nas.home", TTL: 60, Enabled: true},
		}, true)
		if !hasCode(diags, diagnostic.CodeCNAMEConflict) {
			t.Fatalf("期望 cname_conflict，实际 %v", codes(diags))
		}
	})

	t.Run("重复记录只警告", func(t *testing.T) {
		t.Parallel()

		_, diags := Compile([]Record{
			{Type: TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 60, Enabled: true},
			{Type: TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 120, Enabled: false},
		}, true)
		if !hasCode(diags, diagnostic.CodeDuplicateRecord) {
			t.Fatalf("期望 duplicate_record，实际 %v", codes(diags))
		}
		if diagnostic.HasError(diags) {
			t.Fatalf("重复记录不应是错误: %+v", diags)
		}
	})

	t.Run("TTL 越界", func(t *testing.T) {
		t.Parallel()

		_, diags := Compile([]Record{
			{Type: TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 90000, Enabled: true},
		}, true)
		if !hasCode(diags, diagnostic.CodeInvalidTTL) {
			t.Fatalf("期望 invalid_ttl，实际 %v", codes(diags))
		}
	})

	t.Run("全部停用的名字不存在", func(t *testing.T) {
		t.Parallel()

		table, diags := Compile([]Record{
			{Type: TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 60, Enabled: false},
		}, true)
		if diagnostic.HasError(diags) {
			t.Fatalf("不应有错误: %+v", diags)
		}
		if _, ok := table.Lookup("nas.home", dns.TypeA); ok {
			t.Fatal("全部停用的名字不应命中")
		}
	})
}

func TestWildcardLongestSuffixAndExactPriority(t *testing.T) {
	t.Parallel()

	table, diags := Compile([]Record{
		{Type: TypeA, Name: "*.dev.local", Value: "192.168.1.20", TTL: 60, Enabled: true},
		{Type: TypeA, Name: "*.sub.dev.local", Value: "192.168.1.21", TTL: 60, Enabled: true},
		{Type: TypeA, Name: "app.dev.local", Value: "192.168.1.30", TTL: 60, Enabled: true},
	}, true)
	if diagnostic.HasError(diags) {
		t.Fatalf("编译出错: %+v", diags)
	}

	tests := []struct {
		qname string
		want  string
	}{
		{qname: "a.sub.dev.local", want: "192.168.1.21"},
		{qname: "sub.dev.local", want: "192.168.1.20"},
		{qname: "app.dev.local", want: "192.168.1.30"},
	}

	for _, tt := range tests {
		result, ok := table.Lookup(tt.qname, dns.TypeA)
		if !ok || len(result.Answers) != 1 {
			t.Fatalf("Lookup(%q) = %+v/%v", tt.qname, result, ok)
		}
		if got := aValues(t, result); got[0] != tt.want {
			t.Fatalf("Lookup(%q) = %v, 期望 %s", tt.qname, got, tt.want)
		}
	}
}

func TestExplicitPTRWins(t *testing.T) {
	t.Parallel()

	table, diags := Compile([]Record{
		{Type: TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 60, Enabled: true},
		{Type: TypePTR, Name: "10.1.168.192.in-addr.arpa", Value: "other.home", TTL: 120, Enabled: true},
	}, true)
	if diagnostic.HasError(diags) {
		t.Fatalf("编译出错: %+v", diags)
	}

	result, ok := table.Lookup("10.1.168.192.in-addr.arpa", dns.TypePTR)
	if !ok || len(result.Answers) != 1 {
		t.Fatalf("PTR = %+v/%v", result, ok)
	}
	if got := result.Answers[0].(*dns.PTR).Ptr; got != "other.home." {
		t.Fatalf("PTR target = %q, 期望显式记录 other.home.", got)
	}
}

func TestPTRAutoDisabled(t *testing.T) {
	t.Parallel()

	table, diags := Compile([]Record{
		{Type: TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 60, Enabled: true},
	}, false)
	if diagnostic.HasError(diags) {
		t.Fatalf("编译出错: %+v", diags)
	}

	if _, ok := table.Lookup("10.1.168.192.in-addr.arpa", dns.TypePTR); ok {
		t.Fatal("ptrAuto=false 时不应生成 PTR")
	}
}

func TestIPv6AutoPTR(t *testing.T) {
	t.Parallel()

	addr := netip.MustParseAddr("2001:db8::1")
	table, diags := Compile([]Record{
		{Type: TypeAAAA, Name: "v6.home", Value: addr.String(), TTL: 90, Enabled: true},
	}, true)
	if diagnostic.HasError(diags) {
		t.Fatalf("编译出错: %+v", diags)
	}

	reverse, err := dns.ReverseAddr(addr.String())
	if err != nil {
		t.Fatalf("生成反向名: %v", err)
	}
	reverse = strings.TrimSuffix(reverse, ".")
	result, ok := table.Lookup(reverse, dns.TypePTR)
	if !ok || len(result.Answers) != 1 {
		t.Fatalf("IPv6 PTR = %+v/%v", result, ok)
	}
	if got := result.Answers[0].(*dns.PTR).Ptr; got != "v6.home." {
		t.Fatalf("IPv6 PTR target = %q", got)
	}
}

func TestTXTSplit(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("a", 300)
	table, diags := Compile([]Record{
		{Type: TypeTXT, Name: "txt.home", Value: value, TTL: 60, Enabled: true},
	}, true)
	if diagnostic.HasError(diags) {
		t.Fatalf("编译出错: %+v", diags)
	}

	result, ok := table.Lookup("txt.home", dns.TypeTXT)
	if !ok || len(result.Answers) != 1 {
		t.Fatalf("TXT = %+v/%v", result, ok)
	}
	txt := result.Answers[0].(*dns.TXT)
	if len(txt.Txt) != 2 || len(txt.Txt[0]) != 255 || len(txt.Txt[1]) != 45 {
		t.Fatalf("TXT 分段 = %d 段，长度 %d/%d", len(txt.Txt), len(txt.Txt[0]), len(txt.Txt[1]))
	}
}

func TestInvalidRecordsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		record Record
		code   string
	}{
		{name: "非法类型", record: Record{Type: "MX", Name: "a.home", Value: "x", TTL: 60, Enabled: true}, code: diagnostic.CodeInvalidRecordValue},
		{name: "非法名字", record: Record{Type: TypeA, Name: "a..home", Value: "192.168.1.1", TTL: 60, Enabled: true}, code: diagnostic.CodeInvalidDomain},
		{name: "A 值非法", record: Record{Type: TypeA, Name: "a.home", Value: "::1", TTL: 60, Enabled: true}, code: diagnostic.CodeInvalidRecordValue},
		{name: "AAAA 值非法", record: Record{Type: TypeAAAA, Name: "a.home", Value: "192.168.1.1", TTL: 60, Enabled: true}, code: diagnostic.CodeInvalidRecordValue},
		{name: "TXT 超长", record: Record{Type: TypeTXT, Name: "a.home", Value: strings.Repeat("x", 1025), TTL: 60, Enabled: true}, code: diagnostic.CodeInvalidRecordValue},
		{name: "通配多处", record: Record{Type: TypeA, Name: "a.*.home", Value: "192.168.1.1", TTL: 60, Enabled: true}, code: diagnostic.CodeInvalidDomain},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, diags := Compile([]Record{tt.record}, true)
			if !hasCode(diags, tt.code) {
				t.Fatalf("期望 %s，实际 %v", tt.code, codes(diags))
			}
		})
	}
}

func hasCode(diags []diagnostic.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}

	return false
}

func codes(diags []diagnostic.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}

	return out
}
