import type { QueryEvent, QueryFilter } from "../../ipc/types";

export interface LogFilter {
  domain: string;
  qtype?: string;
  source?: string;
  upstream?: string;
  rule?: string;
  rcode?: number;
  minDuration?: number;
  maxDuration?: number;
}

export const EMPTY_FILTER: LogFilter = { domain: "" };

export function matchesFilter(event: QueryEvent, filter: LogFilter): boolean {
  if (
    filter.domain &&
    !event.domain.toLowerCase().includes(filter.domain.trim().toLowerCase())
  ) {
    return false;
  }
  if (filter.qtype && event.qtype.toUpperCase() !== filter.qtype.toUpperCase()) {
    return false;
  }
  if (filter.source && event.source !== filter.source) {
    return false;
  }
  if (filter.upstream && event.upstream !== filter.upstream) {
    return false;
  }
  if (filter.rule && event.rule !== filter.rule) {
    return false;
  }
  if (filter.rcode !== undefined && event.rcode !== filter.rcode) {
    return false;
  }
  if (filter.minDuration !== undefined && event.duration_ms < filter.minDuration) {
    return false;
  }
  if (filter.maxDuration !== undefined && event.duration_ms > filter.maxDuration) {
    return false;
  }
  return true;
}

export function filterEvents(events: readonly QueryEvent[], filter: LogFilter): QueryEvent[] {
  return events.filter((event) => matchesFilter(event, filter));
}

/** 历史查询走 sidecar 侧过滤，字段名与实时过滤保持一致。 */
export function toQueryFilter(filter: LogFilter): QueryFilter {
  return {
    domain: filter.domain || undefined,
    qtype: filter.qtype || undefined,
    source: filter.source || undefined,
    upstream: filter.upstream || undefined,
    rule: filter.rule || undefined,
    rcode: filter.rcode,
    min_duration_ms: filter.minDuration,
    max_duration_ms: filter.maxDuration,
  };
}
