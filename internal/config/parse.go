package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/CedarHuang/Namesplit/internal/diagnostic"
)

// ParseFile 解析 TOML 配置并返回校验明细。
func ParseFile(data []byte) (*Config, []diagnostic.Diagnostic) {
	if len(data) > MaxFileBytes {
		return nil, []diagnostic.Diagnostic{{
			Code:     diagnostic.CodeConfigTooLarge,
			Severity: diagnostic.SeverityError,
			Message:  fmt.Sprintf("配置文件超过 %d 字节", MaxFileBytes),
		}}
	}

	var file fileConfig

	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, []diagnostic.Diagnostic{tomlDiagnostic(err)}
	}

	cfg, diags := file.toConfig()
	diags = append(diags, Validate(cfg)...)

	return cfg, diags
}

// ParseJSON 解析 API 表示的结构化配置并返回校验明细。
func ParseJSON(data []byte) (*Config, []diagnostic.Diagnostic) {
	var raw jsonConfig

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, []diagnostic.Diagnostic{jsonDiagnostic(err)}
	}

	cfg, diags := raw.toConfig()
	diags = append(diags, Validate(cfg)...)

	return cfg, diags
}

// fileConfig 是 TOML 文件的表示；可选字段用指针区分「缺省」与「零值」。
type fileConfig struct {
	SchemaVersion *int                     `toml:"schema_version"`
	Rules         []string                 `toml:"rules"`
	LocalRecords  []string                 `toml:"local_records"`
	Settings      *fileSettings            `toml:"settings"`
	Upstreams     map[string]*fileUpstream `toml:"upstreams"`
	Groups        map[string]*fileGroup    `toml:"groups"`
}

type fileSettings struct {
	DefaultGroup *string `toml:"default_group"`
	LogRingSize  *int    `toml:"log_ring_size"`
	LogPersist   *bool   `toml:"log_persist"`
	PtrAuto      *bool   `toml:"ptr_auto"`
}

type fileUpstream struct {
	Name      *string  `toml:"name"`
	URL       *string  `toml:"url"`
	Bootstrap []string `toml:"bootstrap"`
	TimeoutMS *int     `toml:"timeout_ms"`
	Remark    *string  `toml:"remark"`
}

type fileGroup struct {
	Name      *string   `toml:"name"`
	Upstreams *[]string `toml:"upstreams"`
}

// toConfig 把文件表示转换为已解析配置。
func (f *fileConfig) toConfig() (*Config, []diagnostic.Diagnostic) {
	cfg := &Config{
		Rules:        []Rule{},
		LocalRecords: []Record{},
		Upstreams:    make(map[string]Upstream, len(f.Upstreams)),
		Groups:       make(map[string]Group, len(f.Groups)),
	}
	var diags []diagnostic.Diagnostic

	if f.SchemaVersion == nil {
		diags = append(diags, missingField("schema_version"))
	} else {
		cfg.SchemaVersion = *f.SchemaVersion
	}

	cfg.Settings = Settings{LogRingSize: DefaultLogRingSize, LogPersist: true, PtrAuto: true}
	if f.Settings == nil {
		diags = append(diags, missingField("settings"))
	} else {
		if f.Settings.DefaultGroup == nil {
			diags = append(diags, missingField("settings.default_group"))
		} else {
			cfg.Settings.DefaultGroup = *f.Settings.DefaultGroup
		}
		if f.Settings.LogRingSize != nil {
			cfg.Settings.LogRingSize = *f.Settings.LogRingSize
		}
		if f.Settings.LogPersist != nil {
			cfg.Settings.LogPersist = *f.Settings.LogPersist
		}
		if f.Settings.PtrAuto != nil {
			cfg.Settings.PtrAuto = *f.Settings.PtrAuto
		}
	}

	if f.Upstreams == nil {
		diags = append(diags, missingField("upstreams"))
	} else {
		for id, upstream := range f.Upstreams {
			diags = append(diags, convertFileUpstream(id, upstream, cfg)...)
		}
	}

	if f.Groups == nil {
		diags = append(diags, missingField("groups"))
	} else {
		for id, group := range f.Groups {
			diags = append(diags, convertFileGroup(id, group, cfg)...)
		}
	}

	for index, line := range f.Rules {
		rule, ruleDiags := parseRuleLine(line, index)
		diags = append(diags, ruleDiags...)
		cfg.Rules = append(cfg.Rules, rule)
	}

	for index, line := range f.LocalRecords {
		record, recordDiags := parseRecordLine(line, index)
		diags = append(diags, recordDiags...)
		cfg.LocalRecords = append(cfg.LocalRecords, record)
	}

	return cfg, diags
}

// jsonConfig 是 API 表示的原始结构。
type jsonConfig struct {
	SchemaVersion *int                     `json:"schema_version"`
	Rules         []Rule                   `json:"rules"`
	LocalRecords  []jsonRecord             `json:"local_records"`
	Settings      *jsonSettings            `json:"settings"`
	Upstreams     map[string]*jsonUpstream `json:"upstreams"`
	Groups        map[string]*jsonGroup    `json:"groups"`
}

type jsonSettings struct {
	DefaultGroup *string `json:"default_group"`
	LogRingSize  *int    `json:"log_ring_size"`
	LogPersist   *bool   `json:"log_persist"`
	PtrAuto      *bool   `json:"ptr_auto"`
}

type jsonRecord struct {
	Type    string  `json:"type"`
	Name    string  `json:"name"`
	TTL     *uint32 `json:"ttl"`
	Enabled *bool   `json:"enabled"`
	Value   string  `json:"value"`
}

type jsonUpstream struct {
	Name      *string  `json:"name"`
	URL       *string  `json:"url"`
	Bootstrap []string `json:"bootstrap"`
	TimeoutMS *int     `json:"timeout_ms"`
	Remark    *string  `json:"remark"`
}

type jsonGroup struct {
	Name      *string   `json:"name"`
	Upstreams *[]string `json:"upstreams"`
}

// toConfig 把 API 表示转换为已解析配置。
func (j *jsonConfig) toConfig() (*Config, []diagnostic.Diagnostic) {
	cfg := &Config{
		Rules:        []Rule{},
		LocalRecords: []Record{},
		Upstreams:    make(map[string]Upstream, len(j.Upstreams)),
		Groups:       make(map[string]Group, len(j.Groups)),
	}
	var diags []diagnostic.Diagnostic

	if j.SchemaVersion == nil {
		diags = append(diags, missingField("schema_version"))
	} else {
		cfg.SchemaVersion = *j.SchemaVersion
	}

	cfg.Settings = Settings{LogRingSize: DefaultLogRingSize, LogPersist: true, PtrAuto: true}
	if j.Settings == nil {
		diags = append(diags, missingField("settings"))
	} else {
		if j.Settings.DefaultGroup == nil {
			diags = append(diags, missingField("settings.default_group"))
		} else {
			cfg.Settings.DefaultGroup = *j.Settings.DefaultGroup
		}
		if j.Settings.LogRingSize != nil {
			cfg.Settings.LogRingSize = *j.Settings.LogRingSize
		}
		if j.Settings.LogPersist != nil {
			cfg.Settings.LogPersist = *j.Settings.LogPersist
		}
		if j.Settings.PtrAuto != nil {
			cfg.Settings.PtrAuto = *j.Settings.PtrAuto
		}
	}

	if j.Upstreams == nil {
		diags = append(diags, missingField("upstreams"))
	} else {
		for id, upstream := range j.Upstreams {
			diags = append(diags, convertJSONUpstream(id, upstream, cfg)...)
		}
	}

	if j.Groups == nil {
		diags = append(diags, missingField("groups"))
	} else {
		for id, group := range j.Groups {
			diags = append(diags, convertJSONGroup(id, group, cfg)...)
		}
	}

	for _, rule := range j.Rules {
		rule.Match = strings.ToUpper(strings.TrimSpace(rule.Match))
		rule.Domain = strings.TrimSpace(rule.Domain)
		rule.Group = strings.TrimSpace(rule.Group)
		cfg.Rules = append(cfg.Rules, rule)
	}

	for _, record := range j.LocalRecords {
		converted := Record{
			Type:    strings.ToUpper(strings.TrimSpace(record.Type)),
			Name:    strings.TrimSpace(record.Name),
			TTL:     DefaultRecordTTL,
			Enabled: true,
			Value:   strings.TrimSpace(record.Value),
		}
		if record.TTL != nil {
			converted.TTL = *record.TTL
		}
		if record.Enabled != nil {
			converted.Enabled = *record.Enabled
		}
		cfg.LocalRecords = append(cfg.LocalRecords, converted)
	}

	return cfg, diags
}

// convertFileUpstream 转换单个文件上游。
func convertFileUpstream(id string, upstream *fileUpstream, cfg *Config) []diagnostic.Diagnostic {
	var diags []diagnostic.Diagnostic

	if upstream == nil {
		diags = append(diags, missingField("upstreams."+id))
		upstream = &fileUpstream{}
	}

	converted := Upstream{
		Name:      id,
		Bootstrap: []string{},
		TimeoutMS: DefaultTimeoutMS,
	}
	if upstream.URL == nil {
		diags = append(diags, missingField("upstreams."+id+".url"))
	} else {
		converted.URL = *upstream.URL
	}
	if upstream.Name != nil {
		converted.Name = *upstream.Name
	}
	if upstream.TimeoutMS != nil {
		converted.TimeoutMS = *upstream.TimeoutMS
	}
	if upstream.Bootstrap != nil {
		converted.Bootstrap = upstream.Bootstrap
	}
	if upstream.Remark != nil {
		converted.Remark = *upstream.Remark
	}
	cfg.Upstreams[id] = converted

	return diags
}

// convertFileGroup 转换单个文件组。
func convertFileGroup(id string, group *fileGroup, cfg *Config) []diagnostic.Diagnostic {
	var diags []diagnostic.Diagnostic

	if group == nil {
		diags = append(diags, missingField("groups."+id))
		group = &fileGroup{}
	}

	converted := Group{Name: id, Upstreams: []string{}}
	if group.Name != nil {
		converted.Name = *group.Name
	}
	if group.Upstreams == nil {
		diags = append(diags, missingField("groups."+id+".upstreams"))
	} else {
		converted.Upstreams = *group.Upstreams
	}
	cfg.Groups[id] = converted

	return diags
}

// convertJSONUpstream 转换单个 API 上游。
func convertJSONUpstream(id string, upstream *jsonUpstream, cfg *Config) []diagnostic.Diagnostic {
	var diags []diagnostic.Diagnostic

	if upstream == nil {
		diags = append(diags, missingField("upstreams."+id))
		upstream = &jsonUpstream{}
	}

	converted := Upstream{
		Name:      id,
		Bootstrap: []string{},
		TimeoutMS: DefaultTimeoutMS,
	}
	if upstream.URL == nil {
		diags = append(diags, missingField("upstreams."+id+".url"))
	} else {
		converted.URL = *upstream.URL
	}
	if upstream.Name != nil {
		converted.Name = *upstream.Name
	}
	if upstream.TimeoutMS != nil {
		converted.TimeoutMS = *upstream.TimeoutMS
	}
	if upstream.Bootstrap != nil {
		converted.Bootstrap = upstream.Bootstrap
	}
	if upstream.Remark != nil {
		converted.Remark = *upstream.Remark
	}
	cfg.Upstreams[id] = converted

	return diags
}

// convertJSONGroup 转换单个 API 组。
func convertJSONGroup(id string, group *jsonGroup, cfg *Config) []diagnostic.Diagnostic {
	var diags []diagnostic.Diagnostic

	if group == nil {
		diags = append(diags, missingField("groups."+id))
		group = &jsonGroup{}
	}

	converted := Group{Name: id, Upstreams: []string{}}
	if group.Name != nil {
		converted.Name = *group.Name
	}
	if group.Upstreams == nil {
		diags = append(diags, missingField("groups."+id+".upstreams"))
	} else {
		converted.Upstreams = *group.Upstreams
	}
	cfg.Groups[id] = converted

	return diags
}

// parseRuleLine 解析一行规则。
func parseRuleLine(line string, index int) (Rule, []diagnostic.Diagnostic) {
	path := fmt.Sprintf("rules[%d]", index)
	parts := strings.Split(line, ",")
	if len(parts) != 3 {
		return Rule{}, []diagnostic.Diagnostic{lineError(path, diagnostic.CodeInvalidRuleLine, "规则行必须恰好包含 3 段")}
	}

	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return Rule{}, []diagnostic.Diagnostic{lineError(path, diagnostic.CodeInvalidRuleLine, "规则行存在空段")}
		}
	}

	return Rule{
		Match:  strings.ToUpper(parts[0]),
		Domain: parts[1],
		Group:  parts[2],
	}, nil
}

// parseRecordLine 解析一行本地记录。
func parseRecordLine(line string, index int) (Record, []diagnostic.Diagnostic) {
	path := fmt.Sprintf("local_records[%d]", index)
	parts := strings.SplitN(line, ",", 5)
	if len(parts) != 5 {
		return Record{}, []diagnostic.Diagnostic{lineError(path, diagnostic.CodeInvalidRecordLine, "记录行必须包含 5 段")}
	}

	for i := 0; i < 4; i++ {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return Record{}, []diagnostic.Diagnostic{lineError(path, diagnostic.CodeInvalidRecordLine, "记录行存在空段")}
		}
	}

	ttl, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return Record{}, []diagnostic.Diagnostic{lineError(path+".ttl", diagnostic.CodeInvalidTTL, "记录 TTL 不是整数")}
	}

	enabled, err := parseBool(parts[3])
	if err != nil {
		return Record{}, []diagnostic.Diagnostic{lineError(path+".enabled", diagnostic.CodeInvalidRecordLine, "记录 enabled 必须是 true 或 false")}
	}

	value := strings.TrimSpace(parts[4])
	if value == "" {
		return Record{}, []diagnostic.Diagnostic{lineError(path+".value", diagnostic.CodeInvalidRecordValue, "记录值为空")}
	}

	return Record{
		Type:    strings.ToUpper(parts[0]),
		Name:    parts[1],
		TTL:     uint32(ttl),
		Enabled: enabled,
		Value:   value,
	}, nil
}

// parseBool 只接受 true/false。
func parseBool(text string) (bool, error) {
	switch strings.ToLower(text) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("不是布尔值")
	}
}

// missingField 构造缺少必填字段的诊断。
func missingField(path string) diagnostic.Diagnostic {
	return diagnostic.Diagnostic{
		Path:     path,
		Code:     diagnostic.CodeMissingField,
		Severity: diagnostic.SeverityError,
		Message:  "缺少必填字段",
	}
}

// lineError 构造行级诊断。
func lineError(path, code, message string) diagnostic.Diagnostic {
	return diagnostic.Diagnostic{
		Path:     path,
		Code:     code,
		Severity: diagnostic.SeverityError,
		Message:  message,
	}
}

// tomlDiagnostic 把 TOML 解码错误转换为诊断。
func tomlDiagnostic(err error) diagnostic.Diagnostic {
	var decodeErr *toml.DecodeError
	if !errors.As(err, &decodeErr) {
		return diagnostic.Diagnostic{
			Code:     diagnostic.CodeInvalidTOML,
			Severity: diagnostic.SeverityError,
			Message:  err.Error(),
		}
	}

	line, _ := decodeErr.Position()
	message := decodeErr.Error()
	lower := strings.ToLower(message)

	code := diagnostic.CodeInvalidTOML
	switch {
	case strings.Contains(lower, "unknown field"),
		// go-toml 对文档里存在、目标结构体里没有的表报 "missing table"；
		// 它和未知字段是同一类问题，统一按 unknown_field 上报。
		strings.Contains(lower, "missing table"):
		code = diagnostic.CodeUnknownField
	case strings.Contains(lower, "cannot decode"),
		strings.Contains(lower, "incompatible"),
		strings.Contains(lower, "expected"),
		strings.Contains(lower, "type"):
		code = diagnostic.CodeInvalidType
	}

	return diagnostic.Diagnostic{
		Path:     strings.Join(decodeErr.Key(), "."),
		Code:     code,
		Severity: diagnostic.SeverityError,
		Message:  message,
		Line:     line,
	}
}

// jsonDiagnostic 把 JSON 解码错误转换为诊断。
func jsonDiagnostic(err error) diagnostic.Diagnostic {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return diagnostic.Diagnostic{
			Path:     typeErr.Field,
			Code:     diagnostic.CodeInvalidType,
			Severity: diagnostic.SeverityError,
			Message:  typeErr.Error(),
		}
	}

	code := diagnostic.CodeInvalidValue
	if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
		code = diagnostic.CodeUnknownField
	}

	return diagnostic.Diagnostic{
		Code:     code,
		Severity: diagnostic.SeverityError,
		Message:  err.Error(),
	}
}
