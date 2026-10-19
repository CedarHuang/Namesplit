import { describe, expect, it } from "vitest";

import type { RuleConfig, ValidationDiagnostic } from "../../ipc/types";
import { moveRule, warningsForRule } from "./rules";

const rules: RuleConfig[] = [
  { match: "DOMAIN", domain: "a.test", group: "default" },
  { match: "DOMAIN-SUFFIX", domain: "b.test", group: "default" },
  { match: "DOMAIN-SUFFIX", domain: "c.test", group: "default" },
];

describe("rule helpers", () => {
  it("向下移动保持其余顺序", () => {
    expect(moveRule(rules, 0, 2).map((item) => item.domain)).toEqual([
      "b.test",
      "c.test",
      "a.test",
    ]);
  });

  it("越界或原地移动返回原数组", () => {
    expect(moveRule(rules, 1, 1)).toBe(rules);
    expect(moveRule(rules, -1, 1)).toBe(rules);
    expect(moveRule(rules, 0, 9)).toBe(rules);
  });

  it("按规则索引匹配警告", () => {
    const warnings: ValidationDiagnostic[] = [
      { path: "rules[1]", code: "shadowed_rule", severity: "warning", message: "被遮蔽" },
      { path: "rules[1].group", code: "missing_group", severity: "error", message: "缺组" },
      { path: "rules[2]", code: "duplicate_rule", severity: "warning", message: "重复" },
    ];
    expect(warningsForRule(warnings, 1).map((item) => item.code)).toEqual([
      "shadowed_rule",
      "missing_group",
    ]);
    expect(warningsForRule(warnings, 0)).toEqual([]);
  });
});
