import { useMemo, useState } from "react";
import { Alert, App, Button, Layout, Menu, Typography } from "antd";
import { Outlet, useLocation, useNavigate } from "react-router-dom";

import { PortConflictBanner } from "../PortConflictBanner";
import { useI18n } from "../i18n";
import { restartSidecar } from "../ipc/commands";
import { ipcErrorAdvice, ipcErrorMessage } from "../ipc/errors";
import { useAppState } from "../state/appState";
import { useUnsavedChangesGuard } from "../state/unsavedGuard";
import { openSystemNetworkSettings } from "../systemDns";

const { Content, Header, Sider } = Layout;
const { Text } = Typography;

const NAV_ITEMS = [
  { path: "/overview", key: "nav.overview" },
  { path: "/logs", key: "nav.logs" },
  { path: "/rules", key: "nav.rules" },
  { path: "/upstreams", key: "nav.upstreams" },
  { path: "/local", key: "nav.local" },
  { path: "/settings", key: "nav.settings" },
  { path: "/diagnostics", key: "nav.diagnostics" },
] as const;

export function AppShell() {
  const { t } = useI18n();
  const { modal } = App.useApp();
  const { hasUnsaved } = useUnsavedChangesGuard();
  const navigate = useNavigate();
  const location = useLocation();
  const [reloadNoticeDismissed, setReloadNoticeDismissed] = useState(false);
  const [restartingSidecar, setRestartingSidecar] = useState(false);
  const [restartError, setRestartError] = useState<string | null>(null);
  const [restartAdvice, setRestartAdvice] = useState<string | null>(null);
  const {
    appInfo,
    sidecar,
    systemDns,
    systemDnsError,
    portStatus,
    portError,
    eventsError,
    eventsStale,
    refreshPortStatus,
    refreshSidecarState,
    retryEvents,
  } = useAppState();

  async function restartSidecarService() {
    setRestartingSidecar(true);
    setRestartError(null);
    setRestartAdvice(null);
    try {
      await restartSidecar();
      await refreshSidecarState();
    } catch (error) {
      setRestartError(ipcErrorMessage(error));
      setRestartAdvice(ipcErrorAdvice(error));
    } finally {
      setRestartingSidecar(false);
    }
  }

  function navigateFromMenu(target: string) {
    if (target === location.pathname) {
      return;
    }
    if (!hasUnsaved()) {
      navigate(target);
      return;
    }
    modal.confirm({
      title: t("common.unsavedTitle"),
      content: t("common.unsavedContent"),
      okText: t("common.leave"),
      cancelText: t("common.stay"),
      onOk: () => navigate(target),
    });
  }

  const selectedKey = NAV_ITEMS.find((item) =>
    location.pathname.startsWith(item.path),
  )?.path;

  const menuItems = useMemo(
    () =>
      NAV_ITEMS.map((item) => ({
        key: item.path,
        label: t(item.key),
      })),
    [t],
  );

  const dnsNotLoopback =
    systemDns !== null &&
    systemDns.interfaces.length > 0 &&
    systemDns.interfaces.every((item) => !item.points_to_loopback);
  const rendererReloads = appInfo?.renderer_reloads ?? 0;
  const failedSidecar = sidecar?.state === "failed" ? sidecar : null;
  const configError = failedSidecar?.config_error;
  const configErrorLocation = [
    configError?.path,
    configError?.line,
  ]
    .filter((part): part is string | number => part !== undefined && part !== null)
    .join(":");

  return (
    <Layout style={{ minHeight: "100vh" }}>
      <Sider theme="light" width={200}>
        <div style={{ padding: "20px 16px 12px" }}>
          <Text strong>{t("app.name")}</Text>
        </div>
        <Menu
          mode="inline"
          selectedKeys={selectedKey ? [selectedKey] : []}
          items={menuItems}
          onClick={({ key }) => navigateFromMenu(key)}
        />
      </Sider>
      <Layout>
        <Header style={{ background: "transparent", paddingInline: 24 }}>
          <Text strong style={{ lineHeight: "64px", fontSize: 18 }}>
            {t("app.name")}
          </Text>
        </Header>
        <Content style={{ padding: "0 24px 24px" }}>
          {rendererReloads > 0 && !reloadNoticeDismissed && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 16 }}
              title={t("status.rendererReloaded", { count: rendererReloads })}
              closable={{ onClose: () => setReloadNoticeDismissed(true) }}
            />
          )}
          <PortConflictBanner
            status={portStatus}
            error={portError}
            onRefresh={() => void refreshPortStatus()}
          />
          {eventsStale && (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 16 }}
              title={t("status.eventsRecovering")}
              action={
                <Button size="small" onClick={() => void retryEvents()}>
                  {t("common.retry")}
                </Button>
              }
            />
          )}
          {eventsError && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 16 }}
              title={t("status.eventsStalled", { message: eventsError })}
              action={
                <Button size="small" onClick={() => void retryEvents()}>
                  {t("common.retry")}
                </Button>
              }
            />
          )}
          {failedSidecar && (
            <Alert
              type="error"
              showIcon
              style={{ marginBottom: 16 }}
              title={t("status.sidecarFailed", {
                reason: failedSidecar.reason ?? "",
              })}
              description={
                configError && (configError.message || configErrorLocation) ? (
                  <div>
                    {configError.message && <div>{configError.message}</div>}
                    {configErrorLocation && <Text code>{configErrorLocation}</Text>}
                  </div>
                ) : undefined
              }
              action={
                <Button
                  size="small"
                  loading={restartingSidecar}
                  onClick={() => void restartSidecarService()}
                >
                  {t("common.retry")}
                </Button>
              }
            />
          )}
          {restartError && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 16 }}
              title={restartError}
              description={restartAdvice ?? undefined}
            />
          )}
          {sidecar?.state === "exited" && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 16 }}
              title={t("status.sidecarExited", {
                reason: sidecar.reason ?? "",
              })}
            />
          )}
          {dnsNotLoopback && (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 16 }}
              title={t("status.dnsNotLoopback")}
              action={
                <Button
                  size="small"
                  onClick={() => void openSystemNetworkSettings()}
                >
                  {t("status.openNetworkSettings")}
                </Button>
              }
            />
          )}
          {systemDnsError && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 16 }}
              title={systemDnsError}
            />
          )}
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
