package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/CedarHuang/Namesplit/internal/logstore"
)

// handleQueries 返回一页历史查询。
func (s *Server) handleQueries(w http.ResponseWriter, r *http.Request) {
	filter, err := parseHistoryFilter(r)
	if err != nil {
		writeError(w, err)

		return
	}

	page, err := s.backend.Queries(r.Context(), filter)
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, page)
}

// handleQueriesClear 清空持久化查询记录。
func (s *Server) handleQueriesClear(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.backend.ClearQueries(r.Context())
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]int64{"deleted": deleted})
}

// parseHistoryFilter 解析历史查询参数。
func parseHistoryFilter(r *http.Request) (logstore.Filter, error) {
	values := r.URL.Query()
	filter := logstore.Filter{
		Domain:   values.Get("domain"),
		QType:    values.Get("qtype"),
		Source:   values.Get("source"),
		Group:    values.Get("group"),
		Rule:     values.Get("rule"),
		Upstream: values.Get("upstream"),
		Cursor:   values.Get("cursor"),
	}

	limit := 100
	if text := values.Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > 500 {
			return logstore.Filter{}, invalidRequest("limit 不在 1–500")
		}
		limit = parsed
	}
	filter.Limit = limit

	if text := values.Get("rcode"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil {
			return logstore.Filter{}, invalidRequest("rcode 不是整数")
		}
		filter.Rcode = &parsed
	}

	if text := values.Get("min_duration_ms"); text != "" {
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return logstore.Filter{}, invalidRequest("min_duration_ms 不是数字")
		}
		filter.MinDurationMS = &parsed
	}

	if text := values.Get("max_duration_ms"); text != "" {
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return logstore.Filter{}, invalidRequest("max_duration_ms 不是数字")
		}
		filter.MaxDurationMS = &parsed
	}

	if text := values.Get("from"); text != "" {
		parsed, err := parseTime(text)
		if err != nil {
			return logstore.Filter{}, err
		}
		filter.From = &parsed
	}

	if text := values.Get("to"); text != "" {
		parsed, err := parseTime(text)
		if err != nil {
			return logstore.Filter{}, err
		}
		filter.To = &parsed
	}

	return filter, nil
}

// parseTime 解析 RFC3339 时间。
func parseTime(text string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, invalidRequest(fmt.Sprintf("时间 %q 不是 RFC3339", text))
	}

	return parsed, nil
}

// handleStats 返回本次运行计数器。
func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Stats())
}

// minuteResponse 是分钟聚合响应。
type minuteResponse struct {
	Items              []logstore.MinuteStat `json:"items"`
	PersistenceEnabled bool                  `json:"persistence_enabled"`
}

// handleStatsMinute 返回分钟聚合。
func (s *Server) handleStatsMinute(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()

	var from, to time.Time
	if text := values.Get("from"); text != "" {
		parsed, err := parseTime(text)
		if err != nil {
			writeError(w, err)

			return
		}
		from = parsed
	}
	if text := values.Get("to"); text != "" {
		parsed, err := parseTime(text)
		if err != nil {
			writeError(w, err)

			return
		}
		to = parsed
	}

	items, persistent := s.backend.Minute(from, to)
	if items == nil {
		items = []logstore.MinuteStat{}
	}

	writeJSON(w, http.StatusOK, minuteResponse{Items: items, PersistenceEnabled: persistent})
}

// topResponse 是 Top 统计响应。
type topResponse struct {
	Items              []logstore.TopItem `json:"items"`
	PersistenceEnabled bool               `json:"persistence_enabled"`
}

// handleStatsTop 返回 Top 域名或上游。
func (s *Server) handleStatsTop(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()

	by := values.Get("by")
	if by != "domain" && by != "upstream" {
		writeError(w, invalidRequest("by 必须是 domain 或 upstream"))

		return
	}

	limit := 10
	if text := values.Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, invalidRequest("limit 不在 1–100"))

			return
		}
		limit = parsed
	}

	var since time.Time
	if text := values.Get("since"); text != "" {
		parsed, err := parseTime(text)
		if err != nil {
			writeError(w, err)

			return
		}
		since = parsed
	}

	items, persistent := s.backend.Top(by, since, limit)
	if items == nil {
		items = []logstore.TopItem{}
	}

	writeJSON(w, http.StatusOK, topResponse{Items: items, PersistenceEnabled: persistent})
}

// invalidRequest 构造 400 错误。
func invalidRequest(message string) *Error {
	return &Error{Code: "invalid_request", Message: strings.TrimSpace(message)}
}
