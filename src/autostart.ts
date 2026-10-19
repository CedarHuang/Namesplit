import { invoke } from "@tauri-apps/api/core";

export interface AutostartStatus {
  enabled: boolean;
  path: string;
  path_valid: boolean;
}

/** 读取当前用户的登录自启项（HKCU Run）。 */
export function getAutostart(): Promise<AutostartStatus> {
  return invoke<AutostartStatus>("autostart_get");
}

/** 开启或关闭登录自启。 */
export function setAutostart(enabled: boolean): Promise<AutostartStatus> {
  return invoke<AutostartStatus>("autostart_set", { enabled });
}
