package api

import "net/http"

// rulesTestRequest 是规则测试请求。
type rulesTestRequest struct {
	Domain string `json:"domain"`
	QType  string `json:"qtype,omitempty"`
}

// handleRulesTest 返回域名命中的本地记录与规则。
func (s *Server) handleRulesTest(w http.ResponseWriter, r *http.Request) {
	var request rulesTestRequest
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, err)

		return
	}
	if request.Domain == "" {
		writeError(w, &Error{Code: "invalid_request", Message: "缺少 domain"})

		return
	}

	result, err := s.backend.RulesTest(r.Context(), request.Domain, request.QType)
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}
