import { describe, expect, it } from "vitest";

import { ERROR_TEXTS } from "../i18n/errorText";
import {
  ipcErrorAdvice,
  ipcErrorCode,
  ipcErrorMessage,
  ipcErrorPayload,
  type IpcErrorCode,
} from "./errors";

// 码表必须覆盖后端可能转发的全部 code；新增码时同步这里与 IpcErrorCode。
const EXPECTED_ERROR_CODES = [
  "invalid_request",
  "unauthorized",
  "not_found",
  "method_not_allowed",
  "revision_conflict",
  "payload_too_large",
  "unsupported_media_type",
  "invalid_config",
  "internal",
  "config_write_failed",
  "apply_failed",
  "not_ready",
  "shutdown_in_progress",
  "sidecar_unavailable",
  "system_dns_unavailable",
  "autostart_failed",
  "invalid_prefs_value",
  "open_failed",
  "diagnostics_export_failed",
] as const satisfies readonly IpcErrorCode[];

// 编译期兜底：联合类型新增 code 但下面数组没跟上时，这里的类型会变成 never。
const everyCodeCovered: Exclude<IpcErrorCode, (typeof EXPECTED_ERROR_CODES)[number]> extends never
  ? true
  : never = true;

describe("ipc errors", () => {
  it("每个错误码都有非空文案，且清单无重复", () => {
    expect(everyCodeCovered).toBe(true);
    expect(new Set(EXPECTED_ERROR_CODES).size).toBe(EXPECTED_ERROR_CODES.length);
    for (const code of EXPECTED_ERROR_CODES) {
      expect(ERROR_TEXTS[code].message.length).toBeGreaterThan(0);
      expect(ipcErrorMessage({ code })).toBe(ERROR_TEXTS[code].message);
    }
  });

  it("每个已知错误码都能取到处理建议", () => {
    for (const code of EXPECTED_ERROR_CODES) {
      expect(ipcErrorAdvice({ code })).toBe(ERROR_TEXTS[code].advice ?? null);
    }
    expect(ipcErrorAdvice({ code: "revision_conflict" })).toContain("最新版本");
  });

  it("未知错误码保留后端 message", () => {
    expect(ipcErrorMessage({ code: "some_new_code", message: "后端说明" })).toBe(
      "后端说明",
    );
    expect(ipcErrorAdvice({ code: "some_new_code", message: "后端说明" })).toBeNull();
  });

  it("未知错误码无 message 时给出 code 兜底", () => {
    expect(ipcErrorMessage({ code: "some_new_code" })).toContain("some_new_code");
  });

  it("非结构化错误回退文本", () => {
    expect(ipcErrorMessage(new Error("boom"))).toBe("boom");
    expect(ipcErrorMessage(undefined)).toContain("操作失败");
    expect(ipcErrorMessage("raw failure")).toBe("raw failure");
    expect(ipcErrorAdvice(undefined)).toBeNull();
  });

  it("保留 details 与 current_revision", () => {
    const payload = ipcErrorPayload({
      code: "revision_conflict",
      current_revision: 8,
      details: [{ path: "rules[0]" }],
    });
    expect(ipcErrorCode(payload)).toBe("revision_conflict");
    expect(payload?.current_revision).toBe(8);
    expect(payload?.details).toBeDefined();
  });
});
