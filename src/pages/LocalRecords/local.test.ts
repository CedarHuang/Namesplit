import { describe, expect, it } from "vitest";

import type { LocalRecordConfig } from "../../ipc/types";
import {
  filterRecords,
  keyedRecords,
  recordIssues,
  removeByKey,
  removeByKeys,
  replaceByKey,
  updateByKey,
  updateByKeys,
} from "./local";

function record(overrides: Partial<LocalRecordConfig> = {}): LocalRecordConfig {
  return { type: "A", name: "nas.home", value: "192.168.1.10", ttl: 60, enabled: true, ...overrides };
}

function keys(records: LocalRecordConfig[]): string[] {
  return keyedRecords(records).map((item) => item.key);
}

describe("local record helpers", () => {
  it("标记完全重复记录", () => {
    const issues = recordIssues([record(), record()]);
    expect(issues[0].some((item) => item.key === "local.issue.duplicate")).toBe(true);
    expect(issues[1].some((item) => item.key === "local.issue.duplicate")).toBe(true);
  });

  it("标记 CNAME 与其他类型同名", () => {
    const issues = recordIssues([
      record({ type: "CNAME", value: "nas.home." }),
      record({ type: "A" }),
    ]);
    expect(issues[0].some((item) => item.level === "error")).toBe(true);
    expect(issues[1].some((item) => item.level === "error")).toBe(true);
    expect(issues[0][0].key).toBe("local.issue.cnameConflict");
  });

  it("标记同 RRset TTL 不一致", () => {
    const issues = recordIssues([
      record({ value: "192.168.1.10", ttl: 60 }),
      record({ value: "192.168.1.11", ttl: 120 }),
    ]);
    expect(issues[0].some((item) => item.key === "local.issue.ttlMismatch")).toBe(true);
  });

  it("按名字或值过滤", () => {
    const rows = [record(), record({ name: "dev.local", value: "10.0.0.2" })];
    expect(filterRecords(rows, "nas")).toHaveLength(1);
    expect(filterRecords(rows, "10.0.0.2")).toHaveLength(1);
    expect(filterRecords(rows, "")).toBe(rows);
  });
});

describe("local record keys", () => {
  it("删除中间行后其他行 key 不变", () => {
    const rows = [
      record({ name: "a.home", value: "10.0.0.1" }),
      record({ name: "b.home", value: "10.0.0.2" }),
      record({ name: "c.home", value: "10.0.0.3" }),
    ];
    const before = keys(rows);
    const after = keys(removeByKeys(rows, new Set([before[1]])));
    expect(after).toEqual([before[0], before[2]]);
  });

  it("完全重复的记录获得不同 key", () => {
    const unique = new Set(keys([record(), record()]));
    expect(unique.size).toBe(2);
  });

  it("按 key 替换命中正确记录", () => {
    const rows = [
      record({ name: "a.home", value: "10.0.0.1" }),
      record({ name: "b.home", value: "10.0.0.2" }),
    ];
    const before = keys(rows);
    const replaced = replaceByKey(rows, before[0], {
      type: "A",
      name: "a.home",
      value: "10.0.0.9",
      ttl: 60,
      enabled: true,
    });
    expect(replaced[0].value).toBe("10.0.0.9");
    expect(replaced[1]).toBe(rows[1]);
  });

  it("按 key 集合批量启停只作用于选中项", () => {
    const rows = [
      record({ name: "a.home", value: "10.0.0.1" }),
      record({ name: "b.home", value: "10.0.0.2" }),
      record({ name: "c.home", value: "10.0.0.3" }),
    ];
    const before = keys(rows);
    const updated = updateByKeys(rows, new Set([before[0], before[2]]), (item) => ({
      ...item,
      enabled: false,
    }));
    expect(updated.map((item) => item.enabled)).toEqual([false, true, false]);
  });

  it("批量删除只删除选中项，其余记录 key 不变", () => {
    const rows = [
      record({ name: "a.home", value: "10.0.0.1" }),
      record({ name: "b.home", value: "10.0.0.2" }),
      record({ name: "c.home", value: "10.0.0.3" }),
    ];
    const before = keys(rows);
    const remaining = removeByKeys(rows, new Set([before[1]]));
    expect(remaining.map((item) => item.name)).toEqual(["a.home", "c.home"]);
    expect(keys(remaining)).toEqual([before[0], before[2]]);
  });

  it("按 key 启停单条记录", () => {
    const rows = [
      record({ name: "a.home", value: "10.0.0.1" }),
      record({ name: "b.home", value: "10.0.0.2" }),
    ];
    const before = keys(rows);
    expect(
      updateByKey(rows, before[1], (item) => ({ ...item, enabled: false })).map(
        (item) => item.enabled,
      ),
    ).toEqual([true, false]);
    expect(removeByKey(rows, before[0]).map((item) => item.name)).toEqual(["b.home"]);
  });

  it("key 已不存在时替换/删除保持原内容", () => {
    const rows = [record()];
    const missing = "A|ghost.home|1.1.1.1|60|true#1";
    expect(replaceByKey(rows, missing, record())).toBe(rows);
    expect(removeByKeys(rows, new Set([missing]))).toEqual(rows);
  });
});
