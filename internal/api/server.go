package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"sync/atomic"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
)

// MaxBodyBytes 是请求体上限。
const MaxBodyBytes = 16 << 20

// Deps 是 Server 的依赖。
type Deps struct {
	Token   string
	Backend Backend
	Logger  *slog.Logger
}

// Server 实现内部 HTTP API。
type Server struct {
	backend Backend
	token   string
	logger  *slog.Logger
	mux     *http.ServeMux

	ready        atomic.Bool
	shuttingDown atomic.Bool
}

// NewServer 构造 API 服务并注册路由。
func NewServer(deps Deps) (*Server, error) {
	if deps.Token == "" {
		return nil, errors.New("API token 不能为空")
	}
	if deps.Backend == nil {
		return nil, errors.New("API backend 不能为空")
	}

	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	server := &Server{
		backend: deps.Backend,
		token:   deps.Token,
		logger:  logger,
		mux:     http.NewServeMux(),
	}
	server.routes()

	return server, nil
}

// SetReady 设置服务是否已完成初始化。
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// BeginShutdown 标记进入退出流程。
func (s *Server) BeginShutdown() {
	s.shuttingDown.Store(true)
}

// routes 注册全部路由。
func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/config", s.handleConfigGet)
	s.mux.HandleFunc("PUT /api/config", s.handleConfigSave)
	s.mux.HandleFunc("POST /api/config/validate", s.handleConfigValidate)
	s.mux.HandleFunc("POST /api/config/reset", s.handleConfigReset)
	s.mux.HandleFunc("POST /api/rules/test", s.handleRulesTest)
	s.mux.HandleFunc("GET /api/queries", s.handleQueries)
	s.mux.HandleFunc("DELETE /api/queries", s.handleQueriesClear)
	s.mux.HandleFunc("GET /api/stats", s.handleStats)
	s.mux.HandleFunc("GET /api/stats/minute", s.handleStatsMinute)
	s.mux.HandleFunc("GET /api/stats/top", s.handleStatsTop)
	s.mux.HandleFunc("POST /api/upstreams/test", s.handleUpstreamTest)
	s.mux.HandleFunc("POST /api/shutdown", s.handleShutdown)
}

// ServeHTTP 实现 token 校验与就绪/退出状态门控。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !constantTimeEqual(r.Header.Get("X-Namesplit-Token"), s.token) {
		writeError(w, &Error{Code: "unauthorized", Message: "token 缺失或错误"})

		return
	}

	if r.URL.Path != "/api/health" {
		if s.shuttingDown.Load() {
			writeError(w, &Error{Code: "shutdown_in_progress"})

			return
		}
		if !s.ready.Load() {
			writeError(w, &Error{Code: "not_ready"})

			return
		}
	}

	s.mux.ServeHTTP(w, r)
}

// handleHealth 返回健康状态。
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Health())
}

// handleStatus 返回诊断状态。
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Status())
}

// handleShutdown 接受退出请求。
func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	s.BeginShutdown()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})

	go s.backend.Shutdown()
}

// constantTimeEqual 比较两个 token。
func constantTimeEqual(got, want string) bool {
	if len(got) != len(want) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// writeJSON 写出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// errorBody 是错误响应体。
type errorBody struct {
	Error           string                  `json:"error"`
	Details         []diagnostic.Diagnostic `json:"details,omitempty"`
	CurrentRevision *uint64                 `json:"current_revision,omitempty"`
}

// writeError 把业务错误映射为 HTTP 错误响应。
func writeError(w http.ResponseWriter, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal"})

		return
	}

	writeJSON(w, statusForCode(apiErr.Code), errorBody{
		Error:           apiErr.Code,
		Details:         apiErr.Details,
		CurrentRevision: apiErr.CurrentRevision,
	})
}

// statusForCode 把错误码映射为 HTTP 状态码。
func statusForCode(code string) int {
	switch code {
	case "invalid_request":
		return http.StatusBadRequest
	case "unauthorized":
		return http.StatusUnauthorized
	case "not_found":
		return http.StatusNotFound
	case "method_not_allowed":
		return http.StatusMethodNotAllowed
	case "revision_conflict":
		return http.StatusConflict
	case "payload_too_large":
		return http.StatusRequestEntityTooLarge
	case "unsupported_media_type":
		return http.StatusUnsupportedMediaType
	case "invalid_config":
		return http.StatusUnprocessableEntity
	case "not_ready", "shutdown_in_progress":
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// decodeJSON 解析 JSON 请求体。
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	contentType := r.Header.Get("Content-Type")
	if contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/json" {
			return &Error{Code: "unsupported_media_type", Message: "请求体必须是 application/json"}
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &Error{Code: "payload_too_large", Message: "请求体超过 16 MiB"}
		}

		return &Error{Code: "invalid_request", Message: "请求体不是合法 JSON"}
	}

	return nil
}
