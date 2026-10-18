// Command devstub 是开发用的假 sidecar。
//
// 它实现回环 HTTP 的一个最小子集：token 鉴权、READY 握手、health/config/queries/stats，
// 以及按固定速率推送 NDJSON 事件流，用于在没有真实 DNS 内核时验证 Tauri 应用侧通信。
// 事件的 time 是生成该条事件的时刻，便于消费者计算端到端延迟。
//
// 该命令只用于开发验证，不进入产品分发。
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const tokenHeader = "X-Namesplit-Token"

// validToken 报告 token 是否为 64 位十六进制。测试服务必须拒绝弱 token，
// 否则调用方用非法格式也能跑通，掩盖与真实 sidecar 的差异。
func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for _, r := range token {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// heartbeatInterval 是事件流空闲时写空行的间隔。
// 读取方把空行当作保活信号忽略；间隔必须明显小于读取超时（10s），
// 否则空闲连接会被误判为断线并触发重连。
const heartbeatInterval = 5 * time.Second

// event 是逐查询事件；这里只填必有字段，值固定。
type event struct {
	EventID       uint64   `json:"event_id"`
	Time          string   `json:"time"`
	Domain        string   `json:"domain"`
	QType         string   `json:"qtype"`
	Rule          string   `json:"rule"`
	RuleIndex     int      `json:"rule_index"`
	RuleKind      string   `json:"rule_kind"`
	Source        string   `json:"source"`
	Group         string   `json:"group"`
	Upstream      string   `json:"upstream"`
	UpstreamName  string   `json:"upstream_name"`
	DurationMS    float64  `json:"duration_ms"`
	Rcode         int      `json:"rcode"`
	RcodeName     string   `json:"rcode_name"`
	Protocol      string   `json:"protocol"`
	Answers       []string `json:"answers"`
	Authority     []string `json:"authority"`
	Additional    []string `json:"additional"`
	TTL           uint32   `json:"ttl"`
	CacheHit      bool     `json:"cache_hit"`
	DO            bool     `json:"do"`
	AD            bool     `json:"ad"`
	RequestFlags  reqFlags `json:"request_flags"`
	ResponseFlags resFlags `json:"response_flags"`
}

type reqFlags struct {
	RD bool `json:"rd"`
	CD bool `json:"cd"`
}

type resFlags struct {
	AA bool `json:"aa"`
	TC bool `json:"tc"`
	RA bool `json:"ra"`
}

// stream 是一条事件流连接；关闭 done 让 handler 退出并结束响应。
type stream struct {
	done      chan struct{}
	closeOnce sync.Once
}

func (s *stream) close() {
	s.closeOnce.Do(func() { close(s.done) })
}

type stub struct {
	started    time.Time
	token      string
	ratePerSec float64
	batchEvery time.Duration
	closeAfter time.Duration
	eventSeq   atomic.Uint64

	mu      sync.Mutex
	streams map[*stream]struct{}
}

func main() {
	token := os.Getenv("NAMESPLIT_TOKEN")
	if !validToken(token) {
		fmt.Fprintln(os.Stderr, "devstub: NAMESPLIT_TOKEN 缺失、为空或不是 64 位十六进制")
		os.Exit(2)
	}

	s := &stub{
		started:    time.Now(),
		token:      token,
		ratePerSec: envFloat("DEVSERVER_EVENTS_PER_SEC", 1000),
		batchEvery: time.Duration(envInt("DEVSERVER_BATCH_MS", 50)) * time.Millisecond,
		closeAfter: time.Duration(envInt("DEVSERVER_CLOSE_STREAM_AFTER_MS", 0)) * time.Millisecond,
		streams:    make(map[*stream]struct{}),
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "devstub: 监听回环端口失败: %v\n", err)
		os.Exit(1)
	}

	srv := &http.Server{Handler: s.routes()}
	fmt.Printf("NAMESPLIT_READY {\"port\":%d,\"pid\":%d}\n", ln.Addr().(*net.TCPAddr).Port, os.Getpid())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-shutdownRequested:
	case <-sig:
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "devstub: HTTP 服务退出: %v\n", err)
			os.Exit(1)
		}
	}

	s.closeAllStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// shutdownRequested 由 /api/shutdown 关闭，用于测试脚本优雅收尾。
var shutdownRequested = make(chan struct{})

var shutdownOnce sync.Once

func (s *stub) routes() http.Handler {
	mux := http.NewServeMux()
	handle := func(method, path string, h http.HandlerFunc) {
		mux.HandleFunc(path, s.requireToken(method, h))
	}
	handle(http.MethodGet, "/api/health", s.handleHealth)
	handle(http.MethodGet, "/api/status", s.handleStatus)
	handle(http.MethodGet, "/api/config", s.handleConfig)
	handle(http.MethodGet, "/api/queries", s.handleQueries)
	handle(http.MethodGet, "/api/stats", s.handleStats)
	handle(http.MethodGet, "/api/events", s.handleEvents)
	handle(http.MethodGet, "/api/dev/disconnect", s.handleDevDisconnect)
	handle(http.MethodPost, "/api/shutdown", s.handleShutdown)
	return mux
}

func (s *stub) requireToken(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(tokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		// 先鉴权再校验方法：未认证请求一律 401，不暴露路径支持的方法；
		// 已认证但方法不符时返回 405 并带上 Allow。
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		next(w, r)
	}
}

func (s *stub) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":           true,
		"pid":             os.Getpid(),
		"version":         "0.0.0-devstub",
		"commit":          "devstub",
		"started_at":      s.started.UTC().Format(time.RFC3339Nano),
		"uptime_ms":       time.Since(s.started).Milliseconds(),
		"config_revision": 1,
		"config_error":    nil,
		"dns": map[string]any{
			"ready":      false,
			"udp":        []string{},
			"tcp":        []string{},
			"bind_error": map[string]any{"code": "dns_bind_failed", "message": "devstub 不监听 DNS 端口"},
		},
	})
}

func (s *stub) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         "0.0.0-devstub",
		"commit":          "devstub",
		"uptime_ms":       time.Since(s.started).Milliseconds(),
		"config_revision": 1,
		"config_error":    nil,
		"dns": map[string]any{
			"ready":      false,
			"udp":        []string{},
			"tcp":        []string{},
			"bind_error": map[string]any{"code": "dns_bind_failed", "message": "devstub 不监听 DNS 端口"},
		},
	})
}

func (s *stub) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":     1,
		"config_error": nil,
		"warnings":     []any{},
		"config": map[string]any{
			"schema_version": 1,
			"settings": map[string]any{
				"default_group": "default",
				"log_ring_size": 5000,
				"log_persist":   true,
				"ptr_auto":      true,
			},
			"upstreams": map[string]any{
				"cloudflare": map[string]any{"name": "Cloudflare", "url": "udp://1.1.1.1:53", "timeout_ms": 5000},
			},
			"groups": map[string]any{
				"default": map[string]any{"name": "默认", "upstreams": []string{"cloudflare"}},
			},
			"rules":         []any{},
			"local_records": []any{},
		},
	})
}

func (s *stub) handleQueries(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items":               []any{},
		"next_cursor":         nil,
		"persistence_enabled": true,
	})
}

func (s *stub) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_ms": time.Since(s.started).Milliseconds(),
		"counters": map[string]any{
			"queries":        0,
			"failures":       0,
			"cache_hits":     0,
			"dropped_stream": 0,
			"persist_errors": 0,
		},
		"qps_1s":              0,
		"persistence_enabled": true,
	})
}

func (s *stub) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}

	st := &stream{done: make(chan struct{})}
	s.mu.Lock()
	s.streams[st] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, st)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	lastWrite := time.Now()

	if s.closeAfter > 0 {
		time.AfterFunc(s.closeAfter, st.close)
	}

	ticker := time.NewTicker(s.batchEvery)
	defer ticker.Stop()

	// 用累加器保证小数速率也能长期保持平均速率。
	carry := 0.0
	enc := json.NewEncoder(w)

	for {
		select {
		case <-st.done:
			return
		case <-r.Context().Done():
			return
		case <-ticker.C:
			carry += s.ratePerSec * s.batchEvery.Seconds()
			n := int(carry)
			carry -= float64(n)
			if n <= 0 {
				// 空闲时写空行保活；有事件写出时不写，避免打乱批次内容。
				if time.Since(lastWrite) >= heartbeatInterval {
					if _, err := w.Write([]byte("\n")); err != nil {
						return
					}
					flusher.Flush()
					lastWrite = time.Now()
				}
				continue
			}

			// 单批最多 500 条；高速率时拆成多条 NDJSON 行，避免单行过大。
			remaining := n
			for remaining > 0 {
				size := remaining
				if size > 500 {
					size = 500
				}
				remaining -= size

				batch := make([]event, size)
				now := time.Now().UTC().Format(time.RFC3339Nano)
				for i := range batch {
					id := s.eventSeq.Add(1)
					batch[i] = event{
						EventID:       id,
						Time:          now,
						Domain:        fmt.Sprintf("probe-%d.example.", id),
						QType:         "A",
						RuleIndex:     -1,
						RuleKind:      "default",
						Source:        "upstream",
						Group:         "default",
						Upstream:      "udp://1.1.1.1:53",
						UpstreamName:  "Cloudflare",
						DurationMS:    0.1,
						Rcode:         0,
						RcodeName:     "NOERROR",
						Protocol:      "udp",
						Answers:       []string{fmt.Sprintf("probe-%d.example.\t60\tIN\tA\t192.0.2.1", id)},
						Authority:     []string{},
						Additional:    []string{},
						TTL:           60,
						RequestFlags:  reqFlags{RD: true},
						ResponseFlags: resFlags{RA: true},
					}
				}

				if err := enc.Encode(batch); err != nil {
					return
				}
			}
			flusher.Flush()
			lastWrite = time.Now()
		}
	}
}

// handleDevDisconnect 主动关闭当前所有事件流，仅用于验证断线重连。
func (s *stub) handleDevDisconnect(w http.ResponseWriter, _ *http.Request) {
	n := s.closeAllStreams()
	writeJSON(w, http.StatusOK, map[string]int{"closed": n})
}

func (s *stub) closeAllStreams() int {
	s.mu.Lock()
	streams := s.streams
	s.streams = make(map[*stream]struct{})
	s.mu.Unlock()

	for st := range streams {
		st.close()
	}
	return len(streams)
}

func (s *stub) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})
	shutdownOnce.Do(func() { close(shutdownRequested) })
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func envInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("忽略非法环境变量 %s=%q，使用默认值 %d", name, raw, def)
		return def
	}
	return n
}

func envFloat(name string, def float64) float64 {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || n <= 0 {
		log.Printf("忽略非法环境变量 %s=%q，使用默认值 %v", name, raw, def)
		return def
	}
	return n
}
