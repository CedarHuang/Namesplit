// Tauri 命令失败时 reject 的是后端序列化的错误体，不是 Error 实例；
// UI 必须按 code 本地化提示，message 只作诊断补充。

import {
  ERROR_TEXTS,
  GENERIC_ERROR_TEXT,
  unknownErrorCodeText,
  type ErrorTextEntry,
} from "../i18n/errorText";

/** 后端可能返回的错误码；新增码时必须同步文案表，否则编译失败。 */
export type IpcErrorCode =
  | "invalid_request"
  | "unauthorized"
  | "not_found"
  | "method_not_allowed"
  | "revision_conflict"
  | "payload_too_large"
  | "unsupported_media_type"
  | "invalid_config"
  | "internal"
  | "config_write_failed"
  | "apply_failed"
  | "not_ready"
  | "shutdown_in_progress"
  | "sidecar_unavailable"
  | "system_dns_unavailable"
  | "autostart_failed"
  | "invalid_prefs_value"
  | "open_failed"
  | "diagnostics_export_failed";

export interface IpcErrorPayload {
  code: string;
  message?: string;
  details?: unknown;
  current_revision?: number;
}

function asIpcError(value: unknown): IpcErrorPayload | null {
  if (typeof value !== "object" || value === null) {
    return null;
  }
  const { code, message, details, current_revision } = value as {
    code?: unknown;
    message?: unknown;
    details?: unknown;
    current_revision?: unknown;
  };
  if (typeof code !== "string" || code.length === 0) {
    return null;
  }
  return {
    code,
    message: typeof message === "string" ? message : undefined,
    details,
    current_revision:
      typeof current_revision === "number" ? current_revision : undefined,
  };
}

function knownText(code: string): ErrorTextEntry | null {
  if (!Object.prototype.hasOwnProperty.call(ERROR_TEXTS, code)) {
    return null;
  }
  return ERROR_TEXTS[code as IpcErrorCode];
}

export function ipcErrorPayload(error: unknown): IpcErrorPayload | null {
  return asIpcError(error);
}

export function ipcErrorCode(error: unknown): string | null {
  return asIpcError(error)?.code ?? null;
}

/** 把 invoke 的 reject 值归一为可展示文本。 */
export function ipcErrorMessage(error: unknown): string {
  const payload = asIpcError(error);
  if (payload) {
    const known = knownText(payload.code);
    if (known) {
      return known.message;
    }
    // 未登记的错误码保留后端文案，避免把结构化错误显示成 [object Object]。
    return payload.message ?? unknownErrorCodeText(payload.code);
  }
  if (error instanceof Error) {
    return error.message;
  }
  return typeof error === "string" ? error : GENERIC_ERROR_TEXT;
}

/** 已知错误码的处理建议；未登记或非结构化错误返回 null。 */
export function ipcErrorAdvice(error: unknown): string | null {
  const payload = asIpcError(error);
  if (!payload) {
    return null;
  }
  return knownText(payload.code)?.advice ?? null;
}
