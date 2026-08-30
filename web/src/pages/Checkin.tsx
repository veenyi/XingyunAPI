import React, { useEffect, useState } from 'react';
import {
  Alert, Button, Card, Empty, Form, Input, Modal, Popconfirm, Select,
  Space, Spin, Switch, Tag, TimePicker, Tooltip, message,
} from 'antd';
import {
  PlusOutlined, ReloadOutlined, CheckCircleOutlined, DeleteOutlined,
  GiftOutlined, ClockCircleOutlined, QuestionCircleOutlined, EditOutlined,
} from '@ant-design/icons';
import dayjs from 'dayjs';
import { api } from '../api';
import type { CheckinAccount } from '../api';

const PLATFORM_META: Record<string, { label: string; color: string; desc: string }> = {
  workbuddy: { label: 'WorkBuddy', color: 'blue', desc: '腾讯 CodeBuddy 每日签到领积分' },
  traework: { label: 'TraeWork', color: 'cyan', desc: 'Trae SOLO 每日签到领积分' },
};

const emptyDraft = (): CheckinAccount => ({
  id: '',
  platform: 'workbuddy',
  name: '',
  uid: '',
  access_token: '',
  refresh_token: '',
  enterprise_id: '',
  domain: '',
  device_id: '',
  machine_id: '',
  api_host: '',
  enabled: true,
});

const CheckinPage: React.FC = () => {
  const [accounts, setAccounts] = useState<CheckinAccount[]>([]);
  const [times, setTimes] = useState<string[]>(['09:00']);
  const [loading, setLoading] = useState(true);
  const [running, setRunning] = useState(false);
  const [runId, setRunId] = useState<string | null>(null);
  const [modalOpen, setModalOpen] = useState(false);
  const [editing, setEditing] = useState<CheckinAccount | null>(null);
  const [form] = Form.useForm();

  const load = async () => {
    setLoading(true);
    try {
      const data = await api.listCheckin();
      setAccounts(data.accounts ?? []);
      setTimes(data.times?.length ? data.times : ['09:00']);
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '加载签到账号失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { load(); }, []);

  const openAdd = () => {
    setEditing(null);
    form.setFieldsValue(emptyDraft());
    setModalOpen(true);
  };

  const openEdit = (a: CheckinAccount) => {
    setEditing(a);
    form.setFieldsValue({ ...a, access_token: '', refresh_token: '' });
    setModalOpen(true);
  };

  const submit = async () => {
    const values = await form.validateFields();
    const next: CheckinAccount[] = [...accounts];
    const target: CheckinAccount = {
      ...(editing ?? emptyDraft()),
      ...values,
      id: editing?.id || `ci_${Date.now()}`,
    };
    if (editing) {
      const idx = next.findIndex((a) => a.id === editing.id);
      if (idx >= 0) next[idx] = target;
    } else {
      next.push(target);
    }
    try {
      await api.saveCheckinAccounts(next);
      message.success(editing ? '账号已更新' : '账号已添加');
      setModalOpen(false);
      load();
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '保存失败');
    }
  };

  const toggleEnabled = async (a: CheckinAccount, enabled: boolean) => {
    try {
      await api.saveCheckinAccounts(accounts.map((x) => (x.id === a.id ? { ...x, enabled } : x)));
      load();
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '操作失败');
    }
  };

  const remove = async (id: string) => {
    try {
      await api.removeCheckinAccount(id);
      message.success('已删除');
      load();
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '删除失败');
    }
  };

  const run = async (opts: { id?: string; all?: boolean }) => {
    if (opts.id) setRunId(opts.id); else setRunning(true);
    try {
      const { results } = await api.runCheckin(opts);
      for (const r of results ?? []) {
        if (r.ok) message.success(`${r.name || r.id}：${r.message}${r.credits ? `（剩余 ${r.credits} 积分）` : ''}`);
        else if (r.message.includes('已签到')) message.info(`${r.name || r.id}：${r.message}`);
        else message.warning(`${r.name || r.id}：${r.message}`);
      }
      load();
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '签到请求失败');
    } finally {
      setRunId(null);
      setRunning(false);
    }
  };

  const saveTimes = async (vals: string[]) => {
    try {
      const { times: saved } = await api.saveCheckinConfig(vals.length ? vals : ['09:00']);
      setTimes(saved);
      message.success('签到时刻已保存');
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '保存失败');
    }
  };

  const platform = Form.useWatch('platform', form);

  return (
    <div className="jc-page">
      <div className="jc-banner">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 12 }}>
          <div>
            <div className="jc-banner-sub">多平台自动签到 · 积分领取</div>
            <div className="jc-banner-title">签到中心</div>
          </div>
          <Space>
            <Button icon={<ReloadOutlined />} onClick={load} loading={loading}>刷新</Button>
            <Button type="primary" icon={<CheckCircleOutlined />} onClick={() => run({ all: true })} loading={running} disabled={accounts.length === 0}>
              全部签到
            </Button>
            <Button type="primary" ghost icon={<PlusOutlined />} onClick={openAdd}>添加账号</Button>
          </Space>
        </div>
      </div>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="每天自动签到领积分"
        description="添加 WorkBuddy / TraeWork 账号的 access token 与 refresh token 后，行云会在设定时刻自动签到并刷新 token（凭据加密存储）。Qoder 无签到活动。"
      />

      <Card size="small" style={{ marginBottom: 16 }} title={<span className="jc-section-title"><ClockCircleOutlined />签到时刻</span>}
        extra={(
          <Tooltip title="每天这些时刻自动执行全部账号签到；同一时刻每天只跑一次。">
            <QuestionCircleOutlined style={{ color: '#bbb' }} />
          </Tooltip>
        )}
      >
        <Space wrap>
          <TimePicker.RangePicker disabled />
          {times.map((t) => <Tag key={t} color="green" style={{ fontSize: 14, padding: '2px 10px' }}>{t}</Tag>)}
          <TimePicker
            format="HH:mm"
            placeholder="添加时刻"
            onChange={(v) => { if (v) saveTimes([...times, (v as dayjs.Dayjs).format('HH:mm')].sort()); }}
          />
          {times.length > 1 && (
            <Button size="small" onClick={() => saveTimes(times.slice(0, -1))}>移除最后时刻</Button>
          )}
        </Space>
      </Card>

      <Card size="small" title={<span className="jc-section-title"><GiftOutlined />签到账号</span>}>
        {loading ? (
          <Spin style={{ display: 'block', margin: '40px auto' }} />
        ) : accounts.length === 0 ? (
          <Empty description="还没有签到账号，点右上角「添加账号」开始" style={{ padding: '24px 0' }} />
        ) : (
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(340px, 1fr))', gap: 12 }}>
            {accounts.map((a) => {
              const meta = PLATFORM_META[a.platform] ?? { label: a.platform, color: 'default', desc: '' };
              return (
                <Card key={a.id} size="small" styles={{ body: { padding: 14 } }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                    <Space size={8}>
                      <Tag color={meta.color}>{meta.label}</Tag>
                      <span style={{ fontWeight: 600 }}>{a.name || a.uid || a.id}</span>
                    </Space>
                    <Switch size="small" checked={a.enabled} onChange={(v) => toggleEnabled(a, v)} />
                  </div>
                  <div style={{ fontSize: 12, color: 'var(--jc-fg-muted)', margin: '8px 0 4px' }}>
                    UID：{a.uid || '—'}
                  </div>
                  <div style={{ display: 'flex', alignItems: 'baseline', gap: 6, margin: '6px 0' }}>
                    <span style={{ fontSize: 26, fontWeight: 700, lineHeight: 1 }}>{a.credits ?? '—'}</span>
                    <span style={{ fontSize: 12, color: 'var(--jc-fg-muted)' }}>
                      积分{a.credits_total ? ` / ${a.credits_total}` : ''}
                    </span>
                  </div>
                  <div style={{ fontSize: 12, margin: '4px 0 10px' }}>
                    {a.last_checkin_at ? (
                      <>
                        {a.last_checkin_ok
                          ? <Tag color="success" icon={<CheckCircleOutlined />}>已签到</Tag>
                          : <Tag color="warning">{a.last_result || '未完成'}</Tag>}
                        <span style={{ color: 'var(--jc-fg-muted)', marginLeft: 6 }}>
                          {a.last_checkin_at?.replace('T', ' ').slice(5, 16)}
                        </span>
                      </>
                    ) : (
                      <Tag>未签到</Tag>
                    )}
                  </div>
                  <Space>
                    <Button size="small" type="primary" ghost loading={runId === a.id} disabled={!a.enabled} onClick={() => run({ id: a.id })}>
                      立即签到
                    </Button>
                    <Button size="small" icon={<EditOutlined />} onClick={() => openEdit(a)}>编辑</Button>
                    <Popconfirm title="确认删除该签到账号？" onConfirm={() => remove(a.id)}>
                      <Button size="small" danger icon={<DeleteOutlined />} />
                    </Popconfirm>
                  </Space>
                </Card>
              );
            })}
          </div>
        )}
      </Card>

      <Modal
        title={editing ? '编辑签到账号' : '添加签到账号'}
        open={modalOpen}
        onOk={submit}
        onCancel={() => setModalOpen(false)}
        okText="保存"
        cancelText="取消"
        width={560}
        destroyOnClose
      >
        <Form form={form} layout="vertical" initialValues={emptyDraft()}>
          <Form.Item name="platform" label="平台" rules={[{ required: true }]}>
            <Select
              options={[
                { label: 'WorkBuddy（腾讯 CodeBuddy）', value: 'workbuddy' },
                { label: 'TraeWork（Trae SOLO）', value: 'traework' },
              ]}
            />
          </Form.Item>
          <Space size={12} style={{ display: 'flex' }}>
            <Form.Item name="name" label="备注名" style={{ flex: 1 }}>
              <Input placeholder="例如：主号" />
            </Form.Item>
            <Form.Item name="uid" label="UID" style={{ flex: 1 }} tooltip="账号用户 ID，用于展示与请求头">
              <Input placeholder="user id" />
            </Form.Item>
          </Space>
          <Form.Item
            name="access_token"
            label={editing ? 'Access Token（留空保持不变）' : 'Access Token'}
            extra={editing ? undefined : '登录后从客户端/网页抓包获取'}
          >
            <Input.TextArea rows={2} placeholder={editing ? '●●●●●●●●' : '粘贴 access token'} autoSize />
          </Form.Item>
          <Form.Item
            name="refresh_token"
            label={editing ? 'Refresh Token（留空保持不变）' : 'Refresh Token'}
            extra="用于自动续期，强烈建议填写；只填 access token 过期后需手动更新"
          >
            <Input.TextArea rows={2} placeholder={editing ? '●●●●●●●●' : '粘贴 refresh token'} autoSize />
          </Form.Item>
          {platform === 'workbuddy' && (
            <Space size={12} style={{ display: 'flex' }}>
              <Form.Item name="enterprise_id" label="Enterprise ID（可选）" style={{ flex: 1 }}>
                <Input placeholder="企业/租户 ID" />
              </Form.Item>
              <Form.Item name="domain" label="Domain（可选）" style={{ flex: 1 }}>
                <Input placeholder="部门域名" />
              </Form.Item>
            </Space>
          )}
          {platform === 'traework' && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 12 }}
              message="TraeWork 签到强校验设备指纹：device_id 必须是账号真实注册的设备 ID，随机值会以 9074 被拒。"
            />
          )}
          {platform === 'traework' && (
            <Space size={12} style={{ display: 'flex' }}>
              <Form.Item name="device_id" label="Device ID（x-device-id）" style={{ flex: 1 }} rules={[{ required: true, message: 'TraeWork 签到必需' }]}>
                <Input placeholder="账号真实设备 ID" />
              </Form.Item>
              <Form.Item name="machine_id" label="Machine ID（可选）" style={{ flex: 1 }}>
                <Input placeholder="x-machine-id" />
              </Form.Item>
            </Space>
          )}
        </Form>
      </Modal>
    </div>
  );
};

export default CheckinPage;
