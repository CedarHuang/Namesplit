import type { LocalRecordConfig } from "../../ipc/types";
import type { MessageKey } from "../../i18n";

export interface RecordIssue {
  level: "error" | "warning";
  /** 文案 key；由页面按当前语言取值。 */
  key: MessageKey;
}

/** 列表行身份：内容键 + 同内容出现序号。不能用数组下标，增删后它会指向别的记录。 */
export interface KeyedRecord {
  key: string;
  record: LocalRecordConfig;
}

function nameKey(name: string): string {
  return name.trim().toLowerCase().replace(/\.$/, "");
}

/** 记录的内容身份；同内容记录按出现顺序追加序号，保证任意时刻 key 唯一。 */
export function recordContentKey(record: LocalRecordConfig): string {
  return [
    record.type,
    nameKey(record.name),
    record.value.trim(),
    record.ttl ?? 60,
    record.enabled ?? true,
  ].join("|");
}

/**
 * 为每条记录生成稳定 key。
 *
 * key 只由记录内容与同内容出现顺序决定，因此删除其他行不会改变本行 key；
 * 编辑内容后 key 变化属于预期（新内容就是新身份）。
 */
export function keyedRecords(records: LocalRecordConfig[]): KeyedRecord[] {
  const occurrences = new Map<string, number>();
  return records.map((record) => {
    const base = recordContentKey(record);
    const occurrence = (occurrences.get(base) ?? 0) + 1;
    occurrences.set(base, occurrence);
    return { key: `${base}#${occurrence}`, record };
  });
}

/** 按 key 替换记录；key 已不存在时返回原数组。 */
export function replaceByKey(
  records: LocalRecordConfig[],
  key: string,
  next: LocalRecordConfig,
): LocalRecordConfig[] {
  const target = keyedRecords(records).findIndex((item) => item.key === key);
  if (target < 0) {
    return records;
  }
  return records.map((record, index) => (index === target ? next : record));
}

/** 按 key 集合删除记录。 */
export function removeByKeys(
  records: LocalRecordConfig[],
  keys: ReadonlySet<string>,
): LocalRecordConfig[] {
  if (keys.size === 0) {
    return records;
  }
  const keyed = keyedRecords(records);
  return records.filter((_record, index) => !keys.has(keyed[index].key));
}

/** 按 key 删除单条记录。 */
export function removeByKey(
  records: LocalRecordConfig[],
  key: string,
): LocalRecordConfig[] {
  return removeByKeys(records, new Set([key]));
}

/** 按 key 集合修改记录（启停等批量操作）。 */
export function updateByKeys(
  records: LocalRecordConfig[],
  keys: ReadonlySet<string>,
  change: (record: LocalRecordConfig) => LocalRecordConfig,
): LocalRecordConfig[] {
  if (keys.size === 0) {
    return records;
  }
  const keyed = keyedRecords(records);
  return records.map((record, index) =>
    keys.has(keyed[index].key) ? change(record) : record,
  );
}

/** 按 key 修改单条记录。 */
export function updateByKey(
  records: LocalRecordConfig[],
  key: string,
  change: (record: LocalRecordConfig) => LocalRecordConfig,
): LocalRecordConfig[] {
  return updateByKeys(records, new Set([key]), change);
}

/** 逐行标记重复、CNAME 冲突与同 RRset TTL 不一致；服务端保存时仍会再校验。 */
export function recordIssues(records: LocalRecordConfig[]): RecordIssue[][] {
  const issues: RecordIssue[][] = records.map(() => []);
  const exact = new Map<string, number[]>();
  const byName = new Map<string, number[]>();
  const byNameType = new Map<string, number[]>();

  records.forEach((record, index) => {
    const name = nameKey(record.name);
    exact.set(`${record.type}|${name}|${record.value}`, [
      ...(exact.get(`${record.type}|${name}|${record.value}`) ?? []),
      index,
    ]);
    byName.set(name, [...(byName.get(name) ?? []), index]);
    const nameTypeKey = `${name}|${record.type}`;
    byNameType.set(nameTypeKey, [...(byNameType.get(nameTypeKey) ?? []), index]);
  });

  for (const indexes of exact.values()) {
    if (indexes.length > 1) {
      for (const index of indexes) {
        issues[index].push({ level: "warning", key: "local.issue.duplicate" });
      }
    }
  }

  for (const indexes of byName.values()) {
    const hasCname = indexes.some((index) => records[index].type === "CNAME");
    const hasOther = indexes.some((index) => records[index].type !== "CNAME");
    if (hasCname && hasOther) {
      for (const index of indexes) {
        issues[index].push({
          level: "error",
          key: "local.issue.cnameConflict",
        });
      }
    }
  }

  for (const indexes of byNameType.values()) {
    const ttls = new Set(indexes.map((index) => records[index].ttl ?? 60));
    if (ttls.size > 1) {
      for (const index of indexes) {
        issues[index].push({
          level: "warning",
          key: "local.issue.ttlMismatch",
        });
      }
    }
  }

  return issues;
}

export function filterRecords(
  records: LocalRecordConfig[],
  keyword: string,
): LocalRecordConfig[] {
  const query = keyword.trim().toLowerCase();
  if (!query) {
    return records;
  }
  return records.filter(
    (record) =>
      record.name.toLowerCase().includes(query) ||
      record.value.toLowerCase().includes(query),
  );
}
