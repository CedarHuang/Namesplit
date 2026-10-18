package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/api"
	"github.com/CedarHuang/Namesplit/internal/config"
	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/dnsname"
	"github.com/CedarHuang/Namesplit/internal/event"
	"github.com/CedarHuang/Namesplit/internal/logstore"
	"github.com/CedarHuang/Namesplit/internal/upstream"
	"github.com/CedarHuang/Namesplit/internal/version"
)

// Health 实现 api.Backend。
func (a *App) Health() api.Health {
	_, revision, _, cfgErr := a.cfg.Current()

	return api.Health{
		Ready:          true,
		PID:            os.Getpid(),
		Version:        version.Version,
		Commit:         version.Commit,
		StartedAt:      a.startedAt,
		UptimeMS:       time.Since(a.startedAt).Milliseconds(),
		ConfigRevision: revision,
		ConfigError:    errorJSON(cfgErr),
		DNS:            a.dnsStatus(),
	}
}

// Status 实现 api.Backend。
func (a *App) Status() api.Status {
	_, revision, _, cfgErr := a.cfg.Current()

	return api.Status{
		Version:        version.Version,
		Commit:         version.Commit,
		UptimeMS:       time.Since(a.startedAt).Milliseconds(),
		ConfigRevision: revision,
		ConfigError:    errorJSON(cfgErr),
		DNS:            a.dnsStatus(),
	}
}

// ConfigState 实现 api.Backend。
func (a *App) ConfigState() (api.ConfigState, error) {
	cfg, revision, warnings, cfgErr := a.cfg.Current()

	data, err := json.Marshal(cfg)
	if err != nil {
		return api.ConfigState{}, &api.Error{Code: "internal", Message: err.Error()}
	}

	return api.ConfigState{
		Revision:    revision,
		Config:      data,
		ConfigError: errorJSON(cfgErr),
		Warnings:    warnings,
	}, nil
}

// ConfigValidate 实现 api.Backend。
func (a *App) ConfigValidate(raw json.RawMessage) (api.Validation, error) {
	_, diags := config.ParseJSON(raw)

	errorsOnly := diagnostic.Filter(diags, diagnostic.SeverityError)
	warnings := diagnostic.Filter(diags, diagnostic.SeverityWarning)

	return api.Validation{
		Valid:    len(errorsOnly) == 0,
		Errors:   errorsOnly,
		Warnings: warnings,
	}, nil
}

// ConfigSave 实现 api.Backend。
func (a *App) ConfigSave(
	ctx context.Context,
	baseRevision uint64,
	raw json.RawMessage,
) (api.SaveResult, error) {
	cfg, diags := config.ParseJSON(raw)
	if errorsOnly := diagnostic.Filter(diags, diagnostic.SeverityError); len(errorsOnly) > 0 {
		return api.SaveResult{}, &api.Error{Code: "invalid_config", Details: errorsOnly}
	}

	revision, warnings, saveErr := a.cfg.Save(ctx, baseRevision, cfg, a.buildConfigRuntime)
	if saveErr != nil {
		return api.SaveResult{}, apiErrorFromConfig(saveErr)
	}

	return api.SaveResult{Revision: revision, Warnings: warnings}, nil
}

// ConfigReset 实现 api.Backend。
func (a *App) ConfigReset(ctx context.Context, baseRevision uint64) (api.SaveResult, error) {
	revision, warnings, resetErr := a.cfg.Reset(ctx, baseRevision, a.buildConfigRuntime)
	if resetErr != nil {
		return api.SaveResult{}, apiErrorFromConfig(resetErr)
	}

	return api.SaveResult{Revision: revision, Warnings: warnings}, nil
}

// RulesTest 实现 api.Backend。
func (a *App) RulesTest(_ context.Context, domain string, qtypeText string) (api.RuleTest, error) {
	snap := a.store.current()
	if snap == nil {
		return api.RuleTest{}, &api.Error{Code: "internal", Message: "运行时未就绪"}
	}

	canon, err := dnsname.CanonQuery(domain)
	if err != nil {
		return api.RuleTest{}, &api.Error{Code: "invalid_request", Message: err.Error()}
	}

	qtype := parseQType(qtypeText)
	_, _, warnings, _ := a.cfg.Current()

	result := api.RuleTest{
		Canonical:    canon,
		DefaultGroup: snap.DefaultGroup(),
		Warnings:     warnings,
	}

	if snap.Local() != nil {
		if local, ok := snap.Local().Lookup(domain, qtype); ok {
			answers := make([]string, 0, len(local.Answers))
			var ttl uint32
			for index, rr := range local.Answers {
				answers = append(answers, rr.String())
				if index == 0 || rr.Header().Ttl < ttl {
					ttl = rr.Header().Ttl
				}
			}

			result.Local = api.RuleTestLocal{
				Hit:     true,
				Name:    canon,
				QType:   qtypeName(qtype),
				NODATA:  local.NODATA,
				Answers: answers,
				TTL:     ttl,
			}
		}
	}

	if snap.Rules() != nil {
		if hit, ok := snap.Rules().Match(domain); ok {
			result.Rule = &api.RuleTestRule{
				Index:  hit.Index,
				Match:  string(hit.Kind),
				Domain: hit.Domain,
				Group:  hit.Group,
			}
			result.TargetGroup = hit.Group
		}
	}
	if result.TargetGroup == "" {
		result.TargetGroup = snap.DefaultGroup()
	}

	return result, nil
}

// SubscribeEvents 实现 api.Backend。
func (a *App) SubscribeEvents(ctx context.Context) ([]event.Event, <-chan event.Event, func(), error) {
	if a.logs == nil {
		return nil, nil, nil, &api.Error{Code: "internal", Message: "日志存储未就绪"}
	}

	snapshot, updates, cancel := a.logs.Subscribe(ctx)

	return snapshot, updates, cancel, nil
}

// Queries 实现 api.Backend。
func (a *App) Queries(ctx context.Context, filter logstore.Filter) (logstore.Page, error) {
	return a.logs.Queries(ctx, filter)
}

// ClearQueries 实现 api.Backend。
func (a *App) ClearQueries(ctx context.Context) (int64, error) {
	return a.logs.ClearQueries(ctx)
}

// Stats 实现 api.Backend。
func (a *App) Stats() logstore.Stats {
	return a.logs.Stats()
}

// Minute 实现 api.Backend。
func (a *App) Minute(from, to time.Time) ([]logstore.MinuteStat, bool) {
	return a.logs.Minute(from, to)
}

// Top 实现 api.Backend。
func (a *App) Top(by string, since time.Time, limit int) ([]logstore.TopItem, bool) {
	return a.logs.Top(by, since, limit)
}

// UpstreamTest 实现 api.Backend。
func (a *App) UpstreamTest(ctx context.Context, request api.UpstreamTestRequest) ([]upstream.TestResult, error) {
	cfg, _, _, _ := a.cfg.Current()

	specs, err := resolveTestTargets(cfg, request)
	if err != nil {
		return nil, err
	}

	return upstream.Test(ctx, specs, 10*time.Second, a.logger), nil
}

// Shutdown 实现 api.Backend。
func (a *App) Shutdown() {
	a.triggerShutdown()
}

// resolveTestTargets 把测试请求转换为上游目标。
func resolveTestTargets(cfg *config.Config, request api.UpstreamTestRequest) ([]upstream.Spec, error) {
	switch {
	case request.UpstreamID != "":
		item, ok := cfg.Upstreams[request.UpstreamID]
		if !ok {
			return nil, &api.Error{Code: "not_found", Message: "上游不存在"}
		}

		return []upstream.Spec{specFromConfig(request.UpstreamID, item)}, nil
	case request.GroupID != "":
		group, ok := cfg.Groups[request.GroupID]
		if !ok {
			return nil, &api.Error{Code: "not_found", Message: "上游组不存在"}
		}

		specs := make([]upstream.Spec, 0, len(group.Upstreams))
		for _, id := range group.Upstreams {
			item, ok := cfg.Upstreams[id]
			if !ok {
				return nil, &api.Error{Code: "not_found", Message: "组引用的上游不存在"}
			}
			specs = append(specs, specFromConfig(id, item))
		}

		return specs, nil
	case request.Upstream != nil:
		timeout := config.DefaultTimeoutMS
		if request.Upstream.TimeoutMS != nil {
			timeout = *request.Upstream.TimeoutMS
		}
		if timeout < 100 || timeout > 30000 {
			return nil, &api.Error{Code: "invalid_request", Message: "timeout_ms 不在 100–30000"}
		}

		return []upstream.Spec{{
			Name:      request.Upstream.URL,
			URL:       request.Upstream.URL,
			Bootstrap: request.Upstream.Bootstrap,
			Timeout:   time.Duration(timeout) * time.Millisecond,
		}}, nil
	default:
		return nil, &api.Error{Code: "invalid_request", Message: "缺少测试目标"}
	}
}

// specFromConfig 把配置上游转换为测试目标。
func specFromConfig(id string, item config.Upstream) upstream.Spec {
	return upstream.Spec{
		ID:        id,
		Name:      item.Name,
		URL:       item.URL,
		Bootstrap: item.Bootstrap,
		Timeout:   time.Duration(item.TimeoutMS) * time.Millisecond,
	}
}

// dnsStatus 返回 DNS 监听状态。
func (a *App) dnsStatus() api.DNSStatus {
	if a.kernel == nil {
		return api.DNSStatus{UDP: []string{}, TCP: []string{}}
	}

	status := a.kernel.Status()
	dnsStatus := api.DNSStatus{
		Ready: status.Ready,
		UDP:   status.UDP,
		TCP:   status.TCP,
	}
	if status.BindError != nil {
		targets := make([]api.BindFailure, 0, len(status.BindError.Targets))
		for _, item := range status.BindError.Targets {
			targets = append(targets, api.BindFailure{
				Address:  item.Address,
				Protocol: item.Protocol,
				Error:    item.Error,
			})
		}
		dnsStatus.BindError = &api.BindError{
			Code:    status.BindError.Code,
			Message: status.BindError.Message,
			Targets: targets,
		}
	}

	return dnsStatus
}

// errorJSON 把配置错误转为 JSON；nil 时为 null。
func errorJSON(err *config.Error) json.RawMessage {
	if err == nil {
		return nil
	}

	data, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		return nil
	}

	return data
}

// apiErrorFromConfig 把配置错误映射为 API 错误。
func apiErrorFromConfig(err *config.Error) *api.Error {
	return &api.Error{
		Code:            err.Code,
		Message:         err.Message,
		Details:         err.Details,
		CurrentRevision: err.CurrentRevision,
	}
}

// parseQType 把查询类型文本转换为类型值；空值按 A 处理。
func parseQType(text string) uint16 {
	trimmed := strings.ToUpper(strings.TrimSpace(text))
	if trimmed == "" {
		return dns.TypeA
	}
	if qtype, ok := dns.StringToType[trimmed]; ok {
		return qtype
	}

	return dns.TypeA
}

// qtypeName 返回查询类型文本。
func qtypeName(qtype uint16) string {
	if name, ok := dns.TypeToString[qtype]; ok {
		return name
	}

	return fmt.Sprintf("TYPE%d", qtype)
}
