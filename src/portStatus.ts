import { invoke } from "@tauri-apps/api/core";

export type PortState = "free" | "in_use" | "unavailable" | "unknown";

export interface PortHolder {
  pid: number;
  name: string;
  path?: string;
  /** 占用者是否为当前应用正在监护的 sidecar；只有非自身占用才构成冲突。 */
  is_self: boolean;
}

export interface PortTarget {
  address: string;
  protocol: "udp" | "tcp";
  status: PortState;
  holders: PortHolder[];
}

export interface PortStatus {
  checked_at: string;
  port: number;
  targets: PortTarget[];
  attribution_limited: boolean;
}

/** 53 端口占用与归因（Tauri 应用侧只读检测）。 */
export function getPortStatus(): Promise<PortStatus> {
  return invoke<PortStatus>("port_status");
}

/** 浏览器里直接跑 vite 时没有 Tauri 注入的 IPC。 */
export function isTauri(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}
