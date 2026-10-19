import { useState } from "react";
import { Alert, Button, Card, Divider, Space, Tag, Typography } from "antd";

import { ipcErrorMessage } from "./ipc";
import {
  openSystemNetworkSettings,
  type SystemDnsStatus,
} from "./systemDns";

const { Paragraph, Text } = Typography;

export function SystemDnsPanel(props: {
  status: SystemDnsStatus | null;
  error: string | null;
  onRefresh: () => void;
}) {
  const { status, error, onRefresh } = props;
  const [actionError, setActionError] = useState<string | null>(null);

  const interfaces = status?.interfaces ?? [];
  const pointed = interfaces.filter((item) => item.points_to_loopback);
  const exampleName = interfaces[0]?.name ?? "<网卡名>";
  const commands = [
    `netsh interface ipv4 set dnsservers name="${exampleName}" static 127.0.0.1 primary`,
    `netsh interface ipv6 set dnsservers name="${exampleName}" static ::1 primary`,
  ];

  async function handleOpenSettings() {
    try {
      setActionError(null);
      await openSystemNetworkSettings();
    } catch (openError) {
      setActionError(ipcErrorMessage(openError));
    }
  }

  return (
    <Card
      size="small"
      title="系统 DNS 状态（只读）"
      style={{ marginBottom: 16 }}
      extra={
        <Button size="small" onClick={onRefresh}>
          重新读取
        </Button>
      }
    >
      {error ? (
        <Alert type="warning" showIcon message="读取网卡 DNS 失败" description={error} />
      ) : !status ? (
        <Text type="secondary">正在读取活动网卡…</Text>
      ) : interfaces.length === 0 ? (
        <Text type="secondary">未检测到活动网卡；VPN/虚拟网卡在连接后会出现在这里。</Text>
      ) : (
        <Space direction="vertical" size={0} style={{ width: "100%" }}>
          {interfaces.map((item) => (
            <div
              key={item.guid || item.name}
              style={{
                display: "flex",
                alignItems: "flex-start",
                justifyContent: "space-between",
                gap: 12,
                padding: "6px 0",
                borderBottom: "1px solid rgba(128, 128, 128, 0.2)",
              }}
            >
              <div>
                <Text strong>{item.name}</Text>
                <div>
                  <Text type="secondary">
                    IPv4：{item.ipv4.length > 0 ? item.ipv4.join("、") : "未配置"}
                  </Text>
                </div>
                <div>
                  <Text type="secondary">
                    IPv6：{item.ipv6.length > 0 ? item.ipv6.join("、") : "未配置"}
                  </Text>
                </div>
              </div>
              <Tag color={item.points_to_loopback ? "green" : "default"}>
                {item.points_to_loopback ? "指向本机" : "未指向本机"}
              </Tag>
            </div>
          ))}
          <div style={{ paddingTop: 8 }}>
            {pointed.length > 0 ? (
              <Alert
                type="success"
                showIcon
                message={`已有 ${pointed.length} 个网卡的 DNS 指向本机`}
                description="系统解析会经过 Namesplit。退出应用前会再次检查并提醒。"
              />
            ) : (
              <Text type="secondary">
                当前没有网卡的 DNS 指向本机。按下方引导把需要生效的网卡改为本机地址后，系统解析才会经过 Namesplit。
              </Text>
            )}
          </div>
        </Space>
      )}

      <Divider style={{ margin: "12px 0" }} />

      <Paragraph style={{ marginBottom: 8 }}>
        <Text strong>手动配置引导：</Text>
        将需要生效的网卡 DNS 分别改为 <Text code>127.0.0.1</Text>（IPv4）与{" "}
        <Text code>::1</Text>（IPv6）。应用固定监听 <Text code>53</Text>{" "}
        端口；此步骤需要管理员权限，由你手动完成，Namesplit 不会修改系统 DNS。
      </Paragraph>
      <Space direction="vertical" style={{ width: "100%", marginBottom: 8 }}>
        {commands.map((command) => (
          <Text key={command} code copyable={{ text: command }}>
            {command}
          </Text>
        ))}
      </Space>
      <Space>
        <Button size="small" type="primary" onClick={() => void handleOpenSettings()}>
          打开系统网络设置
        </Button>
        <Text type="secondary">命令中的网卡名需替换为实际名称。</Text>
      </Space>
      {actionError && (
        <Alert
          type="error"
          showIcon
          style={{ marginTop: 8 }}
          message="打开系统设置失败"
          description={actionError}
        />
      )}
    </Card>
  );
}
