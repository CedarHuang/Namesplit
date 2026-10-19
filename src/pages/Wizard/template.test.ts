import { describe, expect, it } from "vitest";

import type { ConfigDocument, UpstreamConfig } from "../../ipc/types";
import { applyTemplate, sameUpstream } from "./template";

function config(overrides: Partial<ConfigDocument> = {}): ConfigDocument {
  return {
    schema_version: 1,
    settings: {
      default_group: "default",
      log_ring_size: 5000,
      log_persist: true,
      ptr_auto: true,
    },
    upstreams: {},
    groups: {},
    rules: [],
    local_records: [],
    ...overrides,
  };
}

const cloudflareUdp: UpstreamConfig = {
  name: "Cloudflare",
  url: "udp://1.1.1.1:53",
  timeout_ms: 5000,
};

describe("wizard template merge", () => {
  it("空配置新增模板上游与组并切换默认组", () => {
    const next = applyTemplate(config(), "udp");

    expect(Object.keys(next.upstreams).sort()).toEqual(["cloudflare", "quad9"]);
    expect(next.groups.default).toEqual({
      name: "Public UDP",
      upstreams: ["cloudflare", "quad9"],
    });
    expect(next.settings.default_group).toBe("default");
    expect(next.settings).toMatchObject({ log_ring_size: 5000, log_persist: true });
  });

  it("保留已有规则、组与其他上游", () => {
    const existing = config({
      upstreams: { corp: { name: "公司内网", url: "udp://10.0.0.53:53" } },
      groups: { corp: { name: "公司", upstreams: ["corp"] } },
      rules: [{ match: "DOMAIN-SUFFIX", domain: "corp.local", group: "corp" }],
    });
    const next = applyTemplate(existing, "udp");

    expect(next.rules).toEqual(existing.rules);
    expect(next.groups.corp).toEqual(existing.groups.corp);
    expect(next.upstreams.corp).toEqual(existing.upstreams.corp);
    expect(next.groups.default).toBeDefined();
    expect(next.settings.default_group).toBe("default");
  });

  it("ID 被不同定义占用时分配新 ID，不覆盖原上游", () => {
    const existing = config({
      upstreams: { cloudflare: { name: "内部 Cloudflare", url: "udp://10.0.0.1:53" } },
    });
    const next = applyTemplate(existing, "udp");

    expect(next.upstreams.cloudflare).toEqual({
      name: "内部 Cloudflare",
      url: "udp://10.0.0.1:53",
    });
    expect(next.upstreams["cloudflare-2"]).toEqual(cloudflareUdp);
    expect(next.groups.default.upstreams).toEqual(["cloudflare-2", "quad9"]);
  });

  it("连续应用两次结果一致（幂等）", () => {
    const once = applyTemplate(config(), "doh");
    const twice = applyTemplate(once, "doh");
    expect(twice).toEqual(once);
    expect(twice.settings.default_group).toBe(once.settings.default_group);
  });

  it("名字与成员一致的组直接复用", () => {
    const existing = config({
      upstreams: {
        cloudflare: {
          name: "Cloudflare",
          url: "https://cloudflare-dns.com/dns-query",
          timeout_ms: 5000,
        },
        google: {
          name: "Google",
          url: "https://dns.google/dns-query",
          timeout_ms: 5000,
        },
      },
      groups: {
        public: { name: "Public DoH", upstreams: ["google", "cloudflare"] },
      },
    });
    const next = applyTemplate(existing, "doh");

    expect(Object.keys(next.groups)).toEqual(["public"]);
    expect(next.settings.default_group).toBe("public");
  });

  it("default 组被占用且不同时新建 default-2", () => {
    const existing = config({
      upstreams: { corp: { name: "公司内网", url: "udp://10.0.0.53:53" } },
      groups: { default: { name: "公司", upstreams: ["corp"] } },
    });
    const next = applyTemplate(existing, "udp");

    expect(next.groups.default).toEqual(existing.groups.default);
    expect(next.groups["default-2"]).toEqual({
      name: "Public UDP",
      upstreams: ["cloudflare", "quad9"],
    });
    expect(next.settings.default_group).toBe("default-2");
  });

  it("上游等价判断忽略 bootstrap 顺序", () => {
    expect(
      sameUpstream(
        { name: "a", url: "https://a.test/dns-query", bootstrap: ["8.8.8.8", "1.1.1.1"] },
        { name: "a", url: "https://a.test/dns-query", bootstrap: ["1.1.1.1", "8.8.8.8"] },
      ),
    ).toBe(true);
    expect(
      sameUpstream(
        { name: "a", url: "udp://1.1.1.1:53" },
        { name: "a", url: "udp://8.8.8.8:53" },
      ),
    ).toBe(false);
  });
});
