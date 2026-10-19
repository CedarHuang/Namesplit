import { invoke } from "@tauri-apps/api/core";

import type {
  AppInfo,
  ConfigDocument,
  ConfigEnvelope,
  ConfigSaveResult,
  QueriesPage,
  MinuteResponse,
  Prefs,
  QueryFilter,
  RuleTestResult,
  SidecarStatePayload,
  StatsResponse,
  StatusResponse,
  TopResponse,
  UpstreamTestResponse,
  ValidateResult,
} from "./types";

export function getAppInfo(): Promise<AppInfo> {
  return invoke<AppInfo>("app_info");
}

/** 读取当前 sidecar 生命周期状态；应用刚启动或状态未知时返回 null。 */
export function getSidecarState(): Promise<SidecarStatePayload | null> {
  return invoke<SidecarStatePayload | null>("sidecar_state");
}

export function openDataDir(): Promise<{ ok: boolean }> {
  return invoke<{ ok: boolean }>("open_data_dir");
}

export function openConfigFile(): Promise<{ ok: boolean }> {
  return invoke<{ ok: boolean }>("open_config_file");
}

export function getPrefs(): Promise<Prefs> {
  return invoke<Prefs>("prefs_get");
}

export function setPrefs(patch: Partial<Prefs>): Promise<Prefs> {
  return invoke<Prefs>("prefs_set", patch);
}

export function getConfig(): Promise<ConfigEnvelope> {
  return invoke<ConfigEnvelope>("config_get");
}

export function validateConfig(config: ConfigDocument): Promise<ValidateResult> {
  return invoke<ValidateResult>("config_validate", { config });
}

export function saveConfig(
  baseRevision: number,
  config: ConfigDocument,
): Promise<ConfigSaveResult> {
  return invoke<ConfigSaveResult>("config_save", {
    base_revision: baseRevision,
    config,
  });
}

export function resetConfig(baseRevision: number): Promise<ConfigSaveResult> {
  return invoke<ConfigSaveResult>("config_reset", {
    base_revision: baseRevision,
  });
}

export function testRule(domain: string, qtype?: string): Promise<RuleTestResult> {
  return invoke<RuleTestResult>("rules_test", { domain, qtype });
}

export function listQueries(filter: QueryFilter = {}): Promise<QueriesPage> {
  return invoke<QueriesPage>("queries_list", { filter });
}

export function clearQueries(): Promise<{ deleted: number }> {
  return invoke<{ deleted: number }>("queries_clear");
}

export function getStats(): Promise<StatsResponse> {
  return invoke<StatsResponse>("stats_get");
}

export function getMinuteStats(from?: string, to?: string): Promise<MinuteResponse> {
  return invoke<MinuteResponse>("stats_minute", { from, to });
}

export function getTop(
  by: "domain" | "upstream",
  limit?: number,
  since?: string,
): Promise<TopResponse> {
  return invoke<TopResponse>("stats_top", { by, limit, since });
}

export function getStatus(): Promise<StatusResponse> {
  return invoke<StatusResponse>("status_get");
}

export function testUpstream(request: {
  upstream_id?: string;
  group_id?: string;
  upstream?: { url: string; bootstrap?: string[]; timeout_ms?: number };
}): Promise<UpstreamTestResponse> {
  return invoke<UpstreamTestResponse>("upstream_test", { request });
}

export function subscribeEvents(): Promise<{ ok: boolean }> {
  return invoke<{ ok: boolean }>("events_subscribe");
}

export function unsubscribeEvents(): Promise<{ ok: boolean }> {
  return invoke<{ ok: boolean }>("events_unsubscribe");
}

export function restartSidecar(): Promise<{ ok: boolean }> {
  return invoke<{ ok: boolean }>("sidecar_restart");
}
