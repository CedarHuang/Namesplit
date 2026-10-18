// Package config 解析、校验并保存 DNS 配置。
//
// 配置文件是 TOML，规则与本地记录以行字符串保存；API 使用结构化 JSON。
// 两种表示共享同一份已解析配置，保存时按固定顺序规范化为 TOML。
package config

// Config 是已解析的 DNS 配置。规则与本地记录的顺序即语义顺序。
type Config struct {
	SchemaVersion int                 `json:"schema_version"`
	Rules         []Rule              `json:"rules"`
	LocalRecords  []Record            `json:"local_records"`
	Settings      Settings            `json:"settings"`
	Upstreams     map[string]Upstream `json:"upstreams"`
	Groups        map[string]Group    `json:"groups"`
}

// Rule 是一条分流规则；Domain 保存用户原文。
type Rule struct {
	Match  string `json:"match"`
	Domain string `json:"domain"`
	Group  string `json:"group"`
}

// Record 是一条本地记录。
type Record struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	TTL     uint32 `json:"ttl"`
	Enabled bool   `json:"enabled"`
	Value   string `json:"value"`
}

// Settings 是全局设置。
type Settings struct {
	DefaultGroup string `json:"default_group"`
	LogRingSize  int    `json:"log_ring_size"`
	LogPersist   bool   `json:"log_persist"`
	PtrAuto      bool   `json:"ptr_auto"`
}

// Upstream 是一个上游服务器。
type Upstream struct {
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Bootstrap []string `json:"bootstrap,omitempty"`
	TimeoutMS int      `json:"timeout_ms"`
	Remark    string   `json:"remark,omitempty"`
}

// Group 是一个上游组。
type Group struct {
	Name      string   `json:"name"`
	Upstreams []string `json:"upstreams"`
}

// 默认值。
const (
	DefaultSchemaVersion = 1
	DefaultLogRingSize   = 5000
	DefaultTimeoutMS     = 5000
	DefaultRecordTTL     = 60
	MaxFileBytes         = 8 << 20
)

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		SchemaVersion: DefaultSchemaVersion,
		Rules:         []Rule{},
		LocalRecords:  []Record{},
		Settings: Settings{
			DefaultGroup: "default",
			LogRingSize:  DefaultLogRingSize,
			LogPersist:   true,
			PtrAuto:      true,
		},
		Upstreams: map[string]Upstream{
			"cloudflare": {
				Name:      "Cloudflare",
				URL:       "udp://1.1.1.1:53",
				Bootstrap: []string{},
				TimeoutMS: DefaultTimeoutMS,
			},
			"google": {
				Name:      "Google",
				URL:       "udp://8.8.8.8:53",
				Bootstrap: []string{},
				TimeoutMS: DefaultTimeoutMS,
			},
		},
		Groups: map[string]Group{
			"default": {
				Name:      "默认",
				Upstreams: []string{"cloudflare", "google"},
			},
		},
	}
}

// Clone 深拷贝配置，避免调用方修改管理器内部状态。
//
// 所有切片都归一为非 nil 的空切片：JSON 输出要求空集合表现为 [] 而不是
// null，上游 bootstrap 也会参与运行时语义比较，nil 与空切片必须等价。
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}

	out := &Config{
		SchemaVersion: c.SchemaVersion,
		Rules:         cloneSlice(c.Rules),
		LocalRecords:  cloneSlice(c.LocalRecords),
		Settings:      c.Settings,
		Upstreams:     make(map[string]Upstream, len(c.Upstreams)),
		Groups:        make(map[string]Group, len(c.Groups)),
	}

	for id, upstream := range c.Upstreams {
		upstream.Bootstrap = cloneSlice(upstream.Bootstrap)
		out.Upstreams[id] = upstream
	}
	for id, group := range c.Groups {
		group.Upstreams = cloneSlice(group.Upstreams)
		out.Groups[id] = group
	}

	return out
}

// cloneSlice 复制切片，并把 nil 归一为空切片。
func cloneSlice[T any](in []T) []T {
	out := make([]T, len(in))
	copy(out, in)

	return out
}
