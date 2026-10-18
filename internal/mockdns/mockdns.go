// Package mockdns 为测试提供本地 DNS 上游：
//
//   - Plain 是明文 UDP 上游，可配置应答 A/AAAA、TTL、RCODE，并记录收到的请求，
//     用于测试规则分流、ECS 剥离、DO 透传与缓存隔离；
//   - Encrypted 用 dnsproxy 自身启动带自签证书的 DoH/DoH3/DoT/DoQ 服务，
//     用于测试加密上游、TLS 校验与 DoH3/DoQ 可用性。
//
// 该包仅用于测试，不进入产品代码路径。
package mockdns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
)

// PlainConfig 是明文 UDP 上游的应答配置。
type PlainConfig struct {
	// A 是 A 查询的主应答地址；nil 表示无 A 记录。
	A net.IP
	// AdditionalA 是追加的 A 应答地址，用于多地址（多值 RRset）场景。
	AdditionalA []net.IP
	// AAAA 是 AAAA 查询的应答地址；nil 表示返回 NODATA。
	AAAA net.IP
	// TTL 是应答 TTL（秒）；0 使用 60。
	TTL uint32
	// Rcode 是强制应答 RCODE；0 表示 NOERROR。
	Rcode int
	// Delay 是写出应答前的延迟，用于模拟超时上游。
	Delay time.Duration
	// Truncate 置位 TC 标志并丢弃应答内容，用于模拟截断响应。
	Truncate bool
}

// Plain 是明文 UDP 上游。
type Plain struct {
	conn *net.UDPConn
	srv  *dns.Server
	cfg  PlainConfig

	mu   sync.Mutex
	seen []*dns.Msg
}

// StartPlain 启动一个明文 UDP 上游，监听 127.0.0.1 随机端口。
func StartPlain(cfg PlainConfig) (p *Plain, err error) {
	if cfg.TTL == 0 {
		cfg.TTL = 60
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("启动 mock UDP 上游: %w", err)
	}

	p = &Plain{conn: conn, cfg: cfg}
	p.srv = &dns.Server{
		PacketConn: conn,
		Handler:    dns.HandlerFunc(p.handle),
	}

	go func() {
		// 正常关闭时 ActivateAndServe 返回的错误可忽略；仅测试辅助代码。
		_ = p.srv.ActivateAndServe()
	}()

	return p, nil
}

// Addr 返回上游地址（host:port）。
func (p *Plain) Addr() string {
	return p.conn.LocalAddr().String()
}

// URL 返回可直接交给 dnsproxy 的上游 URL。
func (p *Plain) URL() string {
	return "udp://" + p.Addr()
}

// Seen 返回收到的查询副本。
func (p *Plain) Seen() (seen []*dns.Msg) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, msg := range p.seen {
		seen = append(seen, msg.Copy())
	}

	return seen
}

// SeenCount 返回收到的查询数。
func (p *Plain) SeenCount() (n int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.seen)
}

// Close 停止上游。
func (p *Plain) Close() (err error) {
	return p.srv.Shutdown()
}

func (p *Plain) handle(w dns.ResponseWriter, req *dns.Msg) {
	p.mu.Lock()
	p.seen = append(p.seen, req.Copy())
	p.mu.Unlock()

	if p.cfg.Delay > 0 {
		time.Sleep(p.cfg.Delay)
	}

	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true

	if p.cfg.Truncate {
		resp.Truncated = true
		_ = w.WriteMsg(resp)

		return
	}

	if p.cfg.Rcode != 0 {
		resp.Rcode = p.cfg.Rcode
		_ = w.WriteMsg(resp)

		return
	}

	if len(req.Question) == 1 {
		q := req.Question[0]
		switch q.Qtype {
		case dns.TypeA:
			for _, ip := range append([]net.IP{p.cfg.A}, p.cfg.AdditionalA...) {
				if ip == nil {
					continue
				}
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    p.cfg.TTL,
					},
					A: ip,
				})
			}
		case dns.TypeAAAA:
			if p.cfg.AAAA != nil {
				resp.Answer = append(resp.Answer, &dns.AAAA{
					Hdr: dns.RR_Header{
						Name:   q.Name,
						Rrtype: dns.TypeAAAA,
						Class:  dns.ClassINET,
						Ttl:    p.cfg.TTL,
					},
					AAAA: p.cfg.AAAA,
				})
			}
		}
	}

	_ = w.WriteMsg(resp)
}

// Encrypted 是用 dnsproxy 启动的本地加密上游，带自签证书。
type Encrypted struct {
	p       *proxy.Proxy
	rootCAs *x509.CertPool

	dohURL  string
	doh3URL string
	dotURL  string
	doqURL  string

	httpsAddr string

	h3Err error
}

// StartEncrypted 启动 DoH/DoT/DoQ 服务，并在可行时同时启动 DoH3。
//
// upstreamAddr 是加密服务转发查询用的明文上游地址，例如 Plain.URL()。
// 返回的证书由自签 CA 签发，客户端必须使用 RootCAs() 才能校验通过；
// 不配置 RootCAs 时会因证书不受信任而失败。
func StartEncrypted(ctx context.Context, upstreamAddr string) (e *Encrypted, err error) {
	return startEncrypted(ctx, upstreamAddr, true)
}

// StartEncryptedWithoutH3 启动不含 DoH3 的加密服务，用于验证 h3:// 客户端
// 确实强制 HTTP/3（不会静默回退到 H2）。
func StartEncryptedWithoutH3(ctx context.Context, upstreamAddr string) (e *Encrypted, err error) {
	return startEncrypted(ctx, upstreamAddr, false)
}

func startEncrypted(ctx context.Context, upstreamAddr string, wantH3 bool) (e *Encrypted, err error) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	cert, pool, err := generateCert()
	if err != nil {
		return nil, err
	}

	uc, err := proxy.ParseUpstreamsConfig(
		[]string{upstreamAddr},
		&upstream.Options{Logger: logger, Timeout: 3 * time.Second},
	)
	if err != nil {
		return nil, fmt.Errorf("构造加密 mock 的上游: %w", err)
	}

	withH3 := wantH3
	e, startErr := startEncryptedProxy(ctx, logger, uc, cert, withH3)
	if startErr != nil && wantH3 {
		// DoH3 在个别环境（UDP/QUIC 被限制等）可能启动失败；降级为不含 H3，
		// 保留 DoH/DoT/DoQ，并把原因交给调用方记录。
		uc, parseErr := proxy.ParseUpstreamsConfig(
			[]string{upstreamAddr},
			&upstream.Options{Logger: logger, Timeout: 3 * time.Second},
		)
		if parseErr != nil {
			return nil, parseErr
		}

		e, err = startEncryptedProxy(ctx, logger, uc, cert, false)
		if err != nil {
			return nil, fmt.Errorf("启动加密 mock 失败: %w（DoH3 首次失败: %v）", err, startErr)
		}
		e.h3Err = startErr
	} else if startErr != nil {
		return nil, startErr
	}

	e.rootCAs = pool

	httpsAddr := e.p.Addr(proxy.ProtoHTTPS)
	if httpsAddr == nil {
		_ = e.p.Shutdown(context.Background())

		return nil, errors.New("加密 mock 未启动 HTTPS 监听")
	}

	e.httpsAddr = httpsAddr.String()
	e.dohURL = "https://" + e.httpsAddr + "/dns-query"
	if e.p.Addr(proxy.ProtoTLS) != nil {
		e.dotURL = "tls://" + e.p.Addr(proxy.ProtoTLS).String()
	}
	if e.p.Addr(proxy.ProtoQUIC) != nil {
		e.doqURL = "quic://" + e.p.Addr(proxy.ProtoQUIC).String()
	}
	if e.h3Err == nil && wantH3 {
		e.doh3URL = "h3://" + e.httpsAddr + "/dns-query"
	}

	return e, nil
}

func startEncryptedProxy(
	ctx context.Context,
	logger *slog.Logger,
	uc *proxy.UpstreamConfig,
	cert tls.Certificate,
	withH3 bool,
) (e *Encrypted, err error) {
	p, err := proxy.New(&proxy.Config{
		Logger:         logger,
		UpstreamConfig: uc,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
		HTTPConfig: &proxy.HTTPConfig{
			ListenAddresses: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:0")},
			HTTP3Enabled:    withH3,
		},
		TLSListenAddr:  []*net.TCPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: 0}},
		QUICListenAddr: []*net.UDPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: 0}},
	})
	if err != nil {
		uc.Close()

		return nil, err
	}

	if err = p.Start(ctx); err != nil {
		uc.Close()

		return nil, err
	}

	return &Encrypted{p: p}, nil
}

// RootCAs 返回信任自签 CA 的证书池。
func (e *Encrypted) RootCAs() *x509.CertPool {
	return e.rootCAs
}

// DoHURL 返回 DoH 上游 URL。
func (e *Encrypted) DoHURL() string {
	return e.dohURL
}

// HTTPSAddr 返回 DoH/DoH3 共用的 TCP 监听地址（host:port）。
func (e *Encrypted) HTTPSAddr() string {
	return e.httpsAddr
}

// DoH3URL 返回 DoH3 上游 URL；启动失败时为空。
func (e *Encrypted) DoH3URL() string {
	return e.doh3URL
}

// DoTURL 返回 DoT 上游 URL。
func (e *Encrypted) DoTURL() string {
	return e.dotURL
}

// DoQURL 返回 DoQ 上游 URL。
func (e *Encrypted) DoQURL() string {
	return e.doqURL
}

// H3Error 返回 DoH3 启动失败原因；nil 表示 DoH3 可用。
func (e *Encrypted) H3Error() error {
	return e.h3Err
}

// Close 关闭加密 mock 服务及其上游。
func (e *Encrypted) Close(ctx context.Context) (err error) {
	return e.p.Shutdown(ctx)
}

// generateCert 生成自签 CA 与 127.0.0.1 的叶子证书。
func generateCert() (cert tls.Certificate, pool *x509.CertPool, err error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("生成 CA 私钥: %w", err)
	}

	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Namesplit Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("签发 CA 证书: %w", err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("解析 CA 证书: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("生成叶子私钥: %w", err)
	}

	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:     []string{"localhost"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("签发叶子证书: %w", err)
	}

	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("解析叶子证书: %w", err)
	}

	pool = x509.NewCertPool()
	pool.AddCert(caCert)

	return tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
		Leaf:        leafCert,
	}, pool, nil
}
