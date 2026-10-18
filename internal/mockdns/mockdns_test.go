package mockdns

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
)

// query 向 addr 发送一次 UDP 查询。
func query(t *testing.T, addr, name string, qtype uint16, timeout time.Duration) (*dns.Msg, error) {
	t.Helper()

	req := new(dns.Msg)
	req.SetQuestion(dns.Fqdn(name), qtype)

	client := &dns.Client{Net: "udp", Timeout: timeout}

	resp, _, err := client.Exchange(req, addr)

	return resp, err
}

func TestPlainAnswerModes(t *testing.T) {
	t.Run("多地址 RRset", func(t *testing.T) {
		mock, err := StartPlain(PlainConfig{
			A:           net.ParseIP("192.0.2.1").To4(),
			AdditionalA: []net.IP{net.ParseIP("192.0.2.2").To4()},
			TTL:         120,
		})
		if err != nil {
			t.Fatalf("启动 mock: %v", err)
		}
		t.Cleanup(func() { _ = mock.Close() })

		resp, err := query(t, mock.Addr(), "multi.test", dns.TypeA, time.Second)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if len(resp.Answer) != 2 {
			t.Fatalf("期望 2 条 A，实际 %d 条", len(resp.Answer))
		}
		for _, rr := range resp.Answer {
			if rr.Header().Ttl != 120 {
				t.Fatalf("期望 TTL 120，实际 %d", rr.Header().Ttl)
			}
		}
	})

	t.Run("NODATA", func(t *testing.T) {
		mock, err := StartPlain(PlainConfig{})
		if err != nil {
			t.Fatalf("启动 mock: %v", err)
		}
		t.Cleanup(func() { _ = mock.Close() })

		resp, err := query(t, mock.Addr(), "nodata.test", dns.TypeA, time.Second)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 {
			t.Fatalf("期望 NOERROR 空应答，实际 rcode=%d answers=%d", resp.Rcode, len(resp.Answer))
		}
	})

	t.Run("NXDOMAIN", func(t *testing.T) {
		mock, err := StartPlain(PlainConfig{Rcode: dns.RcodeNameError})
		if err != nil {
			t.Fatalf("启动 mock: %v", err)
		}
		t.Cleanup(func() { _ = mock.Close() })

		resp, err := query(t, mock.Addr(), "missing.test", dns.TypeA, time.Second)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if resp.Rcode != dns.RcodeNameError {
			t.Fatalf("期望 NXDOMAIN，实际 %s", dns.RcodeToString[resp.Rcode])
		}
	})

	t.Run("TC 截断", func(t *testing.T) {
		mock, err := StartPlain(PlainConfig{
			A:        net.ParseIP("192.0.2.1").To4(),
			Truncate: true,
		})
		if err != nil {
			t.Fatalf("启动 mock: %v", err)
		}
		t.Cleanup(func() { _ = mock.Close() })

		resp, err := query(t, mock.Addr(), "truncated.test", dns.TypeA, time.Second)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if !resp.Truncated {
			t.Fatal("期望 TC=1")
		}
		if len(resp.Answer) != 0 {
			t.Fatalf("截断应答不应带 Answer，实际 %d 条", len(resp.Answer))
		}
	})

	t.Run("延迟超时", func(t *testing.T) {
		mock, err := StartPlain(PlainConfig{
			A:     net.ParseIP("192.0.2.1").To4(),
			Delay: 300 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("启动 mock: %v", err)
		}
		t.Cleanup(func() { _ = mock.Close() })

		if _, err = query(t, mock.Addr(), "slow.test", dns.TypeA, 50*time.Millisecond); err == nil {
			t.Fatal("期望客户端超时，实际成功")
		}
	})
}

func TestEncryptedCertificateValidation(t *testing.T) {
	plain, err := StartPlain(PlainConfig{A: net.ParseIP("192.0.2.10").To4()})
	if err != nil {
		t.Fatalf("启动明文上游: %v", err)
	}
	t.Cleanup(func() { _ = plain.Close() })

	enc, err := StartEncrypted(context.Background(), plain.URL())
	if err != nil {
		t.Fatalf("启动加密上游: %v", err)
	}
	t.Cleanup(func() { _ = enc.Close(context.Background()) })

	req := new(dns.Msg)
	req.SetQuestion("tls.test.", dns.TypeA)

	// 不信任自签 CA：必须失败。
	untrusted, err := upstream.AddressToUpstream(enc.DoHURL(), &upstream.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("构造未信任客户端: %v", err)
	}
	t.Cleanup(func() { _ = untrusted.Close() })
	if _, err = untrusted.Exchange(context.Background(), req); err == nil {
		t.Fatal("不信任自签 CA 时应校验失败")
	}

	// 信任自签 CA：应成功。
	trusted, err := upstream.AddressToUpstream(enc.DoHURL(), &upstream.Options{
		Timeout: 2 * time.Second,
		RootCAs: enc.RootCAs(),
	})
	if err != nil {
		t.Fatalf("构造信任客户端: %v", err)
	}
	t.Cleanup(func() { _ = trusted.Close() })

	resp, err := trusted.Exchange(context.Background(), req)
	if err != nil {
		t.Fatalf("信任 CA 后查询失败: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("期望 1 条 A 应答，实际 rcode=%d answers=%d", resp.Rcode, len(resp.Answer))
	}
}
