package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// tomlFile 是规范化输出的结构。字段顺序即文件中的输出顺序；map 键由编码器
// 按字典序输出。
type tomlFile struct {
	SchemaVersion int                     `toml:"schema_version"`
	Rules         []string                `toml:"rules"`
	LocalRecords  []string                `toml:"local_records"`
	Settings      tomlSettings            `toml:"settings"`
	Upstreams     map[string]tomlUpstream `toml:"upstreams"`
	Groups        map[string]tomlGroup    `toml:"groups"`
}

type tomlSettings struct {
	DefaultGroup string `toml:"default_group"`
	LogRingSize  int    `toml:"log_ring_size"`
	LogPersist   bool   `toml:"log_persist"`
	PtrAuto      bool   `toml:"ptr_auto"`
}

type tomlUpstream struct {
	Name      string   `toml:"name"`
	URL       string   `toml:"url"`
	TimeoutMS int      `toml:"timeout_ms"`
	Bootstrap []string `toml:"bootstrap,omitempty"`
	Remark    string   `toml:"remark,omitempty"`
}

type tomlGroup struct {
	Name      string   `toml:"name"`
	Upstreams []string `toml:"upstreams"`
}

// MarshalTOML 输出规范化 TOML。
func MarshalTOML(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("配置为空")
	}

	file := tomlFile{
		SchemaVersion: cfg.SchemaVersion,
		Rules:         ruleLines(cfg.Rules),
		LocalRecords:  recordLines(cfg.LocalRecords),
		Settings: tomlSettings{
			DefaultGroup: cfg.Settings.DefaultGroup,
			LogRingSize:  cfg.Settings.LogRingSize,
			LogPersist:   cfg.Settings.LogPersist,
			PtrAuto:      cfg.Settings.PtrAuto,
		},
		Upstreams: make(map[string]tomlUpstream, len(cfg.Upstreams)),
		Groups:    make(map[string]tomlGroup, len(cfg.Groups)),
	}

	if file.Rules == nil {
		file.Rules = []string{}
	}
	if file.LocalRecords == nil {
		file.LocalRecords = []string{}
	}

	for id, upstream := range cfg.Upstreams {
		file.Upstreams[id] = tomlUpstream{
			Name:      upstream.Name,
			URL:       upstream.URL,
			TimeoutMS: upstream.TimeoutMS,
			Bootstrap: upstream.Bootstrap,
			Remark:    upstream.Remark,
		}
	}
	for id, group := range cfg.Groups {
		file.Groups[id] = tomlGroup{Name: group.Name, Upstreams: group.Upstreams}
	}

	data, err := toml.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("序列化配置: %w", err)
	}

	return data, nil
}

// WriteAtomic 原子写入文件：先写同目录临时文件，再重命名覆盖。
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件: %w", err)
	}
	tempPath := temp.Name()

	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}

	if _, err := temp.Write(data); err != nil {
		cleanup()

		return fmt.Errorf("写入临时配置: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()

		return fmt.Errorf("同步临时配置: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("关闭临时配置: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)

		return fmt.Errorf("替换配置文件: %w", err)
	}

	return nil
}

// ruleLines 把规则转换为文件行。
func ruleLines(rulesList []Rule) []string {
	lines := make([]string, 0, len(rulesList))
	for _, rule := range rulesList {
		lines = append(lines, fmt.Sprintf("%s, %s, %s",
			strings.ToUpper(strings.TrimSpace(rule.Match)), rule.Domain, rule.Group))
	}

	return lines
}

// recordLines 把本地记录转换为文件行。
func recordLines(records []Record) []string {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		lines = append(lines, fmt.Sprintf("%s, %s, %d, %t, %s",
			strings.ToUpper(strings.TrimSpace(record.Type)),
			record.Name, record.TTL, record.Enabled, record.Value))
	}

	return lines
}
