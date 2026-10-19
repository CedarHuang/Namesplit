import { describe, expect, it } from "vitest";

import type { PortStatus } from "../../portStatus";
import { buildDiagnosticSummary } from "./summary";

const ports: PortStatus = {
  checked_at: "2026-10-20T00:00:00Z",
  port: 53,
  targets: [
    {
      address: "127.0.0.1:53",
      protocol: "udp",
      status: "in_use",
      holders: [{ pid: 42, name: "namesplit-core.exe", is_self: true }],
    },
  ],
  attribution_limited: false,
};

describe("diagnostic summary", () => {
  it("包含状态与端口归因，不包含敏感字段", () => {
    const text = buildDiagnosticSummary({
      appVersion: "0.0.0",
      sidecarVersion: "0.0.0-dev",
      commit: "abc",
      dataDir: "C:\\data",
      configPath: "C:\\data\\config.toml",
      logPath: "C:\\data\\app.log",
      sidecarState: "running",
      status: null,
      ports,
      systemDns: null,
      configRevision: 3,
      validationValid: true,
      errors: [],
      warnings: [],
    });
    expect(text).toContain("sidecar_state=running");
    expect(text).toContain("namesplit-core.exe(42)[self]");
    expect(text).not.toContain("token");
  });
});
