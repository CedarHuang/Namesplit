import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  App,
  Button,
  Card,
  Descriptions,
  Drawer,
  Input,
  InputNumber,
  Popconfirm,
  Segmented,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from "antd";
import type { TableColumnsType, TableRef } from "antd";
import { useSearchParams } from "react-router-dom";

import { useI18n } from "../../i18n";
import { copyText } from "../../clipboard";
import { clearQueries, listQueries } from "../../ipc/commands";
import { ipcErrorMessage } from "../../ipc/errors";
import type { HistoricalQuery, QueryEvent } from "../../ipc/types";
import { isTauri } from "../../portStatus";
import { useAppState, useEvents } from "../../state/appState";
import {
  EMPTY_FILTER,
  filterEvents,
  toQueryFilter,
  type LogFilter,
} from "./filter";

const { Paragraph, Text, Title } = Typography;

type LogRow = QueryEvent | HistoricalQuery;
type Mode = "live" | "history";

function isHistorical(row: LogRow): row is HistoricalQuery {
  return "id" in row;
}

function formatTime(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleTimeString("zh-CN", {
    hour12: false,
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    fractionalSecondDigits: 3,
  });
}

function rcodeColor(rcode: number): string {
  if (rcode === 0) {
    return "green";
  }
  if (rcode === 3) {
    return "gold";
  }
  return "red";
}

const SOURCE_COLOR: Record<string, string> = {
  local: "blue",
  cache: "purple",
  upstream: "green",
};

/** 实时视图只渲染最近一段，完整窗口仍保留在事件缓冲中供过滤与统计使用。 */
const LIVE_VIEW_LIMIT = 1000;

/** 概览 Top 下钻通过 URL 传入过滤条件；历史模式复用同一过滤条。 */
function filterFromSearch(search: URLSearchParams): LogFilter {
  return {
    ...EMPTY_FILTER,
    domain: search.get("domain") ?? "",
    upstream: search.get("upstream") || undefined,
  };
}

export function LogsPage() {
  const { t } = useI18n();
  const { message, modal } = App.useApp();
  const events = useEvents();
  const { logsClearSignal } = useAppState();
  const tableRef = useRef<TableRef>(null);
  const handledClearSignal = useRef(0);
  const requestSeq = useRef(0);
  const resetInFlight = useRef(false);
  const [searchParams, setSearchParams] = useSearchParams();

  const [mode, setMode] = useState<Mode>("live");
  const [paused, setPaused] = useState(false);
  const [frozen, setFrozen] = useState<readonly QueryEvent[]>([]);
  const [autoScroll, setAutoScroll] = useState(true);
  const [clearedAt, setClearedAt] = useState(0);
  const [filter, setFilter] = useState<LogFilter>(() => filterFromSearch(searchParams));
  const [selected, setSelected] = useState<LogRow | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [historyItems, setHistoryItems] = useState<HistoricalQuery[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [persistence, setPersistence] = useState(true);
  const [historyLoading, setHistoryLoading] = useState(false);
  /** 产生当前 cursor 的过滤快照；「加载更多」必须用它，否则新旧条件会拼出错误页。 */
  const [appliedFilter, setAppliedFilter] = useState<LogFilter | null>(null);

  const urlDomain = searchParams.get("domain") ?? "";
  const urlUpstream = searchParams.get("upstream") ?? "";

  // URL → 过滤条：概览 Top 下钻或直接修改地址时生效。
  useEffect(() => {
    setFilter((current) => {
      const nextUpstream = urlUpstream === "" ? undefined : urlUpstream;
      if (current.domain === urlDomain && (current.upstream ?? "") === (nextUpstream ?? "")) {
        return current;
      }
      return { ...current, domain: urlDomain, upstream: nextUpstream };
    });
  }, [urlDomain, urlUpstream]);

  // 过滤条 → URL：地址保持可分享；再次从概览跳到同一域名时参数仍有变化，能重新同步。
  useEffect(() => {
    const currentDomain = searchParams.get("domain") ?? "";
    const currentUpstream = searchParams.get("upstream") ?? "";
    const nextDomain = filter.domain;
    const nextUpstream = filter.upstream ?? "";
    if (currentDomain === nextDomain && currentUpstream === nextUpstream) {
      return;
    }
    const next = new URLSearchParams(searchParams);
    if (nextDomain === "") {
      next.delete("domain");
    } else {
      next.set("domain", nextDomain);
    }
    if (nextUpstream === "") {
      next.delete("upstream");
    } else {
      next.set("upstream", nextUpstream);
    }
    setSearchParams(next, { replace: true });
  }, [filter.domain, filter.upstream, searchParams, setSearchParams]);

  const liveSource = paused ? frozen : events;
  const liveEvents = useMemo(
    () =>
      clearedAt === 0
        ? [...liveSource]
        : liveSource.filter((event) => Date.parse(event.time) > clearedAt),
    [clearedAt, liveSource],
  );
  const filtered = useMemo(() => filterEvents(liveEvents, filter), [filter, liveEvents]);
  const displayed = useMemo(() => filtered.slice(-LIVE_VIEW_LIMIT), [filtered]);
  const newWhilePaused = paused ? Math.max(events.length - frozen.length, 0) : 0;

  const loadHistory = useCallback(
    async (kind: "reset" | "more") => {
      if (!isTauri()) {
        return;
      }
      // 重置请求在途时不允许翻页：此时 cursor 仍属于上一次结果集。
      if (kind === "more" && resetInFlight.current) {
        return;
      }
      const snapshot = kind === "reset" ? filter : appliedFilter;
      if (!snapshot) {
        return;
      }
      if (kind === "more" && !nextCursor) {
        return;
      }
      const seq = ++requestSeq.current;
      if (kind === "reset") {
        resetInFlight.current = true;
        // 重置期间先让「加载更多」失效，避免旧 cursor 被当成同一结果集的下一页。
        setNextCursor(null);
        setAppliedFilter(null);
      }
      setHistoryLoading(true);
      try {
        const page = await listQueries({
          ...toQueryFilter(snapshot),
          limit: 100,
          cursor: kind === "reset" ? undefined : nextCursor ?? undefined,
        });
        // 只接受最后一次请求的结果，先发后到的响应直接丢弃。
        if (seq !== requestSeq.current) {
          return;
        }
        if (kind === "reset") {
          setHistoryItems(page.items);
          setAppliedFilter(snapshot);
        } else {
          setHistoryItems((current) => [...current, ...page.items]);
        }
        setNextCursor(page.next_cursor);
        setPersistence(page.persistence_enabled);
        setError(null);
      } catch (loadError) {
        if (seq === requestSeq.current) {
          setError(ipcErrorMessage(loadError));
        }
      } finally {
        if (seq === requestSeq.current) {
          resetInFlight.current = false;
          setHistoryLoading(false);
        }
      }
    },
    [appliedFilter, filter, nextCursor],
  );

  // 过滤条一旦变化，旧 cursor 就不再对应同一结果集：清空分页数据并丢弃在途请求。
  useEffect(() => {
    requestSeq.current += 1;
    resetInFlight.current = false;
    setHistoryLoading(false);
    setHistoryItems([]);
    setNextCursor(null);
    setAppliedFilter(null);
  }, [filter]);

  useEffect(() => {
    if (mode !== "live" || paused || !autoScroll || displayed.length === 0) {
      return;
    }
    tableRef.current?.scrollTo({ index: displayed.length - 1 });
  }, [autoScroll, displayed.length, mode, paused]);

  function switchMode(next: Mode) {
    setMode(next);
    if (next === "history") {
      void loadHistory("reset");
    }
  }

  function togglePause() {
    if (paused) {
      setPaused(false);
      return;
    }
    setFrozen([...events]);
    setPaused(true);
  }

  function clearView() {
    setClearedAt(Date.now());
    if (paused) {
      setFrozen([]);
    }
  }

  useEffect(() => {
    if (logsClearSignal === handledClearSignal.current) {
      return;
    }
    handledClearSignal.current = logsClearSignal;
    if (logsClearSignal === 0) {
      return;
    }
    setClearedAt(Date.now());
    setFrozen([]);
  }, [logsClearSignal]);

  function clearPersistent() {
    modal.confirm({
      title: t("logs.clearPersistent"),
      content: t("logs.clearPersistent.confirm"),
      okButtonProps: { danger: true },
      onOk: async () => {
        try {
          const result = await clearQueries();
          message.success(t("logs.clearPersistent.done", { count: result.deleted }));
          setHistoryItems([]);
          setNextCursor(null);
          setAppliedFilter(null);
          if (mode === "history") {
            void loadHistory("reset");
          }
        } catch (clearError) {
          setError(ipcErrorMessage(clearError));
        }
      },
    });
  }

  async function copyDetail() {
    if (!selected) {
      return;
    }
    try {
      await copyText(JSON.stringify(selected, null, 2));
      message.success(t("logs.detail.copied"));
    } catch (copyError) {
      setError(ipcErrorMessage(copyError));
    }
  }

  const columns = useMemo<TableColumnsType<LogRow>>(
    () => [
      {
        title: t("logs.columns.time"),
        dataIndex: "time",
        width: 110,
        render: (value: string) => formatTime(value),
      },
      {
        title: t("logs.columns.domain"),
        dataIndex: "domain",
        width: 250,
        ellipsis: true,
      },
      {
        title: t("logs.columns.qtype"),
        dataIndex: "qtype",
        width: 80,
      },
      {
        title: t("logs.columns.source"),
        dataIndex: "source",
        width: 90,
        render: (value: string) => (
          <Tag color={SOURCE_COLOR[value] ?? "default"}>{value}</Tag>
        ),
      },
      {
        title: t("logs.columns.rule"),
        dataIndex: "rule",
        width: 170,
        ellipsis: true,
        render: (value: string, row) =>
          value ? `${value} · ${row.rule_kind}` : `· ${row.rule_kind}`,
      },
      {
        title: t("logs.columns.group"),
        dataIndex: "group",
        width: 100,
        ellipsis: true,
      },
      {
        title: t("logs.columns.upstream"),
        dataIndex: "upstream_name",
        width: 190,
        ellipsis: true,
        render: (_value: string, row) => row.upstream_name || row.upstream || "—",
      },
      {
        title: t("logs.columns.duration"),
        dataIndex: "duration_ms",
        width: 100,
        align: "right",
        render: (value: number) => `${value.toFixed(1)} ms`,
      },
      {
        title: t("logs.columns.rcode"),
        dataIndex: "rcode_name",
        width: 120,
        render: (value: string, row) => (
          <Tag color={rcodeColor(row.rcode)}>{value}</Tag>
        ),
      },
      {
        title: t("logs.columns.protocol"),
        dataIndex: "protocol",
        width: 80,
      },
      {
        title: t("logs.columns.answers"),
        dataIndex: "answers",
        width: 80,
        render: (value: string[]) => value.length,
      },
    ],
    [t],
  );

  const tableData: LogRow[] = mode === "live" ? displayed : historyItems;
  const tableTitle =
    mode === "live" && filtered.length > displayed.length
      ? t("logs.countWindow", { total: filtered.length, shown: displayed.length })
      : t("logs.count", { count: tableData.length });

  return (
    <>
      <Space align="start" style={{ width: "100%", justifyContent: "space-between" }}>
        <div>
          <Title level={4} style={{ marginBottom: 4 }}>
            {t("page.logs.title")}
          </Title>
          <Paragraph type="secondary">{t("page.logs.description")}</Paragraph>
        </div>
        <Segmented
          value={mode}
          onChange={(value) => switchMode(value as Mode)}
          options={[
            { label: t("logs.mode.live"), value: "live" },
            { label: t("logs.mode.history"), value: "history" },
          ]}
        />
      </Space>

      {error && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          title={error}
        />
      )}
      {mode === "history" && !persistence && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("logs.persistence.off")}
        />
      )}
      {paused && (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("logs.pausedHint", { count: newWhilePaused })}
        />
      )}

      <Card size="small" style={{ marginBottom: 16 }}>
        <Space wrap>
          <Input
            allowClear
            style={{ width: 220 }}
            placeholder={t("logs.filter.domain")}
            value={filter.domain}
            onChange={(event) =>
              setFilter((current) => ({ ...current, domain: event.target.value }))
            }
          />
          <Input
            allowClear
            style={{ width: 110 }}
            placeholder={t("logs.filter.qtype")}
            value={filter.qtype ?? ""}
            onChange={(event) =>
              setFilter((current) => ({
                ...current,
                qtype: event.target.value || undefined,
              }))
            }
          />
          <Select
            allowClear
            style={{ width: 130 }}
            placeholder={t("logs.filter.source")}
            value={filter.source}
            onChange={(value) =>
              setFilter((current) => ({ ...current, source: value }))
            }
            options={[
              { value: "local", label: "local" },
              { value: "cache", label: "cache" },
              { value: "upstream", label: "upstream" },
            ]}
          />
          <Input
            allowClear
            style={{ width: 180 }}
            placeholder={t("logs.filter.upstream")}
            value={filter.upstream ?? ""}
            onChange={(event) =>
              setFilter((current) => ({
                ...current,
                upstream: event.target.value || undefined,
              }))
            }
          />
          <Input
            allowClear
            style={{ width: 160 }}
            placeholder={t("logs.filter.rule")}
            value={filter.rule ?? ""}
            onChange={(event) =>
              setFilter((current) => ({
                ...current,
                rule: event.target.value || undefined,
              }))
            }
          />
          <Select
            allowClear
            style={{ width: 150 }}
            placeholder={t("logs.filter.rcode")}
            value={filter.rcode}
            onChange={(value) =>
              setFilter((current) => ({ ...current, rcode: value }))
            }
            options={[
              { value: 0, label: "0 NOERROR" },
              { value: 2, label: "2 SERVFAIL" },
              { value: 3, label: "3 NXDOMAIN" },
              { value: 5, label: "5 REFUSED" },
            ]}
          />
          <InputNumber
            min={0}
            style={{ width: 120 }}
            placeholder={t("logs.filter.minDuration")}
            value={filter.minDuration}
            onChange={(value) =>
              setFilter((current) => ({
                ...current,
                minDuration: value ?? undefined,
              }))
            }
          />
          <InputNumber
            min={0}
            style={{ width: 120 }}
            placeholder={t("logs.filter.maxDuration")}
            value={filter.maxDuration}
            onChange={(value) =>
              setFilter((current) => ({
                ...current,
                maxDuration: value ?? undefined,
              }))
            }
          />
          {mode === "history" && (
            <Button
              type="primary"
              loading={historyLoading}
              onClick={() => void loadHistory("reset")}
            >
              {t("logs.filter.search")}
            </Button>
          )}
          <Button
            onClick={() => {
              // 重置只清过滤条；分页状态由过滤变化统一清空，需再点「查询」重新加载。
              setFilter(EMPTY_FILTER);
            }}
          >
            {t("logs.filter.reset")}
          </Button>
        </Space>
      </Card>

      <Card
        size="small"
        title={tableTitle}
        extra={
          mode === "live" ? (
            <Space>
              <Button size="small" onClick={togglePause}>
                {paused ? t("logs.resume") : t("logs.pause")}
              </Button>
              <Button
                size="small"
                type={autoScroll ? "primary" : "default"}
                onClick={() => setAutoScroll((current) => !current)}
              >
                {t("logs.autoscroll")}
              </Button>
              <Popconfirm
                title={t("logs.clearView")}
                description={t("logs.clearView.confirm")}
                onConfirm={clearView}
              >
                <Button size="small">{t("logs.clearView")}</Button>
              </Popconfirm>
            </Space>
          ) : (
            <Button size="small" danger onClick={clearPersistent}>
              {t("logs.clearPersistent")}
            </Button>
          )
        }
      >
        <Table<LogRow>
          ref={tableRef}
          size="small"
          rowKey={(row) =>
            isHistorical(row) ? `history-${row.id}` : `event-${row.event_id}`
          }
          columns={columns}
          dataSource={tableData}
          virtual={mode === "live"}
          pagination={false}
          scroll={{ x: 1400, y: 480 }}
          locale={{ emptyText: t("logs.empty") }}
          onRow={(row) => ({
            onClick: () => setSelected(row),
            style: { cursor: "pointer" },
          })}
        />
        {mode === "history" && (
          <Space style={{ marginTop: 12 }}>
            <Button
              loading={historyLoading}
              /* 没有「产生当前 cursor 的过滤快照」时不允许翻页，避免旧 cursor 配新过滤。 */
              disabled={appliedFilter === null || !nextCursor}
              onClick={() => void loadHistory("more")}
            >
              {nextCursor
                ? t("logs.history.loadMore")
                : appliedFilter === null
                  ? t("logs.history.queryFirst")
                  : t("logs.history.noMore")}
            </Button>
          </Space>
        )}
      </Card>

      <Drawer
        open={selected !== null}
        size={720}
        title={t("logs.detail.title")}
        onClose={() => setSelected(null)}
        extra={
          <Button size="small" onClick={() => void copyDetail()}>
            {t("logs.detail.copy")}
          </Button>
        }
      >
        {selected && <QueryDetail row={selected} />}
      </Drawer>
    </>
  );
}

function QueryDetail({ row }: { row: LogRow }) {
  const { t } = useI18n();
  const requestFlags = row.request_flags;
  const responseFlags = row.response_flags;

  return (
    <Space orientation="vertical" size="middle" style={{ width: "100%" }}>
      <Descriptions
        size="small"
        column={1}
        items={[
          { key: "time", label: t("logs.columns.time"), children: row.time },
          { key: "domain", label: t("logs.columns.domain"), children: row.domain },
          { key: "qtype", label: t("logs.columns.qtype"), children: row.qtype },
          { key: "source", label: t("logs.columns.source"), children: row.source },
          {
            key: "rule",
            label: t("logs.columns.rule"),
            children: row.rule ? `${row.rule}（${row.rule_kind}）` : row.rule_kind,
          },
          { key: "group", label: t("logs.columns.group"), children: row.group },
          {
            key: "upstream",
            label: t("logs.columns.upstream"),
            children: row.upstream_name || row.upstream || "—",
          },
          {
            key: "duration",
            label: t("logs.columns.duration"),
            children: `${row.duration_ms.toFixed(2)} ms`,
          },
          {
            key: "rcode",
            label: t("logs.columns.rcode"),
            children: `${row.rcode_name}（${row.rcode}）`,
          },
          { key: "protocol", label: t("logs.columns.protocol"), children: row.protocol },
          { key: "ttl", label: "TTL", children: row.ttl },
          {
            key: "flags",
            label: t("logs.detail.flags"),
            children: `DO=${row.do} AD=${row.ad} RD=${requestFlags.rd} CD=${requestFlags.cd} AA=${responseFlags.aa} TC=${responseFlags.tc} RA=${responseFlags.ra}`,
          },
          ...(row.error
            ? [{ key: "error", label: "Error", children: row.error }]
            : []),
        ]}
      />

      <div>
        <Text strong>{t("logs.detail.answers")}</Text>
        <ul style={{ margin: "8px 0 0", paddingLeft: 20 }}>
          {row.answers.length === 0 ? (
            <li>
              <Text type="secondary">—</Text>
            </li>
          ) : (
            row.answers.map((answer) => (
              <li key={answer}>
                <Text code>{answer}</Text>
              </li>
            ))
          )}
        </ul>
      </div>

      {(row.authority.length > 0 || row.additional.length > 0) && (
        <div>
          <Text strong>{t("logs.detail.response")}</Text>
          <ul style={{ margin: "8px 0 0", paddingLeft: 20 }}>
            {[...row.authority, ...row.additional].map((item) => (
              <li key={item}>
                <Text code>{item}</Text>
              </li>
            ))}
          </ul>
        </div>
      )}

      {row.attempts && row.attempts.length > 0 && (
        <div>
          <Text strong>{t("logs.detail.attempts")}</Text>
          <ul style={{ margin: "8px 0 0", paddingLeft: 20 }}>
            {row.attempts.map((attempt) => (
              <li key={`${attempt.upstream}-${attempt.duration_ms}`}>
                <Text code>{attempt.upstream}</Text> {attempt.duration_ms.toFixed(1)} ms
                {attempt.error ? ` · ${attempt.error}` : ""}
              </li>
            ))}
          </ul>
        </div>
      )}
    </Space>
  );
}
