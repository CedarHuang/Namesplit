import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Card, Divider, Space, Switch, Typography } from "antd";

import { getAutostart, setAutostart, type AutostartStatus } from "./autostart";
import { ipcErrorMessage } from "./ipc";

const { Paragraph, Text } = Typography;

export function AutostartPanel() {
  const [status, setStatus] = useState<AutostartStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const refresh = useCallback(async () => {
    try {
      setStatus(await getAutostart());
      setError(null);
    } catch (loadError) {
      setError(ipcErrorMessage(loadError));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  async function handleToggle(enabled: boolean) {
    setSaving(true);
    setError(null);
    try {
      setStatus(await setAutostart(enabled));
    } catch (saveError) {
      setError(ipcErrorMessage(saveError));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card
      size="small"
      title="启动与系统缓存"
      style={{ marginBottom: 16 }}
      extra={
        <Button size="small" onClick={() => void refresh()}>
          重新读取
        </Button>
      }
    >
      <Space direction="vertical" style={{ width: "100%" }}>
        <Space>
          <Switch
            checked={status?.enabled ?? false}
            loading={saving}
            disabled={!status || saving}
            onChange={(checked) => void handleToggle(checked)}
          />
          <Text>登录 Windows 时自动启动 Namesplit</Text>
        </Space>

        {status?.path && (
          <Text type="secondary">
            启动项路径：<Text code>{status.path}</Text>
          </Text>
        )}
        {status?.enabled && !status.path_valid && (
          <Alert
            type="warning"
            showIcon
            message="登录自启路径已失效"
            description="应用可能被移动或删除。请关闭后重新开启自启，或重新解压便携包。"
          />
        )}
        <Text type="secondary">
          登录自启只启动应用，不会修改系统 DNS；网卡 DNS 仍需按上方引导手动配置。
        </Text>

        <Divider style={{ margin: "8px 0" }} />

        <Paragraph style={{ marginBottom: 4 }}>
          <Text strong>系统 DNS 缓存：</Text>
          Windows 会缓存解析结果，刚修改 DNS 或规则后，系统工具可能仍显示旧结果。可在管理员终端执行以下命令清除缓存；应用不会代执行。
        </Paragraph>
        <Text code copyable={{ text: "ipconfig /flushdns" }}>
          ipconfig /flushdns
        </Text>

        {error && (
          <Alert type="error" showIcon message="操作登录自启失败" description={error} />
        )}
      </Space>
    </Card>
  );
}
