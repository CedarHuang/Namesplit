import type { IpcErrorCode } from "../ipc/errors";

export interface ErrorTextEntry {
  message: string;
  advice?: string;
}

/**
 * IPC 错误码的唯一文案来源：message 面向用户，advice 是可操作的处理建议。
 * 码表覆盖后端可能转发的全部错误码；新增码时同步 IpcErrorCode，否则编译失败。
 */
export const ERROR_TEXTS: Record<IpcErrorCode, ErrorTextEntry> = {
  invalid_request: {
    message: "请求参数不合法。",
    advice: "检查输入后重试；若持续出现请查看应用日志。",
  },
  unauthorized: {
    message: "内部接口鉴权失败。",
    advice: "重启应用；仍失败请查看应用日志。",
  },
  not_found: {
    message: "请求的资源不存在。",
    advice: "刷新页面后重试；若持续出现请重启应用。",
  },
  method_not_allowed: {
    message: "该操作不受当前版本支持。",
    advice: "重启应用以使用匹配的版本。",
  },
  revision_conflict: {
    message: "配置已被其他操作修改。",
    advice: "已载入最新版本，请核对后再保存。",
  },
  payload_too_large: {
    message: "提交的内容超过大小限制。",
    advice: "减少配置内容后重试。",
  },
  unsupported_media_type: {
    message: "请求格式不受支持。",
    advice: "重启应用；仍失败请查看应用日志。",
  },
  invalid_config: {
    message: "配置校验失败。",
    advice: "按标记的字段修正后重新保存。",
  },
  internal: {
    message: "应用内部错误。",
    advice: "重试操作；仍失败请重启应用并查看日志。",
  },
  config_write_failed: {
    message: "写入配置文件失败。",
    advice: "确认数据目录可写后重试；原配置未被破坏。",
  },
  apply_failed: {
    message: "新配置无法应用，运行态与文件未改变。",
    advice: "检查规则、上游与本地记录后重试。",
  },
  not_ready: {
    message: "解析服务尚未就绪。",
    advice: "稍等片刻后重试。",
  },
  shutdown_in_progress: {
    message: "应用正在退出，操作未执行。",
    advice: "重新打开应用后再试。",
  },
  sidecar_unavailable: {
    message: "解析服务未运行。",
    advice: "稍后重试；若持续失败请重启解析服务或应用。",
  },
  system_dns_unavailable: {
    message: "读取系统 DNS 配置失败。",
    advice: "稍后重试；仍失败请查看应用日志。",
  },
  autostart_failed: {
    message: "登录自启设置失败。",
    advice: "确认当前用户有权限修改启动项后重试。",
  },
  invalid_prefs_value: {
    message: "偏好设置取值非法。",
    advice: "改回有效取值后重试。",
  },
  open_failed: {
    message: "打开目标失败。",
    advice: "确认目标路径存在且当前用户有权限访问。",
  },
  diagnostics_export_failed: {
    message: "诊断包生成失败。",
    advice: "确认保存位置可写后重试。",
  },
};

/** 非结构化错误的通用兜底文案。 */
export const GENERIC_ERROR_TEXT = "操作失败，请查看应用日志。";

/** 未登记错误码的兜底文案；保留 code 便于日志排查。 */
export function unknownErrorCodeText(code: string): string {
  return `操作失败（${code}）`;
}
