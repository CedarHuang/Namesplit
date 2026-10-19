import { describe, expect, it } from "vitest";

import { EventStore } from "./eventStore";

function event(id: number, domain = `d${id}.test.`) {
  return {
    event_id: id,
    time: "2026-10-19T00:00:00Z",
    domain,
    qtype: "A",
    rule: "",
    rule_index: -1,
    rule_kind: "default",
    source: "upstream" as const,
    group: "default",
    upstream: "udp://1.1.1.1:53",
    upstream_name: "Cloudflare",
    duration_ms: 1,
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
  };
}

function ids(store: EventStore): number[] {
  return store.getEvents().map((item) => item.event_id);
}

describe("EventStore", () => {
  it("按 event_id 去重", () => {
    const store = new EventStore();
    store.append([event(1), event(2), event(1)]);
    expect(ids(store)).toEqual([1, 2]);
  });

  it("快照替换旧缓冲", () => {
    const store = new EventStore();
    store.append([event(1), event(2)]);
    store.replace([event(3)]);
    expect(ids(store)).toEqual([3]);
  });

  it("批次内乱序时按 event_id 排序后写入", () => {
    const store = new EventStore();
    store.append([event(3), event(1), event(2)]);
    expect(ids(store)).toEqual([1, 2, 3]);
  });

  it("迟到旧事件插入正确位置而不是落到队尾", () => {
    const store = new EventStore();
    store.append([event(4), event(5)]);
    store.append([event(2), event(3)]);
    expect(ids(store)).toEqual([2, 3, 4, 5]);
  });

  it("跨批去重后仍保持升序，且不触发重复通知", () => {
    const store = new EventStore();
    let calls = 0;
    store.subscribe(() => {
      calls += 1;
    });
    store.append([event(2), event(4)]);
    store.append([event(1), event(2), event(3)]);
    expect(ids(store)).toEqual([1, 2, 3, 4]);
    expect(calls).toBe(2);
  });

  it("快照替换时排序并去重", () => {
    const store = new EventStore();
    store.replace([event(3), event(1), event(2), event(2)]);
    expect(ids(store)).toEqual([1, 2, 3]);
  });

  it("超过上限时丢弃 id 最小的事件并释放去重集合", () => {
    const store = new EventStore(2);
    store.append([event(1), event(2), event(3)]);
    expect(ids(store)).toEqual([2, 3]);
    // 释放后旧 id 可重新进入，但它仍是最旧的一条，会立即被上限裁剪掉。
    store.append([event(1)]);
    expect(ids(store)).toEqual([2, 3]);
    // 被裁剪的 id 不阻塞后续新事件。
    store.append([event(4)]);
    expect(ids(store)).toEqual([3, 4]);
  });

  it("空批次不写缓冲、不通知", () => {
    const store = new EventStore();
    let calls = 0;
    store.subscribe(() => {
      calls += 1;
    });
    store.append([]);
    expect(ids(store)).toEqual([]);
    expect(calls).toBe(0);
  });

  it("空快照清空缓冲并通知", () => {
    const store = new EventStore();
    store.append([event(1)]);
    let calls = 0;
    store.subscribe(() => {
      calls += 1;
    });
    store.replace([]);
    expect(ids(store)).toEqual([]);
    expect(calls).toBe(1);
  });

  it("重复批次不触发订阅", () => {
    const store = new EventStore();
    let calls = 0;
    store.subscribe(() => {
      calls += 1;
    });
    store.append([event(1)]);
    store.append([event(1)]);
    expect(calls).toBe(1);
  });
});
