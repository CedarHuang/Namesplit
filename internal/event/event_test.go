package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestSummarize(t *testing.T) {
	t.Parallel()

	msg := new(dns.Msg)
	msg.Answer = []dns.RR{
		&dns.A{
			Hdr: dns.RR_Header{Name: "nas.home.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120},
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: "nas.home.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		},
	}
	msg.Ns = []dns.RR{
		&dns.SOA{Hdr: dns.RR_Header{Name: "home.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30}},
	}
	msg.Extra = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "ns.home.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 10}},
	}

	answers, authority, additional, ttl := Summarize(msg)

	if len(answers) != 2 || len(authority) != 1 || len(additional) != 1 {
		t.Fatalf("段落数量 = %d/%d/%d, 期望 2/1/1", len(answers), len(authority), len(additional))
	}
	if ttl != 60 {
		t.Fatalf("TTL = %d, 期望 60", ttl)
	}
}

func TestSummarizeDropsOPT(t *testing.T) {
	t.Parallel()

	msg := new(dns.Msg)
	msg.Answer = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "nas.home.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}},
	}
	msg.Extra = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "ns.home.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 10}},
		&dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Class: 1232}},
	}

	_, _, additional, _ := Summarize(msg)

	if len(additional) != 1 || !strings.HasPrefix(additional[0], "ns.home.") {
		t.Fatalf("additional = %v, 期望仅保留真实 RR", additional)
	}
	for _, rr := range additional {
		if strings.Contains(rr, "OPT PSEUDOSECTION") {
			t.Fatalf("additional 含 OPT 伪段落: %q", rr)
		}
	}
}

func TestSummarizeNil(t *testing.T) {
	t.Parallel()

	answers, authority, additional, ttl := Summarize(nil)
	if len(answers) != 0 || len(authority) != 0 || len(additional) != 0 || ttl != 0 {
		t.Fatalf("空消息摘要 = %v/%v/%v/%d", answers, authority, additional, ttl)
	}
}

func TestEventJSONShape(t *testing.T) {
	t.Parallel()

	ev := Event{
		EventID:      1024,
		Time:         time.Date(2026, 10, 18, 8, 12, 34, 123456789, time.UTC),
		Domain:       "example.com.",
		QType:        "A",
		Rule:         "",
		RuleIndex:    -1,
		RuleKind:     "default",
		Source:       "upstream",
		Group:        "default",
		Upstream:     "udp://1.1.1.1:53",
		UpstreamName: "Cloudflare",
		DurationMS:   18.2,
		Rcode:        0,
		RcodeName:    "NOERROR",
		Protocol:     "udp",
		Answers:      []string{},
		Authority:    []string{},
		Additional:   []string{},
		TTL:          0,
	}

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	for _, key := range []string{
		"event_id", "time", "domain", "qtype", "rule", "rule_index", "rule_kind",
		"source", "group", "upstream", "upstream_name", "duration_ms", "rcode",
		"rcode_name", "protocol", "answers", "authority", "additional", "ttl",
		"cache_hit", "do", "ad", "request_flags", "response_flags",
	} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("缺少字段 %q: %s", key, data)
		}
	}

	for _, key := range []string{"attempts", "error"} {
		if _, ok := decoded[key]; ok {
			t.Fatalf("空值字段 %q 不应出现: %s", key, data)
		}
	}

	if gotTime := decoded["time"].(string); gotTime != "2026-10-18T08:12:34.123456789Z" {
		t.Fatalf("time = %q, 期望 RFC3339Nano UTC", gotTime)
	}

	if answers := decoded["answers"].([]any); len(answers) != 0 {
		t.Fatalf("answers = %v, 期望 []", answers)
	}
}
