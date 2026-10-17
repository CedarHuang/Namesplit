package dnskernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/miekg/dns"
)

// LocalLookup 是本地记录查询钩子。
//
// 返回 nameExists=false 表示本地无此名字，继续按规则分流；nameExists=true 且
// answers 为空表示名字存在但类型不存在（NODATA），不得再请求上游。
type LocalLookup func(q dns.Question) (answers []dns.RR, nameExists bool)

// Config 是内核配置。
type Config struct {
	// Router 是分流规则表，必填。
	Router *Router
	// Groups 是上游组，键为组名；必须覆盖 Router 引用到的所有组。
	Groups map[string]*Group
	// Local 是可选的本地记录钩子。
	Local LocalLookup
	// Logger 用于 dnsproxy 内部日志；nil 时使用 slog 默认 logger。
	Logger *slog.Logger
	// OnEvent 在每个查询完成并写出应答后被同步调用。
	OnEvent func(Event)
	// ListenAddr 是监听地址，默认 "127.0.0.1:0"（随机端口）。UDP 与 TCP
	// 监听同一端口。
	ListenAddr string
}

// Server 是本机 DNS 内核：UDP/TCP 监听 + 规则分流 + 上游组解析 + 逐查询事件。
//
// 监听与规则路由由自研层负责，加密上游、组内策略、缓存由 dnsproxy 负责。
type Server struct {
	router    *Router
	groups    map[string]*Group
	local     LocalLookup
	logger    *slog.Logger
	onEvent   func(Event)
	ctx       context.Context
	cancel    context.CancelFunc
	addr      string
	udpConn   *net.UDPConn
	tcpLn     *net.TCPListener
	udpServer *dns.Server
	tcpServer *dns.Server
	mu        sync.Mutex
	closed    bool
}

// NewServer 构造内核。调用 Start 后才开始监听。
func NewServer(cfg Config) (s *Server, err error) {
	if cfg.Router == nil {
		return nil, errors.New("路由表不能为空")
	}

	groups := make(map[string]*Group, len(cfg.Groups))
	for name, group := range cfg.Groups {
		if group == nil {
			return nil, fmt.Errorf("上游组 %q 为空", name)
		}
		groups[name] = group
	}

	referenced := map[string]struct{}{cfg.Router.DefaultGroup(): {}}
	for _, rule := range cfg.Router.Rules() {
		referenced[rule.Group] = struct{}{}
	}

	for name := range referenced {
		if groups[name] == nil {
			return nil, fmt.Errorf("路由引用的上游组 %q 不存在", name)
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	local := cfg.ListenAddr
	if local == "" {
		local = "127.0.0.1:0"
	}

	return &Server{
		router:  cfg.Router,
		groups:  groups,
		local:   cfg.Local,
		logger:  logger,
		onEvent: cfg.OnEvent,
		addr:    local,
	}, nil
}

// Start 绑定 UDP/TCP 并开始服务，返回实际监听地址。
func (s *Server) Start(ctx context.Context) (addr string, err error) {
	host, portStr, err := net.SplitHostPort(s.addr)
	if err != nil {
		return "", fmt.Errorf("解析监听地址 %q: %w", s.addr, err)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("监听地址 %q 的主机不是 IP 字面量", s.addr)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("解析监听端口 %q: %w", portStr, err)
	}

	tcpLn, udpConn, err := listenDualStack(ip, port)
	if err != nil {
		return "", err
	}

	s.ctx, s.cancel = context.WithCancel(context.Background())

	started := make(chan struct{}, 2)
	notify := func() { started <- struct{}{} }

	s.tcpLn = tcpLn
	s.udpConn = udpConn
	s.tcpServer = &dns.Server{Listener: tcpLn, Handler: s, NotifyStartedFunc: notify}
	s.udpServer = &dns.Server{PacketConn: udpConn, Handler: s, NotifyStartedFunc: notify}

	go s.serve(s.udpServer)
	go s.serve(s.tcpServer)

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-timer.C:
			s.closeListeners()

			return "", errors.New("等待 DNS 监听器启动超时")
		case <-ctx.Done():
			s.closeListeners()

			return "", ctx.Err()
		}
	}

	s.mu.Lock()
	s.addr = s.tcpLn.Addr().String()
	actual := s.addr
	s.mu.Unlock()

	s.logger.Info("DNS 内核已启动", "addr", actual)

	return actual, nil
}

// serve 运行一个 dns.Server，并在非正常关闭时记录日志。
func (s *Server) serve(srv *dns.Server) {
	if err := srv.ActivateAndServe(); err != nil {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if !closed && !errors.Is(err, net.ErrClosed) {
			s.logger.Error("DNS 监听器退出", "error", err)
		}
	}
}

// listenDualStack 让 UDP 与 TCP 尽量使用同一端口。port=0 时挑选空闲端口。
func listenDualStack(ip net.IP, port int) (tcpLn *net.TCPListener, udpConn *net.UDPConn, err error) {
	for attempt := 0; attempt < 32; attempt++ {
		tcpLn, err = net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: port})
		if err != nil {
			return nil, nil, fmt.Errorf("监听 TCP %s:%d: %w", ip, port, err)
		}

		actualPort := tcpLn.Addr().(*net.TCPAddr).Port
		udpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: actualPort})
		if err == nil {
			return tcpLn, udpConn, nil
		}

		tcpLn.Close()
		if port != 0 {
			return nil, nil, fmt.Errorf("监听 UDP %s:%d: %w", ip, actualPort, err)
		}
	}

	return nil, nil, errors.New("无法为 UDP/TCP 找到同一空闲端口")
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.addr
}

// Shutdown 停止监听并关闭所有上游组。
func (s *Server) Shutdown(ctx context.Context) (err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}

	var errs []error
	if s.udpServer != nil {
		errs = append(errs, s.udpServer.ShutdownContext(ctx))
	}
	if s.tcpServer != nil {
		errs = append(errs, s.tcpServer.ShutdownContext(ctx))
	}

	for _, group := range s.groups {
		errs = append(errs, group.Close())
	}

	return errors.Join(errs...)
}

// closeListeners 关闭已绑定的套接字，用于启动失败时兜底。
func (s *Server) closeListeners() {
	if s.udpConn != nil {
		s.udpConn.Close()
	}
	if s.tcpLn != nil {
		s.tcpLn.Close()
	}
}

// ServeDNS 实现 dns.Handler，是逐查询可观测性的采集点。
func (s *Server) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	start := time.Now()
	ev := Event{
		Time:     start,
		Protocol: clientProtocol(w),
	}

	resp := s.resolve(w, req, &ev)

	writeErr := w.WriteMsg(resp)
	ev.DurationMS = durationMS(time.Since(start))
	ev.Rcode = resp.Rcode
	ev.RcodeName = rcodeName(resp.Rcode)
	if writeErr != nil {
		ev.Error = joinErr(ev.Error, fmt.Errorf("写回应答: %w", writeErr))
	}

	if s.onEvent != nil {
		s.onEvent(ev)
	}
}

// resolve 执行「本地记录 → 规则 → 上游组」链路。
func (s *Server) resolve(w dns.ResponseWriter, req *dns.Msg, ev *Event) (resp *dns.Msg) {
	if len(req.Question) != 1 {
		ev.Error = fmt.Sprintf("问题数量为 %d，仅支持 1", len(req.Question))

		return newRcodeMsg(req, dns.RcodeFormatError)
	}

	q := req.Question[0]
	ev.Domain = normalizeName(q.Name)
	ev.QType = qtypeString(q.Qtype)

	if s.local != nil {
		if answers, nameExists := s.local(q); nameExists {
			local := new(dns.Msg)
			local.SetReply(req)
			local.Authoritative = true
			local.Answer = answers

			ev.Source = SourceLocal
			ev.RuleKind = "local"
			ev.Answers, ev.TTL = summarizeAnswers(local)

			return local
		}
	}

	route := s.router.Match(q.Name, q.Qtype)
	ev.Group = route.Group
	ev.Rule = route.Rule
	ev.RuleKind = route.Kind

	group := s.groups[route.Group]
	if group == nil {
		ev.Source = SourceUpstream
		ev.Error = fmt.Sprintf("上游组 %q 不存在", route.Group)

		return newRcodeMsg(req, dns.RcodeServerFailure)
	}

	// 默认剥离客户端带入的 ECS；dnsproxy 不会剥离，需在此显式处理。
	upstreamReq := stripECS(req)
	res := group.Resolve(s.ctx, upstreamReq, protoOf(w), remoteAddr(w))

	ev.Attempts = res.Attempts
	ev.Upstream = res.Upstream
	ev.CacheHit = res.CacheHit

	if res.Error != nil || res.Response == nil {
		ev.Source = SourceUpstream
		if res.Error == nil {
			res.Error = errors.New("上游组未返回应答")
		}
		ev.Error = res.Error.Error()

		return newRcodeMsg(req, dns.RcodeServerFailure)
	}

	if res.CacheHit {
		ev.Source = SourceCache
	} else {
		ev.Source = SourceUpstream
	}

	resp = res.Response
	resp.Id = req.Id
	ev.Answers, ev.TTL = summarizeAnswers(resp)

	return resp
}

// newRcodeMsg 以请求为模板构造单 RCODE 应答。
func newRcodeMsg(req *dns.Msg, rcode int) (resp *dns.Msg) {
	resp = new(dns.Msg)
	resp.SetRcode(req, rcode)

	return resp
}

// clientProtocol 返回客户端到内核的传输协议。
func clientProtocol(w dns.ResponseWriter) (proto string) {
	if w == nil || w.RemoteAddr() == nil {
		return ""
	}

	return strings.ToLower(w.RemoteAddr().Network())
}

// protoOf 返回 dnsproxy 使用的协议枚举。
func protoOf(w dns.ResponseWriter) (proto proxy.Proto) {
	if clientProtocol(w) == "tcp" {
		return proxy.ProtoTCP
	}

	return proxy.ProtoUDP
}

// remoteAddr 返回客户端地址文本。
func remoteAddr(w dns.ResponseWriter) string {
	if w == nil || w.RemoteAddr() == nil {
		return ""
	}

	return w.RemoteAddr().String()
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

// joinErr 拼接两个错误文本，避免引入 errors.Join 的格式化差异。
func joinErr(first string, second error) string {
	if first == "" {
		return second.Error()
	}

	return first + "; " + second.Error()
}
