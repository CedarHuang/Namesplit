import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  Col,
  Empty,
  Row,
  Space,
  Statistic,
  Tag,
  Typography,
} from "antd";
import { useNavigate } from "react-router-dom";

import { Chart } from "../../components/Chart";
import { useI18n } from "../../i18n";
import type { MessageKey } from "../../i18n";
import { getMinuteStats, getStats, getStatus, getTop } from "../../ipc/commands";
import { ipcErrorMessage } from "../../ipc/errors";
import type {
  MinuteResponse,
  StatsResponse,
  StatusResponse,
  TopItem,
} from "../../ipc/types";
import { isTauri } from "../../portStatus";
import { useSettings } from "../../settings";
import { useAppState, useEvents } from "../../state/appState";
import {
  appendPoint,
  percentile,
  ratio,
  sumToday,
  type RealtimePoint,
} from "./metrics";

const { Paragraph, Text, Title } = Typography;

const STATE_LABEL: Record<string, MessageKey> = {
  running: "overview.state.running",
  restarting: "overview.state.restarting",
  exited: "overview.state.exited",
  failed: "overview.state.failed",
  unknown: "overview.state.unknown",
};

const STATE_COLOR: Record<string, string> = {
  running: "green",
  restarting: "gold",
  exited: "gold",
  failed: "red",
  unknown: "default",
};

function formatPercent(value: number | null): string {
  return value === null ? "—" : `${(value * 100).toFixed(1)}%`;
}

function formatUptime(ms: number | null | undefined): string {
  if (!ms) {
    return "—";
  }
  const seconds = Math.floor(ms / 1000);
  const minutes = Math.floor(seconds / 60);
  const hours = Math.floor(minutes / 60);
  if (hours > 0) {
    return `${hours}h ${minutes % 60}m`;
  }
  if (minutes > 0) {
    return `${minutes}m ${seconds % 60}s`;
  }
  return `${seconds}s`;
}

function TopList(props: {
  items: TopItem[];
  emptyText: string;
  onSelect: (key: string) => void;
}) {
  const max = props.items.reduce((value, item) => Math.max(value, item.count), 0);
  if (props.items.length === 0) {
    return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={props.emptyText} />;
  }
  return (
    <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
      {props.items.map((item) => (
        <li key={item.key} style={{ marginBottom: 10 }}>
          <button
            type="button"
            aria-label={item.key}
            onClick={() => props.onSelect(item.key)}
            style={{
              display: "block",
              width: "100%",
              padding: 0,
              border: "none",
              background: "transparent",
              color: "inherit",
              font: "inherit",
              textAlign: "left",
              cursor: "pointer",
            }}
          >
            <span style={{ display: "flex", justifyContent: "space-between", gap: 8 }}>
              <Text ellipsis={{ tooltip: item.key }} style={{ flex: 1 }}>
                {item.key}
              </Text>
              <Text type="secondary">{item.count}</Text>
            </span>
            <span
              style={{
                display: "block",
                height: 4,
                marginTop: 4,
                borderRadius: 2,
                background: "rgba(22, 119, 255, 0.15)",
              }}
            >
              <span
                style={{
                  display: "block",
                  width: `${max > 0 ? (item.count / max) * 100 : 0}%`,
                  height: "100%",
                  borderRadius: 2,
                  background: "#1677ff",
                }}
              />
            </span>
          </button>
        </li>
      ))}
    </ul>
  );
}

export function OverviewPage() {
  const { t } = useI18n();
  const { dark } = useSettings();
  const { sidecar, systemDns } = useAppState();
  const navigate = useNavigate();
  const events = useEvents();
  const eventsRef = useRef(events);
  eventsRef.current = events;

  const [stats, setStats] = useState<StatsResponse | null>(null);
  const [minutes, setMinutes] = useState<MinuteResponse | null>(null);
  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [topDomains, setTopDomains] = useState<TopItem[]>([]);
  const [topUpstreams, setTopUpstreams] = useState<TopItem[]>([]);
  const [points, setPoints] = useState<RealtimePoint[]>([]);
  const [error, setError] = useState<string | null>(null);

  function openLogs(param: "domain" | "upstream", key: string) {
    const search = new URLSearchParams({ [param]: key });
    navigate({ pathname: "/logs", search: `?${search.toString()}` });
  }

  const loadSummary = useCallback(async () => {
    try {
      const [minuteResult, statusResult, domainResult, upstreamResult] =
        await Promise.all([
          getMinuteStats(),
          getStatus(),
          getTop("domain", 5),
          getTop("upstream", 5),
        ]);
      setMinutes(minuteResult);
      setStatus(statusResult);
      setTopDomains(domainResult.items);
      setTopUpstreams(upstreamResult.items);
      setError(null);
    } catch (loadError) {
      setError(ipcErrorMessage(loadError));
    }
  }, []);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }

    let disposed = false;
    const tick = async () => {
      try {
        const next = await getStats();
        if (disposed) {
          return;
        }
        setStats(next);
        const cutoff = Date.now() - 1000;
        const durations = eventsRef.current
          .filter((event) => Date.parse(event.time) >= cutoff)
          .map((event) => event.duration_ms);
        const label = new Date().toLocaleTimeString("zh-CN", { hour12: false });
        setPoints((current) =>
          appendPoint(current, {
            label,
            qps: next.qps_1s,
            p50: percentile(durations, 0.5),
            p95: percentile(durations, 0.95),
          }),
        );
        setError(null);
      } catch (loadError) {
        if (!disposed) {
          setError(ipcErrorMessage(loadError));
        }
      }
    };

    void tick();
    const timer = window.setInterval(() => void tick(), 1000);
    return () => {
      disposed = true;
      window.clearInterval(timer);
    };
  }, []);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }
    void loadSummary();
    const timer = window.setInterval(() => void loadSummary(), 15_000);
    return () => window.clearInterval(timer);
  }, [loadSummary]);

  const today = useMemo(() => sumToday(minutes?.items ?? [], new Date()), [minutes]);
  const cacheHitRate = ratio(today.cacheHits, today.queries);
  const failureRate = ratio(today.failures, today.queries);
  const pointingCount =
    systemDns?.interfaces.filter((item) => item.points_to_loopback).length ?? 0;
  const sidecarState = sidecar?.state ?? "unknown";

  const chartOption = useMemo(() => {
    const textColor = dark ? "#d9d9d9" : "#434343";
    const splitColor = dark ? "rgba(255,255,255,0.12)" : "rgba(0,0,0,0.08)";
    return {
      backgroundColor: "transparent",
      tooltip: { trigger: "axis" },
      legend: {
        top: 0,
        textStyle: { color: textColor },
        data: [t("overview.chart.qps"), t("overview.chart.p50"), t("overview.chart.p95")],
      },
      grid: { left: 48, right: 56, top: 40, bottom: 28 },
      xAxis: {
        type: "category",
        boundaryGap: false,
        data: points.map((point) => point.label),
        axisLabel: { color: textColor, hideOverlap: true },
        axisLine: { lineStyle: { color: splitColor } },
      },
      yAxis: [
        {
          type: "value",
          name: t("overview.chart.qps"),
          nameTextStyle: { color: textColor },
          axisLabel: { color: textColor },
          splitLine: { lineStyle: { color: splitColor } },
        },
        {
          type: "value",
          name: "ms",
          nameTextStyle: { color: textColor },
          axisLabel: { color: textColor },
          splitLine: { show: false },
        },
      ],
      series: [
        {
          name: t("overview.chart.qps"),
          type: "line",
          smooth: true,
          showSymbol: false,
          data: points.map((point) => point.qps),
          lineStyle: { width: 2, color: "#1677ff" },
          itemStyle: { color: "#1677ff" },
        },
        {
          name: t("overview.chart.p50"),
          type: "line",
          yAxisIndex: 1,
          smooth: true,
          showSymbol: false,
          data: points.map((point) => point.p50),
          lineStyle: { width: 1, color: "#52c41a" },
          itemStyle: { color: "#52c41a" },
        },
        {
          name: t("overview.chart.p95"),
          type: "line",
          yAxisIndex: 1,
          smooth: true,
          showSymbol: false,
          data: points.map((point) => point.p95),
          lineStyle: { width: 1, color: "#fa8c16" },
          itemStyle: { color: "#fa8c16" },
        },
      ],
    };
  }, [dark, points, t]);

  return (
    <>
      <Space align="start" style={{ width: "100%", justifyContent: "space-between" }}>
        <div>
          <Title level={4} style={{ marginBottom: 4 }}>
            {t("page.overview.title")}
          </Title>
          <Paragraph type="secondary">{t("page.overview.description")}</Paragraph>
        </div>
        <Button
          onClick={() => {
            void loadSummary();
          }}
        >
          {t("common.refresh")}
        </Button>
      </Space>

      {error && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("overview.error", { message: error })}
        />
      )}

      <Row gutter={[16, 16]}>
        <Col xs={24} md={8}>
          <Card size="small" title={t("overview.status.service")}>
            <Space orientation="vertical" size={4}>
              <Tag color={STATE_COLOR[sidecarState]}>
                {t(STATE_LABEL[sidecarState] ?? "overview.state.unknown")}
              </Tag>
              <Text type="secondary">
                {t("overview.metrics.qps")}: {stats?.qps_1s.toFixed(1) ?? "—"}
              </Text>
              {status && (
                <Text type="secondary">uptime: {formatUptime(status.uptime_ms)}</Text>
              )}
            </Space>
          </Card>
        </Col>
        <Col xs={24} md={8}>
          <Card size="small" title={t("overview.status.listening")}>
            {!status ? (
              <Text type="secondary">—</Text>
            ) : (
              <Space orientation="vertical" size={4}>
                <Tag color={status.dns.ready ? "green" : "gold"}>
                  {status.dns.ready
                    ? t("overview.status.listening.ok")
                    : t("overview.status.listening.partial")}
                </Tag>
                {status.dns.bind_error && (
                  <Text type="danger">{status.dns.bind_error.message}</Text>
                )}
              </Space>
            )}
          </Card>
        </Col>
        <Col xs={24} md={8}>
          <Card size="small" title={t("overview.status.dns")}>
            {!systemDns ? (
              <Text type="secondary">{t("overview.status.dns.unavailable")}</Text>
            ) : (
              <Space orientation="vertical" size={4}>
                <Tag color={pointingCount > 0 ? "green" : "gold"}>
                  {pointingCount > 0
                    ? t("overview.status.dns.pointing", { count: pointingCount })
                    : t("overview.status.dns.none")}
                </Tag>
                <Text type="secondary">{t("overview.status.dns.count", { count: systemDns.interfaces.length })}</Text>
              </Space>
            )}
          </Card>
        </Col>
      </Row>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic title={t("overview.metrics.todayQueries")} value={today.queries} />
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t("overview.metrics.cacheHitRate")}
              value={formatPercent(cacheHitRate)}
            />
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t("overview.metrics.failureRate")}
              value={formatPercent(failureRate)}
            />
          </Card>
        </Col>
        <Col xs={12} md={6}>
          <Card size="small">
            <Statistic
              title={t("overview.metrics.qps")}
              value={stats?.qps_1s ?? 0}
              precision={1}
            />
          </Card>
        </Col>
      </Row>

      {minutes?.persistence_enabled === false && (
        <Alert
          type="info"
          showIcon
          style={{ marginTop: 16 }}
          title={t("overview.persistence.off")}
        />
      )}

      <Card size="small" title={t("overview.chart.title")} style={{ marginTop: 16 }}>
        <Chart option={chartOption} height={280} />
      </Card>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={24} md={12}>
          <Card size="small" title={t("overview.top.domains")}>
            <TopList
              items={topDomains}
              emptyText={t("overview.top.empty")}
              onSelect={(key) => openLogs("domain", key)}
            />
          </Card>
        </Col>
        <Col xs={24} md={12}>
          <Card size="small" title={t("overview.top.upstreams")}>
            <TopList
              items={topUpstreams}
              emptyText={t("overview.top.empty")}
              onSelect={(key) => openLogs("upstream", key)}
            />
          </Card>
        </Col>
      </Row>
    </>
  );
}
