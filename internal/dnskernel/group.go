package dnskernel

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
)

// GroupConfig 描述一个上游组。
//
// dnsproxy 的代理级策略（UpstreamMode）无法按请求切换，所以每个上游组持有
// 一个独立的 proxy.Proxy：组内策略、健康相关的 RTT 统计、缓存互不干扰。
type GroupConfig struct {
	// Name 是上游组名。
	Name string
	// Upstreams 是主上游列表，格式遵循 dnsproxy upstream.AddressToUpstream：
	// `1.1.1.1`、`udp://...`、`tcp://...`、`tls://...`、`https://.../dns-query`、
	// `quic://...`、`h3://...`。
	Upstreams []string
	// Fallbacks 是备用上游列表；主上游全部失败时使用（对应 dnsproxy fallback）。
	Fallbacks []string
	// Mode 是组内策略：load_balance/parallel/fastest_addr。空值使用 load_balance。
	Mode proxy.UpstreamMode
	// Timeout 是单个上游交换超时。
	Timeout time.Duration
	// CacheEnabled 表示启用该组独立缓存。
	CacheEnabled bool
	// CacheSizeBytes 是该组缓存字节上限；0 使用 dnsproxy 默认值。
	CacheSizeBytes int
	// RootCAs 是校验上游证书用的根证书池；nil 表示系统根证书（默认校验）。
	RootCAs *x509.CertPool
	// InsecureSkipVerify 关闭上游证书校验。仅在用户显式选择时才应开启。
	InsecureSkipVerify bool
	// Bootstrap 用于解析上游主机名。nil 时 dnsproxy 使用系统解析器；
	// 调用方应配置固定 IP，避免依赖系统 DNS 造成解析环路。
	Bootstrap upstream.Resolver
}

// Group 是一个上游组解析器，封装 dnsproxy 的协议/策略/缓存能力。
type Group struct {
	name  string
	proxy *proxy.Proxy
	uc    *proxy.UpstreamConfig
	fuc   *proxy.UpstreamConfig
}

// NewGroup 构造上游组。返回后即可并发调用 Resolve，无需启动监听器。
func NewGroup(cfg GroupConfig, logger *slog.Logger) (g *Group, err error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("上游组名不能为空")
	}
	if len(cfg.Upstreams) == 0 {
		return nil, fmt.Errorf("上游组 %q 没有主上游", cfg.Name)
	}

	uc, err := buildUpstreamConfig(cfg.Upstreams, cfg, logger, false)
	if err != nil {
		return nil, fmt.Errorf("上游组 %q: %w", cfg.Name, err)
	}

	var fuc *proxy.UpstreamConfig
	if len(cfg.Fallbacks) > 0 {
		fuc, err = buildUpstreamConfig(cfg.Fallbacks, cfg, logger, true)
		if err != nil {
			uc.Close()

			return nil, fmt.Errorf("上游组 %q 备用上游: %w", cfg.Name, err)
		}
	}

	p, err := proxy.New(&proxy.Config{
		Logger:         logger,
		UpstreamConfig: uc,
		Fallbacks:      fuc,
		UpstreamMode:   cfg.Mode,
		CacheEnabled:   cfg.CacheEnabled,
		CacheSizeBytes: cfg.CacheSizeBytes,
		// 默认剥离客户端带入的 ECS；dnsproxy 自身不剥离，在路由前由 stripECS 处理。
		EnableEDNSClientSubnet: false,
	})
	if err != nil {
		uc.Close()
		if fuc != nil {
			fuc.Close()
		}

		return nil, fmt.Errorf("构造上游组 %q 的 dnsproxy: %w", cfg.Name, err)
	}

	return &Group{name: cfg.Name, proxy: p, uc: uc, fuc: fuc}, nil
}

// buildUpstreamConfig 把配置里的上游字符串转成 dnsproxy 的 UpstreamConfig。
// 每个上游都包一层 queryTrace 采集器：dnsproxy 的 QueryStatistics 在并发/负载
// 均衡模式下只保留胜出上游，落败的尝试需要自己记录。
func buildUpstreamConfig(
	specs []string,
	cfg GroupConfig,
	logger *slog.Logger,
	isFallback bool,
) (uc *proxy.UpstreamConfig, err error) {
	ups := make([]upstream.Upstream, 0, len(specs))
	for _, spec := range specs {
		var u upstream.Upstream
		u, err = upstream.AddressToUpstream(spec, &upstream.Options{
			Logger:             logger,
			Timeout:            cfg.Timeout,
			RootCAs:            cfg.RootCAs,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
			Bootstrap:          cfg.Bootstrap,
		})
		if err != nil {
			for _, opened := range ups {
				opened.Close()
			}

			return nil, fmt.Errorf("解析上游 %q: %w", spec, err)
		}

		ups = append(ups, &tracedUpstream{Upstream: u, fallback: isFallback})
	}

	return &proxy.UpstreamConfig{Upstreams: ups}, nil
}

// Name 返回上游组名。
func (g *Group) Name() string {
	return g.name
}

// Resolution 是一次上游组解析的完整运行时结果。
type Resolution struct {
	// Response 是上游组返回的应答；错误时可能为 nil。
	Response *dns.Msg
	// Upstream 是实际应答的上游地址；缓存命中时为空。
	Upstream string
	// CacheHit 表示命中该组缓存。
	CacheHit bool
	// Attempts 是 dnsproxy 记录的上游交换明细（含并发/回退的每个上游）。
	Attempts []Attempt
	// Error 是解析错误。
	Error error
}

// Resolve 通过该上游组解析 req。proto 必须是客户端到本机内核的协议，
// dnsproxy 用它决定应答截断与缓存行为。
//
// 返回的 Response 归调用方所有，可直接写回客户端。
func (g *Group) Resolve(
	ctx context.Context,
	req *dns.Msg,
	proto proxy.Proto,
	clientAddr string,
) (res Resolution) {
	trace := &queryTrace{}
	ctx = context.WithValue(ctx, queryTraceKey{}, trace)

	dctx := &proxy.DNSContext{Proto: proto, Req: req, Addr: parseAddrPort(clientAddr)}

	res.Error = g.proxy.Resolve(ctx, dctx)
	res.Response = dctx.Res
	if dctx.Upstream != nil {
		res.Upstream = dctx.Upstream.Address()
	}

	res.Attempts = trace.snapshot()

	// 缓存命中时不会发生上游交换，改用 dnsproxy 的统计补出「缓存来源地址」。
	if stats := dctx.QueryStatistics(); stats != nil {
		for _, st := range stats.Main() {
			if st.IsCached {
				res.CacheHit = true
				if res.Upstream == "" {
					res.Upstream = st.Address
				}
				if len(res.Attempts) == 0 {
					res.Attempts = []Attempt{{Upstream: st.Address, Cached: true}}
				}
			}
		}
	}

	return res
}

// Close 关闭上游连接与缓存。
func (g *Group) Close() (err error) {
	if g.fuc != nil {
		err = g.fuc.Close()
	}

	if ucErr := g.uc.Close(); err == nil {
		err = ucErr
	}

	return err
}

// durationMS 把时长转为毫秒浮点。
func durationMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// errString 返回错误文本，nil 时为空。
func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

// queryTraceKey 是 context 中携带逐查询上游交换采集器的键。
type queryTraceKey struct{}

// queryTrace 采集一次查询中真实发生的上游交换。
type queryTrace struct {
	mu       sync.Mutex
	attempts []Attempt
}

// add 追加一条上游交换记录。
func (t *queryTrace) add(a Attempt) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.attempts = append(t.attempts, a)
}

// snapshot 返回当前采集快照。
func (t *queryTrace) snapshot() (attempts []Attempt) {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append(attempts, t.attempts...)
}

// tracedUpstream 是 upstream.Upstream 的采集包装：记录每次真实交换的地址、
// 耗时与错误。dnsproxy 的 QueryStatistics 在并行/负载均衡模式下只暴露胜出上游，
// 因此落败的尝试由本包装记录。
type tracedUpstream struct {
	upstream.Upstream
	fallback bool
}

// type check
var _ upstream.Upstream = (*tracedUpstream)(nil)

// Exchange 实现 upstream.Upstream。
func (u *tracedUpstream) Exchange(ctx context.Context, req *dns.Msg) (resp *dns.Msg, err error) {
	start := time.Now()
	resp, err = u.Upstream.Exchange(ctx, req)

	if trace, ok := ctx.Value(queryTraceKey{}).(*queryTrace); ok {
		trace.add(Attempt{
			Upstream:   u.Address(),
			DurationMS: durationMS(time.Since(start)),
			Fallback:   u.fallback,
			Error:      errString(err),
		})
	}

	return resp, err
}
