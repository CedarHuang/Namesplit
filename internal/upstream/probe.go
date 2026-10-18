package upstream

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
)

// TestResult 是单个上游的连通性测试结果。
type TestResult struct {
	Name      string  `json:"name"`
	Address   string  `json:"address"`
	OK        bool    `json:"ok"`
	LatencyMS float64 `json:"latency_ms"`
	TLSOK     *bool   `json:"tls_ok"`
	Error     string  `json:"error"`
}

// Test 对 targets 依次做连通性测试，总预算为 budget。
//
// 固定查询 example.com 的 A 记录；单个目标超时取剩余预算与该目标超时的较小值。
// 测试结果不进入查询日志与统计。
func Test(ctx context.Context, targets []Spec, budget time.Duration, logger *slog.Logger) (results []TestResult) {
	if budget <= 0 {
		budget = 10 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}

	deadline := time.Now().Add(budget)
	results = make([]TestResult, 0, len(targets))

	for _, target := range targets {
		results = append(results, testOne(ctx, target, deadline, logger))
	}

	return results
}

// testOne 测试单个上游。
func testOne(ctx context.Context, target Spec, deadline time.Time, logger *slog.Logger) TestResult {
	result := TestResult{Name: displayName(target), Address: canonicalURLText(target.URL)}

	// 加密判定必须依据原始 scheme 文本：ParseURL 失败时拿不到 ParsedURL，而
	// 加密协议即使地址非法也要给出 tls_ok=false，不能退化成 null。
	encrypted := encryptedScheme(target.URL)
	fail := func(message string) TestResult {
		result.Error = message
		if encrypted {
			ok := false
			result.TLSOK = &ok
		}

		return result
	}

	if err := ctx.Err(); err != nil {
		return fail(fmt.Sprintf("连通性测试已取消: %v", err))
	}

	parsed, err := ParseURL(target.URL)
	if err != nil {
		return fail(err.Error())
	}
	result.Address = parsed.URL()

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail("连通性测试预算已用尽")
	}

	timeout := target.effectiveTimeout()
	if timeout > remaining {
		timeout = remaining
	}

	options, err := target.optionsFor(parsed, logger)
	if err != nil {
		return fail(err.Error())
	}
	options.Timeout = timeout

	// 使用规范 URL：配置原文带尾点或 Unicode 主机时 dnsproxy 会拒绝。
	resolver, err := upstream.AddressToUpstream(parsed.URL(), options)
	if err != nil {
		return fail(err.Error())
	}
	defer func() { _ = resolver.Close() }()

	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn("example.com"), dns.TypeA)
	req.RecursionDesired = true

	start := time.Now()
	resp, err := exchangeWithBudget(ctx, resolver, req, timeout)
	result.LatencyMS = durationMS(time.Since(start))

	switch {
	case err != nil:
		result.Error = err.Error()
	case resp == nil:
		result.Error = "上游未返回应答"
	default:
		result.OK = true
	}

	if encrypted {
		ok := result.OK
		result.TLSOK = &ok
	}

	return result
}

// encryptedScheme 按原始 URL 文本判断协议是否加密。
//
// 解析失败时拿不到 ParsedURL，但加密协议在不可用时必须给出确定的 tls_ok=false，
// 因此判定只依赖原始 scheme 文本；缺少 scheme 的裸地址按明文处理。
func encryptedScheme(raw string) bool {
	scheme, _, ok := strings.Cut(raw, "://")
	if !ok {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "tls", "https", "quic", "h3":
		return true
	default:
		return false
	}
}
