import { describe, expect, it } from "vitest";

import { appendPoint, percentile, ratio, startOfLocalDay, sumToday } from "./metrics";

describe("overview metrics", () => {
  it("分位数按线性插值", () => {
    expect(percentile([], 0.5)).toBeNull();
    expect(percentile([10], 0.95)).toBe(10);
    expect(percentile([0, 10], 0.5)).toBe(5);
    expect(percentile([0, 10, 20], 0.5)).toBe(10);
  });

  it("比率为零分母时返回 null", () => {
    expect(ratio(1, 0)).toBeNull();
    expect(ratio(3, 4)).toBe(0.75);
  });

  it("实时序列限制长度", () => {
    const point = (qps: number) => ({ label: "t", qps, p50: null, p95: null });
    expect(appendPoint([point(1), point(2)], point(3), 2).map((item) => item.qps)).toEqual([
      2, 3,
    ]);
  });

  it("今日统计只累计本地零点之后", () => {
    const now = new Date(2026, 9, 19, 12, 0, 0);
    const before = new Date(2026, 9, 18, 23, 59, 0).toISOString();
    const after = new Date(2026, 9, 19, 0, 1, 0).toISOString();
    const totals = sumToday(
      [
        { minute: before, queries: 5, failures: 1, cache_hits: 2 },
        { minute: after, queries: 7, failures: 2, cache_hits: 3 },
      ],
      now,
    );
    expect(totals).toEqual({ queries: 7, failures: 2, cacheHits: 3 });
    expect(startOfLocalDay(now).getHours()).toBe(0);
  });
});
