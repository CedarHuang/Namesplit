import { useCallback, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Alert,
  App,
  Button,
  Card,
  Descriptions,
  Divider,
  InputNumber,
  Select,
  Space,
  Switch,
  Tag,
  Typography,
} from "antd";

import { AutostartPanel } from "../../AutostartPanel";
import {
  getAppInfo,
  openConfigFile,
  openDataDir,
  resetConfig,
} from "../../ipc/commands";
import { ipcErrorCode, ipcErrorMessage } from "../../ipc/errors";
import type { AppInfo } from "../../ipc/types";
import { useI18n } from "../../i18n";
import { isTauri } from "../../portStatus";
import { useSettings } from "../../settings";
import { useConfigDraft } from "../../state/useConfigDraft";
import { useUnsavedChanges } from "../../state/unsavedGuard";

const { Paragraph, Text, Title } = Typography;

export function SettingsPage() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const { prefs, error: prefsError, updatePrefs } = useSettings();
  const { message, modal } = App.useApp();
  const {
    config,
    revision,
    dirty,
    error: configError,
    validationErrors,
    warnings,
    refresh: refreshConfig,
    update,
    save,
  } = useConfigDraft();

  const [info, setInfo] = useState<AppInfo | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  useUnsavedChanges(dirty);

  const refreshInfo = useCallback(async () => {
    try {
      setInfo(await getAppInfo());
      setActionError(null);
    } catch (loadError) {
      setActionError(ipcErrorMessage(loadError));
    }
  }, []);

  useEffect(() => {
    if (isTauri()) {
      void refreshInfo();
    }
  }, [refreshInfo]);

  async function handleSaveConfig() {
    const outcome = await save();
    if (outcome.ok) {
      message.success(t("settings.saveSuccess"));
      await refreshInfo();
    } else if (outcome.conflict) {
      message.warning(t("settings.revisionConflict"));
    }
  }

  function reloadConfig() {
    if (!dirty) {
      void refreshConfig();
      return;
    }
    modal.confirm({
      title: t("common.unsavedTitle"),
      content: t("common.reloadDiscardContent"),
      okText: t("settings.refresh"),
      cancelText: t("common.stay"),
      onOk: () => refreshConfig(),
    });
  }

  function handleResetConfig() {
    modal.confirm({
      title: t("settings.resetTitle"),
      content: t("settings.resetContent"),
      okButtonProps: { danger: true },
      onOk: async () => {
        try {
          await resetConfig(revision);
          message.success(t("settings.resetSuccess"));
          await refreshConfig();
          await refreshInfo();
        } catch (resetError) {
          if (ipcErrorCode(resetError) === "revision_conflict") {
            message.warning(t("settings.revisionConflict"));
            await refreshConfig();
            return;
          }
          setActionError(ipcErrorMessage(resetError));
        }
      },
    });
  }

  async function runOpen(action: () => Promise<unknown>) {
    try {
      await action();
      setActionError(null);
    } catch (openError) {
      setActionError(ipcErrorMessage(openError));
    }
  }

  const settings = config?.settings;

  return (
    <>
      <Title level={4}>{t("page.settings.title")}</Title>
      <Paragraph type="secondary">{t("page.settings.description")}</Paragraph>

      <Card size="small" title={t("settings.preferences")} style={{ marginBottom: 16 }}>
        <Space orientation="vertical" size="middle" style={{ width: "100%" }}>
          <Space>
            <Text>{t("settings.language")}</Text>
            <Select
              value={prefs.language}
              style={{ width: 200 }}
              onChange={(language) => void updatePrefs({ language })}
              options={[
                { value: "zh-CN", label: t("settings.language.zhCN") },
                {
                  value: "en-US",
                  label: t("settings.language.enUS"),
                  disabled: true,
                },
              ]}
            />
          </Space>
          <Space>
            <Text>{t("settings.theme")}</Text>
            <Select
              value={prefs.theme}
              style={{ width: 200 }}
              onChange={(theme) => void updatePrefs({ theme })}
              options={[
                { value: "system", label: t("settings.theme.system") },
                { value: "light", label: t("settings.theme.light") },
                { value: "dark", label: t("settings.theme.dark") },
              ]}
            />
          </Space>
          {prefsError && <Alert type="warning" showIcon title={prefsError} />}
        </Space>
      </Card>

      <Card size="small" title={t("settings.storage")} style={{ marginBottom: 16 }}>
        {!settings ? (
          <Text type="secondary">—</Text>
        ) : (
          <Space orientation="vertical" size="middle" style={{ width: "100%" }}>
            <Space>
              <Text>{t("settings.ringSize")}</Text>
              <InputNumber
                min={1000}
                max={20000}
                step={1000}
                value={settings.log_ring_size}
                onChange={(value) =>
                  update((current) => ({
                    ...current,
                    settings: {
                      ...current.settings,
                      log_ring_size: value ?? current.settings.log_ring_size,
                    },
                  }))
                }
              />
              <Text type="secondary">{t("settings.ringSizeHint")}</Text>
            </Space>
            <Space>
              <Text>{t("settings.persist")}</Text>
              <Switch
                checked={settings.log_persist}
                onChange={(checked) =>
                  update((current) => ({
                    ...current,
                    settings: { ...current.settings, log_persist: checked },
                  }))
                }
              />
              <Text type="secondary">{t("settings.persistHint")}</Text>
            </Space>
            <Space>
              {dirty && <Tag color="gold">{t("settings.configDirty")}</Tag>}
              <Button
                type="primary"
                disabled={!dirty}
                onClick={() => void handleSaveConfig()}
              >
                {t("settings.saveConfig")}
              </Button>
              <Button onClick={reloadConfig}>{t("settings.refresh")}</Button>
            </Space>
          </Space>
        )}
      </Card>

      <Card size="small" title={t("settings.appInfo")} style={{ marginBottom: 16 }}>
        <Space orientation="vertical" size="middle" style={{ width: "100%" }}>
          <Descriptions
            size="small"
            column={1}
            items={[
              {
                key: "version",
                label: t("settings.version"),
                children: info?.app_version ?? "—",
              },
              {
                key: "sidecar",
                label: t("settings.sidecarVersion"),
                children: info?.sidecar_version || "—",
              },
              {
                key: "commit",
                label: t("settings.commit"),
                children: info?.commit || "—",
              },
              {
                key: "data",
                label: t("settings.data"),
                children: info ? <Text code>{info.data_dir}</Text> : "—",
              },
              {
                key: "config",
                label: t("settings.configPath"),
                children: info ? <Text code>{info.config_path}</Text> : "—",
              },
              {
                key: "querylog",
                label: t("settings.querylogPath"),
                children: info ? <Text code>{info.querylog_path}</Text> : "—",
              },
              {
                key: "log",
                label: t("settings.logPath"),
                children: info ? <Text code>{info.log_path}</Text> : "—",
              },
            ]}
          />
          <Space>
            <Button size="small" onClick={() => void runOpen(openDataDir)}>
              {t("settings.openDataDir")}
            </Button>
            <Button size="small" onClick={() => void runOpen(openConfigFile)}>
              {t("settings.openConfig")}
            </Button>
            <Button size="small" onClick={() => void refreshInfo()}>
              {t("settings.refresh")}
            </Button>
            <Button size="small" onClick={() => navigate("/wizard")}>
              {t("wizard.reopen")}
            </Button>
          </Space>
        </Space>
      </Card>

      <Card size="small" style={{ marginBottom: 16 }}>
        <Space orientation="vertical" size="middle">
          <Text strong>{t("settings.reset")}</Text>
          <Text type="secondary">{t("settings.resetContent")}</Text>
          <Button danger onClick={handleResetConfig}>
            {t("settings.reset")}
          </Button>
        </Space>
      </Card>

      {(configError || actionError) && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("settings.error", { message: configError ?? actionError ?? "" })}
        />
      )}
      {validationErrors.length > 0 && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("settings.validationErrors")}
          description={
            <ul style={{ margin: 0, paddingLeft: 20 }}>
              {validationErrors.map((item) => (
                <li key={`${item.path}-${item.code}`}>
                  {item.path}：{item.message}
                </li>
              ))}
            </ul>
          }
        />
      )}
      {warnings.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          description={
            <ul style={{ margin: 0, paddingLeft: 20 }}>
              {warnings.map((item) => (
                <li key={`${item.path}-${item.code}`}>{item.message}</li>
              ))}
            </ul>
          }
        />
      )}

      <AutostartPanel />
      <Divider />
    </>
  );
}
