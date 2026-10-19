import { useMemo, useState } from "react";
import {
  Alert,
  App,
  Button,
  Card,
  Col,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Row,
  Select,
  Space,
  Table,
  Tag,
  Typography,
} from "antd";
import type { TableColumnsType } from "antd";

import { useI18n } from "../../i18n";
import { testUpstream } from "../../ipc/commands";
import type {
  GroupConfig,
  UpstreamConfig,
  UpstreamTestResult,
} from "../../ipc/types";
import { ipcErrorMessage } from "../../ipc/errors";
import { useConfigDraft } from "../../state/useConfigDraft";
import { useUnsavedChanges } from "../../state/unsavedGuard";
import { idIssue } from "./validation";

const { Paragraph, Text, Title } = Typography;

interface ServerFormValues {
  id: string;
  name: string;
  url: string;
  timeout_ms?: number;
  bootstrap?: string;
  remark?: string;
}

interface GroupFormValues {
  id: string;
  name: string;
  upstreams: string[];
}

interface TestState {
  loading: boolean;
  results?: UpstreamTestResult[];
  error?: string;
}

function parseBootstrap(value: string | undefined): string[] | undefined {
  const items = (value ?? "")
    .split(/[\s,]+/)
    .map((item) => item.trim())
    .filter(Boolean);
  return items.length > 0 ? items : undefined;
}

export function UpstreamsPage() {
  const { t } = useI18n();
  const { message, modal } = App.useApp();
  const { config, dirty, error, validationErrors, warnings, refresh, update, save } =
    useConfigDraft();
  useUnsavedChanges(dirty);
  const [serverForm] = Form.useForm<ServerFormValues>();
  const [groupForm] = Form.useForm<GroupFormValues>();
  const [serverModal, setServerModal] = useState<{ open: boolean; editId: string | null }>({
    open: false,
    editId: null,
  });
  const [groupModal, setGroupModal] = useState<{ open: boolean; editId: string | null }>({
    open: false,
    editId: null,
  });
  const [tests, setTests] = useState<Record<string, TestState>>({});

  const servers = useMemo(
    () => Object.entries(config?.upstreams ?? {}),
    [config],
  );
  const groups = useMemo(() => Object.entries(config?.groups ?? {}), [config]);
  const defaultGroup = config?.settings.default_group ?? "";

  async function handleSave() {
    const outcome = await save();
    if (outcome.ok) {
      message.success(t("upstreams.saveSuccess"));
    } else if (outcome.conflict) {
      message.warning(t("upstreams.revisionConflict"));
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
      okText: t("upstreams.reload"),
      cancelText: t("common.stay"),
      onOk: () => refresh(),
    });
  }

  function openAddServer() {
    setServerModal({ open: true, editId: null });
    serverForm.setFieldsValue({
      id: "",
      name: "",
      url: "",
      timeout_ms: 5000,
      bootstrap: "",
      remark: "",
    });
  }

  function openEditServer(id: string, server: UpstreamConfig) {
    setServerModal({ open: true, editId: id });
    serverForm.setFieldsValue({
      id,
      name: server.name,
      url: server.url,
      timeout_ms: server.timeout_ms,
      bootstrap: server.bootstrap?.join(", "),
      remark: server.remark,
    });
  }

  function submitServer() {
    serverForm
      .validateFields()
      .then((values) => {
        const id = values.id.trim();
        if (serverModal.editId === null) {
          const issue = idIssue(id, Object.keys(config?.upstreams ?? {}));
          if (issue !== null) {
            message.error(
              issue === "invalid"
                ? t("upstreams.idInvalid")
                : t("upstreams.idExists", { id }),
            );
            return;
          }
        }
        const server: UpstreamConfig = {
          name: values.name.trim(),
          url: values.url.trim(),
          timeout_ms: values.timeout_ms,
          bootstrap: parseBootstrap(values.bootstrap),
          remark: values.remark?.trim() || undefined,
        };
        update((current) => ({
          ...current,
          upstreams: { ...current.upstreams, [id]: server },
        }));
        setServerModal({ open: false, editId: null });
      })
      .catch(() => undefined);
  }

  function deleteServer(id: string) {
    const referenced = groups.some(([, group]) => group.upstreams.includes(id));
    if (referenced) {
      message.warning(t("upstreams.deleteBlockedUpstream"));
      return;
    }
    update((current) => {
      const upstreams = { ...current.upstreams };
      delete upstreams[id];
      return { ...current, upstreams };
    });
  }

  function openAddGroup() {
    setGroupModal({ open: true, editId: null });
    groupForm.setFieldsValue({ id: "", name: "", upstreams: [] });
  }

  function openEditGroup(id: string, group: GroupConfig) {
    setGroupModal({ open: true, editId: id });
    groupForm.setFieldsValue({ id, name: group.name, upstreams: group.upstreams });
  }

  function submitGroup() {
    groupForm
      .validateFields()
      .then((values) => {
        const id = values.id.trim();
        if (groupModal.editId === null) {
          const issue = idIssue(id, Object.keys(config?.groups ?? {}));
          if (issue !== null) {
            message.error(
              issue === "invalid"
                ? t("upstreams.idInvalid")
                : t("upstreams.idExists", { id }),
            );
            return;
          }
        }
        const group: GroupConfig = {
          name: values.name.trim(),
          upstreams: values.upstreams,
        };
        update((current) => ({
          ...current,
          groups: { ...current.groups, [id]: group },
        }));
        setGroupModal({ open: false, editId: null });
      })
      .catch(() => undefined);
  }

  function deleteGroup(id: string) {
    const referencedByRule = config?.rules.some((rule) => rule.group === id) ?? false;
    if (id === defaultGroup || referencedByRule) {
      message.warning(t("upstreams.deleteBlockedGroup"));
      return;
    }
    update((current) => {
      const groups = { ...current.groups };
      delete groups[id];
      return { ...current, groups };
    });
  }

  function setDefaultGroup(id: string) {
    update((current) => ({
      ...current,
      settings: { ...current.settings, default_group: id },
    }));
  }

  async function runTest(key: string, request: { upstream_id?: string; group_id?: string }) {
    setTests((current) => ({ ...current, [key]: { loading: true } }));
    try {
      const response = await testUpstream(request);
      setTests((current) => ({
        ...current,
        [key]: { loading: false, results: response.results },
      }));
    } catch (testError) {
      setTests((current) => ({
        ...current,
        [key]: { loading: false, error: ipcErrorMessage(testError) },
      }));
    }
  }

  function renderTest(key: string) {
    const state = tests[key];
    if (!state) {
      return null;
    }
    if (state.loading) {
      return <Text type="secondary">{t("upstreams.test.running")}</Text>;
    }
    if (state.error) {
      return <Text type="danger">{state.error}</Text>;
    }
    return (
      <Space wrap size={4}>
        {state.results?.map((result) => (
          <Tag key={`${result.name}-${result.address}`} color={result.ok ? "green" : "red"}>
            {result.name || result.address} ·{" "}
            {result.ok ? `${result.latency_ms.toFixed(1)} ms` : t("upstreams.test.fail")}
            {result.tls_ok === false ? ` · ${t("upstreams.test.tls")} ✗` : ""}
            {result.error ? ` · ${result.error}` : ""}
          </Tag>
        ))}
      </Space>
    );
  }

  const serverColumns: TableColumnsType<[string, UpstreamConfig]> = [
    {
      title: t("upstreams.columns.id"),
      width: 130,
      render: (_value, row) => <Text code>{row[0]}</Text>,
    },
    {
      title: t("upstreams.columns.name"),
      width: 150,
      render: (_value, row) => row[1].name,
    },
    {
      title: t("upstreams.columns.url"),
      width: 260,
      ellipsis: true,
      render: (_value, row) => <Text code>{row[1].url}</Text>,
    },
    {
      title: t("upstreams.columns.timeout"),
      width: 100,
      render: (_value, row) => (row[1].timeout_ms ? `${row[1].timeout_ms} ms` : "—"),
    },
    {
      title: t("upstreams.columns.bootstrap"),
      width: 170,
      render: (_value, row) => row[1].bootstrap?.join(", ") || "—",
    },
    {
      title: t("upstreams.columns.remark"),
      width: 140,
      render: (_value, row) => row[1].remark || "—",
    },
    {
      title: t("upstreams.columns.test"),
      width: 220,
      render: (_value, row) => renderTest(`server:${row[0]}`),
    },
    {
      title: t("upstreams.columns.actions"),
      width: 220,
      render: (_value, row) => {
        const id = row[0];
        return (
          <Space>
            <Button
              size="small"
              loading={tests[`server:${id}`]?.loading}
              onClick={() => void runTest(`server:${id}`, { upstream_id: id })}
            >
              {t("upstreams.test")}
            </Button>
            <Button size="small" onClick={() => openEditServer(id, row[1])}>
              {t("upstreams.edit")}
            </Button>
            <Popconfirm
              title={t("upstreams.delete")}
              onConfirm={() => deleteServer(id)}
            >
              <Button size="small" danger>
                {t("upstreams.delete")}
              </Button>
            </Popconfirm>
          </Space>
        );
      },
    },
  ];

  if (!config) {
    return (
      <>
        <Title level={4}>{t("page.upstreams.title")}</Title>
        {error ? (
          <Alert type="error" showIcon title={t("upstreams.error", { message: error })} />
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
            {t("page.upstreams.title")}
          </Title>
          <Paragraph type="secondary">{t("page.upstreams.description")}</Paragraph>
        </div>
        <Space>
          {dirty && <Tag color="gold">{t("upstreams.dirty")}</Tag>}
          <Button onClick={reload}>{t("upstreams.reload")}</Button>
          <Button onClick={openAddServer}>{t("upstreams.add")}</Button>
          <Button onClick={openAddGroup}>{t("upstreams.addGroup")}</Button>
          <Button type="primary" disabled={!dirty} onClick={() => void handleSave()}>
            {t("upstreams.save")}
          </Button>
        </Space>
      </Space>

      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("upstreams.error", { message: error })}
        />
      )}
      {validationErrors.length > 0 && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          title={t("upstreams.validationErrors")}
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

      <Card size="small" title={t("upstreams.servers")} style={{ marginBottom: 16 }}>
        <Table<[string, UpstreamConfig]>
          rowKey={(row) => row[0]}
          size="small"
          columns={serverColumns}
          dataSource={servers}
          pagination={false}
          scroll={{ x: 1390 }}
          locale={{ emptyText: t("upstreams.emptyServers") }}
        />
      </Card>

      <Card size="small" title={t("upstreams.groups")}>
        {groups.length === 0 ? (
          <Text type="secondary">{t("upstreams.emptyGroups")}</Text>
        ) : (
          <Row gutter={[16, 16]}>
            {groups.map(([id, group]) => {
              const key = `group:${id}`;
              const referencedByRule = config.rules.filter((rule) => rule.group === id).length;
              return (
                <Col xs={24} md={12} key={id}>
                  <Card
                    size="small"
                    title={
                      <Space wrap>
                        <Text strong>{group.name}</Text>
                        <Text code>{id}</Text>
                        {id === defaultGroup && (
                          <Tag color="blue">{t("upstreams.group.default")}</Tag>
                        )}
                        {referencedByRule > 0 && (
                          <Tag>{t("rules.columns.group")} {referencedByRule}</Tag>
                        )}
                      </Space>
                    }
                    extra={
                      <Space>
                        <Button
                          size="small"
                          loading={tests[key]?.loading}
                          onClick={() => void runTest(key, { group_id: id })}
                        >
                          {t("upstreams.test")}
                        </Button>
                        <Button size="small" onClick={() => openEditGroup(id, group)}>
                          {t("upstreams.edit")}
                        </Button>
                        {id !== defaultGroup && (
                          <Button size="small" onClick={() => setDefaultGroup(id)}>
                            {t("upstreams.group.setDefault")}
                          </Button>
                        )}
                        <Popconfirm
                          title={t("upstreams.delete")}
                          onConfirm={() => deleteGroup(id)}
                        >
                          <Button size="small" danger>
                            {t("upstreams.delete")}
                          </Button>
                        </Popconfirm>
                      </Space>
                    }
                  >
                    {group.upstreams.length === 0 ? (
                      <Text type="secondary">{t("upstreams.group.empty")}</Text>
                    ) : (
                      <Space orientation="vertical" size={4}>
                        {group.upstreams.map((upstreamId) => {
                          const server = config.upstreams[upstreamId];
                          return (
                            <Text key={upstreamId}>
                              {server ? `${server.name} · ` : ""}
                              <Text code>{server?.url ?? upstreamId}</Text>
                            </Text>
                          );
                        })}
                      </Space>
                    )}
                    <div style={{ marginTop: 8 }}>{renderTest(key)}</div>
                  </Card>
                </Col>
              );
            })}
          </Row>
        )}
      </Card>

      <Modal
        open={serverModal.open}
        title={
          serverModal.editId
            ? t("upstreams.form.editServer")
            : t("upstreams.form.addServer")
        }
        okText={t("upstreams.form.ok")}
        cancelText={t("upstreams.form.cancel")}
        onOk={submitServer}
        onCancel={() => setServerModal({ open: false, editId: null })}
      >
        <Form form={serverForm} layout="vertical">
          <Form.Item
            name="id"
            label={t("upstreams.form.id")}
            rules={[{ required: true, message: t("upstreams.form.idRequired") }]}
          >
            <Input disabled={serverModal.editId !== null} placeholder="cloudflare" />
          </Form.Item>
          <Form.Item
            name="name"
            label={t("upstreams.form.name")}
            rules={[{ required: true, message: t("upstreams.form.nameRequired") }]}
          >
            <Input />
          </Form.Item>
          <Form.Item
            name="url"
            label={t("upstreams.form.url")}
            extra={t("upstreams.form.urlHint")}
            rules={[{ required: true, message: t("upstreams.form.urlRequired") }]}
          >
            <Input placeholder="udp://1.1.1.1:53" />
          </Form.Item>
          <Form.Item name="timeout_ms" label={t("upstreams.form.timeout")}>
            <InputNumber min={100} max={30000} style={{ width: 200 }} />
          </Form.Item>
          <Form.Item name="bootstrap" label={t("upstreams.form.bootstrap")}>
            <Input.TextArea rows={2} placeholder="1.1.1.1, 1.0.0.1" />
          </Form.Item>
          <Form.Item name="remark" label={t("upstreams.form.remark")}>
            <Input />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        open={groupModal.open}
        title={
          groupModal.editId
            ? t("upstreams.form.editGroup")
            : t("upstreams.form.addGroup")
        }
        okText={t("upstreams.form.ok")}
        cancelText={t("upstreams.form.cancel")}
        onOk={submitGroup}
        onCancel={() => setGroupModal({ open: false, editId: null })}
      >
        <Form form={groupForm} layout="vertical">
          <Form.Item
            name="id"
            label={t("upstreams.form.id")}
            rules={[{ required: true, message: t("upstreams.form.idRequired") }]}
          >
            <Input disabled={groupModal.editId !== null} placeholder="corp" />
          </Form.Item>
          <Form.Item
            name="name"
            label={t("upstreams.form.name")}
            rules={[{ required: true, message: t("upstreams.form.nameRequired") }]}
          >
            <Input />
          </Form.Item>
          <Form.Item
            name="upstreams"
            label={t("upstreams.form.members")}
            rules={[{ required: true, message: t("upstreams.form.membersRequired") }]}
          >
            <Select
              mode="multiple"
              options={servers.map(([id, server]) => ({
                value: id,
                label: `${server.name} (${id})`,
              }))}
            />
          </Form.Item>
        </Form>
      </Modal>
    </>
  );
}
