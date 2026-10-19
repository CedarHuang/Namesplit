import { zhCN } from "../../i18n/zh-CN";
import type { ConfigDocument, GroupConfig, UpstreamConfig } from "../../ipc/types";

export type TemplateKind = "udp" | "doh";

interface UpstreamTemplate {
  groupName: string;
  upstreams: Record<string, UpstreamConfig>;
}

/** 向导只提供"快速选一套公共上游"的模板，配置合并规则见 applyTemplate。 */
const TEMPLATES: Record<TemplateKind, UpstreamTemplate> = {
  udp: {
    groupName: "Public UDP",
    upstreams: {
      cloudflare: { name: "Cloudflare", url: "udp://1.1.1.1:53", timeout_ms: 5000 },
      quad9: { name: "Quad9", url: "udp://9.9.9.9:53", timeout_ms: 5000 },
    },
  },
  doh: {
    groupName: "Public DoH",
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
  },
};

const ID_PATTERN = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const MAX_ID_LENGTH = 32;

function normalizeBootstrap(bootstrap: string[] | undefined): string[] {
  return [...(bootstrap ?? [])]
    .map((item) => item.trim())
    .filter(Boolean)
    .sort();
}

/** 两个上游是否等价；用于判断模板上游可以直接复用而不是新增。 */
export function sameUpstream(left: UpstreamConfig, right: UpstreamConfig): boolean {
  return (
    left.name === right.name &&
    left.url === right.url &&
    (left.timeout_ms ?? 0) === (right.timeout_ms ?? 0) &&
    JSON.stringify(normalizeBootstrap(left.bootstrap)) ===
      JSON.stringify(normalizeBootstrap(right.bootstrap)) &&
    (left.remark ?? "") === (right.remark ?? "")
  );
}

function sameMembers(left: string[], right: string[]): boolean {
  if (left.length !== right.length) {
    return false;
  }
  const sortedLeft = [...left].sort();
  const sortedRight = [...right].sort();
  return sortedLeft.every((id, index) => id === sortedRight[index]);
}

/** 复用同定义的模板上游；ID 被不同定义占用时分配 `-2`、`-3`… 等空闲 ID。 */
function ensureUpstream(
  upstreams: Record<string, UpstreamConfig>,
  baseId: string,
  definition: UpstreamConfig,
): string {
  const existing = upstreams[baseId];
  if (existing === undefined) {
    upstreams[baseId] = { ...definition };
    return baseId;
  }
  if (sameUpstream(existing, definition)) {
    return baseId;
  }
  for (let suffix = 2; suffix <= 1000; suffix += 1) {
    const candidate = `${baseId}-${suffix}`.slice(0, MAX_ID_LENGTH);
    if (!ID_PATTERN.test(candidate)) {
      break;
    }
    const current = upstreams[candidate];
    if (current === undefined) {
      upstreams[candidate] = { ...definition };
      return candidate;
    }
    if (sameUpstream(current, definition)) {
      return candidate;
    }
  }
  throw new Error(zhCN["wizard.templateIdExhausted"]);
}

/** 复用名字与成员一致的组；否则用空闲 ID（优先 default）新建，不触碰已有组。 */
function ensureGroup(
  groups: Record<string, GroupConfig>,
  groupName: string,
  members: string[],
): string {
  for (const [id, group] of Object.entries(groups)) {
    if (group.name === groupName && sameMembers(group.upstreams, members)) {
      return id;
    }
  }
  let candidate = "default";
  let suffix = 2;
  while (groups[candidate] !== undefined) {
    candidate = `default-${suffix}`;
    suffix += 1;
  }
  groups[candidate] = { name: groupName, upstreams: [...members] };
  return candidate;
}

/**
 * 把模板合并进现有配置：只新增或复用上游、新增或复用组，并把默认组指向模板组。
 *
 * 任何情况下都不删除已有上游、组或规则，也不修改 `settings` 的其他字段；
 * 因此重复应用同一模板得到相同配置（幂等）。
 */
export function applyTemplate(config: ConfigDocument, kind: TemplateKind): ConfigDocument {
  const template = TEMPLATES[kind];
  const upstreams: Record<string, UpstreamConfig> = { ...config.upstreams };
  const members: string[] = [];
  for (const [baseId, definition] of Object.entries(template.upstreams)) {
    const id = ensureUpstream(upstreams, baseId, definition);
    if (!members.includes(id)) {
      members.push(id);
    }
  }

  const groups: Record<string, GroupConfig> = { ...config.groups };
  const groupId = ensureGroup(groups, template.groupName, members);

  return {
    ...config,
    upstreams,
    groups,
    settings: { ...config.settings, default_group: groupId },
  };
}
