import type { MinuteStat } from "../../ipc/types";

export interface RealtimePoint {
  label: string;
  qps: number;
  p50: number | null;
  p95: number | null;
}

/** 最近 60 个采样点；超出后丢弃最旧数据。 */
export function appendPoint(
  points: RealtimePoint[],
  point: RealtimePoint,
  limit = 60,
): RealtimePoint[] {
  const next = [...points, point];
  return next.length > limit ? next.slice(next.length - limit) : next;
}

/** 线性插值分位数；空数组返回 null，避免图表把无流量显示成 0 延迟。 */
export function percentile(values: number[], ratio: number): number | null {
  if (values.length === 0) {
    return null;
  }
  const sorted = [...values].sort((left, right) => left - right);
  const position = (sorted.length - 1) * Math.min(Math.max(ratio, 0), 1);
  const lower = Math.floor(position);
  const upper = Math.ceil(position);
  if (lower === upper) {
    return sorted[lower];
  }
  const weight = position - lower;
  return sorted[lower] * (1 - weight) + sorted[upper] * weight;
}

export function ratio(part: number, total: number): number | null {
  return total > 0 ? part / total : null;
}

/** 本地日期零点；分钟聚合按 UTC 返回，统计“今日”必须按本地时区截断。 */
export function startOfLocalDay(now: Date): Date {
  return new Date(now.getFullYear(), now.getMonth(), now.getDate());
}

export function sumToday(items: MinuteStat[], now: Date): {
  queries: number;
  failures: number;
  cacheHits: number;
} {
  const since = startOfLocalDay(now).getTime();
  let queries = 0;
  let failures = 0;
  let cacheHits = 0;
  for (const item of items) {
    if (Date.parse(item.minute) < since) {
      continue;
    }
    queries += item.queries;
    failures += item.failures;
    cacheHits += item.cache_hits;
  }
  return { queries, failures, cacheHits };
}
