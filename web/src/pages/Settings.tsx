import React, { useEffect, useState } from 'react';
import {
  Card, Form, Input, Button, InputNumber, Select, Switch, message, Modal,
  Skeleton, Space, Row, Col, Tag, Tooltip, Popconfirm,
} from 'antd';
import {
  SaveOutlined, ReloadOutlined, QuestionCircleOutlined,
  SettingOutlined, CheckCircleOutlined, InfoCircleOutlined, LockOutlined,
  ApiOutlined, CopyOutlined,
} from '@ant-design/icons';
import { api, authApi, clearToken } from '../api';
import type { Settings, CustomProvider } from '../api';
import { copyToClipboard } from '../utils/clipboard';


interface FieldConfig {
  key: string;
  label: string;
  tooltip: string;
  placeholder: string;
  type: 'input' | 'password' | 'number' | 'select' | 'switch';
  options?: { label: string; value: string }[];
  suffix?: string;
  readOnly?: boolean;
  tag?: string;
  // defaultOff: 后端约定"没写过这个键就是关"，与老开关的"!= false 即开"相反。
  defaultOff?: boolean;
}

const FIELD_GROUPS: { title: string; fields: FieldConfig[] }[] = [
  {
    title: '模型配置',
    fields: [
      {
        key: 'default_model',
        label: '默认模型',
        tag: '已生效',
        tooltip: '当客户端未指定模型，且账号未配置默认模型时使用的 JoyCode 模型',
        placeholder: 'JoyAI-Code',
        type: 'select' as const,
        options: [
          { label: 'JoyAI-Code — 主力代码模型（推荐）', value: 'JoyAI-Code' },
          { label: 'Claude-Opus-4.7', value: 'Claude-Opus-4.7' },
          { label: 'GLM-5.1 — 智谱 GLM 5.1', value: 'GLM-5.1' },
          { label: 'GLM-5 — 智谱 GLM 5', value: 'GLM-5' },
          { label: 'GLM-4.7 — 智谱 GLM 4.7', value: 'GLM-4.7' },
          { label: 'Kimi-K2.6 — Moonshot Kimi K2.6', value: 'Kimi-K2.6' },
          { label: 'Kimi-K2.5 — Moonshot Kimi K2.5', value: 'Kimi-K2.5' },
          { label: 'MiniMax-M2.7 — MiniMax M2.7', value: 'MiniMax-M2.7' },
          { label: 'Doubao-Seed-2.0-pro — 豆包 Seed 2.0 Pro', value: 'Doubao-Seed-2.0-pro' },
        ],
      },
      {
        key: 'default_max_tokens',
        label: '默认最大输出 Token',
        tooltip: '客户端未指定 max_tokens 时的默认值。更大值允许更长回复，但消耗更多配额',
        placeholder: '8192',
        type: 'number' as const,
        tag: '已生效',
      },
    ],
  },
  {
    title: '渠道与自动切换',
    fields: [
      {
        key: 'keyfree_enabled',
        label: '免费模型渠道',
        tooltip: '无需任何凭据的公共模型池（不占账号、不参与保活）。开启后这些模型会出现在 /v1/models 里，可直接按模型名调用',
        placeholder: 'false',
        type: 'switch' as const,
        defaultOff: true,
        tag: '已生效',
      },
      {
        key: 'keyfree_base_url',
        label: '免费模型渠道地址',
        tooltip: '留空使用内置默认地址；仅在上游域名变更时填写',
        placeholder: 'https://opencode.ai/zen/v1',
        type: 'input' as const,
        tag: '可选',
      },
      {
        key: 'keyed_enabled',
        label: '自有 API Key 渠道',
        tooltip: '填入你自己的上游 API Key，即可把该账号下的全部模型并入统一入口。Key 加密存放，保存后不再明文回显',
        placeholder: 'false',
        type: 'switch' as const,
        defaultOff: true,
        tag: '已生效',
      },
      {
        key: 'keyed_preset',
        label: 'API Key 预设',
        tooltip: '决定没手填地址时打哪儿。NVIDIA 免费端点：在 build.nvidia.com 注册后生成 nvapi- 开头的 Key，一把 Key 即可调用该账号可见的全部免费模型（实测目录接口免鉴权可列 83 个）。要接别的站点不用改这里，直接在下栏填地址即可覆盖',
        placeholder: '智谱开放平台',
        type: 'select' as const,
        options: [
          { label: '智谱开放平台', value: 'bigmodel' },
          { label: 'NVIDIA 免费端点（build.nvidia.com）', value: 'nvidia' },
          { label: 'B.AI（meta.ai 开放平台）', value: 'bai' },
        ],
        tag: '已生效',
      },
      {
        key: 'keyed_base_url',
        label: 'API Key 渠道地址',
        tooltip: 'OpenAI 兼容的 /v1 基地址，填在这里会覆盖上面的预设；留空即按预设走（默认智谱 https://open.bigmodel.cn/api/paas/v4）',
        placeholder: '留空则使用所选预设的地址',
        type: 'input' as const,
        tag: '已生效',
      },
      {
        key: 'keyed_api_key',
        label: 'API Key 渠道密钥',
        tooltip: '上游账号密钥，加密存放。框内显示的是占位符，不是密钥本身；空白表示尚未配置，清空并保存即删除',
        placeholder: '粘贴上游 API Key',
        type: 'password' as const,
        tag: '已生效',
      },
      {
        key: 'keyed_extra_models',
        label: 'API Key 渠道补录模型',
        tooltip: '上游 /models 不展示但仍能调用的模型，用逗号或空格分隔填在这里。目录拉不到时也以这份名单为准，留空表示完全跟随上游目录',
        placeholder: 'glm-4.7, glm-4-flash',
        type: 'input' as const,
        tag: '已生效',
      },
      {
        key: 'route_failover_enabled',
        label: '限流自动切换',
        tooltip: '当前模型被限流、欠费或掉线时，按「模型与渠道」页排好的顺序自动换到下一个可用模型，而不是直接报错。只在同一计费边界内切换：免费模型渠道、自有 API Key 渠道、行云账号三者互不串；同边界内没有可用候选就照常报错，不会把你的请求偷偷换成别人买单的模型',
        placeholder: 'false',
        type: 'switch' as const,
        defaultOff: true,
        tag: '已生效',
      },
      {
        key: 'health_probe_enabled',
        label: '主动探测模型可用性',
        tooltip: '定期用一条最短对话敲一遍免费模型 / 自有 API Key 渠道的模型，提前发现 429 与掉线，不用等用户撞墙。注意：探针会消耗真实额度',
        placeholder: 'false',
        type: 'switch' as const,
        defaultOff: true,
        tag: '已生效',
      },
      {
        key: 'health_probe_interval_seconds',
        label: '探测间隔（秒）',
        tooltip: '两轮探测之间的等待时间，最小 30 秒。改完下一轮自动生效',
        placeholder: '300',
        type: 'number' as const,
        suffix: '秒',
        tag: '已生效',
      },
    ],
  },
  {
    title: '连接优化',
    fields: [
      {
        key: 'max_retries',
        label: '最大重试次数',
        tooltip: '请求失败时的自动重试次数。网络不稳定时可适当增加',
        placeholder: '3',
        type: 'number' as const,
        tag: '已生效',
      },
      {
        key: 'request_timeout',
        label: '请求超时（秒）',
        tooltip: '与 JoyCode 后端通信的读取超时时间，低于 60 秒会自动调整为 60 秒',
        placeholder: '120',
        type: 'number' as const,
        suffix: '秒',
        tag: '已生效',
      },
      {
        key: 'max_connections',
        label: '最大连接数',
        tooltip: '与 JoyCode 后端的最大并发 HTTP 连接数，修改后 10 秒内自动生效',
        placeholder: '20',
        type: 'number' as const,
        tag: '已生效',
      },
    ],
  },
  {
    title: '日志与监控',
    fields: [
      {
        key: 'enable_request_logging',
        label: '启用请求日志',
        tooltip: '记录每个 API 请求的详细信息（模型、延迟、状态码）。关闭后「数据概览」页面将无数据',
        placeholder: 'true',
        type: 'switch' as const,
        tag: '已生效',
      },
      {
        key: 'log_retention_days',
        label: '日志保留天数',
        tooltip: '请求日志的自动清理周期。超过此天数的日志将每小时自动清理，0 表示永久保留',
        placeholder: '30',
        type: 'number' as const,
        suffix: '天',
        tag: '已生效',
      },
    ],
  },
  {
    title: '账号管理',
    fields: [
      {
        key: 'points_rotate_threshold',
        label: '剩余积分轮换阈值',
        tooltip: '使用聚合 Key 时，账号剩余积分低于该值将自动跳过，切换使用下一个可用账号',
        placeholder: '10',
        type: 'number' as const,
        suffix: '积分',
        tag: '已生效',
      },
      {
        key: 'keepalive_interval_minutes',
        label: '账号保活间隔',
        tooltip: '定期向每个账号发送随机极短消息，模拟 JoyCode 客户端对话，防止账号因长期无客户端活动被上游冻结。间隔越短越保险，但会增加上游请求量',
        placeholder: '请选择',
        type: 'select' as const,
        options: [
          { label: '1 分钟', value: '1' },
          { label: '5 分钟', value: '5' },
          { label: '15 分钟', value: '15' },
          { label: '30 分钟', value: '30' },
          { label: '1 小时', value: '60' },
          { label: '3 小时', value: '180' },
          { label: '6 小时（推荐）', value: '360' },
          { label: '12 小时', value: '720' },
          { label: '24 小时', value: '1440' },
        ],
        tag: '已生效',
      },
    ],
  },
];

const SettingsPage: React.FC = () => {
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [customProviders, setCustomProviders] = useState<CustomProvider[]>([]);
  const [savingCustom, setSavingCustom] = useState(false);
  const [changePwLoading, setChangePwLoading] = useState(false);
  const [aggKey, setAggKey] = useState('sk-joy-aggregate');
  const [rotating, setRotating] = useState(false);
  const [modelOptions, setModelOptions] = useState<{ label: string; value: string }[]>([]);
  const [form] = Form.useForm();
  const [pwForm] = Form.useForm();

  // 加载上游实时模型列表（default_model 下拉）
  useEffect(() => {
    api.listModels()
      .then((ms) => {
        const names = ms.map((m) => m.name).filter(Boolean);
        if (names.length > 0) setModelOptions(names.map((n) => ({ label: n, value: n })));
      })
      .catch(() => {});
  }, []);

  const fetchSettings = async () => {
    setLoading(true);
    try {
      const data = await api.getSettings();
      // 后端设置统一存为字符串。switch 字段需转回布尔再回填表单，
      // 否则存着的 "false"（字符串）会被当成 truthy 而显示成「开」，误导用户。
      const normalized: Record<string, unknown> = { ...data };
      for (const group of FIELD_GROUPS) {
        for (const field of group.fields) {
          if (field.type !== 'switch') continue;
          const v = data[field.key];
          // 两套语义不能混：老开关是「!= false/0 即开」（与后端 SettingEnabledOr 一致），
          // defaultOff 开关是后端约定的「没写过就是关」，只有 1/true 才算开。
          normalized[field.key] = field.defaultOff
            ? v === '1' || v === 'true'
            : v !== 'false' && v !== '0';
        }
      }
      if (data.aggregate_key) setAggKey(data.aggregate_key);
      form.setFieldsValue(normalized);
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '加载设置失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { fetchSettings(); api.getCustomProviders().then(setCustomProviders).catch(() => {}); }, [form]);

  const handleSave = async (values: Settings) => {
    setSaving(true);
    try {
      const payload = Object.fromEntries(
        Object.entries(values).map(([key, value]) => [key, value == null ? '' : String(value)])
      );
      await api.updateSettings(payload);
      message.success('设置已保存');
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '保存设置失败');
    } finally {
      setSaving(false);
    }
  };

  const handleSaveCustomProviders = async () => {
    setSavingCustom(true);
    try {
      await api.saveCustomProviders(customProviders);
      message.success('自定义渠道已保存');
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '保存失败');
    } finally {
      setSavingCustom(false);
    }
  };

  const addCustomProvider = () => {
    setCustomProviders(prev => [...prev, {
      id: 'cp_' + Date.now(),
      name: '',
      base_url: '',
      api_key: '',
      enabled: true,
    }]);
  };

  const removeCustomProvider = (id: string) => {
    setCustomProviders(prev => prev.filter(p => p.id !== id));
  };

  const updateCustomProvider = (id: string, field: string, value: string | boolean) => {
    setCustomProviders(prev => prev.map(p => p.id === id ? { ...p, [field]: value } : p));
  };

  const handleChangePassword = async (values: { old_password: string; new_password: string }) => {

    Modal.confirm({
      title: '确认修改密码',
      content: '修改密码后需要重新登录，确定要继续吗？',
      okText: '确认修改',
      cancelText: '取消',
      okButtonProps: { danger: true },
      onOk: async () => {
        setChangePwLoading(true);
        try {
          await authApi.changePassword(values.old_password, values.new_password);
          message.success('密码修改成功，请重新登录');
          pwForm.resetFields();
          clearToken();
          setTimeout(() => { window.location.href = '/login'; }, 1000);
        } catch (e: unknown) {
          message.error(e instanceof Error ? e.message : '密码修改失败');
        } finally {
          setChangePwLoading(false);
        }
      },
    });
  };

  if (loading) {
    return (
      <div className="jc-page">
        <Skeleton.Button active block style={{ height: 80, marginBottom: 16, borderRadius: 10 }} />
        {[1, 2, 3].map((i) => (
          <Card key={i} size="small" style={{ marginBottom: 16 }}>
            <Skeleton active paragraph={{ rows: 3 }} />
          </Card>
        ))}
      </div>
    );
  }

  const renderField = (field: FieldConfig) => {
    const label = (
      <Space size={4}>
        {field.label}
        <Tooltip title={field.tooltip}><QuestionCircleOutlined style={{ color: '#bbb' }} /></Tooltip>
        {field.tag && (
          <Tag color={field.tag === '已生效' ? 'success' : 'default'} style={{ marginLeft: 4, fontSize: 11 }}>
            {field.tag === '已生效' ? <CheckCircleOutlined /> : <InfoCircleOutlined />} {field.tag}
          </Tag>
        )}
      </Space>
    );

    switch (field.type) {
      case 'number':
        return (
          <Form.Item key={field.key} name={field.key} label={label}>
            <InputNumber
              style={{ width: '100%' }}
              placeholder={field.placeholder}
              addonAfter={field.suffix}
              disabled={field.readOnly}
            />
          </Form.Item>
        );
      case 'select':
        return (
          <Form.Item key={field.key} name={field.key} label={label}>
            <Select placeholder={field.placeholder} options={field.options} allowClear disabled={field.readOnly} />
          </Form.Item>
        );
      case 'password':
        return (
          <Form.Item key={field.key} name={field.key} label={label}>
            {/* 框内是占位符而非真值，提供"显示"按钮只会让人误读，所以关掉。 */}
            <Input.Password placeholder={field.placeholder} visibilityToggle={false} autoComplete="new-password" />
          </Form.Item>
        );
      case 'switch':
        return (
          <Form.Item key={field.key} name={field.key} valuePropName="checked" label={label}>
            <Switch />
          </Form.Item>
        );
      default:
        return (
          <Form.Item key={field.key} name={field.key} label={label}>
            <Input placeholder={field.placeholder} disabled={field.readOnly} />
          </Form.Item>
        );
    }
  };

  return (
    <div className="jc-page">
      <div className="jc-banner">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 12 }}>
          <div>
            <div className="jc-banner-sub">JoyCode API 代理服务 · 系统设置</div>
            <div className="jc-banner-title">代理配置管理</div>
          </div>
          <div style={{ display: 'flex', gap: 8 }}>
            <Button
              icon={<ReloadOutlined />}
              onClick={fetchSettings}
            >
              刷新
            </Button>
          </div>
        </div>
      </div>

      <Card
        size="small"
        style={{ marginBottom: 16 }}
        title={<span className="jc-section-title"><ApiOutlined />聚合 API Key（多账号自动轮询）</span>}
      >
        <div style={{ fontSize: 13, color: 'var(--jc-fg-muted)', lineHeight: 1.8, marginBottom: 12 }}>
          使用同一个 Key 即可自动在多个账号间切换：剩余积分低于阈值的账号会被自动跳过，优先使用积分充足的账号，全部不足时回退到积分最高的账号。兼容 OpenAI / Anthropic 接口。
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
          <Input
            readOnly
            value={aggKey}
            style={{ maxWidth: 320, fontFamily: 'monospace' }}
          />
          <Button
            size="small"
            icon={<CopyOutlined />}
            onClick={async () => {
              const ok = await copyToClipboard(aggKey);
              if (ok) {
                message.success('聚合 Key 已复制');
              } else {
                message.error('复制失败');
              }
            }}
          >
            复制
          </Button>
          <Popconfirm
            title="重新生成聚合 Key？"
            description="生成后旧 Key 立即失效，需在所有客户端中更新"
            onConfirm={async () => {
              setRotating(true);
              try {
                const res = await api.rotateAggregateKey();
                if (res.key) {
                  setAggKey(res.key);
                  message.success('聚合 Key 已重新生成，旧 Key 已失效');
                }
              } catch (e: unknown) {
                message.error(e instanceof Error ? e.message : '重新生成失败');
              } finally {
                setRotating(false);
              }
            }}
          >
            <Button size="small" icon={<ReloadOutlined />} loading={rotating} danger>
              重新生成
            </Button>
          </Popconfirm>
        </div>
      </Card>

      <Form form={form} layout="vertical" onFinish={handleSave}>
        {FIELD_GROUPS.slice(0, 1).map((group) => (
          <Card
            key={group.title}
            size="small"
            style={{ marginBottom: 16 }}
            title={<span className="jc-section-title"><SettingOutlined />{group.title}</span>}
          >
            <Row gutter={[24, 0]}>
              {group.fields.map((field) => {
                // default_model 下拉使用上游实时模型列表（动态）
                const effField = field.key === 'default_model' && modelOptions.length > 0
                  ? { ...field, options: modelOptions }
                  : field;
                return (
                  <Col xs={24} md={12} key={field.key}>
                    {renderField(effField)}
                  </Col>
                );
              })}
            </Row>
          </Card>
        ))}
        

        <Card
          size="small"
          style={{ marginBottom: 16 }}
          title={<span className="jc-section-title"><ApiOutlined />自定义渠道</span>}
        >
          <div style={{ fontSize: 13, color: 'var(--jc-fg-muted)', lineHeight: 1.8, marginBottom: 12 }}>
            添加任意 OpenAI 兼容的上游地址和 Key，模型会直接出现在渠道列表中，与免费模型 / API Key 渠道并列。每个渠道独立计费边界，不会串用额度。
          </div>
          {customProviders.map((cp, idx) => (
            <div key={cp.id} style={{ borderBottom: idx < customProviders.length - 1 ? '1px solid var(--jc-border)' : 'none', paddingBottom: 16, marginBottom: 16 }}>
              <Row gutter={[16, 8]} align="middle">
                <Col xs={24} md={6}>
                  <Form.Item label="渠道名称" style={{ marginBottom: 0 }}>
                    <Input placeholder="例如：OpenRouter" value={cp.name} onChange={e => updateCustomProvider(cp.id, 'name', e.target.value)} />
                  </Form.Item>
                </Col>
                <Col xs={24} md={10}>
                  <Form.Item label="Base URL" style={{ marginBottom: 0 }}>
                    <Input placeholder="https://api.openai.com/v1" value={cp.base_url} onChange={e => updateCustomProvider(cp.id, 'base_url', e.target.value)} />
                  </Form.Item>
                </Col>
                <Col xs={24} md={6}>
                  <Form.Item label="API Key" style={{ marginBottom: 0 }}>
                    <Input.Password placeholder="sk-..." value={cp.api_key} onChange={e => updateCustomProvider(cp.id, 'api_key', e.target.value)} visibilityToggle={false} />
                  </Form.Item>
                </Col>
                <Col xs={24} md={2}>
                  <Form.Item label="启用" style={{ marginBottom: 0 }}>
                    <Switch checked={cp.enabled} onChange={v => updateCustomProvider(cp.id, 'enabled', v)} />
                  </Form.Item>
                </Col>
              </Row>
              <div style={{ textAlign: 'right' }}>
                <Button size="small" danger onClick={() => removeCustomProvider(cp.id)}>删除</Button>
              </div>
            </div>
          ))}
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            <Button size="small" icon={<SaveOutlined />} onClick={handleSaveCustomProviders} loading={savingCustom}>保存渠道</Button>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => { api.getCustomProviders().then(setCustomProviders).catch(() => {}); }}>刷新</Button>
          </div>
          <div style={{ marginTop: 12 }}>
            <Button size="small" type="dashed" onClick={addCustomProvider}>+ 添加渠道</Button>
          </div>
        </Card>

                {FIELD_GROUPS.slice(1).map((group) => (
          <Card
            key={group.title}
            size="small"
            style={{ marginBottom: 16 }}
            title={<span className="jc-section-title"><SettingOutlined />{group.title}</span>}
          >
            <Row gutter={[24, 0]}>
              {group.fields.map((field) => {
                // default_model 下拉使用上游实时模型列表（动态）
                const effField = field.key === 'default_model' && modelOptions.length > 0
                  ? { ...field, options: modelOptions }
                  : field;
                return (
                  <Col xs={24} md={12} key={field.key}>
                    {renderField(effField)}
                  </Col>
                );
              })}
            </Row>
          </Card>
        ))}

<Card
          size="small"
          style={{ marginBottom: 16 }}
          title={<span className="jc-section-title"><LockOutlined />安全设置</span>}>
          <Form form={pwForm} layout="vertical" onFinish={handleChangePassword}>
            <Row gutter={[24, 0]}>
              <Col xs={24} md={8}>
                <Form.Item name="old_password" label="当前密码" rules={[{ required: true, message: '请输入当前密码' }]}>
                  <Input.Password placeholder="输入当前密码" />
                </Form.Item>
              </Col>
              <Col xs={24} md={8}>
                <Form.Item name="new_password" label="新密码" rules={[
                  { required: true, message: '请输入新密码' },
                  { min: 6, message: '密码长度不能少于 6 位' },
                ]}>
                  <Input.Password placeholder="输入新密码（至少 6 位）" />
                </Form.Item>
              </Col>
              <Col xs={24} md={8}>
                <Form.Item label="确认新密码" dependencies={['new_password']} rules={[
                  { required: true, message: '请确认新密码' },
                  ({ getFieldValue }) => ({
                    validator(_, value) {
                      if (!value || getFieldValue('new_password') === value) {
                        return Promise.resolve();
                      }
                      return Promise.reject(new Error('两次输入的密码不一致'));
                    },
                  }),
                ]} name="confirm_password">
                  <Input.Password placeholder="再次输入新密码" />
                </Form.Item>
              </Col>
            </Row>
            <Button
              type="primary"
              htmlType="submit"
              loading={changePwLoading}
              icon={<LockOutlined />}
            >
              修改密码
            </Button>
          </Form>
        </Card>

        <div style={{ display: 'flex', gap: 12, marginTop: 8 }}>
          <Button
            type="primary"
            htmlType="submit"
            loading={saving}
            icon={<SaveOutlined />}
            size="large"
          >
            保存设置
          </Button>
          <Button onClick={fetchSettings} icon={<ReloadOutlined />} size="large">
            恢复当前值
          </Button>
        </div>
      </Form>
    </div>
  );
};

export default SettingsPage;
