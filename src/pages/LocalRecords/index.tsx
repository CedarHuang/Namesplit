import { useMemo, useState } from "react";
import {
  Alert,
  App,
  Button,
  Card,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Typography,
} from "antd";
import type { TableColumnsType } from "antd";

import { useI18n } from "../../i18n";
import { validateConfig } from "../../ipc/commands";
import { ipcErrorMessage } from "../../ipc/errors";
import type {
  LocalRecordConfig,
  LocalRecordType,
  ValidationDiagnostic,
} from "../../ipc/types";
import { useConfigDraft } from "../../state/useConfigDraft";
import { useUnsavedChanges } from "../../state/unsavedGuard";
import {
  filterRecords,
  keyedRecords,
  recordIssues,
  removeByKey,
  removeByKeys,
  replaceByKey,
  updateByKey,
  updateByKeys,
  type KeyedRecord,
} from "./local";

const { Paragraph, Text, Title } = Typography;

interface RecordFormValues {
  type: LocalRecordType;
  name: string;
  value: string;
  ttl?: number;
  enabled?: boolean;
}

const TYPE_COLOR: Record<LocalRecordType, string> = {
  A: "blue",
  AAAA: "cyan",
  CNAME: "purple",
  TXT: "gold",
  PTR: "green",
};

const TYPE_OPTIONS: LocalRecordType[] = ["A", "AAAA", "CNAME", "TXT", "PTR"];

export function LocalRecordsPage() {
  const { t } = useI18n();
  const { message, modal: confirmModal } = App.useApp();
  const { config, dirty, error, validationErrors, warnings, refresh, update, save } =
    useConfigDraft();
  useUnsavedChanges(dirty);
  const [form] = Form.useForm<RecordFormValues>();
  // 编辑按稳定 key 定位记录；数组下标在单行删除后会指向别的记录。
  const [modal, setModal] = useState<{ open: boolean; editingKey: string | null }>({
    open: false,
    editingKey: null,
  });
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [previewErrors, setPreviewErrors] = useState<ValidationDiagnostic[]>([]);

  const records = useMemo<LocalRecordConfig[]>(
    () => config?.local_records ?? [],
    [config],
  );
  const keyed = useMemo(() => keyedRecords(records), [records]);
  const issues = useMemo(() => recordIssues(records), [records]);
  const issuesByKey = useMemo(() => {
    const map = new Map<string, (typeof issues)[number]>();
    keyed.forEach((item, index) => map.set(item.key, issues[index] ?? []));
    return map;
  }, [issues, keyed]);

  const rows = useMemo(() => {
    const visible = new Set(filterRecords(records, search));
    return keyed.filter((item) => visible.has(item.record));
  }, [keyed, records, search]);

  const selectedKeys = useMemo(() => new Set(selected), [selected]);

  const warningMessages = useMemo(() => {
    const seen = new Set<string>();
    const messages: string[] = [];
    issues.forEach((rowIssues, index) => {
      for (const issue of rowIssues) {
        const key = `${index}:${issue.key}`;
        if (!seen.has(key)) {
          seen.add(key);
          messages.push(`${index + 1}: ${t(issue.key)}`);
        }
      }
    });
    return messages;
  }, [issues, t]);

  /** 页面内预校验结果优先展示；服务端保存失败时回退到 hook 返回的诊断。 */
  const shownErrors = previewErrors.length > 0 ? previewErrors : validationErrors;

  async function handleSave() {
    if (!config) {
      return;
    }
    setPreviewErrors([]);
    let validation;
    try {
      validation = await validateConfig(config);
    } catch (validateError) {
      message.error(ipcErrorMessage(validateError));
      return;
    }
    if (!validation.valid) {
      setPreviewErrors(validation.errors);
      message.error(t("local.validateFailed"));
      return;
    }
    const outcome = await save();
    if (outcome.ok) {
      message.success(t("local.saveSuccessFlushDns"));
    } else if (outcome.conflict) {
      message.warning(t("local.revisionConflict"));
    }
  }

  function reload() {
    if (!dirty) {
      setPreviewErrors([]);
      void refresh();
      return;
    }
    confirmModal.confirm({
      title: t("common.unsavedTitle"),
      content: t("common.reloadDiscardContent"),
      okText: t("local.reload"),
      cancelText: t("common.stay"),
      onOk: async () => {
        setPreviewErrors([]);
        await refresh();
      },
    });
  }

  function openAdd() {
    setModal({ open: true, editingKey: null });
    form.setFieldsValue({ type: "A", name: "", value: "", ttl: 60, enabled: true });
  }

  function openEdit(key: string, record: LocalRecordConfig) {
    setModal({ open: true, editingKey: key });
    form.setFieldsValue({
      type: record.type,
      name: record.name,
      value: record.value,
      ttl: record.ttl ?? 60,
      enabled: record.enabled ?? true,
    });
  }

  function submitForm() {
    form
      .validateFields()
      .then((values) => {
        const record: LocalRecordConfig = {
          type: values.type,
          name: values.name.trim(),
          value: values.value.trim(),
          ttl: values.ttl,
          enabled: values.enabled ?? true,
        };
        const editingKey = modal.editingKey;
        update((current) => {
          if (editingKey === null) {
            return {
              ...current,
              local_records: [...current.local_records, record],
            };
          }
          return {
            ...current,
            local_records: replaceByKey(current.local_records, editingKey, record),
          };
        });
        if (editingKey !== null) {
          // 编辑后内容变化，旧 key 失效；把它从选择集合里移除，避免悬空选择。
          setSelected((previous) =>
            previous.includes(editingKey)
              ? previous.filter((key) => key !== editingKey)
              : previous,
          );
        }
        setModal({ open: false, editingKey: null });
      })
      .catch(() => undefined);
  }

  function removeRow(key: string) {
    update((current) => ({
      ...current,
      local_records: removeByKey(current.local_records, key),
    }));
    setSelected((previous) =>
      previous.includes(key) ? previous.filter((item) => item !== key) : previous,
    );
  }

  function applyBatch(change: (record: LocalRecordConfig) => LocalRecordConfig) {
    update((current) => ({
      ...current,
      local_records: updateByKeys(current.local_records, selectedKeys, change),
    }));
    setSelected([]);
  }

  function batchDelete() {
    update((current) => ({
      ...current,
      local_records: removeByKeys(current.local_records, selectedKeys),
    }));
    setSelected([]);
  }

  function toggleEnabled(key: string, enabled: boolean) {
    update((current) => ({
      ...current,
      local_records: updateByKey(current.local_records, key, (record) => ({
        ...record,
        enabled,
      })),
    }));
    // 启停会改变记录内容，旧 key 随之失效；移出选择集合，避免"已选 N 条"与实际勾选不一致。
    setSelected((previous) =>
      previous.includes(key) ? previous.filter((item) => item !== key) : previous,
    );
  }

  const columns: TableColumnsType<KeyedRecord> = [
    {
      title: t("local.columns.type"),
      width: 90,
      render: (_value, row) => (
        <Tag color={TYPE_COLOR[row.record.type]}>{row.record.type}</Tag>
      ),
    },
    {
      title: t("local.columns.name"),
      width: 240,
      ellipsis: true,
      render: (_value, row) => <Text code>{row.record.name}</Text>,
    },
    {
      title: t("local.columns.value"),
      width: 260,
      ellipsis: true,
      render: (_value, row) => <Text code>{row.record.value}</Text>,
    },
    {
      title: t("local.columns.ttl"),
      width: 90,
      render: (_value, row) => row.record.ttl ?? 60,
    },
    {
      title: t("local.columns.enabled"),
      width: 90,
      render: (_value, row) => (
        <Switch
          size="small"
          checked={row.record.enabled ?? true}
          onChange={(checked) => toggleEnabled(row.key, checked)}
        />
      ),
    },
    {
      title: t("local.columns.warnings"),
      width: 110,
      render: (_value, row) => {
        const rowIssues = issuesByKey.get(row.key) ?? [];
        if (rowIssues.length === 0) {
          return <Text type="secondary">—</Text>;
        }
        return (
          <Text
            type={rowIssues.some((item) => item.level === "error") ? "danger" : "warning"}
            title={rowIssues.map((item) => t(item.key)).join("\n")}
          >
            {rowIssues.length}
          </Text>
        );
      },
    },
    {
      title: t("local.columns.actions"),
      width: 150,
      render: (_value, row) => (
        <Space>
          <Button size="small" onClick={() => openEdit(row.key, row.record)}>
            {t("local.edit")}
          </Button>
          <Popconfirm title={t("local.delete")} onConfirm={() => removeRow(row.key)}>
            <Button size="small" danger>
              {t("local.delete")}
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  if (!config) {
    return (
      <>
        <Title level={4}>{t("page.local.title")}</Title>
        {error ? (
          <Alert type="error" showIcon title={t("local.error", { message: error })} />
        ) : (
          <Text type="secondary">—</Text>
        )}
      </>
    );
  }

  return (
    <>
      <Space align="start" style={{ width: "100%", justifyContent: "space-between" }}>
        <div>
          <Title level={4} style={{ marginBottom: 4 }}>
            {t("page.local.title")}
          </Title>
          <Paragraph type="secondary">{t("page.local.description")}</Paragraph>
        </div>
        <Space>
          {dirty && <Tag color="gold">{t("local.dirty")}</Tag>}
          <Space size={4}>
            <Text>{t("local.ptrAuto")}</Text>
            <Switch
              checked={config.settings.ptr_auto}
              onChange={(checked) =>
                update((current) => ({
                  ...current,
                  settings: { ...current.settings, ptr_auto: checked },
                }))
              }
            />
          </Space>
          <Button onClick={reload}>{t("local.reload")}</Button>
          <Button onClick={openAdd}>{t("local.add")}</Button>
          <Button type="primary" disabled={!dirty} onClick={() => void handleSave()}>
            {t("local.save")}
          </Button>
        </Space>
      </Space>
      <Paragraph type="secondary">{t("local.ptrHint")}</Paragraph>

      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("local.error", { message: error })}
        />
      )}
      {shownErrors.length > 0 && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("local.validationErrors")}
          description={
            <ul style={{ margin: 0, paddingLeft: 20 }}>
              {shownErrors.map((item) => (
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
      {warningMessages.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("local.warnings.title")}
          description={
            <ul style={{ margin: 0, paddingLeft: 20 }}>
              {warningMessages.map((item) => (
                <li key={item}>{item}</li>
              ))}
            </ul>
          }
        />
      )}

      <Card size="small">
        <Space wrap style={{ marginBottom: 12 }}>
          <Input
            allowClear
            style={{ width: 260 }}
            placeholder={t("local.search")}
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
          {selected.length > 0 && (
            <>
              <Text type="secondary">
                {t("local.batchSelected", { count: selected.length })}
              </Text>
              <Popconfirm title={t("local.batchDelete")} onConfirm={batchDelete}>
                <Button size="small" danger>
                  {t("local.batchDelete")}
                </Button>
              </Popconfirm>
              <Button
                size="small"
                onClick={() => applyBatch((record) => ({ ...record, enabled: true }))}
              >
                {t("local.batchEnable")}
              </Button>
              <Button
                size="small"
                onClick={() => applyBatch((record) => ({ ...record, enabled: false }))}
              >
                {t("local.batchDisable")}
              </Button>
            </>
          )}
        </Space>
        <Table<KeyedRecord>
          rowKey={(row) => row.key}
          rowSelection={{
            selectedRowKeys: selected,
            onChange: (keys) => setSelected(keys.map(String)),
          }}
          size="small"
          columns={columns}
          dataSource={rows}
          pagination={false}
          scroll={{ x: 1030 }}
          locale={{ emptyText: t("local.empty") }}
        />
      </Card>

      <Modal
        open={modal.open}
        title={
          modal.editingKey === null
            ? t("local.form.addTitle")
            : t("local.form.editTitle")
        }
        okText={t("local.form.ok")}
        cancelText={t("local.form.cancel")}
        onOk={submitForm}
        onCancel={() => setModal({ open: false, editingKey: null })}
      >
        <Form form={form} layout="vertical">
          <Form.Item
            name="type"
            label={t("local.form.type")}
            rules={[{ required: true, message: t("local.form.typeRequired") }]}
          >
            <Select
              options={TYPE_OPTIONS.map((value) => ({ value, label: value }))}
            />
          </Form.Item>
          <Form.Item
            name="name"
            label={t("local.form.name")}
            extra={t("local.form.nameHint")}
            rules={[{ required: true, message: t("local.form.nameRequired") }]}
          >
            <Input placeholder="nas.home" />
          </Form.Item>
          <Form.Item
            name="value"
            label={t("local.form.value")}
            rules={[{ required: true, message: t("local.form.valueRequired") }]}
          >
            <Input placeholder="192.168.1.10" />
          </Form.Item>
          <Form.Item name="ttl" label={t("local.form.ttl")}>
            <InputNumber min={0} max={86400} style={{ width: 180 }} />
          </Form.Item>
          <Form.Item name="enabled" label={t("local.form.enabled")} valuePropName="checked">
            <Switch />
          </Form.Item>
        </Form>
      </Modal>
    </>
  );
}
