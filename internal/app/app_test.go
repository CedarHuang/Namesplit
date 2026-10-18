package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/config"
	"github.com/CedarHuang/Namesplit/internal/dnskernel"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

type runningApp struct {
	cancel context.CancelFunc
	done   chan int
	stdout *syncBuffer
	stderr *syncBuffer
	port   int
}

func startApp(t *testing.T, dataDir string) *runningApp {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	done := make(chan int, 1)

	go func() {
		done <- Run(ctx, Options{
			Token:   testToken,
			DataDir: dataDir,
			Stdout:  stdout,
			Stderr:  stderr,
			Targets: []dnskernel.Target{{Network: "udp", Addr: "127.0.0.1:0"}},
		})
	}()

	app := &runningApp{cancel: cancel, done: done, stdout: stdout, stderr: stderr}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		text := stdout.String()
		if index := strings.Index(text, "NAMESPLIT_READY "); index >= 0 {
			line := text[index+len("NAMESPLIT_READY "):]
			if end := strings.IndexByte(line, '\n'); end >= 0 {
				var ready struct {
					Port int `json:"port"`
				}
				if err := json.Unmarshal([]byte(line[:end]), &ready); err == nil {
					app.port = ready.Port

					return app
				}
			}
		}

		select {
		case code := <-done:
			t.Fatalf("进程提前退出: %d, stderr=%s", code, stderr.String())
		default:
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("等待 READY 超时, stderr=%s", stderr.String())

	return nil
}

func (a *runningApp) stop(t *testing.T) int {
	t.Helper()

	a.cancel()

	select {
	case code := <-a.done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatalf("等待进程退出超时, stderr=%s", a.stderr.String())

		return -1
	}
}

func (a *runningApp) request(t *testing.T, method, path string, body string) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	request, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", a.port, path), reader)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	request.Header.Set("X-Namesplit-Token", testToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("读取响应: %v", err)
	}

	return response, data
}

func TestRunLifecycle(t *testing.T) {
	dataDir := t.TempDir()

	app := startApp(t, dataDir)

	if _, err := os.Stat(filepath.Join(dataDir, "config.toml")); err != nil {
		t.Fatalf("默认配置未创建: %v", err)
	}

	response, body := app.request(t, http.MethodGet, "/api/health", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health = %d %s", response.StatusCode, body)
	}

	var health struct {
		Ready          bool   `json:"ready"`
		ConfigRevision uint64 `json:"config_revision"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("解析 health: %v", err)
	}
	if !health.Ready || health.ConfigRevision != 1 {
		t.Fatalf("health = %+v", health)
	}

	shutdown, body := app.request(t, http.MethodPost, "/api/shutdown", "")
	if shutdown.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown = %d %s", shutdown.StatusCode, body)
	}

	if code := app.stop(t); code != 0 {
		t.Fatalf("退出码 = %d, stderr=%s", code, app.stderr.String())
	}
}

func TestRunInvalidToken(t *testing.T) {
	code := Run(context.Background(), Options{
		Token:   "bad",
		DataDir: t.TempDir(),
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	})
	if code != 2 {
		t.Fatalf("退出码 = %d, 期望 2", code)
	}
}

func TestRunInvalidConfig(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatalf("写入配置: %v", err)
	}

	stderr := &syncBuffer{}
	code := Run(context.Background(), Options{
		Token:   testToken,
		DataDir: dataDir,
		Stdout:  io.Discard,
		Stderr:  stderr,
	})
	if code != 3 {
		t.Fatalf("退出码 = %d, 期望 3", code)
	}
	if !strings.Contains(stderr.String(), "NAMESPLIT_CONFIG_ERROR ") {
		t.Fatalf("stderr 缺少配置错误行: %s", stderr.String())
	}
}

func TestConfigSaveViaAPI(t *testing.T) {
	dataDir := t.TempDir()
	app := startApp(t, dataDir)
	defer app.stop(t)

	_, body := app.request(t, http.MethodGet, "/api/config", "")
	var state struct {
		Revision uint64          `json:"revision"`
		Config   json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("解析配置: %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(state.Config, &cfg); err != nil {
		t.Fatalf("解析配置体: %v", err)
	}
	settings := cfg["settings"].(map[string]any)
	settings["log_ring_size"] = 8000

	updated, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化配置: %v", err)
	}

	payload, _ := json.Marshal(map[string]any{
		"base_revision": state.Revision,
		"config":        json.RawMessage(updated),
	})
	response, body := app.request(t, http.MethodPut, "/api/config", string(payload))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("保存配置 = %d %s", response.StatusCode, body)
	}

	var saved struct {
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatalf("解析保存结果: %v", err)
	}
	if saved.Revision != state.Revision+1 {
		t.Fatalf("revision = %d, 期望 %d", saved.Revision, state.Revision+1)
	}

	data, err := os.ReadFile(filepath.Join(dataDir, "config.toml"))
	if err != nil {
		t.Fatalf("读取配置: %v", err)
	}
	if !strings.Contains(string(data), "log_ring_size = 8000") {
		t.Fatalf("配置文件未更新:\n%s", data)
	}
}

func TestLocalRecordQueryAndHistory(t *testing.T) {
	dataDir := t.TempDir()
	configText := `schema_version = 1
rules = []
local_records = ["A, nas.home, 60, true, 192.168.1.10"]
[settings]
default_group = "default"
log_ring_size = 5000
log_persist = true
ptr_auto = true
[upstreams.upstream]
name = "upstream"
url = "udp://1.1.1.1:53"
timeout_ms = 5000
[groups.default]
name = "default"
upstreams = ["upstream"]
`
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(configText), 0o644); err != nil {
		t.Fatalf("写入配置: %v", err)
	}

	app := startApp(t, dataDir)
	defer app.stop(t)

	deadline := time.Now().Add(5 * time.Second)
	var dnsAddr string
	for time.Now().Before(deadline) {
		_, body := app.request(t, http.MethodGet, "/api/status", "")
		var status struct {
			DNS struct {
				UDP []string `json:"udp"`
			} `json:"dns"`
		}
		if err := json.Unmarshal(body, &status); err == nil && len(status.DNS.UDP) > 0 {
			dnsAddr = status.DNS.UDP[0]

			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dnsAddr == "" {
		t.Fatal("DNS 监听未就绪")
	}

	response, err := exchangeDNS(dnsAddr, "nas.home.", 60)
	if err != nil {
		t.Fatalf("DNS 查询: %v", err)
	}
	if len(response.Answer) != 1 {
		t.Fatalf("应答 = %+v", response.Answer)
	}

	var page struct {
		Items []struct {
			Domain string `json:"domain"`
			Source string `json:"source"`
			Group  string `json:"group"`
		} `json:"items"`
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, body := app.request(t, http.MethodGet, "/api/queries?limit=10", "")
		if err := json.Unmarshal(body, &page); err == nil && len(page.Items) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(page.Items) != 1 {
		t.Fatalf("历史记录 = %+v", page.Items)
	}
	if page.Items[0].Domain != "nas.home." || page.Items[0].Source != "local" || page.Items[0].Group != "" {
		t.Fatalf("事件字段 = %+v", page.Items[0])
	}
}

func exchangeDNS(addr, name string, timeoutSec int) (*dns.Msg, error) {
	client := &dns.Client{Net: "udp", Timeout: time.Duration(timeoutSec) * time.Second}
	query := new(dns.Msg)
	query.SetQuestion(name, dns.TypeA)
	query.RecursionDesired = true

	response, _, err := client.Exchange(query, addr)

	return response, err
}

// snapshotTestConfig 返回构建运行时视图所需的最小合法配置。
func snapshotTestConfig() *config.Config {
	return &config.Config{
		SchemaVersion: config.DefaultSchemaVersion,
		Rules:         []config.Rule{{Match: "DOMAIN-SUFFIX", Domain: "corp.local", Group: "default"}},
		LocalRecords: []config.Record{
			{Type: "A", Name: "nas.home", TTL: 60, Enabled: true, Value: "192.168.1.10"},
		},
		Settings: config.Settings{
			DefaultGroup: "default",
			LogRingSize:  5000,
			LogPersist:   true,
			PtrAuto:      true,
		},
		Upstreams: map[string]config.Upstream{
			"a": {Name: "A", URL: "udp://1.1.1.1:53", Bootstrap: []string{}, TimeoutMS: 5000},
			"b": {Name: "B", URL: "udp://8.8.8.8:53", Bootstrap: []string{}, TimeoutMS: 5000},
		},
		Groups: map[string]config.Group{
			"default": {Name: "默认", Upstreams: []string{"a", "b"}},
		},
	}
}

func TestBuildSnapshotReusesGroups(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		mutate           func(*config.Config)
		wantReuse        bool
		wantLocalChanged bool
	}{
		{
			name:             "配置不变",
			mutate:           func(*config.Config) {},
			wantReuse:        true,
			wantLocalChanged: false,
		},
		{
			name: "仅改规则",
			mutate: func(cfg *config.Config) {
				cfg.Rules = append(cfg.Rules, config.Rule{Match: "DOMAIN", Domain: "api.corp.local", Group: "default"})
			},
			wantReuse:        true,
			wantLocalChanged: false,
		},
		{
			name: "bootstrap 的 nil 与空切片等价",
			mutate: func(cfg *config.Config) {
				cfg.Upstreams["a"] = config.Upstream{
					Name: "A", URL: "udp://1.1.1.1:53", Bootstrap: nil, TimeoutMS: 5000,
				}
			},
			wantReuse:        true,
			wantLocalChanged: false,
		},
		{
			name: "改上游超时",
			mutate: func(cfg *config.Config) {
				item := cfg.Upstreams["a"]
				item.TimeoutMS = 6000
				cfg.Upstreams["a"] = item
			},
			wantReuse:        false,
			wantLocalChanged: false,
		},
		{
			name: "改组内上游顺序",
			mutate: func(cfg *config.Config) {
				cfg.Groups["default"] = config.Group{Name: "默认", Upstreams: []string{"b", "a"}}
			},
			wantReuse:        false,
			wantLocalChanged: false,
		},
		{
			name: "改本地记录",
			mutate: func(cfg *config.Config) {
				cfg.LocalRecords = []config.Record{
					{Type: "A", Name: "nas.home", TTL: 60, Enabled: true, Value: "192.168.1.11"},
				}
			},
			wantReuse:        true,
			wantLocalChanged: true,
		},
		{
			name: "改 PTR 自动生成开关",
			mutate: func(cfg *config.Config) {
				cfg.Settings.PtrAuto = false
			},
			wantReuse:        true,
			wantLocalChanged: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			app := &App{logger: slog.New(slog.DiscardHandler), store: &runtimeStore{}}
			baseCfg := snapshotTestConfig()

			base, reuse, err := app.buildSnapshot(baseCfg)
			if err != nil {
				t.Fatalf("首次构建快照: %v", err)
			}
			if reuse {
				t.Fatal("首次构建不应复用上游组")
			}
			app.store.cur = base
			t.Cleanup(func() {
				base.ownsGroups.Store(true)
				base.close()
			})

			next := baseCfg.Clone()
			test.mutate(next)

			nextSnapshot, gotReuse, err := app.buildSnapshot(next)
			if err != nil {
				t.Fatalf("再次构建快照: %v", err)
			}
			if gotReuse != test.wantReuse {
				t.Fatalf("复用 = %v, 期望 %v", gotReuse, test.wantReuse)
			}

			if gotReuse {
				if nextSnapshot.groups != base.groups {
					t.Fatal("复用时应共享同一个上游组")
				}
				if nextSnapshot.ownsGroups.Load() {
					t.Fatal("复用快照不应持有上游组所有权")
				}
			} else {
				if nextSnapshot.groups == base.groups {
					t.Fatal("不复用时不应共享上游组")
				}
				nextSnapshot.close()
			}

			if got := localRecordsChanged(baseCfg, next); got != test.wantLocalChanged {
				t.Fatalf("本地记录变化 = %v, 期望 %v", got, test.wantLocalChanged)
			}
		})
	}
}

func TestSemanticComparisons(t *testing.T) {
	t.Parallel()

	upstreams := map[string]config.Upstream{
		"a": {Name: "A", URL: "udp://1.1.1.1:53", Bootstrap: nil, TimeoutMS: 5000},
	}
	emptyBootstrap := map[string]config.Upstream{
		"a": {Name: "A", URL: "udp://1.1.1.1:53", Bootstrap: []string{}, TimeoutMS: 5000},
	}
	if !upstreamsSemanticallyEqual(upstreams, emptyBootstrap) {
		t.Fatal("bootstrap 的 nil 与空切片应等价")
	}

	changedUpstream := map[string]config.Upstream{
		"a": {Name: "A", URL: "udp://1.1.1.1:53", Bootstrap: nil, TimeoutMS: 6000},
	}
	if upstreamsSemanticallyEqual(upstreams, changedUpstream) {
		t.Fatal("超时不同应判定为不相等")
	}

	groups := map[string]config.Group{"default": {Name: "默认", Upstreams: nil}}
	emptyGroups := map[string]config.Group{"default": {Name: "默认", Upstreams: []string{}}}
	if !groupsSemanticallyEqual(groups, emptyGroups) {
		t.Fatal("组成员 nil 与空切片应等价")
	}

	changedGroups := map[string]config.Group{"default": {Name: "默认", Upstreams: []string{"a"}}}
	if groupsSemanticallyEqual(groups, changedGroups) {
		t.Fatal("组成员不同应判定为不相等")
	}

	base := snapshotTestConfig()
	same := base.Clone()
	if localRecordsChanged(base, same) {
		t.Fatal("相同本地记录不应判定为变化")
	}
	changed := base.Clone()
	changed.Settings.PtrAuto = false
	if !localRecordsChanged(base, changed) {
		t.Fatal("PTR 开关变化应判定为本地记录变化")
	}
}

type noopRuntime struct{}

func (noopRuntime) Commit()      {}
func (noopRuntime) Close() error { return nil }

func TestConfigStateWarningsJSON(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	manager, loadErr := config.Open(
		filepath.Join(t.TempDir(), "config.toml"),
		func(*config.Config) (config.Runtime, error) { return noopRuntime{}, nil },
		logger,
	)
	if loadErr != nil {
		t.Fatalf("打开配置: %v", loadErr)
	}

	app := &App{cfg: manager, logger: logger}
	state, err := app.ConfigState()
	if err != nil {
		t.Fatalf("读取配置状态: %v", err)
	}
	if state.Warnings == nil {
		t.Fatal("配置状态警告不应为 nil")
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("序列化配置状态: %v", err)
	}
	if !strings.Contains(string(data), `"warnings":[]`) {
		t.Fatalf("配置状态警告输出错误: %s", data)
	}
	if !strings.Contains(string(state.Config), `"rules":[]`) ||
		!strings.Contains(string(state.Config), `"local_records":[]`) {
		t.Fatalf("配置 JSON 空数组输出错误: %s", state.Config)
	}
}
