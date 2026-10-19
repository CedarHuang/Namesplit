import { describe, expect, it } from "vitest";

import { idIssue, isValidUpstreamId } from "./validation";

describe("upstream id validation", () => {
  it("接受合法 ID", () => {
    for (const id of ["a", "cloudflare", "corp_1", "a-b", "0dns", "x".repeat(32)]) {
      expect(isValidUpstreamId(id)).toBe(true);
    }
  });

  it("拒绝非法 ID", () => {
    for (const id of ["", "UPPER", "-a", "_a", "a.b", "a b", "中文", "x".repeat(33)]) {
      expect(isValidUpstreamId(id)).toBe(false);
    }
  });

  it("区分格式错误与重复", () => {
    expect(idIssue("BAD", ["cloudflare"])).toBe("invalid");
    expect(idIssue("cloudflare", ["cloudflare", "quad9"])).toBe("exists");
    expect(idIssue("corp", ["cloudflare", "quad9"])).toBeNull();
    expect(idIssue("corp", [])).toBeNull();
  });
});
