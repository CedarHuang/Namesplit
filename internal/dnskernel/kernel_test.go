package dnskernel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/mockdns"
)

// discardLogger 返回不输出内容的 logger，避免测试噪音。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testGroup 构造上游组并在测试结束时关闭。
func testGroup(t *testing.T, cfg GroupConfig) *Group {
	t.Helper()

	if cfg.Timeout == 0 {
		cfg.Timeout = time.Second
	}
	if cfg.Mode == "" {
		cfg.Mode = proxy.UpstreamModeLoadBalance
	}

	g, err := NewGroup(cfg, discardLogger())
	if err != nil {
		t.Fatalf("构造上游组 %q: %v", cfg.Name, err)
	}
	t.Cleanup(func() { _ = g.Close() })

	return g
}

// startTestKernel 启动内核并返回监听地址与事件通道。
func startTestKernel(t *testing.T, router *Router, groups map[string]*Group, local LocalLookup) (addr string, events <-chan Event) {
	t.Helper()

	ch := make(chan Event, 32)
	srv, err := NewServer(Config{
		Router:     router,
		Groups:     groups,
		Local:      local,
		Logger:     discardLogger(),
		ListenAddr: "127.0.0.1:0",
		OnEvent:    func(e Event) { ch <- e },
	})
	if err != nil {
		t.Fatalf("构造内核: %v", err)
	}

	addr, err = srv.Start(context.Background())
	if err != nil {
		t.Fatalf("启动内核: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	return addr, ch
}

// nextEvent 从事件通道取一条事件。
func nextEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()

	select {
	case ev := <-events:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("等待查询事件超时")

		return Event{}
	}
}

// exchange 发送一次 DNS 查询。
func exchange(t *testing.T, network, addr, name string, qtype uint16, mutate func(*dns.Msg)) (resp *dns.Msg) {
	t.Helper()

	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(name), qtype)
	if mutate != nil {
		mutate(req)
	}

	client := &dns.Client{Net: network, Timeout: 3 * time.Second}
	resp, _, err := client.Exchange(req, addr)
	if err != nil {
		t.Fatalf("查询 %s %s 失败: %v", name, dns.TypeToString[qtype], err)
	}

	return resp
}

func requireA(t *testing.T, resp *dns.Msg, wantIP string) {
	t.Helper()

	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("期望 NOERROR，实际 %s", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("期望 1 条应答，实际 %d 条: %v", len(resp.Answer), resp.Answer)
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("期望 A 记录，实际 %T", resp.Answer[0])
	}
	if got := a.A.String(); got != wantIP {
		t.Fatalf("期望应答 %s，实际 %s", wantIP, got)
	}
}

func TestRouterMatch(t *testing.T) {
	router, err := NewRouter(DefaultGroup, []Rule{
		{Kind: RuleExact, Pattern: "api.exact.test", Group: "exact"},
		{Kind: RuleWildcard, Pattern: "*.exact.test", Group: "wild"},
		{Kind: RuleWildcard, Pattern: "*.wild.test", Group: "wild"},
		{Kind: RuleExact, Pattern: "onlyv6.test", Group: "v6", QTypes: []uint16{dns.TypeAAAA}},
	})
	if err != nil {
		t.Fatalf("构造路由: %v", err)
	}

	cases := []struct {
		name      string
		qtype     uint16
		wantGroup string
		wantKind  string
	}{
		{"api.exact.test", dns.TypeA, "exact", "exact"},
		{"API.EXACT.TEST.", dns.TypeA, "exact", "exact"},
		{"www.api.exact.test", dns.TypeA, "wild", "wildcard"},
		{"x.wild.test", dns.TypeA, "wild", "wildcard"},
		{"a.b.wild.test", dns.TypeA, "wild", "wildcard"},
		{"wild.test", dns.TypeA, DefaultGroup, "default"},
		{"exact.test", dns.TypeA, DefaultGroup, "default"},
		{"onlyv6.test", dns.TypeAAAA, "v6", "exact"},
		{"onlyv6.test", dns.TypeA, DefaultGroup, "default"},
	}

	for _, tc := range cases {
		route := router.Match(tc.name, tc.qtype)
		if route.Group != tc.wantGroup || route.Kind != tc.wantKind {
			t.Errorf("Match(%q, %s) = %+v，期望 group=%s kind=%s",
				tc.name, dns.TypeToString[tc.qtype], route, tc.wantGroup, tc.wantKind)
		}
	}
}

func TestKernelRoutingAndEventFields(t *testing.T) {
	mockA, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 0, 0, 1), TTL: 60})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockA.Close() })

	mockB, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 0, 0, 2), TTL: 60})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockB.Close() })

	mockC, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 0, 0, 3), TTL: 60})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockC.Close() })

	router, err := NewRouter(DefaultGroup, []Rule{
		{Kind: RuleExact, Pattern: "api.exact.test", Group: "g-exact"},
		{Kind: RuleWildcard, Pattern: "*.exact.test", Group: "g-wild"},
		{Kind: RuleWildcard, Pattern: "*.wild.test", Group: "g-wild"},
	})
	if err != nil {
		t.Fatal(err)
	}

	groups := map[string]*Group{
		"g-exact": testGroup(t, GroupConfig{Name: "g-exact", Upstreams: []string{mockA.URL()}, CacheEnabled: true}),
		"g-wild":  testGroup(t, GroupConfig{Name: "g-wild", Upstreams: []string{mockB.URL()}, CacheEnabled: true}),
		DefaultGroup: testGroup(t, GroupConfig{
			Name: "default", Upstreams: []string{mockC.URL()}, CacheEnabled: true,
		}),
	}

	addr, events := startTestKernel(t, router, groups, nil)

	// 1. 精确规则命中 + 全字段事件（UDP）。
	requireA(t, exchange(t, "udp", addr, "api.exact.test", dns.TypeA, nil), "10.0.0.1")
	ev := nextEvent(t, events)
	if ev.Source != SourceUpstream || ev.CacheHit {
		t.Errorf("首次查询来源 = %q cacheHit=%v，期望 upstream", ev.Source, ev.CacheHit)
	}
	if ev.Domain != "api.exact.test." || ev.QType != "A" {
		t.Errorf("域名/类型字段错误: %q %q", ev.Domain, ev.QType)
	}
	if ev.RuleKind != "exact" || ev.Rule != "api.exact.test" || ev.Group != "g-exact" {
		t.Errorf("规则字段错误: %+v", ev)
	}
	if ev.Protocol != "udp" {
		t.Errorf("协议字段 = %q，期望 udp", ev.Protocol)
	}
	if ev.Rcode != dns.RcodeSuccess || ev.RcodeName != "NOERROR" {
		t.Errorf("RCODE 字段错误: %d %s", ev.Rcode, ev.RcodeName)
	}
	if len(ev.Answers) != 1 || ev.TTL != 60 {
		t.Errorf("应答摘要/TTL 字段错误: answers=%v ttl=%d", ev.Answers, ev.TTL)
	}
	if ev.DurationMS <= 0 {
		t.Errorf("耗时字段 = %v，期望 > 0", ev.DurationMS)
	}
	if ev.Upstream != mockA.Addr() || !strings.Contains(ev.Upstream, "127.0.0.1") {
		t.Errorf("上游字段 = %q，期望 %q", ev.Upstream, mockA.Addr())
	}
	if len(ev.Attempts) == 0 {
		t.Errorf("缺少上游交换明细")
	}

	// 2. 同查询命中该组缓存。
	requireA(t, exchange(t, "udp", addr, "api.exact.test", dns.TypeA, nil), "10.0.0.1")
	ev = nextEvent(t, events)
	if ev.Source != SourceCache || !ev.CacheHit {
		t.Errorf("第二次查询来源 = %q cacheHit=%v，期望 cache", ev.Source, ev.CacheHit)
	}
	if ev.Upstream != mockA.Addr() {
		t.Errorf("缓存命中时上游字段 = %q，期望缓存条目记录 %q", ev.Upstream, mockA.Addr())
	}
	if mockA.SeenCount() != 1 {
		t.Errorf("mock A 实际被查询 %d 次，期望 1 次（第二次应命中缓存）", mockA.SeenCount())
	}

	// 3. 通配规则命中。
	requireA(t, exchange(t, "udp", addr, "x.wild.test", dns.TypeA, nil), "10.0.0.2")
	ev = nextEvent(t, events)
	if ev.RuleKind != "wildcard" || ev.Group != "g-wild" || ev.Rule != "*.wild.test" {
		t.Errorf("通配规则字段错误: %+v", ev)
	}

	// 4. apex 不匹配通配，落默认组。
	requireA(t, exchange(t, "udp", addr, "wild.test", dns.TypeA, nil), "10.0.0.3")
	ev = nextEvent(t, events)
	if ev.RuleKind != "default" || ev.Group != DefaultGroup {
		t.Errorf("默认组字段错误: %+v", ev)
	}

	// 5. TCP 客户端协议可观测。
	requireA(t, exchange(t, "tcp", addr, "tcp.wild.test", dns.TypeA, nil), "10.0.0.2")
	ev = nextEvent(t, events)
	if ev.Protocol != "tcp" {
		t.Errorf("TCP 查询协议字段 = %q，期望 tcp", ev.Protocol)
	}
}

func TestKernelECSStrippedAndDOPassed(t *testing.T) {
	mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 1, 2, 3)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	router, err := NewRouter(DefaultGroup, nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*Group{
		DefaultGroup: testGroup(t, GroupConfig{Name: DefaultGroup, Upstreams: []string{mock.URL()}}),
	}
	addr, events := startTestKernel(t, router, groups, nil)

	exchange(t, "udp", addr, "ecs-probe.test", dns.TypeA, func(m *dns.Msg) {
		m.SetEdns0(1232, true)
		opt := m.IsEdns0()
		opt.Option = append(opt.Option, &dns.EDNS0_SUBNET{
			Code:          dns.EDNS0SUBNET,
			Family:        1,
			SourceNetmask: 24,
			SourceScope:   0,
			Address:       net.ParseIP("203.0.113.0").To4(),
		})
	})
	ev := nextEvent(t, events)
	if ev.Rcode != dns.RcodeSuccess {
		t.Fatalf("查询失败: %+v", ev)
	}

	seen := mock.Seen()
	if len(seen) != 1 {
		t.Fatalf("mock 收到 %d 次查询，期望 1 次", len(seen))
	}
	opt := seen[0].IsEdns0()
	if opt == nil {
		t.Fatal("上游收到的查询没有 EDNS0，期望保留 DO 位")
	}
	if !opt.Do() {
		t.Error("上游收到的查询 DO 位丢失")
	}
	for _, o := range opt.Option {
		if o.Option() == dns.EDNS0SUBNET {
			t.Fatalf("上游收到的查询仍带 ECS 选项，默认剥离失败: %v", o)
		}
	}
}

func TestKernelCacheIsolationBetweenGroups(t *testing.T) {
	mockA, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 2, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockA.Close() })

	mockB, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 2, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockB.Close() })

	groupA := testGroup(t, GroupConfig{Name: "A", Upstreams: []string{mockA.URL()}, CacheEnabled: true})
	groupB := testGroup(t, GroupConfig{Name: "B", Upstreams: []string{mockB.URL()}, CacheEnabled: true})

	newReq := func() *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion("isolation.test.", dns.TypeA)

		return m
	}

	resA1 := groupA.Resolve(context.Background(), newReq(), proxy.ProtoUDP, "127.0.0.1:1234")
	if resA1.Error != nil {
		t.Fatalf("组 A 首次解析失败: %v", resA1.Error)
	}
	resA2 := groupA.Resolve(context.Background(), newReq(), proxy.ProtoUDP, "127.0.0.1:1234")
	if !resA2.CacheHit {
		t.Fatalf("组 A 第二次未命中本组缓存: %+v", resA2)
	}

	resB1 := groupB.Resolve(context.Background(), newReq(), proxy.ProtoUDP, "127.0.0.1:1234")
	if resB1.Error != nil {
		t.Fatalf("组 B 首次解析失败: %v", resB1.Error)
	}
	if resB1.CacheHit {
		t.Fatalf("组 B 首次命中缓存，说明缓存跨组共享: %+v", resB1)
	}

	resA3 := groupA.Resolve(context.Background(), newReq(), proxy.ProtoUDP, "127.0.0.1:1234")
	if !resA3.CacheHit {
		t.Fatalf("再次查询组 A 未命中缓存: %+v", resA3)
	}

	ipA1 := firstA(t, resA1.Response)
	ipB1 := firstA(t, resB1.Response)
	if ipA1 != "10.2.0.1" || ipB1 != "10.2.0.2" || ipA1 == ipB1 {
		t.Fatalf("缓存串组: A=%s B=%s", ipA1, ipB1)
	}
	if mockA.SeenCount() != 1 || mockB.SeenCount() != 1 {
		t.Fatalf("上游被查询次数异常: A=%d B=%d，期望各 1 次", mockA.SeenCount(), mockB.SeenCount())
	}
}

func TestKernelUpstreamFailureProducesSERVFAIL(t *testing.T) {
	router, err := NewRouter(DefaultGroup, nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*Group{
		DefaultGroup: testGroup(t, GroupConfig{
			Name:      DefaultGroup,
			Upstreams: []string{"tcp://127.0.0.1:1"},
			Timeout:   300 * time.Millisecond,
			// 缓存关闭，避免负缓存干扰。
		}),
	}
	addr, events := startTestKernel(t, router, groups, nil)

	resp := exchange(t, "udp", addr, "fail.test", dns.TypeA, nil)
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("上游全失败时应答 RCODE = %s，期望 SERVFAIL", dns.RcodeToString[resp.Rcode])
	}
	ev := nextEvent(t, events)
	if ev.Error == "" {
		t.Error("上游失败事件缺少错误信息")
	}
	if ev.Source != SourceUpstream {
		t.Errorf("失败事件来源 = %q，期望 upstream", ev.Source)
	}
}

func TestKernelLocalSourceAndNODATA(t *testing.T) {
	mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 3, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	router, err := NewRouter(DefaultGroup, nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*Group{
		DefaultGroup: testGroup(t, GroupConfig{Name: DefaultGroup, Upstreams: []string{mock.URL()}}),
	}

	local := func(q dns.Question) (answers []dns.RR, nameExists bool) {
		if strings.EqualFold(q.Name, "nas.home.") {
			if q.Qtype != dns.TypeA {
				return nil, true
			}

			return []dns.RR{&dns.A{
				Hdr: dns.RR_Header{
					Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60,
				},
				A: net.IPv4(192, 168, 1, 10),
			}}, true
		}

		return nil, false
	}

	addr, events := startTestKernel(t, router, groups, local)

	requireA(t, exchange(t, "udp", addr, "nas.home", dns.TypeA, nil), "192.168.1.10")
	ev := nextEvent(t, events)
	if ev.Source != SourceLocal || ev.RuleKind != "local" {
		t.Errorf("本地应答来源字段错误: %+v", ev)
	}
	if ev.Upstream != "" || len(ev.Attempts) != 0 {
		t.Errorf("本地应答不应有上游字段: %+v", ev)
	}

	resp := exchange(t, "udp", addr, "nas.home", dns.TypeAAAA, nil)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 {
		t.Errorf("AAAA 应返回 NODATA（NOERROR 空应答），实际 rcode=%s answers=%v",
			dns.RcodeToString[resp.Rcode], resp.Answer)
	}
	ev = nextEvent(t, events)
	if ev.Source != SourceLocal {
		t.Errorf("NODATA 来源 = %q，期望 local", ev.Source)
	}
	if mock.SeenCount() != 0 {
		t.Errorf("本地记录命中时不应查询上游，实际查询 %d 次", mock.SeenCount())
	}
}

func TestKernelFallbackAndParallel(t *testing.T) {
	primary1, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 4, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = primary1.Close() })

	primary2, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 4, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = primary2.Close() })

	fallback, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 4, 9, 9)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fallback.Close() })

	// 主备：主上游不可用，fallback 应答。
	router, err := NewRouter("fallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*Group{
		"fallback": testGroup(t, GroupConfig{
			Name:      "fallback",
			Upstreams: []string{"tcp://127.0.0.1:1"},
			Fallbacks: []string{fallback.URL()},
			Timeout:   500 * time.Millisecond,
		}),
	}
	addr, events := startTestKernel(t, router, groups, nil)

	requireA(t, exchange(t, "udp", addr, "fb.test", dns.TypeA, nil), "10.4.9.9")
	ev := nextEvent(t, events)
	if ev.Upstream != fallback.Addr() {
		t.Errorf("主备切换后上游字段 = %q，期望 fallback %q", ev.Upstream, fallback.Addr())
	}
	sawFallback, sawPrimaryError := false, false
	for _, attempt := range ev.Attempts {
		if attempt.Fallback {
			sawFallback = true
		}
		if !attempt.Fallback && attempt.Error != "" {
			sawPrimaryError = true
		}
	}
	if !sawFallback || !sawPrimaryError {
		t.Errorf("主备尝试明细不完整: %+v", ev.Attempts)
	}

	// 并发模式：两个主上游都应被并发尝试。
	routerParallel, err := NewRouter("parallel", nil)
	if err != nil {
		t.Fatal(err)
	}
	groupsParallel := map[string]*Group{
		"parallel": testGroup(t, GroupConfig{
			Name:      "parallel",
			Upstreams: []string{primary1.URL(), primary2.URL()},
			Mode:      proxy.UpstreamModeParallel,
		}),
	}
	addrParallel, eventsParallel := startTestKernel(t, routerParallel, groupsParallel, nil)

	parallelResp := exchange(t, "udp", addrParallel, "parallel.test", dns.TypeA, nil)
	if parallelResp.Rcode != dns.RcodeSuccess || len(parallelResp.Answer) != 1 {
		t.Fatalf("并发模式查询失败: %v %v", parallelResp.Rcode, parallelResp.Answer)
	}
	ip := firstA(t, parallelResp)
	if ip != "10.4.0.1" && ip != "10.4.0.2" {
		t.Fatalf("并发模式应答 IP = %s，期望来自两个主上游之一", ip)
	}
	ev = nextEvent(t, eventsParallel)
	// dnsproxy 的 parallel 模式在第一个成功应答后立即返回，落败上游的交换
	// 可能在事件快照之后才结束，因此这里只要求至少记录到胜出上游。
	if len(ev.Attempts) == 0 {
		t.Errorf("并发模式没有记录任何上游尝试: %+v", ev)
	}
	for _, attempt := range ev.Attempts {
		if attempt.Error != "" {
			t.Errorf("并发模式上游尝试出现错误: %+v", ev.Attempts)
		}
	}
}

func TestEncryptedUpstreamsAndTLSVerification(t *testing.T) {
	plain, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 5, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	enc, err := mockdns.StartEncrypted(ctx, plain.URL())
	if err != nil {
		t.Fatalf("启动加密 mock: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = enc.Close(shutdownCtx)
	})

	router, err := NewRouter("doh", []Rule{
		{Kind: RuleExact, Pattern: "dot.test", Group: "dot"},
		{Kind: RuleExact, Pattern: "doq.test", Group: "doq"},
		{Kind: RuleExact, Pattern: "untrusted.test", Group: "untrusted"},
		{Kind: RuleExact, Pattern: "insecure.test", Group: "insecure"},
	})
	if err != nil {
		t.Fatal(err)
	}

	groups := map[string]*Group{
		"doh": testGroup(t, GroupConfig{
			Name: "doh", Upstreams: []string{enc.DoHURL()}, RootCAs: enc.RootCAs(), Timeout: 3 * time.Second,
		}),
		"dot": testGroup(t, GroupConfig{
			Name: "dot", Upstreams: []string{enc.DoTURL()}, RootCAs: enc.RootCAs(), Timeout: 3 * time.Second,
		}),
		"doq": testGroup(t, GroupConfig{
			Name: "doq", Upstreams: []string{enc.DoQURL()}, RootCAs: enc.RootCAs(), Timeout: 3 * time.Second,
		}),
		// 默认系统根证书：自签证书校验失败。
		"untrusted": testGroup(t, GroupConfig{
			Name: "untrusted", Upstreams: []string{enc.DoHURL()}, Timeout: 3 * time.Second,
		}),
		// 显式关闭校验：仅用于验证高级开关可用。
		"insecure": testGroup(t, GroupConfig{
			Name: "insecure", Upstreams: []string{enc.DoHURL()}, InsecureSkipVerify: true, Timeout: 3 * time.Second,
		}),
	}
	if enc.DoH3URL() != "" {
		groups["doh3"] = testGroup(t, GroupConfig{
			Name: "doh3", Upstreams: []string{enc.DoH3URL()}, RootCAs: enc.RootCAs(), Timeout: 3 * time.Second,
		})
	}

	// DoH3 规则按实际启动结果注册（启动失败时跳过该协议）。
	rules := []Rule{
		{Kind: RuleExact, Pattern: "dot.test", Group: "dot"},
		{Kind: RuleExact, Pattern: "doq.test", Group: "doq"},
	}
	if _, ok := groups["doh3"]; ok {
		rules = append(rules, Rule{Kind: RuleExact, Pattern: "doh3.test", Group: "doh3"})
	}
	router, err = NewRouter("doh", append(rules,
		Rule{Kind: RuleExact, Pattern: "untrusted.test", Group: "untrusted"},
		Rule{Kind: RuleExact, Pattern: "insecure.test", Group: "insecure"},
	))
	if err != nil {
		t.Fatal(err)
	}

	addr, events := startTestKernel(t, router, groups, nil)

	// DoH / DoT / DoQ / DoH3 均应成功。
	for _, name := range []string{"doh.test", "dot.test", "doq.test", "doh3.test"} {
		group := strings.TrimSuffix(name, ".test")
		if groups[group] == nil {
			t.Logf("跳过 %s：本地 mock 的 DoH3 未启动（%v）", name, enc.H3Error())
			continue
		}

		resp := exchange(t, "udp", addr, name, dns.TypeA, nil)
		requireA(t, resp, "10.5.0.1")
		ev := nextEvent(t, events)
		if ev.Rcode != dns.RcodeSuccess || ev.Error != "" {
			t.Errorf("%s 加密上游失败: %+v", name, ev)
		}
	}
	// 未配置 RootCAs 时必须证书校验失败。
	resp := exchange(t, "udp", addr, "untrusted.test", dns.TypeA, nil)
	if resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("自签证书默认校验应失败并返回 SERVFAIL，实际 %s", dns.RcodeToString[resp.Rcode])
	}
	ev := nextEvent(t, events)
	if ev.Error == "" {
		t.Error("证书校验失败事件缺少错误信息")
	}

	// 显式关闭校验后应成功。
	requireA(t, exchange(t, "udp", addr, "insecure.test", dns.TypeA, nil), "10.5.0.1")
	ev = nextEvent(t, events)
	if ev.Error != "" {
		t.Errorf("InsecureSkipVerify 解析失败: %+v", ev)
	}
}

func TestKernelLoadBalanceDistribution(t *testing.T) {
	mockA, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 7, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockA.Close() })

	mockB, err := mockdns.StartPlain(mockdns.PlainConfig{A: net.IPv4(10, 7, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockB.Close() })

	router, err := NewRouter(DefaultGroup, nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*Group{
		DefaultGroup: testGroup(t, GroupConfig{
			Name:      DefaultGroup,
			Upstreams: []string{mockA.URL(), mockB.URL()},
			Mode:      proxy.UpstreamModeLoadBalance,
		}),
	}
	addr, events := startTestKernel(t, router, groups, nil)

	const queries = 40
	for i := 0; i < queries; i++ {
		name := fmt.Sprintf("lb-%d.test", i)
		requireAnyA(t, exchange(t, "udp", addr, name, dns.TypeA, nil))
		nextEvent(t, events)
	}

	// dnsproxy 的 load_balance 是按 RTT 权重的随机选择，不是严格轮询；
	// 这里只验证两个上游都被使用过，不约束分布比例。
	if mockA.SeenCount() == 0 || mockB.SeenCount() == 0 {
		t.Fatalf("load_balance 未使用全部上游: A=%d B=%d", mockA.SeenCount(), mockB.SeenCount())
	}
	t.Logf("load_balance %d 次查询分布: A=%d B=%d", queries, mockA.SeenCount(), mockB.SeenCount())
}

// requireAnyA 断言应答成功且至少有一条 A 记录。
func requireAnyA(t *testing.T, resp *dns.Msg) {
	t.Helper()

	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) == 0 {
		t.Fatalf("期望成功应答，实际 rcode=%s answers=%v", dns.RcodeToString[resp.Rcode], resp.Answer)
	}
}

func firstA(t *testing.T, msg *dns.Msg) string {
	t.Helper()

	if msg == nil || len(msg.Answer) == 0 {
		t.Fatal("应答为空")
	}
	a, ok := msg.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("期望 A 记录，实际 %T", msg.Answer[0])
	}

	return a.A.String()
}
