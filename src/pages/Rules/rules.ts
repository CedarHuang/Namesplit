import type { RuleConfig, ValidationDiagnostic } from "../../ipc/types";

/** 返回新数组，避免拖拽排序时修改原配置对象。 */
export function moveRule(rules: RuleConfig[], from: number, to: number): RuleConfig[] {
  if (from === to || from < 0 || to < 0 || from >= rules.length || to >= rules.length) {
    return rules;
  }
  const next = [...rules];
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved);
  return next;
}

/** 配置警告的 path 使用 `rules[i]` / `rules[i].field`，按索引匹配到对应行。 */
export function warningsForRule(
  warnings: ValidationDiagnostic[],
  index: number,
): ValidationDiagnostic[] {
  const prefix = `rules[${index}]`;
  return warnings.filter(
    (warning) => warning.path === prefix || warning.path.startsWith(`${prefix}.`),
  );
}
