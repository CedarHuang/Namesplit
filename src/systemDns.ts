import { invoke } from "@tauri-apps/api/core";

export interface InterfaceDns {
  guid: string;
  name: string;
  ipv4: string[];
  ipv6: string[];
  points_to_loopback: boolean;
}

export interface SystemDnsStatus {
  checked_at: string;
  interfaces: InterfaceDns[];
}

/** 只读读取活动网卡的 DNS 配置。 */
export function getSystemDns(): Promise<SystemDnsStatus> {
  return invoke<SystemDnsStatus>("system_dns_get");
}

/** 打开 Windows「网络和 Internet」设置页；应用本身不写网卡配置。 */
export function openSystemNetworkSettings(): Promise<{ ok: boolean }> {
  return invoke<{ ok: boolean }>("open_system_network_settings");
}
