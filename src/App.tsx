import { useCallback, useEffect, useState } from "react";
import { ConfigProvider, Layout, Typography, theme } from "antd";
import zhCN from "antd/locale/zh_CN";
import { listen } from "@tauri-apps/api/event";

import { PortConflictBanner } from "./PortConflictBanner";
import { getPortStatus, isTauri, type PortStatus } from "./portStatus";
import { SystemDnsPanel } from "./SystemDnsPanel";
import { getSystemDns, type SystemDnsStatus } from "./systemDns";
import { AutostartPanel } from "./AutostartPanel";
import { ipcErrorMessage } from "./ipc";

const { Content, Header } = Layout;
const { Paragraph, Title } = Typography;

export default function App() {
  const [dark, setDark] = useState(
    () => window.matchMedia("(prefers-color-scheme: dark)").matches,
  );
  const [portStatus, setPortStatus] = useState<PortStatus | null>(null);
  const [portError, setPortError] = useState<string | null>(null);
  const [systemDns, setSystemDns] = useState<SystemDnsStatus | null>(null);
  const [systemDnsError, setSystemDnsError] = useState<string | null>(null);

  useEffect(() => {
    const query = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (event: MediaQueryListEvent) => setDark(event.matches);
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, []);

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

  useEffect(() => {
    if (isTauri()) {
      void refreshSystemDns();
    }
  }, [refreshSystemDns]);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }

    // 端口冲突结论只取决于"是否有非自身占用或无法绑定"：sidecar 就绪事件早于
    // DNS 实际绑定时，目标要么空闲、要么已被外部占用，结论不会因绑定完成而翻转。
    // 因此订阅建立后补一次快照，并在每次 sidecar 状态变化时重查即可收敛。
    let disposed = false;
    let unlisten: (() => void) | undefined;
    void listen<{ state: string }>("sidecar://state", () => {
      if (!disposed) {
        void refreshPortStatus();
      }
    })
      .then((stop) => {
        if (disposed) {
          stop();
          return;
        }
        unlisten = stop;
        // 订阅建立前的状态变化已经错过，这里补一次。
        void refreshPortStatus();
      })
      .catch((error) => {
        if (!disposed) {
          setPortError(ipcErrorMessage(error));
        }
      });

    return () => {
      disposed = true;
      unlisten?.();
    };
  }, [refreshPortStatus]);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }

    let disposed = false;
    let unlisten: (() => void) | undefined;
    void listen<SystemDnsStatus>("system_dns://changed", (event) => {
      if (!disposed) {
        setSystemDns(event.payload);
        setSystemDnsError(null);
      }
    }).then((stop) => {
      if (disposed) {
        stop();
      } else {
        unlisten = stop;
      }
    });

    // 手工改 DNS 不一定触发网卡变化通知，窗口重新获得焦点时兜底刷新。
    const onFocus = () => void refreshSystemDns();
    window.addEventListener("focus", onFocus);

    return () => {
      disposed = true;
      unlisten?.();
      window.removeEventListener("focus", onFocus);
    };
  }, [refreshSystemDns]);

  return (
    <ConfigProvider
      locale={zhCN}
      theme={{ algorithm: dark ? theme.darkAlgorithm : theme.defaultAlgorithm }}
    >
      <Layout style={{ minHeight: "100vh" }}>
        <Header style={{ background: "transparent" }}>
          <Title level={3} style={{ margin: 0, lineHeight: "64px" }}>
            Namesplit
          </Title>
        </Header>
        <Content style={{ padding: 24 }}>
          <PortConflictBanner
            status={portStatus}
            error={portError}
            onRefresh={() => void refreshPortStatus()}
          />
          {isTauri() && (
            <>
              <SystemDnsPanel
                status={systemDns}
                error={systemDnsError}
                onRefresh={() => void refreshSystemDns()}
              />
              <AutostartPanel />
            </>
          )}
          <Paragraph>界面开发中。</Paragraph>
        </Content>
      </Layout>
    </ConfigProvider>
  );
}
