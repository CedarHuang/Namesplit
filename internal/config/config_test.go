package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
)

const sampleTOML = `schema_version = 1

rules = [
  "DOMAIN-SUFFIX, corp.local, corp",
  "DOMAIN, api.corp.local, corp",
]

local_records = [
  "A, nas.home, 60, true, 192.168.1.10",
  "A, nas.home, 60, true, 192.168.1.11",
  "CNAME, cname.dev.local, 60, true, nas.home",
  "TXT, hello.home, 300, true, hello, world",
]

[settings]
default_group = "default"
log_ring_size = 5000
log_persist = true
ptr_auto = true

[upstreams.cloudflare]
name = "Cloudflare"
url = "udp://1.1.1.1:53"
timeout_ms = 5000

[upstreams.corp-dns]
name = "公司内网"
url = "tls://10.0.0.53"
timeout_ms = 5000
remark = "公司 DNS"

[upstreams.google]
name = "Google"
url = "udp://8.8.8.8:53"
timeout_ms = 5000

[groups.corp]
name = "公司"
upstreams = ["corp-dns"]

[groups.default]
name = "默认"
upstreams = ["cloudflare", "google"]
`

func hasCode(diags []diagnostic.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}

	return false
}

func hasError(diags []diagnostic.Diagnostic) bool {
	return diagnostic.HasError(diags)
}

func TestDefaultRoundTrip(t *testing.T) {
	t.Parallel()

	data, err := MarshalTOML(Default())
	if err != nil {
		t.Fatalf("序列化默认配置: %v", err)
	}

	parsed, diags := ParseFile(data)
	if hasError(diags) {
		t.Fatalf("默认配置回读出错: %+v", diags)
	}

	if !reflect.DeepEqual(parsed, Default()) {
		t.Fatalf("默认配置回读不一致:\n got: %+v\nwant: %+v", parsed, Default())
	}

	if !strings.HasPrefix(string(data), "schema_version = 1\n") {
		t.Fatalf("规范化输出开头错误:\n%s", data)
	}
}

func TestDefaultJSONShape(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(Default())
	if err != nil {
		t.Fatalf("序列化默认配置: %v", err)
	}
	text := string(data)
	for _, want := range []string{`"rules":[]`, `"local_records":[]`} {
		if !strings.Contains(text, want) {
			t.Fatalf("默认配置 JSON 缺少 %s:\n%s", want, text)
		}
	}
}

func TestCloneNormalizesEmptySlices(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		SchemaVersion: DefaultSchemaVersion,
		Rules:         nil,
		LocalRecords:  nil,
		Upstreams: map[string]Upstream{
			"a": {Name: "a", URL: "udp://1.1.1.1", TimeoutMS: DefaultTimeoutMS, Bootstrap: nil},
		},
		Groups: map[string]Group{"default": {Name: "默认", Upstreams: nil}},
	}

	clone := cfg.Clone()
	if clone.Rules == nil {
		t.Fatal("Rules 不应为 nil")
	}
	if clone.LocalRecords == nil {
		t.Fatal("LocalRecords 不应为 nil")
	}
	if clone.Upstreams["a"].Bootstrap == nil {
		t.Fatal("Bootstrap 不应为 nil")
	}
	if clone.Groups["default"].Upstreams == nil {
		t.Fatal("组上游不应为 nil")
	}

	data, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("序列化克隆配置: %v", err)
	}
	if !strings.Contains(string(data), `"rules":[]`) {
		t.Fatalf("克隆配置 JSON 缺少空数组:\n%s", data)
	}
}

func TestParseSample(t *testing.T) {
	t.Parallel()

	cfg, diags := ParseFile([]byte(sampleTOML))
	if hasError(diags) {
		t.Fatalf("样例配置解析出错: %+v", diags)
	}

	if len(cfg.Rules) != 2 || cfg.Rules[0].Match != "DOMAIN-SUFFIX" || cfg.Rules[1].Domain != "api.corp.local" {
		t.Fatalf("规则 = %+v", cfg.Rules)
	}
	if len(cfg.LocalRecords) != 4 {
		t.Fatalf("记录数量 = %d", len(cfg.LocalRecords))
	}
	if got := cfg.LocalRecords[3].Value; got != "hello, world" {
		t.Fatalf("TXT 值 = %q", got)
	}
	if cfg.Upstreams["corp-dns"].Name != "公司内网" {
		t.Fatalf("上游名 = %q", cfg.Upstreams["corp-dns"].Name)
	}
	if cfg.Upstreams["cloudflare"].Bootstrap == nil {
		t.Fatal("bootstrap 应为空切片而不是 nil")
	}
	if got := cfg.Groups["default"].Upstreams; len(got) != 2 || got[0] != "cloudflare" {
		t.Fatalf("组上游 = %+v", got)
	}
}

func TestParseJSONDefaults(t *testing.T) {
	t.Parallel()

	input := `{
		"schema_version": 1,
		"settings": {"default_group": "default"},
		"upstreams": {"west": {"url": "udp://1.1.1.1"}},
		"groups": {"default": {"upstreams": ["west"]}},
		"local_records": [{"type": "A", "name": "nas.home", "value": "192.168.1.10"}]
	}`

	cfg, diags := ParseJSON([]byte(input))
	if hasError(diags) {
		t.Fatalf("JSON 配置解析出错: %+v", diags)
	}

	if cfg.Settings.LogRingSize != DefaultLogRingSize || !cfg.Settings.LogPersist || !cfg.Settings.PtrAuto {
		t.Fatalf("设置默认值 = %+v", cfg.Settings)
	}
	upstream := cfg.Upstreams["west"]
	if upstream.Name != "west" || upstream.TimeoutMS != DefaultTimeoutMS {
		t.Fatalf("上游默认值 = %+v", upstream)
	}
	if cfg.Groups["default"].Name != "default" {
		t.Fatalf("组默认名 = %q", cfg.Groups["default"].Name)
	}
	record := cfg.LocalRecords[0]
	if record.TTL != DefaultRecordTTL || !record.Enabled || record.Type != "A" {
		t.Fatalf("记录默认值 = %+v", record)
	}
}

func TestParseJSONUnknownField(t *testing.T) {
	t.Parallel()

	_, diags := ParseJSON([]byte(`{"schema_version":1,"settings":{"default_group":"default"},"upstreams":{},"groups":{},"unknown":1}`))
	if !hasCode(diags, diagnostic.CodeUnknownField) {
		t.Fatalf("期望 unknown_field，实际 %+v", diags)
	}
}

func TestNormalizedOutputOrder(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Upstreams["zzz"] = Upstream{Name: "Z", URL: "udp://9.9.9.9", Bootstrap: []string{}, TimeoutMS: 5000}
	cfg.Groups["zzz"] = Group{Name: "Z组", Upstreams: []string{"zzz"}}
	cfg.Rules = []Rule{{Match: "domain-suffix", Domain: "z.test", Group: "zzz"}}
	cfg.LocalRecords = []Record{{Type: "a", Name: "nas.home", TTL: 60, Enabled: true, Value: "192.168.1.10"}}

	data, err := MarshalTOML(cfg)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	text := string(data)

	checks := []string{
		"schema_version = 1",
		"rules = [",
		`DOMAIN-SUFFIX, z.test, zzz`,
		"local_records = [",
		`A, nas.home, 60, true, 192.168.1.10`,
		"[settings]",
		"[upstreams.cloudflare]",
		"[upstreams.google]",
		"[upstreams.zzz]",
		"[groups.default]",
		"[groups.zzz]",
	}
	last := -1
	for _, check := range checks {
		index := strings.Index(text, check)
		if index < 0 {
			t.Fatalf("输出缺少 %q:\n%s", check, text)
		}
		if index < last {
			t.Fatalf("输出顺序错误：%q 出现在前一项之前:\n%s", check, text)
		}
		last = index
	}
}

func TestQuotingRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Upstreams["cloudflare"] = Upstream{
		Name:      `引号 " 与反斜杠 \ 和制表符	`,
		URL:       "udp://1.1.1.1:53",
		TimeoutMS: 5000,
		Bootstrap: []string{},
		Remark:    "备注 \"quoted\"",
	}
	cfg.LocalRecords = []Record{{Type: "TXT", Name: "txt.home", TTL: 60, Enabled: true, Value: "a,b \"c\" \\d \u007f\u0001"}}

	data, err := MarshalTOML(cfg)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	parsed, diags := ParseFile(data)
	if hasError(diags) {
		t.Fatalf("回读出错: %+v\n%s", diags, data)
	}
	if parsed.Upstreams["cloudflare"].Name != cfg.Upstreams["cloudflare"].Name {
		t.Fatalf("回读名称 = %q", parsed.Upstreams["cloudflare"].Name)
	}
	if parsed.LocalRecords[0].Value != cfg.LocalRecords[0].Value {
		t.Fatalf("回读 TXT = %q", parsed.LocalRecords[0].Value)
	}
}

func TestValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		toml string
		code string
	}{
		{
			name: "未知字段",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
unknown = 1
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeUnknownField,
		},
		{
			name: "未知表",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
[foo]
bar = 1
`,
			code: diagnostic.CodeUnknownField,
		},
		{
			name: "未知子表",
			toml: `schema_version = 1
[settings]
default_group = "default"
[settings.extra]
bar = 1
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeUnknownField,
		},
		{
			name: "版本不支持",
			toml: `schema_version = 2
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeUnsupportedSchemaVersion,
		},
		{
			name: "缺少 settings",
			toml: `schema_version = 1
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeMissingField,
		},
		{
			name: "环形缓冲越界",
			toml: `schema_version = 1
[settings]
default_group = "default"
log_ring_size = 100
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidRingSize,
		},
		{
			name: "非法上游 URL",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams.a]
url = "http://1.1.1.1/dns-query"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidUpstreamURL,
		},
		{
			name: "域名缺少 bootstrap",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams.a]
url = "udp://dns.example:53"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidBootstrap,
		},
		{
			name: "bootstrap 非 IP",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams.a]
url = "udp://dns.example:53"
bootstrap = ["dns.example"]
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidBootstrap,
		},
		{
			name: "超时越界",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
timeout_ms = 50
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidTimeout,
		},
		{
			name: "非法上游 ID",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams."Bad_ID"]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["Bad_ID"]
`,
			code: diagnostic.CodeInvalidUpstreamID,
		},
		{
			name: "组引用不存在的上游",
			toml: `schema_version = 1
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a", "missing"]
`,
			code: diagnostic.CodeMissingUpstream,
		},
		{
			name: "默认组不存在",
			toml: `schema_version = 1
[settings]
default_group = "missing"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeMissingGroup,
		},
		{
			name: "规则引用不存在的组",
			toml: `schema_version = 1
rules = ["DOMAIN, a.test, missing"]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeMissingGroup,
		},
		{
			name: "规则行段数错误",
			toml: `schema_version = 1
rules = ["DOMAIN, a.test"]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidRuleLine,
		},
		{
			name: "通配规则",
			toml: `schema_version = 1
rules = ["DOMAIN, *.a.test, default"]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeWildcardNotSupported,
		},
		{
			name: "重复规则",
			toml: `schema_version = 1
rules = ["DOMAIN-SUFFIX, a.test, default", "DOMAIN-SUFFIX, A.Test, default"]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeDuplicateRule,
		},
		{
			name: "记录行段数错误",
			toml: `schema_version = 1
local_records = ["A, nas.home, 60, true"]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidRecordLine,
		},
		{
			name: "记录 TTL 越界",
			toml: `schema_version = 1
local_records = ["A, nas.home, 90000, true, 192.168.1.10"]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeInvalidTTL,
		},
		{
			name: "CNAME 冲突",
			toml: `schema_version = 1
local_records = [
  "A, x.home, 60, true, 192.168.1.10",
  "CNAME, x.home, 60, true, nas.home",
]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`,
			code: diagnostic.CodeCNAMEConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, diags := ParseFile([]byte(tt.toml))
			if !hasCode(diags, tt.code) {
				t.Fatalf("期望 %s，实际 %+v", tt.code, diags)
			}
		})
	}
}

func TestWarningsDoNotBlock(t *testing.T) {
	t.Parallel()

	input := `schema_version = 1
rules = ["DOMAIN-SUFFIX, example.com, default", "DOMAIN, api.example.com, default"]
local_records = [
  "A, nas.home, 60, true, 192.168.1.10",
  "A, nas.home, 60, true, 192.168.1.10",
]
[settings]
default_group = "default"
[upstreams.a]
url = "udp://1.1.1.1"
[groups.default]
upstreams = ["a"]
`

	_, diags := ParseFile([]byte(input))
	if hasError(diags) {
		t.Fatalf("警告不应阻止: %+v", diags)
	}
	if !hasCode(diags, diagnostic.CodeShadowedRule) || !hasCode(diags, diagnostic.CodeDuplicateRecord) {
		t.Fatalf("缺少警告: %+v", diags)
	}
}

type fakeRuntime struct {
	commits int
	closes  int
}

func (r *fakeRuntime) Commit()      { r.commits++ }
func (r *fakeRuntime) Close() error { r.closes++; return nil }

func TestManagerLifecycle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	var runtimes []*fakeRuntime
	build := func(*Config) (Runtime, error) {
		runtime := &fakeRuntime{}
		runtimes = append(runtimes, runtime)

		return runtime, nil
	}

	manager, loadErr := Open(path, build, nil)
	if loadErr != nil {
		t.Fatalf("打开配置: %v", loadErr)
	}
	if len(runtimes) != 1 || runtimes[0].commits != 1 {
		t.Fatalf("首次构建 = %+v", runtimes)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("默认配置未写盘: %v", err)
	}

	cfg, revision, warnings, currentErr := manager.Current()
	if revision != 1 || currentErr != nil || warnings == nil || len(warnings) != 0 {
		t.Fatalf("初始状态 revision=%d warnings=%v err=%v", revision, warnings, currentErr)
	}

	cfg.Settings.LogRingSize = 8000
	newRevision, saveWarnings, saveErr := manager.Save(context.Background(), 1, cfg, build)
	if saveErr != nil {
		t.Fatalf("保存配置: %v", saveErr)
	}
	if newRevision != 2 || saveWarnings == nil || len(saveWarnings) != 0 {
		t.Fatalf("保存结果 revision=%d warnings=%v", newRevision, saveWarnings)
	}
	if len(runtimes) != 2 || runtimes[1].commits != 1 {
		t.Fatalf("保存后运行时 = %+v", runtimes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取写盘配置: %v", err)
	}
	if !strings.Contains(string(data), "log_ring_size = 8000") {
		t.Fatalf("写盘内容未更新:\n%s", data)
	}

	_, _, conflict := manager.Save(context.Background(), 1, cfg, build)
	if conflict == nil || conflict.Code != "revision_conflict" || conflict.CurrentRevision == nil || *conflict.CurrentRevision != 2 {
		t.Fatalf("版本冲突 = %+v", conflict)
	}

	invalid := cfg.Clone()
	invalid.Settings.LogRingSize = 1
	before, _ := os.ReadFile(path)
	_, _, invalidErr := manager.Save(context.Background(), 2, invalid, build)
	if invalidErr == nil || invalidErr.Code != "invalid_config" {
		t.Fatalf("非法配置保存 = %+v", invalidErr)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("非法配置保存不应修改文件")
	}

	_, _, resetErr := manager.Reset(context.Background(), 2, build)
	if resetErr != nil {
		t.Fatalf("恢复默认: %v", resetErr)
	}
	reset, revision, _, _ := manager.Current()
	if revision != 3 || reset.Settings.LogRingSize != DefaultLogRingSize {
		t.Fatalf("恢复默认后 revision=%d cfg=%+v", revision, reset)
	}
}

func TestManagerBuildFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	buildSuccess := func(*Config) (Runtime, error) { return &fakeRuntime{}, nil }
	manager, loadErr := Open(path, buildSuccess, nil)
	if loadErr != nil {
		t.Fatalf("打开配置: %v", loadErr)
	}

	buildErr := errors.New("构建失败")
	buildFail := func(*Config) (Runtime, error) { return nil, buildErr }

	cfg, _, _, _ := manager.Current()
	_, _, saveErr := manager.Save(context.Background(), 1, cfg, buildFail)
	if saveErr == nil || saveErr.Code != "apply_failed" {
		t.Fatalf("构建失败错误 = %+v", saveErr)
	}
}

func TestOpenInvalidFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("schema_version = 2\n"), 0o644); err != nil {
		t.Fatalf("写入配置: %v", err)
	}

	_, loadErr := Open(path, func(*Config) (Runtime, error) { return &fakeRuntime{}, nil }, nil)
	if loadErr == nil {
		t.Fatal("期望加载失败")
	}
	if !hasCode(loadErr.Details, diagnostic.CodeUnsupportedSchemaVersion) {
		t.Fatalf("加载错误 = %+v", loadErr)
	}
}

func TestOpenTooLargeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	data := append([]byte("#"), []byte(strings.Repeat("a", MaxFileBytes))...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写入配置: %v", err)
	}

	_, loadErr := Open(path, func(*Config) (Runtime, error) { return &fakeRuntime{}, nil }, nil)
	if loadErr == nil || loadErr.Code != diagnostic.CodeConfigTooLarge {
		t.Fatalf("加载错误 = %+v", loadErr)
	}
}

// findDiagnostic 按 code 与 path 精确查找诊断。
func findDiagnostic(diags []diagnostic.Diagnostic, code, path string) (diagnostic.Diagnostic, bool) {
	for _, diag := range diags {
		if diag.Code == code && diag.Path == path {
			return diag, true
		}
	}

	return diagnostic.Diagnostic{}, false
}

func TestUnknownTablePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		toml string
		path string
	}{
		{
			name: "顶层未知表",
			toml: `schema_version = 1
[settings]
default_group = "default"
[foo]
bar = 1
`,
			path: "foo",
		},
		{
			name: "设置未知子表",
			toml: `schema_version = 1
[settings]
default_group = "default"
[settings.extra]
bar = 1
`,
			path: "settings.extra",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, diags := ParseFile([]byte(test.toml))
			if _, ok := findDiagnostic(diags, diagnostic.CodeUnknownField, test.path); !ok {
				t.Fatalf("缺少 %s 的 unknown_field 诊断: %+v", test.path, diags)
			}
		})
	}
}
