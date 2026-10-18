package api

import (
	"net/http"
)

// upstreamTestRequest 是连通性测试请求的传输形态。
type upstreamTestRequest struct {
	UpstreamID string         `json:"upstream_id"`
	GroupID    string         `json:"group_id"`
	Upstream   *DraftUpstream `json:"upstream"`
}

// handleUpstreamTest 测试已保存或草稿上游的连通性。
func (s *Server) handleUpstreamTest(w http.ResponseWriter, r *http.Request) {
	var request upstreamTestRequest
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, err)

		return
	}

	provided := 0
	if request.UpstreamID != "" {
		provided++
	}
	if request.GroupID != "" {
		provided++
	}
	if request.Upstream != nil {
		provided++
	}
	if provided != 1 {
		writeError(w, &Error{
			Code:    "invalid_request",
			Message: "upstream_id、group_id、upstream 必须且只能提供一个",
		})

		return
	}
	if request.Upstream != nil && request.Upstream.URL == "" {
		writeError(w, &Error{Code: "invalid_request", Message: "草稿上游缺少 url"})

		return
	}

	results, err := s.backend.UpstreamTest(r.Context(), UpstreamTestRequest{
		UpstreamID: request.UpstreamID,
		GroupID:    request.GroupID,
		Upstream:   request.Upstream,
	})
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
