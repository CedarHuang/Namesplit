// UI 与 sidecar 之间的载荷类型。字段名必须保持 snake_case：Tauri IPC 与 sidecar
// 的 JSON 都按字段名原样序列化，前端改成 camelCase 会在运行时读不到值。

export interface RequestFlags {
  rd: boolean;
  cd: boolean;
}

export interface ResponseFlags {
  aa: boolean;
  tc: boolean;
  ra: boolean;
}

export interface Attempt {
  upstream: string;
  duration_ms: number;
  error?: string;
}

export interface QueryEvent {
  event_id: number;
  time: string;
  domain: string;
  qtype: string;
  rule: string;
  rule_index: number;
  rule_kind: string;
  source: "local" | "cache" | "upstream";
  group: string;
  upstream: string;
  upstream_name: string;
  duration_ms: number;
  rcode: number;
  rcode_name: string;
  protocol: string;
  answers: string[];
  authority: string[];
  additional: string[];
  ttl: number;
  cache_hit: boolean;
  do: boolean;
  ad: boolean;
  request_flags: RequestFlags;
  response_flags: ResponseFlags;
  attempts?: Attempt[];
  error?: string;
}

export interface HistoricalQuery extends QueryEvent {
  id: number;
}

export interface QueryFilter {
  limit?: number;
  cursor?: string;
  domain?: string;
  qtype?: string;
  source?: string;
  group?: string;
  rule?: string;
  upstream?: string;
  rcode?: number;
  min_duration_ms?: number;
  max_duration_ms?: number;
  from?: string;
  to?: string;
}

export interface ValidationDiagnostic {
  path: string;
  code: string;
  severity: "error" | "warning";
  message: string;
  line?: number;
}

export interface Settings {
  default_group: string;
  log_ring_size: number;
  log_persist: boolean;
  ptr_auto: boolean;
}

export interface UpstreamConfig {
  name: string;
  url: string;
  bootstrap?: string[];
  timeout_ms?: number;
  remark?: string;
}

export interface GroupConfig {
  name: string;
  upstreams: string[];
}

export interface RuleConfig {
  match: "DOMAIN" | "DOMAIN-SUFFIX";
  domain: string;
  group: string;
}

export type LocalRecordType = "A" | "AAAA" | "CNAME" | "TXT" | "PTR";

export interface LocalRecordConfig {
  type: LocalRecordType;
  name: string;
  value: string;
  ttl?: number;
  enabled?: boolean;
}

export interface ConfigDocument {
  schema_version: number;
  settings: Settings;
  upstreams: Record<string, UpstreamConfig>;
  groups: Record<string, GroupConfig>;
  rules: RuleConfig[];
  local_records: LocalRecordConfig[];
}

export interface ConfigError {
  code: string;
  message: string;
  line?: number;
  path?: string;
  details?: ValidationDiagnostic[];
}

export interface ConfigEnvelope {
  revision: number;
  config: ConfigDocument;
  config_error: ConfigError | null;
  warnings: ValidationDiagnostic[];
}

export interface ConfigSaveResult {
  revision: number;
  warnings: ValidationDiagnostic[];
}

export interface ValidateResult {
  valid: boolean;
  errors: ValidationDiagnostic[];
  warnings: ValidationDiagnostic[];
}

export interface RuleTestLocal {
  hit: boolean;
  name?: string;
  qtype?: string;
  nodata?: boolean;
  answers?: string[];
  ttl?: number;
}

export interface RuleTestResult {
  canonical: string;
  local: RuleTestLocal;
  rule: {
    index: number;
    match: "DOMAIN" | "DOMAIN-SUFFIX";
    domain: string;
    group: string;
  } | null;
  target_group: string;
  default_group: string;
  warnings: ValidationDiagnostic[];
}

export interface QueriesPage {
  items: HistoricalQuery[];
  next_cursor: string | null;
  persistence_enabled: boolean;
}

export interface StatsCounters {
  queries: number;
  failures: number;
  cache_hits: number;
  dropped_stream: number;
  persist_errors: number;
}

export interface StatsResponse {
  uptime_ms: number;
  counters: StatsCounters;
  qps_1s: number;
  persistence_enabled: boolean;
}

export interface MinuteStat {
  minute: string;
  queries: number;
  failures: number;
  cache_hits: number;
  latency_avg_ms?: number;
  latency_max_ms?: number;
}

export interface MinuteResponse {
  items: MinuteStat[];
  persistence_enabled: boolean;
}

export interface TopItem {
  key: string;
  count: number;
}

export interface TopResponse {
  items: TopItem[];
  persistence_enabled: boolean;
}

export interface DnsBindTarget {
  address: string;
  protocol: string;
  error?: string;
}

export interface DnsBindError {
  code: string;
  message: string;
  targets: DnsBindTarget[];
}

export interface DnsStatus {
  ready: boolean;
  udp: string[];
  tcp: string[];
  bind_error: DnsBindError | null;
}

export interface StatusResponse {
  version: string;
  commit: string;
  uptime_ms: number;
  config_revision: number;
  config_error: ConfigError | null;
  dns: DnsStatus;
}

export interface UpstreamTestResult {
  name: string;
  address: string;
  ok: boolean;
  latency_ms: number;
  tls_ok: boolean | null;
  error: string;
}

export interface UpstreamTestResponse {
  results: UpstreamTestResult[];
}

export interface SidecarStatePayload {
  state: "running" | "exited" | "restarting" | "failed";
  pid?: number;
  restart_count?: number;
  reason?: string;
  config_error?: ConfigError;
}

export interface AppInfo {
  app_version: string;
  commit: string;
  sidecar_version: string;
  data_dir: string;
  config_path: string;
  querylog_path: string;
  log_path: string;
  sidecar_log_path: string;
  renderer_reloads: number;
}

export interface Prefs {
  language: string;
  theme: string;
}
