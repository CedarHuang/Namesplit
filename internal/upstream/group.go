package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/event"
)

// Result 是一次上游组解析的结果。
type Result struct {
	// Response 是上游返回的应答；失败时可能为 nil。
	Response *dns.Msg
	// Upstream 是实际应答的上游 URL；缓存命中时取自缓存条目。
	Upstream string
	// UpstreamName 是应答上游的显示名。
	UpstreamName string
	// CacheHit 表示应答来自组内缓存。
	CacheHit bool
	// Attempts 是已结束的上游交换明细。
	Attempts []event.Attempt
	// Error 是解析错误。
	Error error
}

// Group 是一个上游组：组内成员并发查询，先成功者生效。
type Group struct {
	id     string
	proxy  *proxy.Proxy
	config *proxy.UpstreamConfig
	names  map[string]string
}

// NewGroup 构建一个上游组。
//
// 每个组持有独立的 dnsproxy 实例与缓存，因此同名查询在不同组之间不会共享答案。
func NewGroup(id string, members []Spec, logger *slog.Logger) (*Group, error) {
	if id == "" {
		return nil, errors.New("上游组 ID 不能为空")
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("上游组 %q 没有成员", id)
	}
	if logger == nil {
		logger = slog.Default()
	}

	config := &proxy.UpstreamConfig{}
	names := make(map[string]string, len(members))

	for _, member := range members {
		parsed, options, err := member.options(logger)
		if err != nil {
			_ = config.Close()

			return nil, fmt.Errorf("上游组 %q: %w", id, err)
		}

		// 必须用规范 URL：配置原文可能带尾点或 Unicode 主机，dnsproxy 不会归一，
		// 会以「非法地址」在运行期拒绝。
		resolver, err := upstream.AddressToUpstream(parsed.URL(), options)
		if err != nil {
			_ = config.Close()

			return nil, fmt.Errorf("上游组 %q: %w", id, err)
		}

		config.Upstreams = append(config.Upstreams, &tracedUpstream{
			Upstream: resolver,
			timeout:  member.effectiveTimeout(),
		})
		names[resolver.Address()] = displayName(member)
	}

	p, err := proxy.New(&proxy.Config{
		Logger:          logger,
		UpstreamConfig:  config,
		UpstreamMode:    proxy.UpstreamModeParallel,
		CacheEnabled:    true,
		CacheMinTTL:     0,
		CacheMaxTTL:     86400,
		CacheOptimistic: false,
		// 请求侧 ECS 在内核进入上游前剥离；此处不启用 dnsproxy 自带的 ECS 处理。
		EnableEDNSClientSubnet: false,
	})
	if err != nil {
		_ = config.Close()

		return nil, fmt.Errorf("上游组 %q: %w", id, err)
	}

	return &Group{id: id, proxy: p, config: config, names: names}, nil
}

// Resolve 通过该组解析 req。proto 是客户端到本机的协议。
func (g *Group) Resolve(
	ctx context.Context,
	req *dns.Msg,
	proto proxy.Proto,
	clientAddr string,
) (result Result) {
	trace := &exchangeTrace{}
	ctx = context.WithValue(ctx, exchangeTraceKey{}, trace)

	dctx := &proxy.DNSContext{Proto: proto, Req: req, Addr: parseAddrPort(clientAddr)}
	result.Error = g.proxy.Resolve(ctx, dctx)
	result.Response = dctx.Res

	if dctx.Upstream != nil {
		result.Upstream = dctx.Upstream.Address()
	}

	if stats := dctx.QueryStatistics(); stats != nil {
		for _, item := range stats.Main() {
			if !item.IsCached {
				continue
			}

			result.CacheHit = true
			if result.Upstream == "" {
				result.Upstream = item.Address
			}
		}
	}

	result.Attempts = trace.snapshot()
	result.UpstreamName = g.names[result.Upstream]

	return result
}

// ClearCache 清空该组的缓存。
func (g *Group) ClearCache() {
	g.proxy.ClearCache()
}

// Close 关闭组内上游连接。
func (g *Group) Close() error {
	return g.config.Close()
}

// effectiveTimeout 返回该上游的单次交换总预算。
func (s Spec) effectiveTimeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}

	return DefaultTimeout
}

// options 把 Spec 转换为 dnsproxy 上游选项，并返回解析后的 URL。
//
// 调用方必须使用返回的 ParsedURL.URL() 构造上游：配置原文可能带尾点或
// Unicode 主机，dnsproxy 不会归一。
func (s Spec) options(logger *slog.Logger) (ParsedURL, *upstream.Options, error) {
	parsed, err := ParseURL(s.URL)
	if err != nil {
		return ParsedURL{}, nil, err
	}

	options, err := s.optionsFor(parsed, logger)
	if err != nil {
		return ParsedURL{}, nil, err
	}

	return parsed, options, nil
}

// optionsFor 在已解析的 URL 上构造 dnsproxy 上游选项。
func (s Spec) optionsFor(parsed ParsedURL, logger *slog.Logger) (*upstream.Options, error) {
	options := &upstream.Options{
		Logger: logger,
		// dnsproxy 的 Timeout 是单次尝试超时；对外的单次交换预算由
		// tracedUpstream.Exchange 的 exchangeWithBudget 强制。
		Timeout: s.effectiveTimeout(),
		RootCAs: s.RootCAs,
	}

	if !parsed.HostIsName {
		return options, nil
	}

	if len(s.Bootstrap) == 0 {
		return nil, fmt.Errorf("上游主机 %q 是域名，必须配置 bootstrap", parsed.Host)
	}

	addrs := make([]netip.Addr, 0, len(s.Bootstrap))
	for _, text := range s.Bootstrap {
		addr, err := netip.ParseAddr(text)
		if err != nil {
			return nil, fmt.Errorf("bootstrap %q 不是 IP 字面量", text)
		}

		addrs = append(addrs, addr)
	}
	options.Bootstrap = upstream.StaticResolver(addrs)

	return options, nil
}

// displayName 返回上游显示名。
func displayName(spec Spec) string {
	if spec.Name != "" {
		return spec.Name
	}

	return canonicalURLText(spec.URL)
}

// parseAddrPort 解析客户端地址，失败时返回零值。
func parseAddrPort(addr string) (ap netip.AddrPort) {
	if addr == "" {
		return netip.AddrPort{}
	}

	parsed, err := netip.ParseAddrPort(addr)
	if err != nil {
		return netip.AddrPort{}
	}

	return parsed
}

// exchangeTraceKey 是 context 中携带交换采集器的键。
type exchangeTraceKey struct{}

// exchangeTrace 采集一次查询中已结束的上游交换。
type exchangeTrace struct {
	mu       sync.Mutex
	attempts []event.Attempt
}

// add 追加一条交换记录。
func (t *exchangeTrace) add(attempt event.Attempt) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.attempts = append(t.attempts, attempt)
}

// snapshot 返回当前已结束交换的快照。
func (t *exchangeTrace) snapshot() (attempts []event.Attempt) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append(attempts, t.attempts...)
}

// tracedUpstream 记录每次真实交换的地址、耗时与错误，并强制单次交换的总预算。
type tracedUpstream struct {
	upstream.Upstream
	timeout time.Duration
}

// type check
var _ upstream.Upstream = (*tracedUpstream)(nil)

// Exchange 实现 upstream.Upstream。
func (u *tracedUpstream) Exchange(ctx context.Context, req *dns.Msg) (resp *dns.Msg, err error) {
	start := time.Now()
	resp, err = exchangeWithBudget(ctx, u.Upstream, req, u.timeout)

	if trace, ok := ctx.Value(exchangeTraceKey{}).(*exchangeTrace); ok {
		trace.add(event.Attempt{
			Upstream:   u.Address(),
			DurationMS: durationMS(time.Since(start)),
			Error:      errorText(err),
		})
	}

	return resp, err
}

// durationMS 把时长转为毫秒浮点。
func durationMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// errorText 返回错误文本，nil 时为空。
func errorText(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
