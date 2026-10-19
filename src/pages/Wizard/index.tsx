import { useState } from "react";
import {
  Alert,
  Button,
  Card,
  Layout,
  Radio,
  Select,
  Space,
  Steps,
  Typography,
} from "antd";
import { useNavigate } from "react-router-dom";

import { markWizardCompleted } from "../../app/FirstRunGate";
import { useI18n } from "../../i18n";
import { getConfig, saveConfig } from "../../ipc/commands";
import { ipcErrorCode, ipcErrorMessage } from "../../ipc/errors";
import { openSystemNetworkSettings } from "../../systemDns";
import { useAppState } from "../../state/appState";
import { applyTemplate } from "./template";

const { Content } = Layout;
const { Paragraph, Text, Title } = Typography;

type TemplateKey = "udp" | "doh" | "custom";

export function WizardPage() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const { systemDns } = useAppState();
  const [step, setStep] = useState(0);
  const [template, setTemplate] = useState<TemplateKey>("udp");
  const [applying, setApplying] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selectedInterface, setSelectedInterface] = useState<string | null>(null);

  const interfaces = systemDns?.interfaces ?? [];
  const pointingCount = interfaces.filter((item) => item.points_to_loopback).length;
  // 默认选已指向本机的第一块网卡，否则第一块；用户主动切换后以所选为准。
  const defaultInterface =
    interfaces.find((item) => item.points_to_loopback)?.name ?? interfaces[0]?.name ?? null;
  const activeInterface = interfaces.some((item) => item.name === selectedInterface)
    ? selectedInterface
    : defaultInterface;
  const commands =
    activeInterface === null
      ? []
      : [
          `netsh interface ipv4 set dnsservers name="${activeInterface}" static 127.0.0.1 primary`,
          `netsh interface ipv6 set dnsservers name="${activeInterface}" static ::1 primary`,
        ];

  function finish() {
    markWizardCompleted();
    navigate("/overview", { replace: true });
  }

  async function applySelectedTemplate(): Promise<boolean> {
    if (template === "custom") {
      return true;
    }
    setApplying(true);
    try {
      const envelope = await getConfig();
      await saveConfig(envelope.revision, applyTemplate(envelope.config, template));
      setError(null);
      return true;
    } catch (applyError) {
      if (ipcErrorCode(applyError) === "revision_conflict") {
        setError(t("settings.revisionConflict"));
      } else {
        setError(t("wizard.applyError", { message: ipcErrorMessage(applyError) }));
      }
      return false;
    } finally {
      setApplying(false);
    }
  }

  async function next() {
    if (step === 0) {
      if (await applySelectedTemplate()) {
        setStep(1);
      }
      return;
    }
    if (step === 1) {
      setStep(2);
      return;
    }
    finish();
  }

  return (
    <Layout style={{ minHeight: "100vh" }}>
      <Content style={{ maxWidth: 880, width: "100%", margin: "0 auto", padding: 32 }}>
        <Space
          align="start"
          style={{ width: "100%", justifyContent: "space-between", marginBottom: 24 }}
        >
          <Title level={3} style={{ margin: 0 }}>
            {t("wizard.title")}
          </Title>
          <Button onClick={finish}>{t("wizard.skip")}</Button>
        </Space>

        <Steps
          current={step}
          items={[
            { title: t("wizard.step.template") },
            { title: t("wizard.step.dns") },
            { title: t("wizard.step.done") },
          ]}
          style={{ marginBottom: 24 }}
        />

        {error && (
          <Alert
            type="error"
            showIcon
            style={{ marginBottom: 16 }}
            title={error}
          />
        )}

        {step === 0 && (
          <Card>
            <Radio.Group
              value={template}
              onChange={(event) => setTemplate(event.target.value as TemplateKey)}
              style={{ width: "100%" }}
            >
              <Space orientation="vertical" size="middle" style={{ width: "100%" }}>
                {(
                  [
                    ["udp", t("wizard.template.udp"), t("wizard.template.udp.desc")],
                    ["doh", t("wizard.template.doh"), t("wizard.template.doh.desc")],
                    [
                      "custom",
                      t("wizard.template.custom"),
                      t("wizard.template.custom.desc"),
                    ],
                  ] as const
                ).map(([key, label, description]) => (
                  <Radio key={key} value={key}>
                    <Space orientation="vertical" size={0}>
                      <Text strong>{label}</Text>
                      <Text type="secondary">{description}</Text>
                    </Space>
                  </Radio>
                ))}
              </Space>
            </Radio.Group>
            <Paragraph type="secondary" style={{ marginTop: 16, marginBottom: 0 }}>
              {t("wizard.template.note")}
            </Paragraph>
          </Card>
        )}

        {step === 1 && (
          <Card title={t("wizard.dns.title")}>
            <Space orientation="vertical" size="middle" style={{ width: "100%" }}>
              <Text strong>{t("wizard.dns.addresses")}</Text>
              <Text code>{t("wizard.dns.ipv4")}</Text>
              <Text code>{t("wizard.dns.ipv6")}</Text>
              <Text code>{t("wizard.dns.port")}</Text>
              <Text type="secondary">{t("wizard.dns.note")}</Text>
              <Space>
                <Button onClick={() => void openSystemNetworkSettings()}>
                  {t("wizard.dns.openSettings")}
                </Button>
              </Space>
              <Text strong>{t("wizard.dns.interface")}</Text>
              {interfaces.length === 0 ? (
                <Alert type="warning" showIcon title={t("wizard.dns.noInterface")} />
              ) : (
                <Select
                  style={{ maxWidth: 360, width: "100%" }}
                  value={activeInterface ?? undefined}
                  onChange={(value) => setSelectedInterface(value)}
                  options={interfaces.map((item) => ({
                    value: item.name,
                    label: item.name,
                  }))}
                />
              )}
              <Text type="secondary">{t("wizard.dns.interfaceHint")}</Text>
              {commands.length > 0 && (
                <>
                  <Text strong>{t("wizard.dns.commands")}</Text>
                  {commands.map((command) => (
                    <Text key={command} code copyable={{ text: command }}>
                      {command}
                    </Text>
                  ))}
                </>
              )}
              <Text strong>{t("wizard.dns.current")}</Text>
              {pointingCount > 0 ? (
                <Alert
                  type="success"
                  showIcon
                  title={t("wizard.dns.pointing", { count: pointingCount })}
                />
              ) : (
                <Alert type="info" showIcon title={t("wizard.dns.notPointing")} />
              )}
            </Space>
          </Card>
        )}

        {step === 2 && (
          <Card title={t("wizard.done.title")}>
            <Space orientation="vertical" size="middle">
              <Text>
                {template === "custom"
                  ? t("wizard.done.custom")
                  : t("wizard.done.template", {
                      template: t(
                        template === "udp"
                          ? "wizard.template.udp"
                          : "wizard.template.doh",
                      ),
                    })}
              </Text>
              <Text type="secondary">{t("wizard.done.hint")}</Text>
            </Space>
          </Card>
        )}

        <Space style={{ marginTop: 24 }}>
          {step > 0 && (
            <Button onClick={() => setStep((current) => current - 1)}>
              {t("wizard.back")}
            </Button>
          )}
          <Button type="primary" loading={applying} onClick={() => void next()}>
            {step === 2
              ? t("wizard.finish")
              : step === 0
                ? t("wizard.applyTemplate")
                : t("wizard.next")}
          </Button>
        </Space>
      </Content>
    </Layout>
  );
}
