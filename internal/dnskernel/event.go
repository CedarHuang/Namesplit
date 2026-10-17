package dnskernel

import (
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Source 是应答来源分类。
type Source string

const (
	// SourceLocal 表示应答来自本地记录，未发起上游查询。
	SourceLocal Source = "local"
	// SourceCache 表示应答来自上游组缓存。
	SourceCache Source = "cache"
	// SourceUpstream 表示应答来自上游服务器。
	SourceUpstream Source = "upstream"
)

// Attempt 记录一次上游交换的地址、耗时与错误。
//
// 并发或主备切换时一次查询可能产生多条记录；缓存命中时 Cached 为 true，
// 且通常没有真实交换。
type Attempt struct {
	// Upstream 是上游地址，与 dnsproxy 的 UpstreamStatistics.Address 一致。
	Upstream string `json:"upstream"`
	// DurationMS 是该次交换耗时（毫秒）。
	DurationMS float64 `json:"duration_ms"`
	// Cached 表示该条统计来自缓存命中，而非真实交换。
	Cached bool `json:"cached,omitempty"`
	// Fallback 表示该次交换走的是备用（fallback）上游。
	Fallback bool `json:"fallback,omitempty"`
	// Error 是该次交换的错误文本，成功时为空。
	Error string `json:"error,omitempty"`
}

// Event 是一条逐查询事件。
//
// 字段来自运行时数据：路由结果、dnsproxy 的 DNSContext/QueryStatistics
// 以及应答报文，不做推算。
type Event struct {
	// Time 是收到查询的时间。
	Time time.Time `json:"time"`
	// Domain 是查询名（小写、FQDN，带结尾点）。
	Domain string `json:"domain"`
	// QType 是查询类型文本，例如 "A"。
	QType string `json:"qtype"`
	// Rule 是命中的规则原文；未命中规则时为 ""。
	Rule string `json:"rule"`
	// RuleKind 是命中类型：exact/wildcard/default/local。
	RuleKind string `json:"rule_kind"`
	// Source 是应答来源：local/cache/upstream。
	Source Source `json:"source"`
	// Group 是目标上游组名；本地应答为空。
	Group string `json:"group"`
	// Upstream 是实际应答的上游地址；本地应答为空，缓存命中时为缓存条目记录的地址。
	Upstream string `json:"upstream"`
	// DurationMS 是从收到查询到写出应答的总耗时（毫秒，含本地/缓存/上游）。
	DurationMS float64 `json:"duration_ms"`
	// Rcode 是应答 RCODE 数值。
	Rcode int `json:"rcode"`
	// RcodeName 是 RCODE 文本，例如 "NOERROR"。
	RcodeName string `json:"rcode_name"`
	// Protocol 是客户端到本机内核的协议：udp/tcp（对应 dnsproxy.Proto）。
	Protocol string `json:"protocol"`
	// Answers 是应答 RR 摘要列表（含 TTL）。
	Answers []string `json:"answers"`
	// TTL 是应答中最小 TTL（秒）；无应答时为 0。
	TTL uint32 `json:"ttl"`
	// CacheHit 表示缓存命中。
	CacheHit bool `json:"cache_hit"`
	// Attempts 是上游交换明细；本地应答为空。
	Attempts []Attempt `json:"attempts,omitempty"`
	// Error 是解析或上游交换错误文本；成功时为空。
	Error string `json:"error,omitempty"`
}

// summarizeAnswers 返回应答 RR 的文本摘要与最小 TTL。
func summarizeAnswers(msg *dns.Msg) (answers []string, ttl uint32) {
	if msg == nil {
		return nil, 0
	}

	var minTTL uint32
	for i, rr := range msg.Answer {
		if i == 0 || rr.Header().Ttl < minTTL {
			minTTL = rr.Header().Ttl
		}

		answers = append(answers, rr.String())
	}

	return answers, minTTL
}

// qtypeString 返回查询类型的可读文本。
func qtypeString(qtype uint16) string {
	if t, ok := dns.TypeToString[qtype]; ok {
		return t
	}

	return fmt.Sprintf("TYPE%d", qtype)
}

// rcodeName 返回 RCODE 的可读文本。
func rcodeName(rcode int) string {
	if s, ok := dns.RcodeToString[rcode]; ok {
		return s
	}

	return fmt.Sprintf("RCODE%d", rcode)
}

// normalizeName 将域名归一为小写 FQDN。
func normalizeName(name string) string {
	return strings.ToLower(dns.Fqdn(name))
}
