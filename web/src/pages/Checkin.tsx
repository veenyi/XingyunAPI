import React, { useEffect, useState, useRef, useCallback } from 'react';
import {
  Alert, Button, Card, Empty, Form, Input, Modal, Popconfirm, Select,
  Space, Spin, Switch, Tag, TimePicker, Tooltip, Typography, message,
} from 'antd';
import {
  PlusOutlined, ReloadOutlined, CheckCircleOutlined, DeleteOutlined,
  GiftOutlined, ClockCircleOutlined, QuestionCircleOutlined, EditOutlined,
  QrcodeOutlined,
} from '@ant-design/icons';
import { QRCodeCanvas } from 'qrcode.react';
import dayjs from 'dayjs';
import { api } from '../api';
import type { CheckinAccount } from '../api';

const PLATFORM_META: Record<string, { label: string; color: string; desc: string }> = {
  workbuddy: { label: 'WorkBuddy', color: 'blue', desc: '腾讯 CodeBuddy 每日签到领积分' },
  traework: { label: 'TraeWork', color: 'cyan', desc: 'Trae SOLO 每日签到领积分' },
  qoder: { label: 'Qoder', color: 'purple', desc: 'Qoder CN 账号（设备流登录，自动保活）' },
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

  // WorkBuddy 扫码登录
  const [wbOpen, setWbOpen] = useState(false);
  const [wbStatus, setWbStatus] = useState<'loading' | 'waiting' | 'ok' | 'expired' | 'error'>('loading');
  const [wbAuthURL, setWbAuthURL] = useState('');
  const [wbMsg, setWbMsg] = useState('');
  const wbSessionRef = useRef('');
  const wbTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const wbStop = () => { if (wbTimerRef.current) { clearTimeout(wbTimerRef.current); wbTimerRef.current = undefined; } };

  // Qoder 设备流登录
  const [qoderOpen, setQoderOpen] = useState(false);
  const [qoderStatus, setQoderStatus] = useState<'loading' | 'waiting' | 'ok' | 'expired' | 'error'>('loading');
  const [qoderAuthURL, setQoderAuthURL] = useState('');
  const [qoderMsg, setQoderMsg] = useState('');
  const qoderSessionRef = useRef('');
  const qoderTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const qoderStop = () => { if (qoderTimerRef.current) { clearTimeout(qoderTimerRef.current); qoderTimerRef.current = undefined; } };

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

  useEffect(() => { load(); }, []); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => { wbStop(); qoderStop(); }, []);

  const wbPoll = useCallback(async () => {
    if (!wbSessionRef.current) return;
    try {
      const r = await api.wbLoginStatus(wbSessionRef.current);
      if (r.status === 'ok') {
        setWbStatus('ok');
        setWbMsg(`已登录：${r.account?.nickname || r.account?.uid || ''}`);
        wbStop();
        load();
        return;
      }
      if (r.status === 'expired') { setWbStatus('expired'); setWbMsg(r.message || '二维码已过期'); wbStop(); return; }
      if (r.status === 'error') { setWbStatus('error'); setWbMsg(r.message || '登录失败'); wbStop(); return; }
      setWbStatus('waiting');
      if (r.message) setWbMsg(r.message);
      wbTimerRef.current = setTimeout(wbPoll, 2000);
    } catch (e: unknown) {
      setWbStatus('error'); setWbMsg(e instanceof Error ? e.message : '轮询失败'); wbStop();
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const wbStart = useCallback(async () => {
    wbStop();
    setWbStatus('loading'); setWbMsg(''); setWbAuthURL('');
    try {
      const r = await api.wbLoginInit();
      wbSessionRef.current = r.session_id;
      setWbAuthURL(r.auth_url);
      setWbStatus('waiting');
      wbTimerRef.current = setTimeout(wbPoll, 1500);
    } catch (e: unknown) {
      setWbStatus('error'); setWbMsg(e instanceof Error ? e.message : '发起登录失败');
    }
  }, [wbPoll]);

  const openWBLogin = () => { setWbOpen(true); wbStart(); };
  const closeWBLogin = () => { wbStop(); setWbOpen(false); };

  const qoderPoll = useCallback(async () => {
    if (!qoderSessionRef.current) return;
    try {
      const r = await api.qoderLoginStatus(qoderSessionRef.current);
      if (r.status === 'ok') {
        setQoderStatus('ok');
        setQoderMsg(`已登录：${r.account?.nickname || r.account?.uid || ''}`);
        qoderStop();
        load();
        return;
      }
      if (r.status === 'expired') { setQoderStatus('expired'); setQoderMsg(r.message || '登录已过期'); qoderStop(); return; }
      if (r.status === 'error') { setQoderStatus('error'); setQoderMsg(r.message || '登录失败'); qoderStop(); return; }
      setQoderStatus('waiting');
      if (r.message) setQoderMsg(r.message);
      qoderTimerRef.current = setTimeout(qoderPoll, 2000);
    } catch (e: unknown) {
      setQoderStatus('error'); setQoderMsg(e instanceof Error ? e.message : '轮询失败'); qoderStop();
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const qoderStart = useCallback(async () => {
    qoderStop();
    setQoderStatus('loading'); setQoderMsg(''); setQoderAuthURL('');
    try {
      const r = await api.qoderLoginInit();
      qoderSessionRef.current = r.session_id;
      setQoderAuthURL(r.auth_url);
      setQoderStatus('waiting');
      qoderTimerRef.current = setTimeout(qoderPoll, 1500);
    } catch (e: unknown) {
      setQoderStatus('error'); setQoderMsg(e instanceof Error ? e.message : '发起登录失败');
    }
  }, [qoderPoll]);

  const openQoderLogin = () => { setQoderOpen(true); qoderStart(); };
  const closeQoderLogin = () => { qoderStop(); setQoderOpen(false); };

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
        if (r.ok) message.success(`${r.name || r.id}：${r.message}${r.credits != null ? `（剩余 ${r.credits} 积分）` : ''}`);
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
            <Button icon={<QrcodeOutlined />} onClick={openWBLogin}>扫码登录 WorkBuddy</Button>
            <Button style={{ color: '#722ed1', borderColor: '#722ed1' }} icon={<QrcodeOutlined />} onClick={openQoderLogin}>登录 Qoder</Button>
            <Button type="primary" ghost icon={<PlusOutlined />} onClick={openAdd}>手动添加</Button>
          </Space>
        </div>
      </div>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="每天自动签到领积分"
        description="添加 WorkBuddy / TraeWork 账号的 access token 与 refresh token 后，行云会在设定时刻自动签到并刷新 token（凭据加密存储）。Qoder 账号通过设备流登录，自动保活无需签到。"
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
                    <span style={{ fontSize: 26, fontWeight: 700, lineHeight: 1 }}>{typeof a.credits === 'number' ? a.credits.toFixed(2) : (a.credits ?? '—')}</span>
                    <span style={{ fontSize: 12, color: 'var(--jc-fg-muted)' }}>
                      积分{a.credits_total != null ? ` / ${typeof a.credits_total === 'number' ? a.credits_total.toFixed(2) : a.credits_total}` : ''}
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
                      {a.platform === 'qoder' ? '刷新积分' : '立即签到'}
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
        title="扫码登录 WorkBuddy"
        open={wbOpen}
        onCancel={closeWBLogin}
        footer={[
          <Button key="refresh" icon={<ReloadOutlined />} onClick={wbStart}>重新获取二维码</Button>,
          <Button key="close" type="primary" onClick={closeWBLogin}>{wbStatus === 'ok' ? '完成' : '关闭'}</Button>,
        ]}
        width={420}
        destroyOnClose
      >
        <div style={{ textAlign: 'center', padding: '8px 0 4px' }}>
          {wbStatus === 'loading' && <Spin style={{ margin: '40px auto' }} tip="正在生成二维码…" ><div style={{ height: 120 }} /></Spin>}
          {(wbStatus === 'waiting' || wbStatus === 'ok') && wbAuthURL && (
            <>
              <QRCodeCanvas value={wbAuthURL} size={200} style={{ margin: '0 auto', display: wbStatus === 'ok' ? 'none' : 'block' }} />
              <div style={{ marginTop: 12, color: 'var(--jc-fg-muted)', fontSize: 13 }}>
                {wbStatus === 'ok'
                  ? <Space direction="vertical"><CheckCircleOutlined style={{ color: '#52c41a', fontSize: 28 }} />{wbMsg || '登录成功，账号已添加'}</Space>
                  : '用手机微信扫码，或在已登录 WorkBuddy 的浏览器中打开下方链接完成授权'}
              </div>
              {wbStatus === 'waiting' && (
                <Typography.Link href={wbAuthURL} target="_blank" style={{ fontSize: 12, wordBreak: 'break-all', display: 'inline-block', marginTop: 8 }}>
                  点此打开授权页 →
                </Typography.Link>
              )}
            </>
          )}
          {(wbStatus === 'expired' || wbStatus === 'error') && (
            <Alert type="warning" showIcon message={wbMsg || (wbStatus === 'expired' ? '二维码已过期' : '登录失败')} style={{ textAlign: 'left' }}
              description="点「重新获取二维码」再试一次。" />
          )}
        </div>
      </Modal>

      <Modal
        title="登录 Qoder"
        open={qoderOpen}
        onCancel={closeQoderLogin}
        footer={[
          <Button key="refresh" icon={<ReloadOutlined />} onClick={qoderStart}>重新获取链接</Button>,
          <Button key="close" type="primary" onClick={closeQoderLogin}>{qoderStatus === 'ok' ? '完成' : '关闭'}</Button>,
        ]}
        width={460}
        destroyOnClose
      >
        <div style={{ textAlign: 'center', padding: '8px 0 4px' }}>
          {qoderStatus === 'loading' && <Spin style={{ margin: '40px auto' }} tip="正在生成登录链接…"><div style={{ height: 80 }} /></Spin>}
          {(qoderStatus === 'waiting' || qoderStatus === 'ok') && qoderAuthURL && (
            <>
              {qoderStatus === 'ok' ? (
                <Space direction="vertical"><CheckCircleOutlined style={{ color: '#52c41a', fontSize: 28 }} />{qoderMsg || '登录成功，账号已添加'}</Space>
              ) : (
                <>
                  <div style={{ margin: '12px 0', fontSize: 14 }}>请在浏览器中打开下方链接完成 Qoder 账号授权：</div>
                  <Typography.Link href={qoderAuthURL} target="_blank" style={{ fontSize: 13, wordBreak: 'break-all', display: 'inline-block', margin: '8px 0' }}>
                    {qoderAuthURL}
                  </Typography.Link>
                  <div style={{ color: 'var(--jc-fg-muted)', fontSize: 12, marginTop: 8 }}>
                    授权完成后将自动添加账号；行云会定期刷新 token 保持在线
                  </div>
                  {qoderMsg && <div style={{ color: 'var(--jc-fg-muted)', fontSize: 12, marginTop: 4 }}>{qoderMsg}</div>}
                </>
              )}
            </>
          )}
          {(qoderStatus === 'expired' || qoderStatus === 'error') && (
            <Alert type="warning" showIcon message={qoderMsg || (qoderStatus === 'expired' ? '登录已过期' : '登录失败')} style={{ textAlign: 'left' }}
              description="点「重新获取链接」再试一次。" />
          )}
        </div>
      </Modal>

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
                { label: 'Qoder（设备流登录）', value: 'qoder' },
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
          {platform !== 'qoder' && (
            <>
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
            </>
          )}
          {platform === 'qoder' && (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 12 }}
              message="Qoder 账号通过设备流登录添加"
              description="关闭此弹窗后，点页面顶部「登录 Qoder」按钮发起授权，无需手动填写凭据。"
            />
          )}
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
