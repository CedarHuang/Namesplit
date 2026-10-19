import type { QueryEvent } from "../ipc/types";

export const EVENT_BUFFER_LIMIT = 5000;

interface Listener {
  (): void;
}

/**
 * 实时事件缓冲：按 event_id 去重、升序排列、限制条目数、快照替换。
 *
 * 中继内部断线重连时 sidecar 会补发环形缓冲快照，批次不带快照标记，
 * 旧事件可能晚于新事件到达，因此所有写入都必须按 event_id 有序合并，
 * 不能假设到达顺序就是时间顺序。
 *
 * sidecar 重启后 event_id 会从头开始，因此收到新快照或重启事件时必须 clear，
 * 否则旧 id 会误伤新事件。
 */
export class EventStore {
  private events: QueryEvent[] = [];
  private ids = new Set<number>();
  private listeners = new Set<Listener>();
  private version = 0;

  constructor(private readonly limit = EVENT_BUFFER_LIMIT) {}

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getVersion = (): number => this.version;

  getEvents = (): readonly QueryEvent[] => this.events;

  /** 用快照替换本地缓冲；快照本身可能乱序，写入前统一升序并去重。 */
  replace(batch: QueryEvent[]): void {
    this.events = [];
    this.ids.clear();
    const accepted = this.sortedUnique(batch);
    for (const event of accepted) {
      this.ids.add(event.event_id);
    }
    this.events = accepted;
    this.trim();
    this.notify();
  }

  append(batch: QueryEvent[]): void {
    if (batch.length === 0) {
      return;
    }
    if (this.merge(batch)) {
      this.notify();
    }
  }

  clear(): void {
    const hadEvents = this.events.length > 0;
    this.events = [];
    this.ids.clear();
    if (hadEvents) {
      this.notify();
    }
  }

  /**
   * 去重后按 event_id 升序合并；返回是否实际写入。
   *
   * 批次已有序且首 id 大于当前最大 id 时直接追加（常态下均摊 O(n)）；
   * 否则做一次稳定归并（O(n+k)），保证迟到旧事件插入正确位置而不是落到队尾。
   */
  private merge(batch: QueryEvent[]): boolean {
    const accepted = this.sortedUnique(batch, this.ids);
    if (accepted.length === 0) {
      return false;
    }

    const currentMax =
      this.events.length === 0 ? Number.NEGATIVE_INFINITY : this.events[this.events.length - 1].event_id;
    // 新数组：消费方用引用比较判断是否需要重算过滤结果，不能在原数组上追加。
    if (accepted[0].event_id > currentMax) {
      this.events = this.events.concat(accepted);
    } else {
      this.events = mergeSorted(this.events, accepted);
    }
    for (const event of accepted) {
      this.ids.add(event.event_id);
    }
    this.trim();
    return true;
  }

  /** 批次内去重 + 跨批去重（skip）+ 升序；乱序批次只排序一次。 */
  private sortedUnique(batch: readonly QueryEvent[], skip?: Set<number>): QueryEvent[] {
    const accepted: QueryEvent[] = [];
    const seen = new Set<number>();
    for (const event of batch) {
      const id = event.event_id;
      if (seen.has(id) || skip?.has(id)) {
        continue;
      }
      seen.add(id);
      accepted.push(event);
    }
    for (let index = 1; index < accepted.length; index += 1) {
      if (accepted[index].event_id < accepted[index - 1].event_id) {
        accepted.sort((left, right) => left.event_id - right.event_id);
        break;
      }
    }
    return accepted;
  }

  /** 超出上限时丢弃 id 最小（最旧）的事件，并从去重集合中释放。 */
  private trim(): void {
    const overflow = this.events.length - this.limit;
    if (overflow <= 0) {
      return;
    }
    const dropped = this.events.slice(0, overflow);
    this.events = this.events.slice(overflow);
    for (const event of dropped) {
      this.ids.delete(event.event_id);
    }
  }

  private notify(): void {
    this.version += 1;
    for (const listener of this.listeners) {
      listener();
    }
  }
}

function mergeSorted(current: readonly QueryEvent[], incoming: readonly QueryEvent[]): QueryEvent[] {
  const merged: QueryEvent[] = [];
  let left = 0;
  let right = 0;
  while (left < current.length && right < incoming.length) {
    if (current[left].event_id <= incoming[right].event_id) {
      merged.push(current[left]);
      left += 1;
    } else {
      merged.push(incoming[right]);
      right += 1;
    }
  }
  while (left < current.length) {
    merged.push(current[left]);
    left += 1;
  }
  while (right < incoming.length) {
    merged.push(incoming[right]);
    right += 1;
  }
  return merged;
}
