package dnskernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// Target 是一个监听目标。
type Target struct {
	// Network 为 udp 或 tcp。
	Network string
	// Addr 为 IP:端口。
	Addr string
}

// BindFailure 描述一个监听目标的失败。
type BindFailure struct {
	Address  string `json:"address"`
	Protocol string `json:"protocol"`
	Error    string `json:"error,omitempty"`
}

// BindError 描述部分或全部监听目标失败。
type BindError struct {
	Code    string        `json:"code"`
	Message string        `json:"message"`
	Targets []BindFailure `json:"targets"`
}

// Status 是 DNS 监听状态。
type Status struct {
	Ready     bool
	UDP       []string
	TCP       []string
	BindError *BindError
}

// DefaultTargets 返回默认的四个监听目标。
func DefaultTargets() []Target {
	return []Target{
		{Network: "udp", Addr: "127.0.0.1:53"},
		{Network: "tcp", Addr: "127.0.0.1:53"},
		{Network: "udp", Addr: "[::1]:53"},
		{Network: "tcp", Addr: "[::1]:53"},
	}
}

// headerQRBit 是 DNS 报头中 QR 标志所在的位（最高位）。
const headerQRBit = 1 << 15

// acceptDNSMessage 决定报文是否进入内核处理。
//
// dns 库默认的 MsgAcceptFunc 会在进入 Handler 之前就拒绝非 QUERY opcode 与
// 多问题请求，内核的协议拒绝分支与日志因此永远走不到。这里只丢弃应答报文
// （QR=1），其余交给 ServeDNS 判定，保证拒绝原因可观测。
func acceptDNSMessage(header dns.Header) dns.MsgAcceptAction {
	if header.Bits&headerQRBit != 0 {
		return dns.MsgIgnore
	}

	return dns.MsgAccept
}

// logInvalidMessage 记录库层无法解析的报文。
func (s *Server) logInvalidMessage(message []byte, err error) {
	s.logger.Warn("丢弃无法解析的 DNS 报文", "bytes", len(message), "error", err)
}

// startListeners 绑定全部目标；单个目标失败不阻止其他目标服务。
func (s *Server) startListeners(ctx context.Context) {
	started := make(chan struct{}, len(s.targets)*2)

	for _, target := range s.targets {
		switch target.Network {
		case "udp":
			s.startUDP(ctx, target, started)
		case "tcp":
			s.startTCP(ctx, target, started)
		default:
			s.addBindFailure(target, fmt.Errorf("不支持的监听协议"))
		}
	}

	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

	s.mu.Lock()
	expected := len(s.udpServers) + len(s.tcpServers)
	s.mu.Unlock()

	for i := 0; i < expected; i++ {
		select {
		case <-started:
		case <-timeout.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// startUDP 绑定并启动一个 UDP 监听。
func (s *Server) startUDP(ctx context.Context, target Target, started chan<- struct{}) {
	addrPort, err := netip.ParseAddrPort(target.Addr)
	if err != nil {
		s.addBindFailure(target, fmt.Errorf("监听地址不合法: %w", err))

		return
	}

	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(addrPort))
	if err != nil {
		s.addBindFailure(target, err)

		return
	}

	server := &dns.Server{
		PacketConn: conn,
		Handler:    s,
		// 自定义接收策略与无法解析回调：默认策略会在库层提前拒绝，内核的
		// 拒绝分支与日志就不会被执行。
		MsgAcceptFunc:  acceptDNSMessage,
		MsgInvalidFunc: s.logInvalidMessage,
		NotifyStartedFunc: func() {
			started <- struct{}{}
		},
	}

	s.mu.Lock()
	s.udpServers = append(s.udpServers, server)
	s.udpAddrs = append(s.udpAddrs, conn.LocalAddr().String())
	s.mu.Unlock()

	go func() {
		if err := server.ActivateAndServe(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Error("UDP 监听退出", "addr", conn.LocalAddr().String(), "error", err)
		}
	}()
}

// startTCP 绑定并启动一个 TCP 监听。
func (s *Server) startTCP(ctx context.Context, target Target, started chan<- struct{}) {
	addrPort, err := netip.ParseAddrPort(target.Addr)
	if err != nil {
		s.addBindFailure(target, fmt.Errorf("监听地址不合法: %w", err))

		return
	}

	listener, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(addrPort))
	if err != nil {
		s.addBindFailure(target, err)

		return
	}

	server := &dns.Server{
		Listener: listener,
		Handler:  s,
		// 自定义接收策略与无法解析回调：默认策略会在库层提前拒绝，内核的
		// 拒绝分支与日志就不会被执行。
		MsgAcceptFunc:  acceptDNSMessage,
		MsgInvalidFunc: s.logInvalidMessage,
		NotifyStartedFunc: func() {
			started <- struct{}{}
		},
	}

	s.mu.Lock()
	s.tcpServers = append(s.tcpServers, server)
	s.tcpAddrs = append(s.tcpAddrs, listener.Addr().String())
	s.mu.Unlock()

	go func() {
		if err := server.ActivateAndServe(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Error("TCP 监听退出", "addr", listener.Addr().String(), "error", err)
		}
	}()
}

// addBindFailure 记录一个监听失败。
func (s *Server) addBindFailure(target Target, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.bindFailures == nil {
		s.bindFailures = make([]BindFailure, 0)
	}
	s.bindFailures = append(s.bindFailures, BindFailure{
		Address:  target.Addr,
		Protocol: target.Network,
		Error:    err.Error(),
	})
}

// Status 返回当前监听状态。
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 空切片必须非 nil：上层直接把状态序列化为 JSON，nil 会输出 null 而不是 []。
	status := Status{
		UDP: append([]string{}, s.udpAddrs...),
		TCP: append([]string{}, s.tcpAddrs...),
	}

	total := len(status.UDP) + len(status.TCP)
	status.Ready = len(s.bindFailures) == 0 && total == len(s.targets)

	if len(s.bindFailures) > 0 {
		status.BindError = &BindError{
			Code:    "dns_bind_failed",
			Message: "部分或全部 DNS 监听目标绑定失败",
			Targets: append([]BindFailure{}, s.bindFailures...),
		}
	}

	return status
}

// shutdownListeners 关闭全部监听。
func (s *Server) shutdownListeners(ctx context.Context) error {
	s.mu.Lock()
	udpServers := append([]*dns.Server(nil), s.udpServers...)
	tcpServers := append([]*dns.Server(nil), s.tcpServers...)
	s.udpServers = nil
	s.tcpServers = nil
	s.mu.Unlock()

	var errs []error
	for _, server := range udpServers {
		errs = append(errs, server.ShutdownContext(ctx))
	}
	for _, server := range tcpServers {
		errs = append(errs, server.ShutdownContext(ctx))
	}

	return errors.Join(errs...)
}

// loggerOr 返回非空 logger。
func loggerOr(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}

	return logger
}
