import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  App,
  Button,
  Card,
  Descriptions,
  Form,
  Input,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from "antd";
import type { TableColumnsType } from "antd";

import { useI18n } from "../../i18n";
import { getConfig, saveConfig, testRule } from "../../ipc/commands";
import { ipcErrorCode, ipcErrorPayload, ipcErrorMessage } from "../../ipc/errors";
import type {
  ConfigEnvelope,
  RuleConfig,
  RuleTestResult,
  ValidationDiagnostic,
} from "../../ipc/types";
import { isTauri } from "../../portStatus";
import { useUnsavedChanges } from "../../state/unsavedGuard";
import { moveRule, warningsForRule } from "./rules";

const { Paragraph, Text, Title } = Typography;

interface RuleFormValues {
  match: "DOMAIN" | "DOMAIN-SUFFIX";
  domain: string;
  group: string;
}

function extractDiagnostics(details: unknown): ValidationDiagnostic[] {
  if (!Array.isArray(details)) {
    return [];
  }
  return details.filter(
    (item): item is ValidationDiagnostic =>
      typeof item === "object" &&
      item !== null &&
      typeof (item as ValidationDiagnostic).code === "string" &&
      typeof (item as ValidationDiagnostic).message === "string",
  );
}

export function RulesPage() {
  const { t } = useI18n();
  const { message, modal } = App.useApp();
  const [form] = Form.useForm<RuleFormValues>();
  // 拖拽与编辑都按规则对象身份定位：数组下标会在增删/排序后指向别的规则。
  const draggedRule = useRef<RuleConfig | null>(null);

  const [envelope, setEnvelope] = useState<ConfigEnvelope | null>(null);
  const [revision, setRevision] = useState(0);
  const [rules, setRules] = useState<RuleConfig[]>([]);
  const [warnings, setWarnings] = useState<ValidationDiagnostic[]>([]);
  const [dirty, setDirty] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [validationErrors, setValidationErrors] = useState<ValidationDiagnostic[]>([]);
  const [modalOpen, setModalOpen] = useState(false);
  const [editingRule, setEditingRule] = useState<RuleConfig | null>(null);

  const [testDomain, setTestDomain] = useState("");
  const [testQtype, setTestQtype] = useState("A");
  const [testResult, setTestResult] = useState<RuleTestResult | null>(null);
  const [testError, setTestError] = useState<string | null>(null);
  useUnsavedChanges(dirty);

  const refresh = useCallback(async () => {
    try {
      const next = await getConfig();
      setEnvelope(next);
      setRevision(next.revision);
      setRules(next.config.rules);
      setWarnings(next.warnings);
      setDirty(false);
      setError(null);
      setValidationErrors([]);
    } catch (loadError) {
      setError(ipcErrorMessage(loadError));
    }
  }, []);

  useEffect(() => {
    if (isTauri()) {
      void refresh();
    }
  }, [refresh]);

  const groupOptions = useMemo(() => {
    if (!envelope) {
      return [];
    }
    return Object.entries(envelope.config.groups).map(([id, group]) => ({
      value: id,
      label: `${group.name} (${id})`,
    }));
  }, [envelope]);

  const defaultGroup = envelope?.config.settings.default_group ?? "";

  async function handleSave() {
    if (!envelope) {
      return;
    }
    const nextConfig = { ...envelope.config, rules };
    try {
      const result = await saveConfig(revision, nextConfig);
      setRevision(result.revision);
      setWarnings(result.warnings);
      setEnvelope({
        ...envelope,
        revision: result.revision,
        config: nextConfig,
        warnings: result.warnings,
      });
      setDirty(false);
      setValidationErrors([]);
      setError(null);
      message.success(t("rules.saveSuccess"));
    } catch (saveError) {
      const payload = ipcErrorPayload(saveError);
      if (ipcErrorCode(saveError) === "revision_conflict") {
        // 冲突时只把基线更新到最新，保留草稿与 dirty，让用户核对后重试。
        try {
          const latest = await getConfig();
          setEnvelope(latest);
          setRevision(latest.revision);
          setWarnings(latest.warnings);
          message.warning(t("rules.revisionConflict"));
        } catch (conflictError) {
          setError(ipcErrorMessage(conflictError));
        }
        return;
      }
      setError(ipcErrorMessage(saveError));
      setValidationErrors(extractDiagnostics(payload?.details));
    }
  }

  function reload() {
    if (!dirty) {
      void refresh();
      return;
    }
    modal.confirm({
      title: t("common.unsavedTitle"),
      content: t("common.reloadDiscardContent"),
      okText: t("rules.reload"),
      cancelText: t("common.stay"),
      onOk: () => refresh(),
    });
  }

  function openAdd() {
    setEditingRule(null);
    form.setFieldsValue({
      match: "DOMAIN-SUFFIX",
      domain: "",
      group: defaultGroup,
    });
    setModalOpen(true);
  }

  function openEdit(rule: RuleConfig) {
    setEditingRule(rule);
    form.setFieldsValue(rule);
    setModalOpen(true);
  }

  function submitForm() {
    form
      .validateFields()
      .then((values) => {
        const nextRule: RuleConfig = { ...values };
        // 用对象身份在当前数组里定位被编辑行；行已被删除时忽略本次提交。
        const index = editingRule === null ? -1 : rules.indexOf(editingRule);
        if (editingRule !== null && index < 0) {
          setModalOpen(false);
          setEditingRule(null);
          return;
        }
        setRules(
          editingRule === null
            ? [...rules, nextRule]
            : rules.map((rule, ruleIndex) => (ruleIndex === index ? nextRule : rule)),
        );
        setDirty(true);
        setModalOpen(false);
        setEditingRule(null);
      })
      .catch(() => undefined);
  }

  function removeRule(rule: RuleConfig) {
    setRules((current) => current.filter((currentRule) => currentRule !== rule));
    setDirty(true);
  }

  async function runTest() {
    if (!testDomain.trim()) {
      return;
    }
    try {
      const result = await testRule(testDomain.trim(), testQtype || undefined);
      setTestResult(result);
      setTestError(null);
    } catch (runError) {
      setTestError(ipcErrorMessage(runError));
    }
  }

  const columns = useMemo<TableColumnsType<RuleConfig>>(
    () => [
      {
        title: t("rules.columns.order"),
        width: 70,
        render: (_value, _row, index) => (
          <Text type="secondary" style={{ cursor: "grab" }}>
            {index + 1}
          </Text>
        ),
      },
      {
        title: t("rules.columns.match"),
        dataIndex: "match",
        width: 150,
        render: (value: string) => (
          <Tag color={value === "DOMAIN" ? "blue" : "purple"}>{value}</Tag>
        ),
      },
      {
        title: t("rules.columns.domain"),
        dataIndex: "domain",
        render: (value: string) => <Text code>{value}</Text>,
      },
      {
        title: t("rules.columns.group"),
        dataIndex: "group",
        width: 180,
      },
      {
        title: t("rules.columns.warnings"),
        width: 100,
        render: (_value, _row, index) => {
          const rowWarnings = warningsForRule(warnings, index);
          if (rowWarnings.length === 0) {
            return <Text type="secondary">—</Text>;
          }
          return (
            <Text type="warning" title={rowWarnings.map((item) => item.message).join("\n")}>
              {rowWarnings.length}
            </Text>
          );
        },
      },
      {
        title: t("rules.columns.actions"),
        width: 160,
        render: (_value, row) => (
          <Space>
            <Button size="small" onClick={() => openEdit(row)}>
              {t("rules.edit")}
            </Button>
            <Popconfirm
              title={t("rules.delete")}
              onConfirm={() => removeRule(row)}
            >
              <Button size="small" danger>
                {t("rules.delete")}
              </Button>
            </Popconfirm>
          </Space>
        ),
      },
    ],
    [t, warnings],
  );

  return (
    <>
      <Space align="start" style={{ width: "100%", justifyContent: "space-between" }}>
        <div>
          <Title level={4} style={{ marginBottom: 4 }}>
            {t("page.rules.title")}
          </Title>
          <Paragraph type="secondary">{t("page.rules.description")}</Paragraph>
        </div>
        <Space>
          {dirty && <Tag color="gold">{t("rules.dirty")}</Tag>}
          <Button onClick={reload}>{t("rules.reload")}</Button>
          <Button onClick={openAdd}>{t("rules.add")}</Button>
          <Button type="primary" disabled={!dirty} onClick={() => void handleSave()}>
            {t("rules.save")}
          </Button>
        </Space>
      </Space>

      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("rules.error", { message: error })}
        />
      )}
      {validationErrors.length > 0 && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("rules.validationErrors")}
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
          title={t("rules.shadow.title")}
          description={
            <ul style={{ margin: 0, paddingLeft: 20 }}>
              {warnings.map((item) => (
                <li key={`${item.path}-${item.code}`}>{item.message}</li>
              ))}
            </ul>
          }
        />
      )}

      <Card size="small" style={{ marginBottom: 16 }}>
        <Table<RuleConfig>
          rowKey={(rule) => `${rule.match}:${rule.domain}`}
          size="small"
          columns={columns}
          dataSource={rules}
          pagination={false}
          locale={{ emptyText: t("rules.empty") }}
          onRow={(rule) => ({
            draggable: true,
            onDragStart: (event) => {
              draggedRule.current = rule;
              event.dataTransfer.effectAllowed = "move";
            },
            onDragOver: (event) => {
              event.preventDefault();
              event.dataTransfer.dropEffect = "move";
            },
            onDrop: (event) => {
              event.preventDefault();
              const moved = draggedRule.current;
              draggedRule.current = null;
              if (moved === null || moved === rule) {
                return;
              }
              const from = rules.indexOf(moved);
              const to = rules.indexOf(rule);
              if (from < 0 || to < 0 || from === to) {
                return;
              }
              setRules(moveRule(rules, from, to));
              setDirty(true);
            },
            onDragEnd: () => {
              draggedRule.current = null;
            },
          })}
        />
      </Card>

      <Card size="small" title={t("rules.test.title")}>
        <Space wrap style={{ marginBottom: 12 }}>
          <Input
            style={{ width: 280 }}
            placeholder={t("rules.test.domain")}
            value={testDomain}
            onChange={(event) => setTestDomain(event.target.value)}
            onPressEnter={() => void runTest()}
          />
          <Input
            style={{ width: 160 }}
            placeholder={t("rules.test.qtype")}
            value={testQtype}
            onChange={(event) => setTestQtype(event.target.value)}
          />
          <Button onClick={() => void runTest()} disabled={!testDomain.trim()}>
            {t("rules.test.run")}
          </Button>
        </Space>
        <Paragraph type="secondary">{t("rules.test.hint")}</Paragraph>
        {testError && <Alert type="warning" showIcon title={testError} />}
        {testResult && (
          <>
            <Descriptions
              size="small"
              column={1}
              items={[
                {
                  key: "canonical",
                  label: t("rules.test.canonical"),
                  children: testResult.canonical,
                },
                {
                  key: "local",
                  label: t("rules.test.local"),
                  children: testResult.local.hit
                    ? testResult.local.nodata
                      ? "NODATA"
                      : (testResult.local.answers ?? []).join(", ")
                    : t("rules.test.noLocal"),
                },
                {
                  key: "rule",
                  label: t("rules.test.rule"),
                  children: testResult.rule
                    ? `${testResult.rule.match} ${testResult.rule.domain} → ${testResult.rule.group}`
                    : t("rules.test.defaultRule"),
                },
                {
                  key: "target",
                  label: t("rules.test.target"),
                  children: testResult.target_group,
                },
                {
                  key: "default",
                  label: t("rules.test.defaultGroup"),
                  children: testResult.default_group,
                },
              ]}
            />
            {testResult.warnings.length > 0 && (
              <Alert
                type="warning"
                showIcon
                style={{ marginTop: 12 }}
                title={t("rules.test.warnings")}
                description={
                  <ul style={{ margin: 0, paddingLeft: 20 }}>
                    {testResult.warnings.map((item) => (
                      <li key={`${item.path}-${item.code}`}>{item.message}</li>
                    ))}
                  </ul>
                }
              />
            )}
          </>
        )}
      </Card>

      <Modal
        open={modalOpen}
        title={
          editingRule === null
            ? t("rules.form.addTitle")
            : t("rules.form.editTitle")
        }
        okText={t("rules.form.ok")}
        cancelText={t("rules.form.cancel")}
        onOk={submitForm}
        onCancel={() => {
          setModalOpen(false);
          setEditingRule(null);
        }}
      >
        <Form form={form} layout="vertical">
          <Form.Item
            name="match"
            label={t("rules.form.match")}
            rules={[{ required: true }]}
          >
            <Select
              options={[
                { value: "DOMAIN", label: "DOMAIN" },
                { value: "DOMAIN-SUFFIX", label: "DOMAIN-SUFFIX" },
              ]}
            />
          </Form.Item>
          <Form.Item
            name="domain"
            label={t("rules.form.domain")}
            rules={[{ required: true, message: t("rules.form.domainRequired") }]}
          >
            <Input placeholder="corp.local" />
          </Form.Item>
          <Form.Item
            name="group"
            label={t("rules.form.group")}
            rules={[{ required: true, message: t("rules.form.groupRequired") }]}
          >
            <Select options={groupOptions} placeholder={defaultGroup} />
          </Form.Item>
        </Form>
      </Modal>
    </>
  );
}
