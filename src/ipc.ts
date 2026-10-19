// Tauri 命令失败时 reject 的是后端序列化的错误体（本应用为 `{code, message}`），
// 不是 Error 实例；UI 必须按 code 本地化提示，message 只作诊断补充。

export interface IpcErrorPayload {
  code: string;
  message?: string;
}

/** 已知错误码的用户可读文案；新增命令错误码时在此登记。 */
const CODE_MESSAGES: Record<string, string> = {
  system_dns_unavailable: "读取系统 DNS 配置失败，请稍后重试。",
  autostart_failed: "登录自启设置失败，请稍后重试。",
  open_failed: "打开系统设置失败，请稍后重试。",
};

function asIpcError(value: unknown): IpcErrorPayload | null {
  if (typeof value !== "object" || value === null) {
    return null;
  }
  const { code, message } = value as { code?: unknown; message?: unknown };
  if (typeof code !== "string" || code.length === 0) {
    return null;
  }
  return { code, message: typeof message === "string" ? message : undefined };
}

/** 把 invoke 的 reject 值归一为可展示文本。 */
export function ipcErrorMessage(error: unknown): string {
  const payload = asIpcError(error);
  if (payload) {
    const known = CODE_MESSAGES[payload.code];
    if (known) {
      return known;
    }
    // 未登记的错误码保留后端文案，避免把结构化错误显示成 [object Object]。
    return payload.message ?? `操作失败（${payload.code}）`;
  }
  if (error instanceof Error) {
    return error.message;
  }
  return typeof error === "string" ? error : "操作失败，请查看应用日志。";
}
