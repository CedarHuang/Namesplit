// Package upstream 解析上游地址、构建按组隔离的解析器并执行并发查询。
//
// 组内成员被视为等价集合：每条查询并发发给所有成员，第一个成功的响应生效。
// 每个组持有独立的 dnsproxy 实例与缓存，同名查询在不同组之间不会串答案。
package upstream

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/CedarHuang/Namesplit/internal/dnsname"
)

// Spec 描述一个上游服务器。
type Spec struct {
	// ID 是配置中的引用标识。
	ID string
	// Name 是显示名；为空时使用 URL。
	Name string
	// URL 是上游地址，支持 udp/tcp/tls/https/quic/h3 或裸 IP。
	URL string
	// Bootstrap 是解析 URL 主机名时使用的 IP 字面量。
	Bootstrap []string
	// Timeout 是单次交换的超时。
	Timeout time.Duration
	// RootCAs 是校验上游证书使用的根证书池；nil 表示使用系统根证书。
	RootCAs *x509.CertPool
}

// ParsedURL 是上游 URL 的结构化结果。
type ParsedURL struct {
	// Scheme 是协议名：udp/tcp/tls/https/quic/h3。
	Scheme string
	// Host 是归一后的主机名或 IP 字面量。
	Host string
	// Port 是端口。
	Port int
	// Path 是 DoH 路径；仅 https/h3 非空。
	Path string
	// HostIsName 表示 Host 是域名而不是 IP 字面量。
	HostIsName bool
}

// Encrypted 表示该 URL 使用 TLS。
func (p ParsedURL) Encrypted() bool {
	switch p.Scheme {
	case "tls", "https", "quic", "h3":
		return true
	default:
		return false
	}
}

// URL 返回由解析结果重建的规范 URL。
//
// 不能直接使用配置原文：主机可能被 IDNA、去尾点、小写归一，而 dnsproxy 会
// 按原文校验域名并拒绝尾点主机。IPv6 主机归一后不再带方括号，重新拼接时必须
// 经过 net.JoinHostPort，否则端口会被当作地址的一部分。路径只对 DoH 有意义，
// 其余协议重建时丢弃。
func (p ParsedURL) URL() string {
	if p.Scheme == "" {
		return ""
	}

	address := p.Scheme + "://" + net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	if p.Scheme == "https" || p.Scheme == "h3" {
		address += p.Path
	}

	return address
}

// canonicalURLText 返回用于展示与探测结果的规范 URL 文本。
//
// 无法解析时退回原文，保证错误信息仍能定位用户填写的地址。
func canonicalURLText(raw string) string {
	parsed, err := ParseURL(raw)
	if err != nil {
		return strings.TrimSpace(raw)
	}

	return parsed.URL()
}

// DefaultTimeout 是未指定超时时的单次交换上限。
const DefaultTimeout = 5 * time.Second

// ParseURL 校验并解析上游地址。
func ParseURL(raw string) (ParsedURL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ParsedURL{}, errors.New("上游地址为空")
	}

	if !strings.Contains(trimmed, "://") {
		return parseBareAddress(trimmed)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ParsedURL{}, fmt.Errorf("上游地址无法解析: %w", err)
	}
	if parsed.User != nil {
		return ParsedURL{}, errors.New("上游地址不允许包含用户信息")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if !knownScheme(scheme) {
		return ParsedURL{}, fmt.Errorf("不支持的上游协议 %q", scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return ParsedURL{}, errors.New("上游地址缺少主机")
	}

	port := defaultPort(scheme)
	if portText := parsed.Port(); portText != "" {
		port, err = parsePort(portText)
		if err != nil {
			return ParsedURL{}, err
		}
	}

	if (scheme == "https" || scheme == "h3") && (parsed.Path == "" || parsed.Path == "/") {
		return ParsedURL{}, fmt.Errorf("%s 上游必须包含非空路径", scheme)
	}

	canonHost, hostIsName, err := canonicalHost(host)
	if err != nil {
		return ParsedURL{}, err
	}

	return ParsedURL{
		Scheme:     scheme,
		Host:       canonHost,
		Port:       port,
		Path:       parsed.Path,
		HostIsName: hostIsName,
	}, nil
}

// parseBareAddress 解析不带协议的 IP 地址，按 UDP 处理。
func parseBareAddress(raw string) (ParsedURL, error) {
	if addr, err := netip.ParseAddr(raw); err == nil {
		return ParsedURL{Scheme: "udp", Host: addr.String(), Port: 53}, nil
	}

	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		return ParsedURL{}, fmt.Errorf("裸地址 %q 不是 IP 或 host:port", raw)
	}

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ParsedURL{}, fmt.Errorf("裸地址只支持 IP 字面量，实际为 %q", host)
	}

	port, err := parsePort(portText)
	if err != nil {
		return ParsedURL{}, err
	}

	return ParsedURL{Scheme: "udp", Host: addr.String(), Port: port}, nil
}

// knownScheme 判断协议是否受支持。
func knownScheme(scheme string) bool {
	switch scheme {
	case "udp", "tcp", "tls", "https", "quic", "h3":
		return true
	default:
		return false
	}
}

// defaultPort 返回协议未显式指定端口时的默认值。
func defaultPort(scheme string) int {
	switch scheme {
	case "tls", "quic":
		return 853
	case "https", "h3":
		return 443
	default:
		return 53
	}
}

// parsePort 校验端口文本。
func parsePort(text string) (int, error) {
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("端口 %q 不在 1–65535", text)
	}

	return port, nil
}

// canonicalHost 归一主机：IP 取标准文本，域名经 IDNA/小写/去尾点归一。
//
// 归一结果用于重建上游 URL；配置原文可能带尾点或 Unicode，dnsproxy 只做
// 域名合法性校验，不会替调用方归一。
func canonicalHost(host string) (canon string, hostIsName bool, err error) {
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		return addr.String(), false, nil
	}

	canon, err = dnsname.Canon(host)
	if err != nil {
		return "", false, fmt.Errorf("上游主机 %q 不合法: %w", host, err)
	}

	return canon, true, nil
}
