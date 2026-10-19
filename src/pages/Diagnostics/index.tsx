import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Alert,
  App,
  Button,
  Card,
  Col,
  Descriptions,
  Row,
  Space,
  Table,
  Tag,
  Typography,
} from "antd";
import type { TableColumnsType } from "antd";

import { useI18n } from "../../i18n";
import { copyText } from "../../clipboard";
import { getConfig, getStatus, openConfigFile, openDataDir, validateConfig } from "../../ipc/commands";
import { ipcErrorMessage } from "../../ipc/errors";
import type { ConfigEnvelope, StatusResponse, ValidateResult } from "../../ipc/types";
import type { PortState, PortTarget } from "../../portStatus";
import { isTauri } from "../../portStatus";
import type { InterfaceDns } from "../../systemDns";
import { useAppState } from "../../state/appState";
import { buildDiagnosticSummary } from "./summary";

const { Paragraph, Text, Title } = Typography;

const PORT_COLOR: Record<PortState, string> = {
  free: "green",
  in_use: "red",
  unavailable: "default",
  unknown: "gold",
};

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

export function DiagnosticsPage() {
  const { t } = useI18n();
  const { message } = App.useApp();
  const {
    appInfo,
    sidecar,
    systemDns,
    portStatus,
    refreshPortStatus,
    refreshSystemDns,
  } = useAppState();

  const [status, setStatus] = useState<StatusResponse | null>(null);
  const [envelope, setEnvelope] = useState<ConfigEnvelope | null>(null);
  const [validation, setValidation] = useState<ValidateResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [nextStatus, nextEnvelope] = await Promise.all([getStatus(), getConfig()]);
      setStatus(nextStatus);
      setEnvelope(nextEnvelope);
      try {
        setValidation(await validateConfig(nextEnvelope.config));
      } catch {
        setValidation(null);
      }
      setError(null);
    } catch (loadError) {
      setError(ipcErrorMessage(loadError));
    } finally {
      setLoading(false);
    }
    await Promise.all([refreshPortStatus(), refreshSystemDns()]);
  }, [refreshPortStatus, refreshSystemDns]);

  useEffect(() => {
    if (isTauri()) {
      void refresh();
    }
  }, [refresh]);

  const portColumns = useMemo<TableColumnsType<PortTarget>>(
    () => [
      { title: t("diagnostics.port.address"), dataIndex: "address", width: 160 },
      { title: t("diagnostics.port.protocol"), dataIndex: "protocol", width: 100 },
      {
        title: t("diagnostics.port.status"),
        dataIndex: "status",
        width: 130,
        render: (value: PortState) => <Tag color={PORT_COLOR[value]}>{value}</Tag>,
      },
      {
        title: t("diagnostics.port.holders"),
        dataIndex: "holders",
        render: (holders: PortTarget["holders"]) =>
          holders.length === 0
            ? "—"
            : holders
                .map(
                  (holder) =>
                    `${holder.name || "—"} (${holder.pid})${holder.is_self ? " [self]" : ""}`,
                )
                .join(", "),
      },
    ],
    [t],
  );

  async function copySummary() {
    try {
      await copyText(
        buildDiagnosticSummary({
          appVersion: appInfo?.app_version ?? "—",
          sidecarVersion: appInfo?.sidecar_version || status?.version || "—",
          commit: appInfo?.commit || status?.commit || "—",
          dataDir: appInfo?.data_dir ?? "—",
          configPath: appInfo?.config_path ?? "—",
          logPath: appInfo?.log_path ?? "—",
          sidecarState: sidecar?.state ?? "unknown",
          sidecarReason: sidecar?.reason,
          status,
          ports: portStatus,
          systemDns,
          configRevision: envelope?.revision ?? null,
          validationValid: validation?.valid ?? null,
          errors: validation?.errors ?? [],
          warnings: validation?.warnings ?? [],
        }),
      );
      message.success(t("diagnostics.copied"));
    } catch (copyError) {
      setError(ipcErrorMessage(copyError));
    }
  }

  const sidecarState = sidecar?.state ?? "unknown";

  return (
    <>
      <Space align="start" style={{ width: "100%", justifyContent: "space-between" }}>
        <div>
          <Title level={4} style={{ marginBottom: 4 }}>
            {t("page.diagnostics.title")}
          </Title>
          <Paragraph type="secondary">{t("page.diagnostics.description")}</Paragraph>
        </div>
        <Space wrap>
          <Button loading={loading} onClick={() => void refresh()}>
            {t("diagnostics.refresh")}
          </Button>
          <Button onClick={() => void copySummary()}>{t("diagnostics.copy")}</Button>
          <Button onClick={() => void openDataDir()}>{t("diagnostics.openDataDir")}</Button>
          <Button onClick={() => void openConfigFile()}>{t("diagnostics.openConfig")}</Button>
        </Space>
      </Space>

      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("diagnostics.error", { message: error })}
        />
      )}
      {!status && !error && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("diagnostics.unavailable")}
        />
      )}

      <Row gutter={[16, 16]}>
        <Col xs={24} md={8}>
          <Card size="small" title={t("diagnostics.service")}>
            <Descriptions
              size="small"
              column={1}
              items={[
                {
                  key: "state",
                  label: t("diagnostics.state"),
                  children: <Tag>{sidecarState}</Tag>,
                },
                {
                  key: "pid",
                  label: t("diagnostics.pid"),
                  children: sidecar?.pid ?? "—",
                },
                {
                  key: "restarts",
                  label: t("diagnostics.restarts"),
                  children: sidecar?.restart_count ?? "—",
                },
                {
                  key: "uptime",
                  label: t("diagnostics.uptime"),
                  children: formatUptime(status?.uptime_ms),
                },
                ...(sidecar?.reason
                  ? [
                      {
                        key: "reason",
                        label: t("diagnostics.reason"),
                        children: sidecar.reason,
                      },
                    ]
                  : []),
              ]}
            />
          </Card>
        </Col>

        <Col xs={24} md={8}>
          <Card size="small" title={t("diagnostics.listening")}>
            <Space orientation="vertical" size={4} style={{ width: "100%" }}>
              <Tag color={status?.dns.ready ? "green" : "gold"}>
                {status?.dns.ready ? "ready" : "not ready"}
              </Tag>
              <Text>
                {t("diagnostics.udp")}: {status?.dns.udp.join(", ") || "—"}
              </Text>
              <Text>
                {t("diagnostics.tcp")}: {status?.dns.tcp.join(", ") || "—"}
              </Text>
              {status?.dns.bind_error && (
                <Alert
                  type="error"
                  showIcon
                  title={status.dns.bind_error.message}
                  description={status.dns.bind_error.targets
                    .map(
                      (target) =>
                        `${target.address}/${target.protocol}${target.error ? `：${target.error}` : ""}`,
                    )
                    .join("；")}
                />
              )}
            </Space>
          </Card>
        </Col>

        <Col xs={24} md={8}>
          <Card size="small" title={t("diagnostics.config")}>
            <Descriptions
              size="small"
              column={1}
              items={[
                {
                  key: "revision",
                  label: t("diagnostics.revision"),
                  children: envelope?.revision ?? "—",
                },
                {
                  key: "valid",
                  label: t("diagnostics.valid"),
                  children: validation ? (
                    <Tag color={validation.valid ? "green" : "red"}>
                      {validation.valid
                        ? t("diagnostics.validOk")
                        : t("diagnostics.validFail")}
                    </Tag>
                  ) : (
                    "—"
                  ),
                },
                {
                  key: "errors",
                  label: t("diagnostics.errors"),
                  children: validation?.errors.length ?? "—",
                },
                {
                  key: "warnings",
                  label: t("diagnostics.warnings"),
                  children: validation?.warnings.length ?? "—",
                },
              ]}
            />
            {envelope?.config_error && (
              <Alert type="error" showIcon title={envelope.config_error.message} />
            )}
          </Card>
        </Col>
      </Row>

      <Card
        size="small"
        title={t("diagnostics.ports")}
        style={{ marginTop: 16 }}
        extra={
          portStatus?.attribution_limited ? (
            <Text type="warning">{t("diagnostics.attributionLimited")}</Text>
          ) : null
        }
      >
        <Table<PortTarget>
          rowKey={(row) => `${row.address}-${row.protocol}`}
          size="small"
          columns={portColumns}
          dataSource={portStatus?.targets ?? []}
          pagination={false}
        />
      </Card>

      <Card size="small" title={t("diagnostics.systemDns")} style={{ marginTop: 16 }}>
        <Table<InterfaceDns>
          rowKey={(row) => row.guid || row.name}
          size="small"
          columns={[
            { title: t("diagnostics.interface"), dataIndex: "name" },
            {
              title: t("diagnostics.ipv4"),
              dataIndex: "ipv4",
              render: (value: string[]) => value.join(", ") || "—",
            },
            {
              title: t("diagnostics.ipv6"),
              dataIndex: "ipv6",
              render: (value: string[]) => value.join(", ") || "—",
            },
            {
              title: t("diagnostics.pointing"),
              dataIndex: "points_to_loopback",
              render: (value: boolean) => (
                <Tag color={value ? "green" : "default"}>
                  {value ? t("diagnostics.pointing") : t("diagnostics.notPointing")}
                </Tag>
              ),
            },
          ]}
          dataSource={systemDns?.interfaces ?? []}
          pagination={false}
        />
      </Card>

      <Card size="small" title={t("diagnostics.versions")} style={{ marginTop: 16 }}>
        <Descriptions
          size="small"
          column={1}
          items={[
            {
              key: "appVersion",
              label: t("diagnostics.appVersion"),
              children: appInfo?.app_version ?? "—",
            },
            {
              key: "sidecarVersion",
              label: t("diagnostics.sidecarVersion"),
              children: appInfo?.sidecar_version || status?.version || "—",
            },
            {
              key: "commit",
              label: t("diagnostics.commit"),
              children: appInfo?.commit || status?.commit || "—",
            },
            {
              key: "dataDir",
              label: t("diagnostics.dataDir"),
              children: appInfo ? <Text code>{appInfo.data_dir}</Text> : "—",
            },
            {
              key: "configPath",
              label: t("diagnostics.configPath"),
              children: appInfo ? <Text code>{appInfo.config_path}</Text> : "—",
            },
            {
              key: "logPath",
              label: t("diagnostics.logPath"),
              children: appInfo ? <Text code>{appInfo.log_path}</Text> : "—",
            },
          ]}
        />
      </Card>

      {(validation?.errors.length ?? 0) > 0 && (
        <Alert
          type="error"
          showIcon
          style={{ marginTop: 16 }}
          title={t("diagnostics.errors")}
          description={
            <ul style={{ margin: 0, paddingLeft: 20 }}>
              {validation?.errors.map((item) => (
                <li key={`${item.path}-${item.code}`}>
                  {item.path}：{item.message}
                </li>
              ))}
            </ul>
          }
        />
      )}
    </>
  );
}
