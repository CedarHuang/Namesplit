// Package dnskernel 监听 UDP/TCP，按「本地记录 → 规则 → 上游组」解析查询并
// 发射逐查询事件。
package dnskernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxy"
	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/dnsname"
	"github.com/CedarHuang/Namesplit/internal/event"
	"github.com/CedarHuang/Namesplit/internal/localrecords"
	"github.com/CedarHuang/Namesplit/internal/rules"
	"github.com/CedarHuang/Namesplit/internal/upstream"
)

// View 是一次查询使用的只读运行时视图。
type View interface {
	Rules() *rules.Table
	Local() *localrecords.Table
	Groups() *upstream.Manager
	DefaultGroup() string
}

// Store 提供运行时视图；release 用于延迟释放旧运行时。
type Store interface {
	Acquire() (View, func())
}

// Config 是内核配置。
type Config struct {
	Store   Store
	Sink    func(event.Event)
	Logger  *slog.Logger
	Targets []Target
}

// Server 是 DNS 内核。
type Server struct {
	store  Store
	sink   func(event.Event)
	logger *slog.Logger

	targets []Target
	ctx     context.Context
	cancel  context.CancelFunc

	eventID atomic.Uint64

	mu           sync.Mutex
	udpServers   []*dns.Server
	tcpServers   []*dns.Server
	udpAddrs     []string
	tcpAddrs     []string
	bindFailures []BindFailure
	closed       bool
}

// New 构造内核。Start 之前不会监听端口。
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("内核缺少运行时视图")
	}

	targets := cfg.Targets
	if len(targets) == 0 {
		targets = DefaultTargets()
	}

	return &Server{
		store:   cfg.Store,
		sink:    cfg.Sink,
		logger:  loggerOr(cfg.Logger),
		targets: targets,
	}, nil
}

// Start 绑定并开始服务全部目标。单个目标绑定失败不会返回错误，失败信息通过
// Status 暴露。
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.ctx, s.cancel = context.WithCancel(ctx)
	s.startListeners(s.ctx)

	return nil
}

// Shutdown 停止监听并等待在途查询结束。
func (s *Server) Shutdown(ctx context.Context) error {
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

	if ctx == nil {
		ctx = context.Background()
	}

	return s.shutdownListeners(ctx)
}

// ServeDNS 实现 dns.Handler。
func (s *Server) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	start := time.Now()

	if resp := rejectProtocol(req); resp != nil {
		s.logger.Warn("拒绝协议级请求",
			"opcode", req.Opcode,
			"qdcount", len(req.Question),
			"qclass", questionClass(req),
			"rcode", resp.Rcode,
			"protocol", protocolOf(w),
		)
		_ = w.WriteMsg(resp)

		return
	}

	view, release := s.store.Acquire()
	defer release()

	protocol := protocolOf(w)
	q := req.Question[0]

	ev := event.Event{
		Time:         start.UTC(),
		Domain:       dnsname.FQDN(q.Name),
		QType:        qtypeName(q.Qtype),
		RuleIndex:    -1,
		Protocol:     protocol,
		Answers:      []string{},
		Authority:    []string{},
		Additional:   []string{},
		RequestFlags: event.RequestFlags{RD: req.RecursionDesired, CD: req.CheckingDisabled},
	}
	if opt := req.IsEdns0(); opt != nil {
		ev.DO = opt.Do()
	}

	resp := s.resolve(view, req, &ev, protocol, remoteAddr(w))
	truncateResponse(req, resp, protocol)

	ev.DurationMS = durationMS(time.Since(start))
	ev.Rcode = resp.Rcode
	ev.RcodeName = event.RcodeName(resp.Rcode)
	ev.AD = resp.AuthenticatedData
	ev.ResponseFlags = event.ResponseFlags{
		AA: resp.Authoritative,
		TC: resp.Truncated,
		RA: resp.RecursionAvailable,
	}
	ev.Answers, ev.Authority, ev.Additional, ev.TTL = event.Summarize(resp)

	if err := w.WriteMsg(resp); err != nil {
		s.logger.Error("写回应答失败", "domain", ev.Domain, "error", err)

		return
	}

	ev.EventID = s.eventID.Add(1)
	if s.sink != nil {
		s.sink(ev)
	}
}

// resolve 执行「本地记录 → 规则 → 默认组 → 上游组」链路。
func (s *Server) resolve(
	view View,
	req *dns.Msg,
	ev *event.Event,
	protocol string,
	clientAddr string,
) *dns.Msg {
	q := req.Question[0]

	if view.Local() != nil {
		result, ok := view.Local().Lookup(q.Name, q.Qtype)
		if ok {
			resp := new(dns.Msg)
			resp.SetReply(req)
			resp.Authoritative = true
			resp.RecursionAvailable = true
			resp.Answer = result.Answers

			ev.Source = "local"
			ev.RuleKind = "local"
			ev.Rule = ""
			ev.RuleIndex = -1

			return resp
		}
	}

	groupID := view.DefaultGroup()
	ev.RuleKind = "default"
	if view.Rules() != nil {
		if hit, ok := view.Rules().Match(q.Name); ok {
			groupID = hit.Group
			ev.Rule = hit.Domain
			ev.RuleIndex = hit.Index
			switch hit.Kind {
			case rules.KindDomain:
				ev.RuleKind = "exact"
			case rules.KindSuffix:
				ev.RuleKind = "suffix"
			}
		}
	}
	ev.Group = groupID

	var group *upstream.Group
	if view.Groups() != nil {
		group = view.Groups().Group(groupID)
	}
	if group == nil {
		ev.Source = "upstream"
		ev.Error = fmt.Sprintf("上游组 %q 不存在", groupID)

		return servfail(req)
	}

	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	result := group.Resolve(ctx, stripECS(req), proxyProto(protocol), clientAddr)
	ev.Attempts = result.Attempts

	if result.Error != nil || result.Response == nil {
		ev.Source = "upstream"
		ev.Upstream = ""
		ev.UpstreamName = ""
		if result.Error != nil {
			ev.Error = result.Error.Error()
		} else {
			ev.Error = "上游未返回应答"
		}

		return servfail(req)
	}

	ev.Upstream = result.Upstream
	ev.UpstreamName = result.UpstreamName
	ev.CacheHit = result.CacheHit
	if result.CacheHit {
		ev.Source = "cache"
	} else {
		ev.Source = "upstream"
	}

	response := result.Response
	response.Id = req.Id
	stripOPT(response)

	return response
}

// questionClass 返回首个问题的类别；没有问题时为 0。
func questionClass(req *dns.Msg) int {
	if req == nil || len(req.Question) == 0 {
		return 0
	}

	return int(req.Question[0].Qclass)
}

// rejectProtocol 处理协议级拒绝；返回 nil 表示继续正常解析。
func rejectProtocol(req *dns.Msg) *dns.Msg {
	switch {
	case req.Opcode != dns.OpcodeQuery:
		return rcodeReply(req, dns.RcodeNotImplemented)
	case len(req.Question) != 1:
		return rcodeReply(req, dns.RcodeFormatError)
	case req.Question[0].Qclass != dns.ClassINET:
		return rcodeReply(req, dns.RcodeRefused)
	default:
		return nil
	}
}

// rcodeReply 构造协议拒绝应答。
func rcodeReply(req *dns.Msg, rcode int) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetRcode(req, rcode)
	resp.RecursionAvailable = true

	return resp
}

// servfail 构造 SERVFAIL 应答。
func servfail(req *dns.Msg) *dns.Msg {
	return rcodeReply(req, dns.RcodeServerFailure)
}

// truncateResponse 按客户端 EDNS 大小截断 UDP 应答。
func truncateResponse(req, resp *dns.Msg, protocol string) {
	if protocol != "udp" || resp == nil {
		return
	}

	size := dns.MinMsgSize
	if opt := req.IsEdns0(); opt != nil {
		if udpSize := int(opt.UDPSize()); udpSize > size {
			size = udpSize
		}
	}

	resp.Truncate(size)
}

// protocolOf 返回客户端到本机的传输协议。
func protocolOf(w dns.ResponseWriter) string {
	if w == nil || w.RemoteAddr() == nil {
		return "udp"
	}

	return strings.ToLower(w.RemoteAddr().Network())
}

// proxyProto 把协议文本转换为 dnsproxy 协议枚举。
func proxyProto(protocol string) proxy.Proto {
	if strings.HasPrefix(protocol, "tcp") {
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

// qtypeName 返回查询类型文本。
func qtypeName(qtype uint16) string {
	if name, ok := dns.TypeToString[qtype]; ok {
		return name
	}

	return fmt.Sprintf("TYPE%d", qtype)
}

// durationMS 把时长转换为毫秒。
func durationMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
