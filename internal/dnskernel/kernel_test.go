package dnskernel

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/event"
	"github.com/CedarHuang/Namesplit/internal/localrecords"
	"github.com/CedarHuang/Namesplit/internal/mockdns"
	"github.com/CedarHuang/Namesplit/internal/rules"
	"github.com/CedarHuang/Namesplit/internal/upstream"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// capturedRecord 是一条被捕获的日志记录。
type capturedRecord struct {
	message string
	attrs   map[string]string
}

// captureHandler 是并发安全的 slog.Handler，用于断言内核日志。
type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]string)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()

		return true
	})

	h.mu.Lock()
	defer h.mu.Unlock()

	h.records = append(h.records, capturedRecord{message: record.Message, attrs: attrs})

	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// snapshot 返回当前已捕获记录的副本。
func (h *captureHandler) snapshot() (records []capturedRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append(records, h.records...)
}

// findByMessage 返回消息匹配的首条记录。
func findByMessage(records []capturedRecord, message string) (capturedRecord, bool) {
	for _, record := range records {
		if record.message == message {
			return record, true
		}
	}

	return capturedRecord{}, false
}

type fakeView struct {
	rules        *rules.Table
	local        *localrecords.Table
	groups       *upstream.Manager
	defaultGroup string
}

func (v *fakeView) Rules() *rules.Table        { return v.rules }
func (v *fakeView) Local() *localrecords.Table { return v.local }
func (v *fakeView) Groups() *upstream.Manager  { return v.groups }
func (v *fakeView) DefaultGroup() string       { return v.defaultGroup }

type fakeStore struct {
	view *fakeView
}

func (s *fakeStore) Acquire() (View, func()) { return s.view, func() {} }

func compileRules(t *testing.T, input []rules.Rule) *rules.Table {
	t.Helper()

	table, diags := rules.Compile(input)
	if len(diags) > 0 {
		t.Fatalf("编译规则: %+v", diags)
	}

	return table
}

func compileLocal(t *testing.T, input []localrecords.Record, ptrAuto bool) *localrecords.Table {
	t.Helper()

	table, diags := localrecords.Compile(input, ptrAuto)
	if diags != nil {
		t.Fatalf("编译本地记录: %+v", diags)
	}

	return table
}

func newManager(t *testing.T, specs map[string]upstream.Spec, groups map[string][]string) *upstream.Manager {
	t.Helper()

	manager, err := upstream.NewManager(specs, groups, discardLogger())
	if err != nil {
		t.Fatalf("构造上游管理器: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	return manager
}

func startKernel(t *testing.T, store Store, sink func(event.Event), targets []Target) (*Server, Status) {
	t.Helper()

	return startKernelWithLogger(t, store, sink, targets, discardLogger())
}

func startKernelWithLogger(
	t *testing.T,
	store Store,
	sink func(event.Event),
	targets []Target,
	logger *slog.Logger,
) (*Server, Status) {
	t.Helper()

	server, err := New(Config{Store: store, Sink: sink, Logger: logger, Targets: targets})
	if err != nil {
		t.Fatalf("构造内核: %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("启动内核: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	return server, server.Status()
}

func testTargets() []Target {
	return []Target{
		{Network: "udp", Addr: "127.0.0.1:0"},
		{Network: "tcp", Addr: "127.0.0.1:0"},
	}
}

func exchange(t *testing.T, addr string, network string, msg *dns.Msg) *dns.Msg {
	t.Helper()

	client := &dns.Client{Net: network, Timeout: 3 * time.Second}
	response, _, err := client.Exchange(msg, addr)
	if err != nil {
		t.Fatalf("查询 %s/%s: %v", addr, network, err)
	}

	return response
}

func aValues(t *testing.T, msg *dns.Msg) []string {
	t.Helper()

	if msg == nil {
		return nil
	}

	values := make([]string, 0)
	for _, rr := range msg.Answer {
		if record, ok := rr.(*dns.A); ok {
			values = append(values, record.A.String())
		}
	}

	return values
}

func receiveEvent(t *testing.T, events <-chan event.Event) event.Event {
	t.Helper()

	select {
	case ev := <-events:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("未收到查询事件")
	}

	return event.Event{}
}

func TestRoutingAndEventFields(t *testing.T) {
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

	mockC, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 3}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock C: %v", err)
	}
	t.Cleanup(func() { _ = mockC.Close() })

	manager := newManager(t,
		map[string]upstream.Spec{
			"a": {ID: "a", Name: "上游 A", URL: mockA.URL(), Timeout: 2 * time.Second},
			"b": {ID: "b", Name: "上游 B", URL: mockB.URL(), Timeout: 2 * time.Second},
			"c": {ID: "c", Name: "上游 C", URL: mockC.URL(), Timeout: 2 * time.Second},
		},
		map[string][]string{
			"corp":    {"a", "b"},
			"default": {"c"},
		},
	)

	view := &fakeView{
		rules: compileRules(t, []rules.Rule{
			{Kind: rules.KindDomain, Domain: "api.corp.local", Group: "corp"},
			{Kind: rules.KindSuffix, Domain: "corp.local", Group: "corp"},
		}),
		groups:       manager,
		defaultGroup: "default",
	}
	events := make(chan event.Event, 16)
	_, status := startKernel(t, &fakeStore{view: view}, func(ev event.Event) { events <- ev }, testTargets())
	if !status.Ready {
		t.Fatalf("状态未就绪: %+v", status)
	}

	udpAddr := status.UDP[0]
	response := exchange(t, udpAddr, "udp", newQuery("api.corp.local.", dns.TypeA))
	if got := aValues(t, response); len(got) != 1 || (got[0] != "192.0.2.1" && got[0] != "192.0.2.2") {
		t.Fatalf("应答 = %v", got)
	}

	ev := receiveEvent(t, events)
	if ev.Domain != "api.corp.local." || ev.QType != "A" {
		t.Fatalf("事件域名/类型 = %+v", ev)
	}
	if ev.Time.Location() != time.UTC {
		t.Fatalf("事件时间不是 UTC: %v", ev.Time)
	}
	if ev.Rule != "api.corp.local" || ev.RuleIndex != 0 || ev.RuleKind != "exact" {
		t.Fatalf("规则字段 = %+v", ev)
	}
	if ev.Source != "upstream" || ev.Group != "corp" || ev.Protocol != "udp" {
		t.Fatalf("来源字段 = %+v", ev)
	}
	if ev.Upstream == "" || ev.UpstreamName == "" || len(ev.Attempts) == 0 {
		t.Fatalf("上游字段 = %+v", ev)
	}
	if ev.TTL != 60 || len(ev.Answers) != 1 {
		t.Fatalf("应答字段 = %+v", ev)
	}
	if ev.EventID != 1 {
		t.Fatalf("event_id = %d", ev.EventID)
	}

	exchange(t, udpAddr, "udp", newQuery("www.corp.local.", dns.TypeA))
	suffixEvent := receiveEvent(t, events)
	if suffixEvent.RuleIndex != 1 || suffixEvent.RuleKind != "suffix" {
		t.Fatalf("后缀规则事件 = %+v", suffixEvent)
	}

	exchange(t, udpAddr, "udp", newQuery("other.test.", dns.TypeA))
	defaultEvent := receiveEvent(t, events)
	if defaultEvent.RuleKind != "default" || defaultEvent.RuleIndex != -1 || defaultEvent.Rule != "" || defaultEvent.Group != "default" {
		t.Fatalf("默认组事件 = %+v", defaultEvent)
	}

	exchange(t, udpAddr, "udp", newQuery("api.corp.local.", dns.TypeA))
	cacheEvent := receiveEvent(t, events)
	if !cacheEvent.CacheHit || cacheEvent.Source != "cache" || len(cacheEvent.Attempts) != 0 {
		t.Fatalf("缓存事件 = %+v", cacheEvent)
	}

	tcpAddr := status.TCP[0]
	exchange(t, tcpAddr, "tcp", newQuery("tcp.corp.local.", dns.TypeA))
	tcpEvent := receiveEvent(t, events)
	if tcpEvent.Protocol != "tcp" || tcpEvent.RuleIndex != 1 {
		t.Fatalf("TCP 事件 = %+v", tcpEvent)
	}
}

func TestLocalRecordsPath(t *testing.T) {
	t.Parallel()

	view := &fakeView{
		rules: compileRules(t, nil),
		local: compileLocal(t, []localrecords.Record{
			{Type: localrecords.TypeA, Name: "nas.home", Value: "192.168.1.10", TTL: 60, Enabled: true},
		}, true),
		defaultGroup: "default",
	}
	events := make(chan event.Event, 4)
	_, status := startKernel(t, &fakeStore{view: view}, func(ev event.Event) { events <- ev }, testTargets())

	response := exchange(t, status.UDP[0], "udp", newQuery("nas.home.", dns.TypeA))
	if got := aValues(t, response); len(got) != 1 || got[0] != "192.168.1.10" {
		t.Fatalf("本地应答 = %v", got)
	}
	if !response.Authoritative {
		t.Fatal("本地应答应置 AA")
	}

	ev := receiveEvent(t, events)
	if ev.Source != "local" || ev.RuleKind != "local" || ev.Rule != "" || ev.RuleIndex != -1 {
		t.Fatalf("本地事件 = %+v", ev)
	}
	if ev.Group != "" || ev.Upstream != "" || ev.UpstreamName != "" || len(ev.Attempts) != 0 {
		t.Fatalf("本地事件上游字段 = %+v", ev)
	}

	exchange(t, status.UDP[0], "udp", newQuery("nas.home.", dns.TypeAAAA))
	nodata := receiveEvent(t, events)
	if nodata.Source != "local" || len(nodata.Answers) != 0 || nodata.Rcode != 0 {
		t.Fatalf("NODATA 事件 = %+v", nodata)
	}
}

func TestProtocolRejectsAreHandledByKernel(t *testing.T) {
	t.Parallel()

	view := &fakeView{rules: compileRules(t, nil), defaultGroup: "default"}
	events := make(chan event.Event, 4)
	handler := &captureHandler{}
	_, status := startKernelWithLogger(
		t,
		&fakeStore{view: view},
		func(ev event.Event) { events <- ev },
		testTargets(),
		slog.New(handler),
	)
	addr := status.UDP[0]

	multiple := new(dns.Msg)
	multiple.Id = dns.Id()
	multiple.RecursionDesired = true
	multiple.Question = []dns.Question{
		{Name: "a.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
		{Name: "b.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
	}
	if resp := exchange(t, addr, "udp", multiple); resp.Rcode != dns.RcodeFormatError {
		t.Fatalf("多问题 RCODE = %d", resp.Rcode)
	}

	statusOpcode := newQuery("a.test.", dns.TypeA)
	statusOpcode.Opcode = dns.OpcodeStatus
	if resp := exchange(t, addr, "udp", statusOpcode); resp.Rcode != dns.RcodeNotImplemented {
		t.Fatalf("非 QUERY opcode RCODE = %d", resp.Rcode)
	}

	chaos := newQuery("a.test.", dns.TypeA)
	chaos.Question[0].Qclass = dns.ClassCHAOS
	if resp := exchange(t, addr, "udp", chaos); resp.Rcode != dns.RcodeRefused {
		t.Fatalf("非 IN 类 RCODE = %d", resp.Rcode)
	}

	time.Sleep(50 * time.Millisecond)
	if len(events) != 0 {
		t.Fatalf("协议拒绝产生了事件: %d", len(events))
	}

	records := handler.snapshot()
	if len(records) != 3 {
		t.Fatalf("拒绝日志数量 = %d, 期望 3: %+v", len(records), records)
	}
	for _, record := range records {
		if record.message != "拒绝协议级请求" {
			t.Fatalf("日志消息 = %q", record.message)
		}
		for _, key := range []string{"opcode", "qdcount", "qclass"} {
			if _, ok := record.attrs[key]; !ok {
				t.Fatalf("日志缺少 %q: %+v", key, record.attrs)
			}
		}
	}

	byOpcode, ok := findRecord(records, "opcode", fmt.Sprint(dns.OpcodeStatus))
	if !ok || byOpcode.attrs["qdcount"] != "1" || byOpcode.attrs["qclass"] != fmt.Sprint(dns.ClassINET) {
		t.Fatalf("非 QUERY opcode 日志 = %+v", byOpcode)
	}
	if _, ok := findRecord(records, "qdcount", "2"); !ok {
		t.Fatalf("缺少多问题日志: %+v", records)
	}
	if _, ok := findRecord(records, "qclass", fmt.Sprint(dns.ClassCHAOS)); !ok {
		t.Fatalf("缺少非 IN 类日志: %+v", records)
	}
}

// findRecord 按属性值查找日志记录。
func findRecord(records []capturedRecord, key, value string) (capturedRecord, bool) {
	for _, record := range records {
		if record.attrs[key] == value {
			return record, true
		}
	}

	return capturedRecord{}, false
}

func TestInvalidMessageIsLogged(t *testing.T) {
	t.Parallel()

	view := &fakeView{rules: compileRules(t, nil), defaultGroup: "default"}
	handler := &captureHandler{}
	_, status := startKernelWithLogger(
		t,
		&fakeStore{view: view},
		nil,
		testTargets(),
		slog.New(handler),
	)

	conn, err := net.Dial("udp", status.UDP[0])
	if err != nil {
		t.Fatalf("连接内核: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// 不足 12 字节的消息连报头都解析不了，库层会走 MsgInvalidFunc 并直接丢弃。
	if _, err := conn.Write([]byte{0x12, 0x34, 0x01}); err != nil {
		t.Fatalf("发送非法报文: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if record, ok := findByMessage(handler.snapshot(), "丢弃无法解析的 DNS 报文"); ok {
			if record.attrs["bytes"] != "3" {
				t.Fatalf("日志 bytes = %q", record.attrs["bytes"])
			}
			if record.attrs["error"] == "" {
				t.Fatalf("日志缺少 error: %+v", record)
			}

			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("未记录无法解析报文的日志")
}

func TestAcceptDNSMessage(t *testing.T) {
	t.Parallel()

	if action := acceptDNSMessage(dns.Header{Bits: headerQRBit}); action != dns.MsgIgnore {
		t.Fatalf("QR=1 的动作 = %v, 期望 MsgIgnore", action)
	}
	if action := acceptDNSMessage(dns.Header{Bits: 0}); action != dns.MsgAccept {
		t.Fatalf("QR=0 的动作 = %v, 期望 MsgAccept", action)
	}
}

func TestECSStrippedAndDOPreserved(t *testing.T) {
	t.Parallel()

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	manager := newManager(t,
		map[string]upstream.Spec{"a": {ID: "a", URL: mock.URL(), Timeout: 2 * time.Second}},
		map[string][]string{"default": {"a"}},
	)
	view := &fakeView{rules: compileRules(t, nil), groups: manager, defaultGroup: "default"}
	events := make(chan event.Event, 4)
	_, status := startKernel(t, &fakeStore{view: view}, func(ev event.Event) { events <- ev }, testTargets())

	query := newQuery("ecs.test.", dns.TypeA)
	query.SetEdns0(1232, true)
	opt := query.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_SUBNET{
		Code:          dns.EDNS0SUBNET,
		Family:        1,
		SourceNetmask: 24,
		Address:       net.IP{203, 0, 113, 0},
	})

	response := exchange(t, status.UDP[0], "udp", query)

	seen := mock.Seen()
	if len(seen) != 1 {
		t.Fatalf("上游收到 %d 条查询", len(seen))
	}
	seenOpt := seen[0].IsEdns0()
	if seenOpt == nil {
		t.Fatal("上游未收到 EDNS")
	}
	if !seenOpt.Do() {
		t.Fatal("DO 位未透传")
	}
	for _, option := range seenOpt.Option {
		if option.Option() == dns.EDNS0SUBNET {
			t.Fatal("ECS 未剥离")
		}
	}

	if response.IsEdns0() != nil {
		t.Fatalf("响应不应携带 OPT: %v", response.Extra)
	}
	ev := receiveEvent(t, events)
	if !ev.DO {
		t.Fatalf("事件 DO 位 = false")
	}
	for _, rr := range ev.Additional {
		if strings.Contains(rr, "OPT PSEUDOSECTION") {
			t.Fatalf("事件 additional 含 OPT 伪 RR: %q", rr)
		}
	}
}

func TestStripOPT(t *testing.T) {
	t.Parallel()

	msg := new(dns.Msg)
	msg.Extra = []dns.RR{
		&dns.A{Hdr: dns.RR_Header{Name: "ns.test.", Rrtype: dns.TypeA, Class: dns.ClassINET}},
		&dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Class: 1232}},
	}

	stripOPT(msg)

	if msg.IsEdns0() != nil {
		t.Fatal("OPT 未被剥离")
	}
	if len(msg.Extra) != 1 || msg.Extra[0].Header().Rrtype != dns.TypeA {
		t.Fatalf("残留附加段 = %v", msg.Extra)
	}

	// nil 与无附加段的消息必须安全通过。
	stripOPT(nil)
	stripOPT(new(dns.Msg))
}

func TestUpstreamFailureProducesSERVFAIL(t *testing.T) {
	t.Parallel()

	manager := newManager(t,
		map[string]upstream.Spec{"bad": {ID: "bad", URL: "tcp://127.0.0.1:1", Timeout: 200 * time.Millisecond}},
		map[string][]string{"default": {"bad"}},
	)
	view := &fakeView{rules: compileRules(t, nil), groups: manager, defaultGroup: "default"}
	events := make(chan event.Event, 4)
	_, status := startKernel(t, &fakeStore{view: view}, func(ev event.Event) { events <- ev }, testTargets())

	response := exchange(t, status.UDP[0], "udp", newQuery("fail.test.", dns.TypeA))
	if response.Rcode != dns.RcodeServerFailure {
		t.Fatalf("RCODE = %d", response.Rcode)
	}

	ev := receiveEvent(t, events)
	if ev.Source != "upstream" || ev.Error == "" {
		t.Fatalf("失败事件 = %+v", ev)
	}
	if ev.Upstream != "" || ev.UpstreamName != "" || len(ev.Answers) != 0 || ev.TTL != 0 {
		t.Fatalf("失败事件字段 = %+v", ev)
	}
	if len(ev.Attempts) == 0 {
		t.Fatal("失败事件应包含 attempts")
	}
}

func TestStatusSlicesAreNonNil(t *testing.T) {
	t.Parallel()

	// 未启动的内核没有任何监听地址，但状态切片必须是非 nil 空切片，JSON 才会
	// 输出 [] 而不是 null。
	server, err := New(Config{Store: &fakeStore{view: &fakeView{}}, Targets: testTargets()})
	if err != nil {
		t.Fatalf("构造内核: %v", err)
	}
	status := server.Status()
	if status.UDP == nil || status.TCP == nil {
		t.Fatalf("监听地址切片为 nil: %+v", status)
	}
	if len(status.UDP) != 0 || len(status.TCP) != 0 {
		t.Fatalf("未启动内核的监听地址 = %+v", status)
	}

	// 绑定失败时 Targets 也不能是 nil。
	_, failed := startKernel(t, &fakeStore{view: &fakeView{}}, nil, []Target{
		{Network: "udp", Addr: "not-an-address"},
	})
	if failed.BindError == nil {
		t.Fatal("绑定失败应产生 BindError")
	}
	if failed.BindError.Targets == nil {
		t.Fatalf("BindError.Targets 为 nil: %+v", failed.BindError)
	}
}

func TestPartialBindFailureStillServes(t *testing.T) {
	t.Parallel()

	view := &fakeView{
		rules: compileRules(t, nil),
		local: compileLocal(t, []localrecords.Record{
			{Type: localrecords.TypeA, Name: "test.home", Value: "192.168.1.1", TTL: 60, Enabled: true},
		}, true),
		defaultGroup: "default",
	}
	_, status := startKernel(t, &fakeStore{view: view}, nil, []Target{
		{Network: "udp", Addr: "127.0.0.1:0"},
		{Network: "tcp", Addr: "not-an-address"},
	})

	if status.Ready {
		t.Fatal("部分绑定失败时 Ready 应为 false")
	}
	if status.BindError == nil || len(status.BindError.Targets) != 1 {
		t.Fatalf("BindError = %+v", status.BindError)
	}
	if len(status.UDP) != 1 {
		t.Fatalf("UDP 地址 = %v", status.UDP)
	}

	response := exchange(t, status.UDP[0], "udp", newQuery("test.home.", dns.TypeA))
	if got := aValues(t, response); len(got) != 1 {
		t.Fatalf("应答 = %v", got)
	}
}

func TestUDPTruncation(t *testing.T) {
	t.Parallel()

	view := &fakeView{
		rules: compileRules(t, nil),
		local: compileLocal(t, []localrecords.Record{
			{Type: localrecords.TypeTXT, Name: "big.home", Value: strings.Repeat("a", 1024), TTL: 60, Enabled: true},
		}, true),
		defaultGroup: "default",
	}
	_, status := startKernel(t, &fakeStore{view: view}, nil, testTargets())

	response := exchange(t, status.UDP[0], "udp", newQuery("big.home.", dns.TypeTXT))
	if !response.Truncated {
		t.Fatalf("UDP 应答未截断，长度=%d", response.Len())
	}
}

func TestKernelOverheadP50P99(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("short 或 race 模式跳过时序基准")
	}

	mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}, TTL: 60})
	if err != nil {
		t.Fatalf("启动 mock: %v", err)
	}
	t.Cleanup(func() { _ = mock.Close() })

	manager := newManager(t,
		map[string]upstream.Spec{"a": {ID: "a", URL: mock.URL(), Timeout: 2 * time.Second}},
		map[string][]string{"default": {"a"}},
	)
	view := &fakeView{rules: compileRules(t, nil), groups: manager, defaultGroup: "default"}
	_, status := startKernel(t, &fakeStore{view: view}, nil, testTargets())
	kernelAddr := status.UDP[0]
	mockAddr := mock.Addr()

	kernelClient := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
	directClient := &dns.Client{Net: "udp", Timeout: 3 * time.Second}

	const samples = 500
	overheads := make([]time.Duration, 0, samples)

	for i := 0; i < samples; i++ {
		domain := fmt.Sprintf("q-%d.test.", i)

		start := time.Now()
		if _, _, err := directClient.Exchange(newQuery(domain, dns.TypeA), mockAddr); err != nil {
			t.Fatalf("直连查询: %v", err)
		}
		direct := time.Since(start)

		start = time.Now()
		if _, _, err := kernelClient.Exchange(newQuery(domain, dns.TypeA), kernelAddr); err != nil {
			t.Fatalf("内核查询: %v", err)
		}
		kernel := time.Since(start)

		overheads = append(overheads, kernel-direct)
	}

	sort.Slice(overheads, func(i, j int) bool { return overheads[i] < overheads[j] })
	p50 := overheads[len(overheads)*50/100]
	p99 := overheads[len(overheads)*99/100]

	t.Logf("内核额外开销 p50=%v p99=%v", p50, p99)
	if p50 > time.Millisecond {
		t.Fatalf("p50 = %v，超过 1ms", p50)
	}
	if p99 > 5*time.Millisecond {
		t.Fatalf("p99 = %v，超过 5ms", p99)
	}
}

func newQuery(domain string, qtype uint16) *dns.Msg {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(domain), qtype)
	msg.RecursionDesired = true

	return msg
}
