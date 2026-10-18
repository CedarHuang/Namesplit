package api

import (
	"encoding/json"
	"net/http"
)

// validateRequest 是配置校验请求。
type validateRequest struct {
	Config json.RawMessage `json:"config"`
}

// saveRequest 是配置保存请求。
type saveRequest struct {
	BaseRevision *uint64         `json:"base_revision"`
	Config       json.RawMessage `json:"config"`
}

// resetRequest 是恢复默认配置请求。
type resetRequest struct {
	BaseRevision *uint64 `json:"base_revision"`
}

// handleConfigGet 返回当前配置。
func (s *Server) handleConfigGet(w http.ResponseWriter, _ *http.Request) {
	state, err := s.backend.ConfigState()
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, state)
}

// handleConfigValidate 校验配置，始终返回 200。
func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	var request validateRequest
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, err)

		return
	}

	result, err := s.backend.ConfigValidate(request.Config)
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleConfigSave 校验、应用并保存配置。
func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	var request saveRequest
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, err)

		return
	}
	if request.BaseRevision == nil {
		writeError(w, &Error{Code: "invalid_request", Message: "缺少 base_revision"})

		return
	}

	result, err := s.backend.ConfigSave(r.Context(), *request.BaseRevision, request.Config)
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleConfigReset 恢复内置默认配置。
func (s *Server) handleConfigReset(w http.ResponseWriter, r *http.Request) {
	var request resetRequest
	if err := s.decodeJSON(w, r, &request); err != nil {
		writeError(w, err)

		return
	}
	if request.BaseRevision == nil {
		writeError(w, &Error{Code: "invalid_request", Message: "缺少 base_revision"})

		return
	}

	result, err := s.backend.ConfigReset(r.Context(), *request.BaseRevision)
	if err != nil {
		writeError(w, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}
