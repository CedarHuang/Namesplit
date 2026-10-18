package upstream

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/mockdns"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestParseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		scheme     string
		host       string
		port       int
		hostIsName bool
		wantErr    bool
	}{
		{name: "裸 IPv4", raw: "1.1.1.1", scheme: "udp", host: "1.1.1.1", port: 53},
		{name: "裸 IPv4 带端口", raw: "1.1.1.1:5353", scheme: "udp", host: "1.1.1.1", port: 5353},
		{name: "裸 IPv6", raw: "::1", scheme: "udp", host: "::1", port: 53},
		{name: "UDP", raw: "udp://9.9.9.9", scheme: "udp", host: "9.9.9.9", port: 53},
		{name: "TCP", raw: "tcp://9.9.9.9:5353", scheme: "tcp", host: "9.9.9.9", port: 5353},
		{name: "DoT 默认端口", raw: "tls://10.0.0.53", scheme: "tls", host: "10.0.0.53", port: 853},
		{name: "DoQ 默认端口", raw: "quic://10.0.0.53", scheme: "quic", host: "10.0.0.53", port: 853},
		{name: "DoH 域名", raw: "https://dns.example/dns-query", scheme: "https", host: "dns.example", port: 443, hostIsName: true},
		{name: "DoH3 域名", raw: "h3://dns.example/dns-query", scheme: "h3", host: "dns.example", port: 443, hostIsName: true},
		{name: "DoH IPv6", raw: "https://[2001:db8::1]/dns-query", scheme: "https", host: "2001:db8::1", port: 443},
		{name: "DoH 缺路径", raw: "https://dns.example", wantErr: true},
		{name: "DoH 路径为根", raw: "https://dns.example/", wantErr: true},
		{name: "未知协议", raw: "http://1.1.1.1/dns-query", wantErr: true},
		{name: "裸域名", raw: "dns.example:53", wantErr: true},
		{name: "空地址", raw: "", wantErr: true},
		{name: "缺主机", raw: "udp://", wantErr: true},
		{name: "端口越界", raw: "udp://1.1.1.1:70000", wantErr: true},
		{name: "非法域名", raw: "https://a..b/dns-query", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := ParseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseURL(%q) = %+v, 期望错误", tt.raw, parsed)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseURL(%q) 返回错误: %v", tt.raw, err)
			}
			if parsed.Scheme != tt.scheme || parsed.Host != tt.host || parsed.Port != tt.port {
				t.Fatalf("ParseURL(%q) = %+v", tt.raw, parsed)
			}
			if parsed.HostIsName != tt.hostIsName {
				t.Fatalf("ParseURL(%q).HostIsName = %v, 期望 %v", tt.raw, parsed.HostIsName, tt.hostIsName)
			}
		})
	}
}

func TestParseURLNormalizesHostAndURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		scheme     string
		host       string
		port       int
		hostIsName bool
		wantURL    string
	}{
		{
			name: "尾点与大小写", raw: "UDP://DNS.Example.:53",
			scheme: "udp", host: "dns.example", port: 53, hostIsName: true,
			wantURL: "udp://dns.example:53",
		},
		{
			name: "IDNA 主机", raw: "udp://BÜCHER.Example.:53",
			scheme: "udp", host: "xn--bcher-kva.example", port: 53, hostIsName: true,
			wantURL: "udp://xn--bcher-kva.example:53",
		},
		{
			name: "DoH 保留路径", raw: "https://DNS.Example.:8443/dns-query",
			scheme: "https", host: "dns.example", port: 8443, hostIsName: true,
			wantURL: "https://dns.example:8443/dns-query",
		},
		{
			name: "DoH3 保留路径", raw: "h3://DNS.Example.:8443/dns-query",
			scheme: "h3", host: "dns.example", port: 8443, hostIsName: true,
			wantURL: "h3://dns.example:8443/dns-query",
		},
		{
			name: "IPv6 DoH", raw: "https://[2001:DB8::1]/dns-query",
			scheme: "https", host: "2001:db8::1", port: 443,
			wantURL: "https://[2001:db8::1]:443/dns-query",
		},
		{
			name: "IPv6 UDP 带端口", raw: "udp://[::1]:5353",
			scheme: "udp", host: "::1", port: 5353,
			wantURL: "udp://[::1]:5353",
		},
		{
			name: "裸 IP 补默认协议", raw: "1.1.1.1",
			scheme: "udp", host: "1.1.1.1", port: 53,
			wantURL: "udp://1.1.1.1:53",
		},
		{
			name: "DoT 默认端口", raw: "tls://10.0.0.53",
			scheme: "tls", host: "10.0.0.53", port: 853,
			wantURL: "tls://10.0.0.53:853",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := ParseURL(tt.raw)
			if err != nil {
				t.Fatalf("ParseURL(%q) 返回错误: %v", tt.raw, err)
			}
			if parsed.Scheme != tt.scheme || parsed.Host != tt.host || parsed.Port != tt.port || parsed.HostIsName != tt.hostIsName {
				t.Fatalf("ParseURL(%q) = %+v", tt.raw, parsed)
			}
			if got := parsed.URL(); got != tt.wantURL {
				t.Fatalf("ParsedURL.URL() = %q, 期望 %q", got, tt.wantURL)
			}
		})
	}
}

func TestNewGroupRequiresBootstrapForHostname(t *testing.T) {
	t.Parallel()

	if _, err := NewGroup("g", []Spec{{URL: "udp://dns.example:53"}}, discardLogger()); err == nil {
		t.Fatal("主机名为域名且无 bootstrap 时应报错")
	}

	group, err := NewGroup("g", []Spec{{
		URL:       "udp://dns.example:53",
		Bootstrap: []string{"127.0.0.1"},
	}}, discardLogger())
	if err != nil {
		t.Fatalf("带 bootstrap 的组构建失败: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })
}

// 校验通过的上游必须能直接构造成运行期上游：dnsproxy 不归一尾点/大小写主机，
// 配置原文直接传入会在 NewGroup 阶段报非法地址。
func TestNewGroupAcceptsNonCanonicalHost(t *testing.T) {
	t.Parallel()

	group, err := NewGroup("g", []Spec{{
		URL:       "udp://DNS.Example.:53",
		Bootstrap: []string{"127.0.0.1"},
	}}, discardLogger())
	if err != nil {
		t.Fatalf("带尾点与大写主机的组构建失败: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })
}

func TestGroupResolvePlainAndCache(t *testing.T) {
	t.Parallel()

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	group, err := NewGroup("g", []Spec{{ID: "mock", Name: "Mock", URL: mock.URL(), Timeout: 2 * time.Second}}, discardLogger())
	if err != nil {
		t.Fatalf("构造上游组: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })

	result := group.Resolve(context.Background(), newQuery("example.test"), proxy.ProtoUDP, "127.0.0.1:12345")
	if result.Error != nil || result.Response == nil {
		t.Fatalf("解析失败: %v", result.Error)
	}
	if result.CacheHit {
		t.Fatal("首次解析不应命中缓存")
	}
	if result.UpstreamName != "Mock" {
		t.Fatalf("UpstreamName = %q, 期望 Mock", result.UpstreamName)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %d, 期望 1", len(result.Attempts))
	}
	if got := aValues(result.Response); len(got) != 1 || got[0] != "192.0.2.1" {
		t.Fatalf("应答 = %v", got)
	}

	cached := group.Resolve(context.Background(), newQuery("example.test"), proxy.ProtoUDP, "127.0.0.1:12345")
	if cached.Error != nil || cached.Response == nil {
		t.Fatalf("缓存解析失败: %v", cached.Error)
	}
	if !cached.CacheHit {
		t.Fatal("第二次解析应命中缓存")
	}
	if len(cached.Attempts) != 0 {
		t.Fatalf("缓存命中不应有 attempts: %+v", cached.Attempts)
	}
	if cached.Upstream == "" {
		t.Fatal("缓存命中应记录缓存条目的上游地址")
	}
}

func TestGroupConcurrentFastestWins(t *testing.T) {
	t.Parallel()

	slow, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 2}, TTL: 60, Delay: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("启动慢 mock: %v", err)
	}
	t.Cleanup(func() { _ = slow.Close() })

	fast, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动快 mock: %v", err)
	}
	t.Cleanup(func() { _ = fast.Close() })

	group, err := NewGroup("g", []Spec{
		{ID: "slow", URL: slow.URL(), Timeout: 2 * time.Second},
		{ID: "fast", URL: fast.URL(), Timeout: 2 * time.Second},
	}, discardLogger())
	if err != nil {
		t.Fatalf("构造上游组: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })

	start := time.Now()
	result := group.Resolve(context.Background(), newQuery("example.test"), proxy.ProtoUDP, "127.0.0.1:12345")
	if result.Error != nil || result.Response == nil {
		t.Fatalf("解析失败: %v", result.Error)
	}
	if elapsed := time.Since(start); elapsed >= slowDelay {
		t.Fatalf("并发解析耗时 %v，未先返回快上游", elapsed)
	}

	if got := aValues(result.Response); got[0] != "192.0.2.1" {
		t.Fatalf("应答 = %v, 期望快上游答案", got)
	}
}

const slowDelay = 250 * time.Millisecond

func TestGroupUnavailableMemberDoesNotBlock(t *testing.T) {
	t.Parallel()

	unavailable, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 2}, TTL: 60, Delay: time.Second})
	if err != nil {
		t.Fatalf("启动慢 mock: %v", err)
	}
	t.Cleanup(func() { _ = unavailable.Close() })

	healthy, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动健康 mock: %v", err)
	}
	t.Cleanup(func() { _ = healthy.Close() })

	group, err := NewGroup("g", []Spec{
		{ID: "unavailable", URL: unavailable.URL(), Timeout: 100 * time.Millisecond},
		{ID: "healthy", URL: healthy.URL(), Timeout: 2 * time.Second},
	}, discardLogger())
	if err != nil {
		t.Fatalf("构造上游组: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })

	result := group.Resolve(context.Background(), newQuery("example.test"), proxy.ProtoUDP, "127.0.0.1:12345")
	if result.Error != nil || result.Response == nil {
		t.Fatalf("单成员不可用导致解析失败: %v", result.Error)
	}
	if got := aValues(result.Response); got[0] != "192.0.2.1" {
		t.Fatalf("应答 = %v", got)
	}
}

func TestManagerCacheIsolation(t *testing.T) {
	t.Parallel()

	mockA, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock A: %v", err)
	}
	t.Cleanup(func() { _ = mockA.Close() })

	mockB, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 2}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock B: %v", err)
	}
	t.Cleanup(func() { _ = mockB.Close() })

	manager, err := NewManager(
		map[string]Spec{
			"a": {ID: "a", URL: mockA.URL(), Timeout: 2 * time.Second},
			"b": {ID: "b", URL: mockB.URL(), Timeout: 2 * time.Second},
		},
		map[string][]string{"A": {"a"}, "B": {"b"}},
		discardLogger(),
	)
	if err != nil {
		t.Fatalf("构造管理器: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	groupA := manager.Group("A")
	groupB := manager.Group("B")
	if groupA == nil || groupB == nil {
		t.Fatal("缺少上游组")
	}

	resolve := func(group *Group) string {
		t.Helper()

		result := group.Resolve(context.Background(), newQuery("split.test"), proxy.ProtoUDP, "127.0.0.1:12345")
		if result.Error != nil || result.Response == nil {
			t.Fatalf("解析失败: %v", result.Error)
		}

		values := aValues(result.Response)
		if len(values) != 1 {
			t.Fatalf("应答 = %v", values)
		}

		return values[0]
	}

	if got := resolve(groupA); got != "192.0.2.1" {
		t.Fatalf("组 A 应答 = %s", got)
	}
	if got := resolve(groupB); got != "192.0.2.2" {
		t.Fatalf("组 B 应答 = %s", got)
	}
}

func TestManagerValidatesReferences(t *testing.T) {
	t.Parallel()

	if _, err := NewManager(
		map[string]Spec{"a": {ID: "a", URL: "udp://127.0.0.1:53"}},
		map[string][]string{"A": {"missing"}},
		discardLogger(),
	); err == nil {
		t.Fatal("引用不存在的上游时应报错")
	}

	if _, err := NewManager(map[string]Spec{}, map[string][]string{}, discardLogger()); err == nil {
		t.Fatal("没有组时应报错")
	}
}

func TestEncryptedUpstreams(t *testing.T) {
	t.Parallel()

	plain, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 10}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
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

	tests := []struct {
		name string
		url  string
		tls  bool
	}{
		{name: "DoH", url: enc.DoHURL(), tls: true},
		{name: "DoT", url: enc.DoTURL(), tls: true},
		{name: "DoQ", url: enc.DoQURL(), tls: true},
	}
	if enc.DoH3URL() != "" {
		tests = append(tests, struct {
			name string
			url  string
			tls  bool
		}{name: "DoH3", url: enc.DoH3URL(), tls: true})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			group, err := NewGroup("g", []Spec{{
				ID:      tt.name,
				URL:     tt.url,
				Timeout: 3 * time.Second,
				RootCAs: enc.RootCAs(),
			}}, discardLogger())
			if err != nil {
				t.Fatalf("构造加密上游组: %v", err)
			}
			t.Cleanup(func() { _ = group.Close() })

			result := group.Resolve(context.Background(), newQuery("enc.test"), proxy.ProtoUDP, "127.0.0.1:12345")
			if result.Error != nil || result.Response == nil {
				t.Fatalf("加密解析失败: %v", result.Error)
			}
			if got := aValues(result.Response); got[0] != "192.0.2.10" {
				t.Fatalf("应答 = %v", got)
			}
		})
	}

	t.Run("自签证书默认校验失败", func(t *testing.T) {
		t.Parallel()

		group, err := NewGroup("g", []Spec{{
			ID:      "untrusted",
			URL:     enc.DoHURL(),
			Timeout: 3 * time.Second,
		}}, discardLogger())
		if err != nil {
			t.Fatalf("构造上游组: %v", err)
		}
		t.Cleanup(func() { _ = group.Close() })

		result := group.Resolve(context.Background(), newQuery("enc.test"), proxy.ProtoUDP, "127.0.0.1:12345")
		if result.Error == nil {
			t.Fatal("自签证书应校验失败")
		}
	})
}

func TestConnectivityTest(t *testing.T) {
	t.Parallel()

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	results := Test(context.Background(), []Spec{
		{ID: "ok", Name: "可用", URL: mock.URL(), Timeout: time.Second},
		{ID: "bad", Name: "不可用", URL: "tcp://127.0.0.1:1", Timeout: 200 * time.Millisecond},
	}, 3*time.Second, discardLogger())

	if len(results) != 2 {
		t.Fatalf("结果数量 = %d", len(results))
	}
	if !results[0].OK || results[0].Error != "" {
		t.Fatalf("可用上游结果 = %+v", results[0])
	}
	if results[0].TLSOK != nil {
		t.Fatal("明文上游的 tls_ok 应为 null")
	}
	if results[1].OK {
		t.Fatalf("不可用上游结果 = %+v", results[1])
	}
}

// 加密判定必须基于原始 scheme 文本：解析失败的上游没有 ParsedURL，但仍要
// 明确报告 tls_ok=false，而不是 null。
func TestConnectivityTestEncryptedFailuresReportTLSOKFalse(t *testing.T) {
	t.Parallel()

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	results := Test(context.Background(), []Spec{
		{Name: "DoH 缺路径", URL: "https://dns.example"},
		{Name: "DoH3 缺路径", URL: "h3://dns.example"},
		{Name: "DoT 端口越界", URL: "tls://dns.example:70000"},
		{Name: "合法 UDP", URL: mock.URL()},
	}, 3*time.Second, discardLogger())

	if len(results) != 4 {
		t.Fatalf("结果数量 = %d", len(results))
	}
	for _, result := range results[:3] {
		if result.Error == "" {
			t.Fatalf("非法加密上游应报错: %+v", result)
		}
		if result.TLSOK == nil || *result.TLSOK {
			t.Fatalf("%q 的 tls_ok = %v, 期望 false", result.Name, result.TLSOK)
		}
	}
	if results[3].TLSOK != nil {
		t.Fatalf("明文上游的 tls_ok 应为 null: %+v", results[3])
	}
	if results[3].Address != mock.URL() {
		t.Fatalf("address = %q, 期望规范 URL %q", results[3].Address, mock.URL())
	}
}

// 已取消且地址非法的加密上游同样要给出 tls_ok=false，不能被提前返回吞掉。
func TestConnectivityTestCanceledEncryptedReportsTLSOKFalse(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := Test(ctx, []Spec{{URL: "https://dns.example"}}, 10*time.Second, discardLogger())
	if len(results) != 1 {
		t.Fatalf("结果数量 = %d", len(results))
	}
	if results[0].Error == "" {
		t.Fatalf("取消后的结果 = %+v", results[0])
	}
	if results[0].TLSOK == nil || *results[0].TLSOK {
		t.Fatalf("tls_ok = %v, 期望 false", results[0].TLSOK)
	}
}

// timeout 是单上游失败判定的总时长；dnsproxy 内部会重试一次，外层必须强制
// 预算，否则实际墙钟会翻倍。
func TestGroupResolveHonorsTimeoutBudget(t *testing.T) {
	t.Parallel()

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{
		A:     []byte{192, 0, 2, 1},
		Delay: time.Second,
	})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	group, err := NewGroup("g", []Spec{{URL: mock.URL(), Timeout: 250 * time.Millisecond}}, discardLogger())
	if err != nil {
		t.Fatalf("构造上游组: %v", err)
	}
	t.Cleanup(func() { _ = group.Close() })

	start := time.Now()
	result := group.Resolve(context.Background(), newQuery("slow.test"), proxy.ProtoUDP, "127.0.0.1:12345")
	elapsed := time.Since(start)

	if elapsed > 600*time.Millisecond {
		t.Fatalf("timeout=250ms 的解析耗时 %v，超过预算", elapsed)
	}
	if result.Error == nil {
		t.Fatal("延迟上游应返回超时错误")
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
	if result.Attempts[0].DurationMS > 600 {
		t.Fatalf("attempt 耗时 = %.1fms", result.Attempts[0].DurationMS)
	}
}

// 连通性测试的总预算是硬上界，不能被内部重试放大。
func TestConnectivityTestBudgetIsHardBound(t *testing.T) {
	t.Parallel()

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{
		A:     []byte{192, 0, 2, 1},
		Delay: time.Second,
	})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	start := time.Now()
	results := Test(context.Background(), []Spec{{
		URL:     mock.URL(),
		Timeout: 5 * time.Second,
	}}, 200*time.Millisecond, discardLogger())
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("budget=200ms 的连通性测试耗时 %v", elapsed)
	}
	if len(results) != 1 || results[0].OK || results[0].Error == "" {
		t.Fatalf("结果 = %+v", results)
	}
}

// 调用方取消后不得再发起或继续探测。
func TestConnectivityTestStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{
		A:     []byte{192, 0, 2, 1},
		Delay: time.Second,
	})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	results := Test(ctx, []Spec{
		{URL: mock.URL()},
		{URL: mock.URL()},
	}, 10*time.Second, discardLogger())
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Fatalf("取消后仍耗时 %v", elapsed)
	}
	if len(results) != 2 {
		t.Fatalf("结果数量 = %d", len(results))
	}
	for _, result := range results {
		if result.OK || result.Error == "" {
			t.Fatalf("取消后的结果 = %+v", result)
		}
	}
}

func newQuery(domain string) *dns.Msg {
	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	req.RecursionDesired = true

	return req
}

func aValues(msg *dns.Msg) []string {
	if msg == nil {
		return nil
	}

	values := make([]string, 0, len(msg.Answer))
	for _, rr := range msg.Answer {
		if a, ok := rr.(*dns.A); ok {
			values = append(values, a.A.String())
		}
	}

	return values
}

func TestDisplayName(t *testing.T) {
	t.Parallel()

	if got := displayName(Spec{Name: "显示名", URL: "udp://1.1.1.1"}); got != "显示名" {
		t.Fatalf("displayName = %q", got)
	}
	if got := displayName(Spec{URL: "udp://1.1.1.1"}); got != "udp://1.1.1.1:53" {
		t.Fatalf("displayName = %q, 期望规范 URL", got)
	}
	// 无法解析的地址没有规范形式，仍展示原文以便定位。
	if got := displayName(Spec{URL: "https://dns.example"}); got != "https://dns.example" {
		t.Fatalf("displayName = %q", got)
	}
}

func TestErrorText(t *testing.T) {
	t.Parallel()

	if got := errorText(nil); got != "" {
		t.Fatalf("errorText(nil) = %q", got)
	}
	if got := errorText(context.Canceled); !strings.Contains(got, "canceled") {
		t.Fatalf("errorText = %q", got)
	}
}
