import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Card, Divider, Space, Switch, Typography } from "antd";

import { getAutostart, setAutostart, type AutostartStatus } from "./autostart";
import { useI18n } from "./i18n";
import { ipcErrorMessage } from "./ipc/errors";

const { Paragraph, Text } = Typography;

export function AutostartPanel() {
  const { t } = useI18n();
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
      title={t("autostart.title")}
      style={{ marginBottom: 16 }}
      extra={
        <Button size="small" onClick={() => void refresh()}>
          {t("autostart.reload")}
        </Button>
      }
    >
      <Space orientation="vertical" style={{ width: "100%" }}>
        <Space>
          <Switch
            checked={status?.enabled ?? false}
            loading={saving}
            disabled={!status || saving}
            onChange={(checked) => void handleToggle(checked)}
          />
          <Text>{t("autostart.label")}</Text>
        </Space>

        {status?.path && (
          <Text type="secondary">
            {t("autostart.path")}
            <Text code>{status.path}</Text>
          </Text>
        )}
        {status?.enabled && !status.path_valid && (
          <Alert
            type="warning"
            showIcon
            title={t("autostart.invalidTitle")}
            description={t("autostart.invalidDesc")}
          />
        )}
        <Text type="secondary">{t("autostart.note")}</Text>

        <Divider style={{ margin: "8px 0" }} />

        <Paragraph style={{ marginBottom: 4 }}>
          <Text strong>{t("autostart.cacheTitle")}</Text>
          {t("autostart.cacheDesc")}
        </Paragraph>
        <Text code copyable={{ text: "ipconfig /flushdns" }}>
          ipconfig /flushdns
        </Text>

        {error && (
          <Alert
            type="error"
            showIcon
            title={t("autostart.errorTitle")}
            description={error}
          />
        )}
      </Space>
    </Card>
  );
}
