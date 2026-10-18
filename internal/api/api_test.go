package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
	"github.com/CedarHuang/Namesplit/internal/event"
	"github.com/CedarHuang/Namesplit/internal/logstore"
	"github.com/CedarHuang/Namesplit/internal/upstream"
)

const testToken = "test-token"

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

type fakeBackend struct {
	health        Health
	status        Status
	configState   ConfigState
	validation    Validation
	saveResult    SaveResult
	saveErr       error
	rulesTest     RuleTest
	events        []event.Event
	eventsUpdates chan event.Event
	queries       logstore.Page
	queryFilter   logstore.Filter
	clearDeleted  int64
	stats         logstore.Stats
	minuteItems   []logstore.MinuteStat
	minutePersist bool
	topItems      []logstore.TopItem
	topPersist    bool
	upstreams     []upstream.TestResult
	shutdowns     atomic.Int32
}

func (f *fakeBackend) Health() Health { return f.health }
func (f *fakeBackend) Status() Status { return f.status }

func (f *fakeBackend) ConfigState() (ConfigState, error) { return f.configState, nil }

func (f *fakeBackend) ConfigValidate(json.RawMessage) (Validation, error) {
	return f.validation, nil
}

func (f *fakeBackend) ConfigSave(_ context.Context, _ uint64, _ json.RawMessage) (SaveResult, error) {
	return f.saveResult, f.saveErr
}

func (f *fakeBackend) ConfigReset(_ context.Context, _ uint64) (SaveResult, error) {
	return f.saveResult, f.saveErr
}

func (f *fakeBackend) RulesTest(_ context.Context, _, _ string) (RuleTest, error) {
	return f.rulesTest, nil
}

func (f *fakeBackend) SubscribeEvents(_ context.Context) ([]event.Event, <-chan event.Event, func(), error) {
	if f.eventsUpdates == nil {
		ch := make(chan event.Event)
		close(ch)

		return f.events, ch, func() {}, nil
	}

	return f.events, f.eventsUpdates, func() {}, nil
}

func (f *fakeBackend) Queries(_ context.Context, filter logstore.Filter) (logstore.Page, error) {
	f.queryFilter = filter

	return f.queries, nil
}

func (f *fakeBackend) ClearQueries(context.Context) (int64, error) { return f.clearDeleted, nil }

func (f *fakeBackend) Stats() logstore.Stats { return f.stats }

func (f *fakeBackend) Minute(_, _ time.Time) ([]logstore.MinuteStat, bool) {
	return f.minuteItems, f.minutePersist
}

func (f *fakeBackend) Top(_ string, _ time.Time, _ int) ([]logstore.TopItem, bool) {
	return f.topItems, f.topPersist
}

func (f *fakeBackend) UpstreamTest(context.Context, UpstreamTestRequest) ([]upstream.TestResult, error) {
	return f.upstreams, nil
}

func (f *fakeBackend) Shutdown() { f.shutdowns.Add(1) }

func newTestServer(t *testing.T, backend *fakeBackend) *Server {
	t.Helper()

	server, err := NewServer(Deps{Token: testToken, Backend: backend, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("构造 API 服务: %v", err)
	}
	server.SetReady(true)

	return server
}

func request(t *testing.T, server http.Handler, method, path, body string, token string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Namesplit-Token", token)
	}

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)

	return recorder
}

func decodeError(t *testing.T, recorder *httptest.ResponseRecorder) errorBody {
	t.Helper()

	var body errorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误体: %v, body=%s", err, recorder.Body.String())
	}

	return body
}

func TestAuthAndReadyGate(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{health: Health{Ready: true}}
	server, err := NewServer(Deps{Token: testToken, Backend: backend, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("构造 API 服务: %v", err)
	}

	if got := request(t, server, http.MethodGet, "/api/status", "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 状态码 = %d", got.Code)
	}
	if got := request(t, server, http.MethodGet, "/api/status", "", "wrong"); got.Code != http.StatusUnauthorized {
		t.Fatalf("错误 token 状态码 = %d", got.Code)
	}

	ready := request(t, server, http.MethodGet, "/api/health", "", testToken)
	if ready.Code != http.StatusOK {
		t.Fatalf("health 状态码 = %d", ready.Code)
	}

	notReady := request(t, server, http.MethodGet, "/api/status", "", testToken)
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("未就绪状态码 = %d", notReady.Code)
	}
	if body := decodeError(t, notReady); body.Error != "not_ready" {
		t.Fatalf("未就绪错误 = %q", body.Error)
	}

	server.BeginShutdown()
	shutdown := request(t, server, http.MethodGet, "/api/status", "", testToken)
	if shutdown.Code != http.StatusServiceUnavailable {
		t.Fatalf("退出中状态码 = %d", shutdown.Code)
	}
	if body := decodeError(t, shutdown); body.Error != "shutdown_in_progress" {
		t.Fatalf("退出中错误 = %q", body.Error)
	}
}

func TestConfigHandlers(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{
		configState: ConfigState{
			Revision: 3,
			Config:   json.RawMessage(`{"schema_version":1}`),
			Warnings: []diagnostic.Diagnostic{},
		},
		validation: Validation{Valid: false, Errors: []diagnostic.Diagnostic{{Code: "invalid_ttl"}}},
		saveResult: SaveResult{Revision: 4, Warnings: []diagnostic.Diagnostic{}},
	}
	server := newTestServer(t, backend)

	got := request(t, server, http.MethodGet, "/api/config", "", testToken)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"revision":3`) {
		t.Fatalf("GET /api/config = %d %s", got.Code, got.Body.String())
	}

	validate := request(t, server, http.MethodPost, "/api/config/validate", `{"config":{"schema_version":1}}`, testToken)
	if validate.Code != http.StatusOK || !strings.Contains(validate.Body.String(), `"valid":false`) {
		t.Fatalf("validate = %d %s", validate.Code, validate.Body.String())
	}

	missingBase := request(t, server, http.MethodPut, "/api/config", `{"config":{}}`, testToken)
	if missingBase.Code != http.StatusBadRequest {
		t.Fatalf("缺少 base_revision 状态码 = %d", missingBase.Code)
	}

	saved := request(t, server, http.MethodPut, "/api/config", `{"base_revision":3,"config":{"schema_version":1}}`, testToken)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"revision":4`) {
		t.Fatalf("保存 = %d %s", saved.Code, saved.Body.String())
	}

	backend.saveErr = &Error{Code: "revision_conflict", CurrentRevision: uint64Ptr(3)}
	conflict := request(t, server, http.MethodPut, "/api/config", `{"base_revision":2,"config":{}}`, testToken)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"current_revision":3`) {
		t.Fatalf("冲突 = %d %s", conflict.Code, conflict.Body.String())
	}
}

func TestContentTypeAndBodyLimit(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, &fakeBackend{})

	req := httptest.NewRequest(http.MethodPost, "/api/config/validate", strings.NewReader(`{}`))
	req.Header.Set("X-Namesplit-Token", testToken)
	req.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("错误 Content-Type 状态码 = %d", recorder.Code)
	}

	large := `{"config":"` + strings.Repeat("a", MaxBodyBytes) + `"}`
	req = httptest.NewRequest(http.MethodPost, "/api/config/validate", strings.NewReader(large))
	req.Header.Set("X-Namesplit-Token", testToken)
	req.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求体状态码 = %d", recorder.Code)
	}
}

func TestQueriesHandler(t *testing.T) {
	t.Parallel()

	next := "9"
	backend := &fakeBackend{queries: logstore.Page{
		Items:              []logstore.HistoryItem{},
		NextCursor:         &next,
		PersistenceEnabled: true,
	}}
	server := newTestServer(t, backend)

	got := request(t, server, http.MethodGet,
		"/api/queries?limit=50&domain=example&qtype=A&source=cache&rcode=0&min_duration_ms=1.5&from=2026-10-18T00:00:00Z",
		"", testToken)
	if got.Code != http.StatusOK {
		t.Fatalf("查询历史状态码 = %d", got.Code)
	}
	if backend.queryFilter.Limit != 50 || backend.queryFilter.Domain != "example" || backend.queryFilter.Source != "cache" {
		t.Fatalf("过滤条件 = %+v", backend.queryFilter)
	}
	if backend.queryFilter.Rcode == nil || *backend.queryFilter.Rcode != 0 {
		t.Fatalf("rcode 过滤 = %+v", backend.queryFilter.Rcode)
	}

	invalid := request(t, server, http.MethodGet, "/api/queries?limit=0", "", testToken)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("非法 limit 状态码 = %d", invalid.Code)
	}

	badTime := request(t, server, http.MethodGet, "/api/queries?from=yesterday", "", testToken)
	if badTime.Code != http.StatusBadRequest {
		t.Fatalf("非法时间状态码 = %d", badTime.Code)
	}
}

func TestStatsHandlers(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{
		stats: logstore.Stats{
			UptimeMS:           10,
			Counters:           logstore.Counters{Queries: 2, CacheHits: 1},
			QPS1s:              5,
			PersistenceEnabled: true,
		},
		minuteItems:   []logstore.MinuteStat{{Minute: "2026-10-18T08:12:00Z", Queries: 2}},
		minutePersist: true,
		topItems:      []logstore.TopItem{{Key: "a.test.", Count: 2}},
		topPersist:    true,
	}
	server := newTestServer(t, backend)

	stats := request(t, server, http.MethodGet, "/api/stats", "", testToken)
	if stats.Code != http.StatusOK || !strings.Contains(stats.Body.String(), `"qps_1s":5`) {
		t.Fatalf("stats = %d %s", stats.Code, stats.Body.String())
	}

	minute := request(t, server, http.MethodGet, "/api/stats/minute?from=2026-10-18T00:00:00Z", "", testToken)
	if minute.Code != http.StatusOK || !strings.Contains(minute.Body.String(), `"persistence_enabled":true`) {
		t.Fatalf("minute = %d %s", minute.Code, minute.Body.String())
	}

	top := request(t, server, http.MethodGet, "/api/stats/top?by=domain&limit=5", "", testToken)
	if top.Code != http.StatusOK || !strings.Contains(top.Body.String(), `"key":"a.test."`) {
		t.Fatalf("top = %d %s", top.Code, top.Body.String())
	}

	if got := request(t, server, http.MethodGet, "/api/stats/top?by=bad", "", testToken); got.Code != http.StatusBadRequest {
		t.Fatalf("非法 by 状态码 = %d", got.Code)
	}
	if got := request(t, server, http.MethodGet, "/api/stats/minute?from=bad", "", testToken); got.Code != http.StatusBadRequest {
		t.Fatalf("非法分钟时间状态码 = %d", got.Code)
	}
}

func TestRulesAndUpstreamsHandlers(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{
		rulesTest: RuleTest{Canonical: "api.corp.local", TargetGroup: "corp", DefaultGroup: "default"},
		upstreams: []upstream.TestResult{{Name: "mock", Address: "udp://1.1.1.1:53", OK: true}},
	}
	server := newTestServer(t, backend)

	rules := request(t, server, http.MethodPost, "/api/rules/test", `{"domain":"api.corp.local"}`, testToken)
	if rules.Code != http.StatusOK || !strings.Contains(rules.Body.String(), `"target_group":"corp"`) {
		t.Fatalf("rules = %d %s", rules.Code, rules.Body.String())
	}
	if got := request(t, server, http.MethodPost, "/api/rules/test", `{}`, testToken); got.Code != http.StatusBadRequest {
		t.Fatalf("缺少 domain 状态码 = %d", got.Code)
	}

	ok := request(t, server, http.MethodPost, "/api/upstreams/test", `{"upstream_id":"mock"}`, testToken)
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), `"results"`) {
		t.Fatalf("upstream test = %d %s", ok.Code, ok.Body.String())
	}

	for _, body := range []string{
		`{}`,
		`{"upstream_id":"a","group_id":"b"}`,
		`{"upstream":{}}`,
	} {
		if got := request(t, server, http.MethodPost, "/api/upstreams/test", body, testToken); got.Code != http.StatusBadRequest {
			t.Fatalf("非法 upstream 请求 %s 状态码 = %d", body, got.Code)
		}
	}
}

func TestEventsStream(t *testing.T) {
	t.Parallel()

	updates := make(chan event.Event, 4)
	backend := &fakeBackend{
		events: []event.Event{
			{EventID: 1, Domain: "a.test.", Answers: []string{}, Authority: []string{}, Additional: []string{}},
			{EventID: 2, Domain: "b.test.", Answers: []string{}, Authority: []string{}, Additional: []string{}},
		},
		eventsUpdates: updates,
	}
	server := newTestServer(t, backend)

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	req, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("X-Namesplit-Token", testToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("建立事件流: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	updates <- event.Event{EventID: 3, Domain: "c.test.", Answers: []string{}, Authority: []string{}, Additional: []string{}}
	close(updates)

	scanner := bufio.NewScanner(resp.Body)
	lines := make([]string, 0, 3)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}

	if len(lines) < 2 {
		t.Fatalf("事件行数 = %d, lines=%v", len(lines), lines)
	}

	var first []event.Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("解析快照: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("快照事件数 = %d", len(first))
	}

	var second []event.Event
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("解析实时事件: %v", err)
	}
	if len(second) != 1 || second[0].EventID != 3 {
		t.Fatalf("实时事件 = %+v", second)
	}
}

func TestShutdownHandler(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{}
	server := newTestServer(t, backend)

	got := request(t, server, http.MethodPost, "/api/shutdown", "", testToken)
	if got.Code != http.StatusAccepted {
		t.Fatalf("shutdown 状态码 = %d", got.Code)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && backend.shutdowns.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if backend.shutdowns.Load() != 1 {
		t.Fatalf("backend.Shutdown 调用次数 = %d", backend.shutdowns.Load())
	}
}

func TestInternalErrorMapping(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, &fakeBackend{})
	_ = server

	if status := statusForCode("invalid_config"); status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid_config 状态码 = %d", status)
	}
	if status := statusForCode("unknown_code"); status != http.StatusInternalServerError {
		t.Fatalf("未知错误码状态码 = %d", status)
	}

	recorder := httptest.NewRecorder()
	writeError(recorder, errors.New("boom"))
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), `"internal"`) {
		t.Fatalf("内部错误 = %d %s", recorder.Code, recorder.Body.String())
	}
}

func uint64Ptr(value uint64) *uint64 { return &value }

func TestRuleTestLocalJSONShape(t *testing.T) {
	t.Parallel()

	miss, err := json.Marshal(RuleTestLocal{})
	if err != nil {
		t.Fatalf("序列化未命中结果: %v", err)
	}
	if string(miss) != `{"hit":false}` {
		t.Fatalf("未命中输出 = %s, 期望 {\"hit\":false}", miss)
	}

	hit, err := json.Marshal(RuleTestLocal{Hit: true, Name: "nas.home", QType: "A"})
	if err != nil {
		t.Fatalf("序列化命中结果: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(hit, &fields); err != nil {
		t.Fatalf("解析命中结果: %v", err)
	}
	for _, key := range []string{"hit", "name", "qtype", "nodata", "answers", "ttl"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("命中输出缺少 %s: %s", key, hit)
		}
	}
	if string(fields["answers"]) != `[]` {
		t.Fatalf("空 answers 输出 = %s, 期望 []", fields["answers"])
	}
}

func TestRulesTestHandlerLocalShape(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{rulesTest: RuleTest{
		Canonical: "nas.home",
		Local:     RuleTestLocal{Hit: true, Name: "nas.home", QType: "A"},
	}}
	server := newTestServer(t, backend)

	response := request(t, server, http.MethodPost, "/api/rules/test", `{"domain":"nas.home"}`, testToken)
	if response.Code != http.StatusOK {
		t.Fatalf("规则测试状态码 = %d, body=%s", response.Code, response.Body.String())
	}

	var body struct {
		Local map[string]json.RawMessage `json:"local"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析规则测试响应: %v", err)
	}
	for _, key := range []string{"hit", "name", "qtype", "nodata", "answers", "ttl"} {
		if _, ok := body.Local[key]; !ok {
			t.Fatalf("规则测试 local 缺少 %s: %s", key, response.Body.String())
		}
	}
}

func TestConfigWarningsJSONShape(t *testing.T) {
	t.Parallel()

	state, err := json.Marshal(ConfigState{
		Revision: 1,
		Config:   json.RawMessage(`{}`),
		Warnings: []diagnostic.Diagnostic{},
	})
	if err != nil {
		t.Fatalf("序列化配置状态: %v", err)
	}
	if !strings.Contains(string(state), `"warnings":[]`) {
		t.Fatalf("配置状态警告输出错误: %s", state)
	}

	saved, err := json.Marshal(SaveResult{Revision: 2, Warnings: []diagnostic.Diagnostic{}})
	if err != nil {
		t.Fatalf("序列化保存结果: %v", err)
	}
	if !strings.Contains(string(saved), `"warnings":[]`) {
		t.Fatalf("保存结果警告输出错误: %s", saved)
	}
}
