import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";

import {
  getAppInfo,
  getConfig,
  getSidecarState,
  subscribeEvents,
  unsubscribeEvents,
} from "../ipc/commands";
import { ipcErrorMessage } from "../ipc/errors";
import type {
  AppInfo,
  QueryEvent,
  SidecarStatePayload,
} from "../ipc/types";
import type { PortStatus } from "../portStatus";
import { getPortStatus, isTauri } from "../portStatus";
import type { SystemDnsStatus } from "../systemDns";
import { getSystemDns } from "../systemDns";
import { EventStore } from "./eventStore";

interface DroppedEventsPayload {
  dropped_batches: number;
  dropped_events: number;
}

interface AppStateValue {
  appInfo: AppInfo | null;
  sidecar: SidecarStatePayload | null;
  systemDns: SystemDnsStatus | null;
  systemDnsError: string | null;
  portStatus: PortStatus | null;
  portError: string | null;
  configRevision: number | null;
  configError: string | null;
  eventsError: string | null;
  eventsStale: boolean;
  logsClearSignal: number;
  eventStore: EventStore;
  refreshPortStatus: () => Promise<void>;
  refreshSystemDns: () => Promise<void>;
  refreshConfig: () => Promise<void>;
  refreshSidecarState: () => Promise<void>;
  retryEvents: () => Promise<void>;
}

const AppStateContext = createContext<AppStateValue | null>(null);

export function AppStateProvider({ children }: { children: ReactNode }) {
  const eventStoreRef = useRef<EventStore | null>(null);
  if (!eventStoreRef.current) {
    eventStoreRef.current = new EventStore();
  }
  const eventStore = eventStoreRef.current;
  const expectSnapshot = useRef(true);
  const previousSidecarState = useRef<SidecarStatePayload["state"] | null>(null);

  const [appInfo, setAppInfo] = useState<AppInfo | null>(null);
  const [sidecar, setSidecar] = useState<SidecarStatePayload | null>(null);
  const [systemDns, setSystemDns] = useState<SystemDnsStatus | null>(null);
  const [systemDnsError, setSystemDnsError] = useState<string | null>(null);
  const [portStatus, setPortStatus] = useState<PortStatus | null>(null);
  const [portError, setPortError] = useState<string | null>(null);
  const [configRevision, setConfigRevision] = useState<number | null>(null);
  const [configError, setConfigError] = useState<string | null>(null);
  const [eventsError, setEventsError] = useState<string | null>(null);
  const [eventsStale, setEventsStale] = useState(false);
  const [logsClearSignal, setLogsClearSignal] = useState(0);

  const refreshPortStatus = useCallback(async () => {
    try {
      setPortStatus(await getPortStatus());
      setPortError(null);
    } catch (error) {
      setPortError(ipcErrorMessage(error));
    }
  }, []);

  const refreshSystemDns = useCallback(async () => {
    try {
      setSystemDns(await getSystemDns());
      setSystemDnsError(null);
    } catch (error) {
      setSystemDnsError(ipcErrorMessage(error));
    }
  }, []);

  const refreshConfig = useCallback(async () => {
    try {
      const envelope = await getConfig();
      setConfigRevision(envelope.revision);
      setConfigError(envelope.config_error?.message ?? null);
    } catch (error) {
      const message = ipcErrorMessage(error);
      setConfigError(message);
    }
  }, []);

  const refreshSidecarState = useCallback(async () => {
    try {
      const state = await getSidecarState();
      if (!state) {
        return;
      }
      setSidecar(state);
      // 首次读取用于对齐当前状态；否则随后的 running 事件会被误判成进程重启。
      if (previousSidecarState.current === null) {
        previousSidecarState.current = state.state;
      }
    } catch {
      // 状态读取失败时保留已知状态；生命周期事件到达后会再次更新。
    }
  }, []);

  const retryEvents = useCallback(async () => {
    setEventsStale(true);
    expectSnapshot.current = true;
    try {
      await subscribeEvents();
      setEventsError(null);
    } catch (error) {
      setEventsError(ipcErrorMessage(error));
    }
  }, []);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }
    void refreshSystemDns();
    void refreshPortStatus();
    void refreshConfig();
    void refreshSidecarState();
    getAppInfo()
      .then((info) => setAppInfo(info))
      .catch(() => undefined);
  }, [refreshConfig, refreshPortStatus, refreshSidecarState, refreshSystemDns]);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }

    let disposed = false;
    let unlistenSidecar: (() => void) | undefined;
    let unlistenDns: (() => void) | undefined;
    let unlistenEvents: (() => void) | undefined;
    let unlistenDropped: (() => void) | undefined;
    let unlistenClear: (() => void) | undefined;

    // 批次合并窗口。重启瞬间窗口里可能还有旧进程的批次，必须能在这里丢弃。
    let pending: QueryEvent[] = [];
    let flushTimer: number | undefined;

    void listen<SidecarStatePayload>("sidecar://state", (event) => {
      if (disposed) {
        return;
      }
      const payload = event.payload;
      const previous = previousSidecarState.current;
      previousSidecarState.current = payload.state;
      setSidecar(payload);
      void refreshPortStatus();

      if (payload.state === "running") {
        if (previous !== null && previous !== "running") {
          // sidecar 进程重启后 event_id 会从头开始；必须先清空旧缓冲与去重集合，
          // 否则新事件会被旧 id 当成重复丢弃。合并窗口里的旧批次同样要丢掉，
          // 否则它们会抢先占据快照位置并占用新进程的 event_id。
          eventStore.clear();
          pending = [];
          if (flushTimer !== undefined) {
            window.clearTimeout(flushTimer);
            flushTimer = undefined;
          }
        }
        // 订阅（或重启重订阅）后的首批数据是环形缓冲快照，需要整体替换本地缓冲。
        expectSnapshot.current = true;
        if (document.visibilityState === "visible") {
          void subscribeEvents().catch((error) => setEventsError(ipcErrorMessage(error)));
        }
        void refreshConfig();
      }
    }).then((stop) => {
      if (disposed) {
        stop();
      } else {
        unlistenSidecar = stop;
      }
    });

    // 事件批次按 150ms 合并后再进入 React，避免高 QPS 下渲染频率超过主线程能力。
    const flush = () => {
      flushTimer = undefined;
      if (pending.length === 0) {
        return;
      }
      const batch = pending;
      pending = [];
      if (expectSnapshot.current) {
        expectSnapshot.current = false;
        eventStore.replace(batch);
      } else {
        eventStore.append(batch);
      }
      // 快照或实时批次已落盘，说明与 sidecar 的事件流重新对齐。
      setEventsStale(false);
      setEventsError(null);
    };

    // 隐藏时停止事件流，避免无窗口渲染时继续解析与缓冲；恢复可见后重新订阅拿快照。
    const onVisibilityChange = () => {
      if (document.visibilityState === "hidden") {
        pending = [];
        if (flushTimer !== undefined) {
          window.clearTimeout(flushTimer);
          flushTimer = undefined;
        }
        void unsubscribeEvents().catch(() => undefined);
        return;
      }
      expectSnapshot.current = true;
      void subscribeEvents().catch((error) => setEventsError(ipcErrorMessage(error)));
    };
    document.addEventListener("visibilitychange", onVisibilityChange);

    void listen<QueryEvent[]>("query://events", (event) => {
      if (disposed) {
        return;
      }
      pending.push(...event.payload);
      if (flushTimer === undefined) {
        flushTimer = window.setTimeout(flush, 150);
      }
    }).then((stop) => {
      if (disposed) {
        stop();
      } else {
        unlistenEvents = stop;
      }
    });

    // 中继队列丢弃批次后，UI 只能靠重新订阅拿到环形缓冲快照补齐。
    void listen<DroppedEventsPayload>("query://dropped", () => {
      if (disposed) {
        return;
      }
      setEventsStale(true);
      // 窗口隐藏时订阅意图已被清除，此处重建事件流会把隐藏期间的暂停语义破坏掉；
      // 恢复可见时的订阅会重新拿快照并对齐。
      if (document.visibilityState !== "visible") {
        return;
      }
      // 快照已在途中时无需重建事件流，避免连续丢批反复重启中继。
      if (expectSnapshot.current) {
        return;
      }
      expectSnapshot.current = true;
      void subscribeEvents().catch((error) => setEventsError(ipcErrorMessage(error)));
    }).then((stop) => {
      if (disposed) {
        stop();
      } else {
        unlistenDropped = stop;
      }
    });

    void listen<SystemDnsStatus>("system_dns://changed", (event) => {
      if (!disposed) {
        setSystemDns(event.payload);
        setSystemDnsError(null);
      }
    }).then((stop) => {
      if (disposed) {
        stop();
      } else {
        unlistenDns = stop;
      }
    });

    void listen("logs://clear", () => {
      if (!disposed) {
        setLogsClearSignal((current) => current + 1);
      }
    }).then((stop) => {
      if (disposed) {
        stop();
      } else {
        unlistenClear = stop;
      }
    });

    const onFocus = () => void refreshSystemDns();
    window.addEventListener("focus", onFocus);

    if (document.visibilityState === "visible") {
      expectSnapshot.current = true;
      void subscribeEvents().catch((error) => setEventsError(ipcErrorMessage(error)));
    }

    return () => {
      disposed = true;
      if (flushTimer !== undefined) {
        window.clearTimeout(flushTimer);
      }
      document.removeEventListener("visibilitychange", onVisibilityChange);
      unlistenSidecar?.();
      unlistenDns?.();
      unlistenEvents?.();
      unlistenDropped?.();
      unlistenClear?.();
      window.removeEventListener("focus", onFocus);
      // 事件流跟随应用进程生命周期；开发模式下 StrictMode 会卸载再挂载，
      // 在清理阶段取消订阅会把刚建立的订阅一起停掉。
    };
  }, [eventStore, refreshConfig, refreshPortStatus, refreshSystemDns]);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }
    const timer = window.setInterval(() => {
      void invoke("ui_heartbeat").catch(() => undefined);
    }, 3_000);
    void invoke("ui_heartbeat").catch(() => undefined);
    return () => window.clearInterval(timer);
  }, []);

  const value = useMemo<AppStateValue>(
    () => ({
      appInfo,
      sidecar,
      systemDns,
      systemDnsError,
      portStatus,
      portError,
      configRevision,
      configError,
      eventsError,
      eventsStale,
      logsClearSignal,
      eventStore,
      refreshPortStatus,
      refreshSystemDns,
      refreshConfig,
      refreshSidecarState,
      retryEvents,
    }),
    [
      appInfo,
      sidecar,
      systemDns,
      systemDnsError,
      portStatus,
      portError,
      configRevision,
      configError,
      eventsError,
      eventsStale,
      logsClearSignal,
      eventStore,
      refreshPortStatus,
      refreshSystemDns,
      refreshConfig,
      refreshSidecarState,
      retryEvents,
    ],
  );

  return <AppStateContext.Provider value={value}>{children}</AppStateContext.Provider>;
}

export function useAppState(): AppStateValue {
  const value = useContext(AppStateContext);
  if (!value) {
    throw new Error("useAppState 必须在 AppStateProvider 内使用");
  }
  return value;
}

/** 订阅实时事件；事件批次到达时组件重渲染，直接从 store 读取当前数组。 */
export function useEvents(): readonly QueryEvent[] {
  const { eventStore } = useAppState();
  const version = useSyncExternalStore(
    eventStore.subscribe,
    eventStore.getVersion,
    eventStore.getVersion,
  );
  void version;
  return eventStore.getEvents();
}
