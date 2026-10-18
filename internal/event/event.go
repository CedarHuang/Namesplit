// Package event 定义逐查询事件及其序列化规则。
//
// 必有字段始终输出；数组字段无内容时输出 []；attempts 与 error 为空时省略。
package event

import (
	"fmt"
	"time"

	"github.com/miekg/dns"
)

// Attempt 是一次已结束的上游交换。
//
// 只记录真实发生的交换；并发竞速中落败的交换也计入已结束的尝试，而缓存命中
// 与 fallback 不产生该类记录，因此不设 cached/fallback 标记。
type Attempt struct {
	Upstream   string  `json:"upstream"`
	DurationMS float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}

// RequestFlags 是请求标志。
type RequestFlags struct {
	RD bool `json:"rd"`
	CD bool `json:"cd"`
}

// ResponseFlags 是应答标志。
type ResponseFlags struct {
	AA bool `json:"aa"`
	TC bool `json:"tc"`
	RA bool `json:"ra"`
}

// Event 是一条逐查询事件。
type Event struct {
	EventID       uint64        `json:"event_id"`
	Time          time.Time     `json:"time"`
	Domain        string        `json:"domain"`
	QType         string        `json:"qtype"`
	Rule          string        `json:"rule"`
	RuleIndex     int           `json:"rule_index"`
	RuleKind      string        `json:"rule_kind"`
	Source        string        `json:"source"`
	Group         string        `json:"group"`
	Upstream      string        `json:"upstream"`
	UpstreamName  string        `json:"upstream_name"`
	DurationMS    float64       `json:"duration_ms"`
	Rcode         int           `json:"rcode"`
	RcodeName     string        `json:"rcode_name"`
	Protocol      string        `json:"protocol"`
	Answers       []string      `json:"answers"`
	Authority     []string      `json:"authority"`
	Additional    []string      `json:"additional"`
	TTL           uint32        `json:"ttl"`
	CacheHit      bool          `json:"cache_hit"`
	DO            bool          `json:"do"`
	AD            bool          `json:"ad"`
	RequestFlags  RequestFlags  `json:"request_flags"`
	ResponseFlags ResponseFlags `json:"response_flags"`
	Attempts      []Attempt     `json:"attempts,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// Summarize 提取应答三个段的 RR 文本与最小 TTL。
//
// 返回的切片永不为 nil，保证序列化为 [] 而不是 null。
func Summarize(msg *dns.Msg) (answers, authority, additional []string, ttl uint32) {
	answers = make([]string, 0)
	authority = make([]string, 0)
	additional = make([]string, 0)

	if msg == nil {
		return answers, authority, additional, 0
	}

	for _, section := range []struct {
		rrs []dns.RR
		out *[]string
	}{
		{rrs: msg.Answer, out: &answers},
		{rrs: msg.Ns, out: &authority},
		{rrs: msg.Extra, out: &additional},
	} {
		for _, rr := range section.rrs {
			// OPT 是 EDNS 伪 RR，String() 会输出多行伪段落，混进事件文本会污染
			// 额外段；响应不携带 OPT，这里统一过滤。
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}

			*section.out = append(*section.out, rr.String())
		}
	}

	for i, rr := range msg.Answer {
		if i == 0 || rr.Header().Ttl < ttl {
			ttl = rr.Header().Ttl
		}
	}

	return answers, authority, additional, ttl
}

// RcodeName 返回 RCODE 的可读文本。
func RcodeName(rcode int) string {
	if s, ok := dns.RcodeToString[rcode]; ok {
		return s
	}

	return fmt.Sprintf("RCODE%d", rcode)
}
