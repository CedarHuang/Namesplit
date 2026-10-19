const ID_PATTERN = /^[a-z0-9][a-z0-9_-]{0,31}$/;

/**
 * 上游/组 ID 只允许小写字母、数字、下划线与短横线，且必须以字母或数字开头、最长 32 字符。
 *
 * UI 侧先做同样的格式判断，避免把必然被配置校验拒绝的 ID 交给保存请求。
 */
export function isValidUpstreamId(id: string): boolean {
  return ID_PATTERN.test(id);
}

export type IdIssue = "invalid" | "exists" | null;

/**
 * 新增时的 ID 校验；编辑态 ID 不可改，不需要做存在性检查。
 *
 * `existingIds` 由调用方传入当前配置的 ID 集合，避免静默覆盖既有定义。
 */
export function idIssue(id: string, existingIds: Iterable<string>): IdIssue {
  if (!isValidUpstreamId(id)) {
    return "invalid";
  }
  for (const existing of existingIds) {
    if (existing === id) {
      return "exists";
    }
  }
  return null;
}
