import { describe, expect, it } from "vitest";

import type { QueryEvent } from "../../ipc/types";
import { filterEvents, matchesFilter, toQueryFilter } from "./filter";

function event(overrides: Partial<QueryEvent> = {}): QueryEvent {
  return {
    event_id: 1,
    time: "2026-10-19T00:00:00Z",
    domain: "api.corp.local.",
    qtype: "A",
    rule: "corp.local",
    rule_index: 0,
    rule_kind: "suffix",
    source: "upstream",
    group: "corp",
    upstream: "tls://10.0.0.53",
    upstream_name: "公司内网",
    duration_ms: 12,
    rcode: 0,
    rcode_name: "NOERROR",
    protocol: "udp",
    answers: [],
    authority: [],
    additional: [],
    ttl: 60,
    cache_hit: false,
    do: false,
    ad: false,
    request_flags: { rd: true, cd: false },
    response_flags: { aa: false, tc: false, ra: true },
    ...overrides,
  };
}

describe("log filters", () => {
  it("域名子串大小写不敏感", () => {
    expect(matchesFilter(event(), { domain: "CORP" })).toBe(true);
    expect(matchesFilter(event(), { domain: "other" })).toBe(false);
  });

  it("组合过滤", () => {
    const rows = [
      event({ event_id: 1 }),
      event({ event_id: 2, source: "cache", cache_hit: true, duration_ms: 1 }),
      event({ event_id: 3, rcode: 3, rcode_name: "NXDOMAIN" }),
    ];
    expect(
      filterEvents(rows, { domain: "", source: "cache", maxDuration: 5 }).map(
        (item) => item.event_id,
      ),
    ).toEqual([2]);
    expect(filterEvents(rows, { domain: "", rcode: 3 }).map((item) => item.event_id)).toEqual([3]);
  });

  it("历史过滤去掉空字段", () => {
    expect(toQueryFilter({ domain: "", minDuration: 10 })).toEqual({
      domain: undefined,
      qtype: undefined,
      source: undefined,
      upstream: undefined,
      rule: undefined,
      rcode: undefined,
      min_duration_ms: 10,
      max_duration_ms: undefined,
    });
  });
});
