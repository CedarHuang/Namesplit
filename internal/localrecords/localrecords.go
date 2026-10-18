// Package localrecords 编译本地记录并执行「按名字权威」的本地应答。
//
// 名字存在性由启用中的记录定义；命中名字后有该类型则返回多值 RRset（RRset 内
// TTL 取最小），无该类型且无 CNAME 则 NODATA；有 CNAME 时按标准 DNS 行为返回
// CNAME（不追目标）。PTR 支持显式记录与 A/AAAA 自动生成。
package localrecords

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/dnsname"
)

// Type 是本地记录类型。
type Type string

const (
	TypeA     Type = "A"
	TypeAAAA  Type = "AAAA"
	TypeCNAME Type = "CNAME"
	TypeTXT   Type = "TXT"
	TypePTR   Type = "PTR"
)

// Record 是一条本地记录；Name/Value 在编译后保存规范形式。
type Record struct {
	Type    Type
	Name    string
	Value   string
	TTL     uint32
	Enabled bool
	// Line 是记录在配置文件中的行号；来自 API 时为 0。
	Line int
	// Index 由 Compile 按输入顺序写入，调用方无需设置。
	Index int
}

// Result 是一次本地应答；ok=false 表示名字不存在（交给规则），
// ok=true 且 NODATA=true 表示名字存在但无该类型。
type Result struct {
	// Name 是查询名的规范形式（无结尾点）。
	Name string
	// Answers 是应答 RR；NODATA 时为空。
	Answers []dns.RR
	// NODATA 表示名字存在但请求类型缺失。
	NODATA bool
}

// Table 是编译后的本地记录表。
type Table struct {
	exact    map[string][]Record
	wildcard map[string][]Record
	autoPTR  map[string]autoPTR
}

// autoPTR 是 A/AAAA 自动生成的反向解析条目。
type autoPTR struct {
	target string
	ttl    uint32
}

// Compile 编译本地记录并返回校验明细。
//
// ptrAuto 控制是否由启用的 A/AAAA 记录生成 PTR。违规记录不会进入表；调用方
// 在明细含 error 时不得应用该表。
func Compile(records []Record, ptrAuto bool) (*Table, []diagnostic.Diagnostic) {
	table := &Table{
		exact:    make(map[string][]Record, len(records)),
		wildcard: make(map[string][]Record),
		autoPTR:  make(map[string]autoPTR),
	}

	var diags []diagnostic.Diagnostic
	seen := make(map[recordKey]int, len(records))
	hasCNAME := make(map[string]bool, len(records))
	hasOther := make(map[string]bool, len(records))

	compiled := make([]Record, 0, len(records))

	for i, record := range records {
		path := fmt.Sprintf("local_records[%d]", i)

		typ, ok := normalizeType(record.Type)
		if !ok {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".type",
				Code:     diagnostic.CodeInvalidRecordValue,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("不支持的记录类型 %q", string(record.Type)),
				Line:     record.Line,
			})

			continue
		}

		if record.TTL > 86400 {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".ttl",
				Code:     diagnostic.CodeInvalidTTL,
				Severity: diagnostic.SeverityError,
				Message:  fmt.Sprintf("TTL %d 超出 0–86400", record.TTL),
				Line:     record.Line,
			})

			continue
		}

		name, wildcard, err := compileName(record.Name)
		if err != nil {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".name",
				Code:     diagnostic.CodeInvalidDomain,
				Severity: diagnostic.SeverityError,
				Message:  err.Error(),
				Line:     record.Line,
			})

			continue
		}

		value, err := compileValue(typ, record.Value)
		if err != nil {
			code := diagnostic.CodeInvalidRecordValue
			if typ == TypeCNAME || typ == TypePTR {
				code = diagnostic.CodeInvalidDomain
			}
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path + ".value",
				Code:     code,
				Severity: diagnostic.SeverityError,
				Message:  err.Error(),
				Line:     record.Line,
			})

			continue
		}

		nameKey := name
		if wildcard {
			nameKey = "*." + name
		}

		key := recordKey{name: nameKey, typ: typ, value: value}
		if _, ok := seen[key]; ok {
			diags = append(diags, diagnostic.Diagnostic{
				Path:     path,
				Code:     diagnostic.CodeDuplicateRecord,
				Severity: diagnostic.SeverityWarning,
				Message:  fmt.Sprintf("重复记录：%s %s %s", typ, nameKey, value),
				Line:     record.Line,
			})
		}
		seen[key] = i

		if typ == TypeCNAME {
			if hasOther[nameKey] {
				diags = append(diags, diagnostic.Diagnostic{
					Path:     path,
					Code:     diagnostic.CodeCNAMEConflict,
					Severity: diagnostic.SeverityError,
					Message:  fmt.Sprintf("CNAME 与其他类型在名字 %q 上共存", nameKey),
					Line:     record.Line,
				})
			}
			hasCNAME[nameKey] = true
		} else {
			if hasCNAME[nameKey] {
				diags = append(diags, diagnostic.Diagnostic{
					Path:     path,
					Code:     diagnostic.CodeCNAMEConflict,
					Severity: diagnostic.SeverityError,
					Message:  fmt.Sprintf("CNAME 与其他类型在名字 %q 上共存", nameKey),
					Line:     record.Line,
				})
			}
			hasOther[nameKey] = true
		}

		compiledRecord := Record{
			Type:    typ,
			Name:    name,
			Value:   value,
			TTL:     record.TTL,
			Enabled: record.Enabled,
			Line:    record.Line,
			Index:   i,
		}
		if wildcard {
			compiledRecord.Name = "*." + name
		}
		compiled = append(compiled, compiledRecord)

		if wildcard {
			table.wildcard[name] = append(table.wildcard[name], compiledRecord)
		} else {
			table.exact[name] = append(table.exact[name], compiledRecord)
		}
	}

	if ptrAuto {
		table.buildAutoPTR(compiled)
	}

	return table, diags
}

// Lookup 返回 qname/qtype 的本地应答。
//
// ok=false 表示名字不存在，调用方应继续规则路由。
func (t *Table) Lookup(qname string, qtype uint16) (Result, bool) {
	canon, err := dnsname.CanonQuery(qname)
	if err != nil {
		return Result{}, false
	}

	owner := dnsname.FQDN(canon)
	result := Result{Name: canon}

	records, found := t.find(canon)

	if qtype == dns.TypePTR {
		if found {
			if answers := buildAnswers(owner, records, dns.TypePTR); len(answers) > 0 {
				result.Answers = answers

				return result, true
			}
		}

		if auto, ok := t.autoPTR[canon]; ok {
			result.Answers = []dns.RR{&dns.PTR{
				Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: auto.ttl},
				Ptr: dnsname.FQDN(auto.target),
			}}

			return result, true
		}
	}

	if !found {
		return Result{}, false
	}

	if qtype == dns.TypeANY {
		result.Answers = buildAll(owner, records)
		result.NODATA = len(result.Answers) == 0

		return result, true
	}

	if answers := buildAnswers(owner, records, qtype); len(answers) > 0 {
		result.Answers = answers

		return result, true
	}

	// 标准 CNAME 行为：非 CNAME/ANY 查询返回 CNAME RRset，不追目标。
	if qtype != dns.TypeCNAME {
		if answers := buildAnswers(owner, records, dns.TypeCNAME); len(answers) > 0 {
			result.Answers = answers

			return result, true
		}
	}

	result.NODATA = true

	return result, true
}

// find 按精确优先、通配最长后缀返回启用记录。
func (t *Table) find(canon string) ([]Record, bool) {
	if records, ok := t.exact[canon]; ok && anyEnabled(records) {
		return records, true
	}

	// 通配不含 apex：从第二级后缀开始匹配。
	suffixList := dnsname.Suffixes(canon)
	for _, suffix := range suffixList[1:] {
		if records, ok := t.wildcard[suffix]; ok && anyEnabled(records) {
			return records, true
		}
	}

	return nil, false
}

// buildAutoPTR 按记录数组顺序为 A/AAAA 建立反向索引，同一 IP 取第一条。
func (t *Table) buildAutoPTR(records []Record) {
	for _, record := range records {
		if !record.Enabled || record.Type == TypePTR || record.Type == TypeTXT || record.Type == TypeCNAME {
			continue
		}
		if strings.HasPrefix(record.Name, "*.") {
			continue
		}

		addr, err := netip.ParseAddr(record.Value)
		if err != nil {
			continue
		}

		reverse, err := dns.ReverseAddr(addr.String())
		if err != nil {
			continue
		}
		reverse = strings.TrimSuffix(reverse, ".")
		if _, exists := t.autoPTR[reverse]; exists {
			continue
		}

		t.autoPTR[reverse] = autoPTR{target: record.Name, ttl: record.TTL}
	}
}

// buildAnswers 构造单一类型的 RRset，TTL 统一取该 RRset 最小值。
func buildAnswers(owner string, records []Record, qtype uint16) []dns.RR {
	answers := make([]dns.RR, 0)
	minTTL := uint32(0)

	for _, record := range records {
		if !record.Enabled || typeToDNS(record.Type) != qtype {
			continue
		}

		rr, ok := newRR(owner, record, record.TTL)
		if !ok {
			continue
		}

		if len(answers) == 0 || record.TTL < minTTL {
			minTTL = record.TTL
		}
		answers = append(answers, rr)
	}

	if len(answers) == 0 {
		return nil
	}

	for _, rr := range answers {
		rr.Header().Ttl = minTTL
	}

	return answers
}

// buildAll 构造 ANY 应答：按记录顺序输出，同一类型的 TTL 取该 RRset 最小值。
func buildAll(owner string, records []Record) []dns.RR {
	minTTLs := make(map[uint16]uint32, len(records))
	for _, record := range records {
		if !record.Enabled {
			continue
		}

		qtype := typeToDNS(record.Type)
		if current, ok := minTTLs[qtype]; !ok || record.TTL < current {
			minTTLs[qtype] = record.TTL
		}
	}

	answers := make([]dns.RR, 0, len(records))
	for _, record := range records {
		if !record.Enabled {
			continue
		}

		qtype := typeToDNS(record.Type)
		rr, ok := newRR(owner, record, minTTLs[qtype])
		if !ok {
			continue
		}

		answers = append(answers, rr)
	}

	if len(answers) == 0 {
		return nil
	}

	return answers
}

// newRR 把一条记录转换为 dns.RR。
func newRR(owner string, record Record, ttl uint32) (dns.RR, bool) {
	header := dns.RR_Header{Name: owner, Rrtype: typeToDNS(record.Type), Class: dns.ClassINET, Ttl: ttl}

	switch record.Type {
	case TypeA:
		addr, err := netip.ParseAddr(record.Value)
		if err != nil {
			return nil, false
		}

		return &dns.A{Hdr: header, A: net.IP(addr.AsSlice())}, true
	case TypeAAAA:
		addr, err := netip.ParseAddr(record.Value)
		if err != nil {
			return nil, false
		}

		return &dns.AAAA{Hdr: header, AAAA: net.IP(addr.AsSlice())}, true
	case TypeCNAME:
		return &dns.CNAME{Hdr: header, Target: dnsname.FQDN(record.Value)}, true
	case TypePTR:
		return &dns.PTR{Hdr: header, Ptr: dnsname.FQDN(record.Value)}, true
	case TypeTXT:
		return &dns.TXT{Hdr: header, Txt: splitTXT(record.Value)}, true
	default:
		return nil, false
	}
}

// compileName 返回规范名字与是否为通配记录。
func compileName(raw string) (name string, wildcard bool, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, fmt.Errorf("记录名字为空")
	}

	if strings.Contains(trimmed, "*") {
		if !strings.HasPrefix(trimmed, "*.") || strings.Count(trimmed, "*") != 1 {
			return "", false, fmt.Errorf("通配记录只允许前导 *. 且只有一个 *")
		}

		suffix, err := dnsname.Canon(strings.TrimPrefix(trimmed, "*."))
		if err != nil {
			return "", false, err
		}

		return suffix, true, nil
	}

	name, err = dnsname.Canon(trimmed)
	if err != nil {
		return "", false, err
	}

	return name, false, nil
}

// compileValue 校验并规范化记录值。
func compileValue(typ Type, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("记录值为空")
	}

	switch typ {
	case TypeA:
		addr, err := netip.ParseAddr(value)
		if err != nil || !addr.Is4() {
			return "", fmt.Errorf("A 记录的值不是 IPv4 地址")
		}

		return addr.String(), nil
	case TypeAAAA:
		addr, err := netip.ParseAddr(value)
		if err != nil || !addr.Is6() || addr.Is4In6() {
			return "", fmt.Errorf("AAAA 记录的值不是 IPv6 地址")
		}

		return addr.String(), nil
	case TypeCNAME:
		target, err := dnsname.Canon(value)
		if err != nil {
			return "", err
		}

		return target, nil
	case TypePTR:
		target, err := dnsname.Canon(value)
		if err != nil {
			return "", err
		}

		return target, nil
	case TypeTXT:
		if len(value) > 1024 {
			return "", fmt.Errorf("TXT 记录超过 1024 字节")
		}

		return value, nil
	default:
		return "", fmt.Errorf("不支持的记录类型 %q", typ)
	}
}

// normalizeType 把大小写不敏感的类型文本规范为常量。
func normalizeType(typ Type) (Type, bool) {
	switch Type(strings.ToUpper(strings.TrimSpace(string(typ)))) {
	case TypeA:
		return TypeA, true
	case TypeAAAA:
		return TypeAAAA, true
	case TypeCNAME:
		return TypeCNAME, true
	case TypeTXT:
		return TypeTXT, true
	case TypePTR:
		return TypePTR, true
	default:
		return "", false
	}
}

// typeToDNS 返回记录类型对应的 DNS 类型值。
func typeToDNS(typ Type) uint16 {
	switch typ {
	case TypeA:
		return dns.TypeA
	case TypeAAAA:
		return dns.TypeAAAA
	case TypeCNAME:
		return dns.TypeCNAME
	case TypeTXT:
		return dns.TypeTXT
	case TypePTR:
		return dns.TypePTR
	default:
		return 0
	}
}

// anyEnabled 判断记录列表中是否有启用记录。
func anyEnabled(records []Record) bool {
	for _, record := range records {
		if record.Enabled {
			return true
		}
	}

	return false
}

// splitTXT 把 TXT 值按 255 字节分段。
func splitTXT(value string) []string {
	const maxLen = 255

	out := make([]string, 0, len(value)/maxLen+1)
	for len(value) > maxLen {
		out = append(out, value[:maxLen])
		value = value[maxLen:]
	}
	out = append(out, value)

	return out
}

// recordKey 是记录重复判定的键（TTL/enabled 不参与）。
type recordKey struct {
	name  string
	typ   Type
	value string
}
