import React, { useEffect, useState } from 'react';
import {
  Card, Form, Input, InputNumber, Select, Switch, Button, Space, Row, Col,
  Tag, Tooltip, message, Skeleton,
} from 'antd';
import {
  SaveOutlined, ReloadOutlined, QuestionCircleOutlined, CheckCircleOutlined,
  InfoCircleOutlined, ApiOutlined, EyeInvisibleOutlined, PlusOutlined,
} from '@ant-design/icons';
import { api } from '../api';
import type { CustomProvider, ChannelPreset } from '../api';

interface FieldConfig {
  key: string;
  label: string;
  tooltip: string;
  placeholder: string;
  type: 'input' | 'number' | 'select' | 'switch' | 'password';
  options?: { label: string; value: string }[];
  suffix?: string;
  defaultOff?: boolean;
  tag?: string;
}

// 渠道与自动切换相关设置，从系统设置整体迁移到「模型与渠道」页。
const CHANNEL_FIELDS: FieldConfig[] = [
  { key: 'keyfree_enabled', label: 'OpenCode 免费池', tooltip: '无需凭据的公共模型池（opencode.ai/zen 匿名端点）。开启后其免费目录并入统一入口，可点名或按前缀调用。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
  { key: 'keyfree_base_url', label: 'OpenCode 池地址', tooltip: '留空使用内置 https://opencode.ai/zen/v1；仅上游域名变更时填写。', placeholder: 'https://opencode.ai/zen/v1', type: 'input', tag: '可选' },
  { key: 'router9_enabled', label: '9Router 免费池', tooltip: '接入本地运行的 9Router（开源免费模型聚合代理）。它把几十上百个免费上游聚合到一个 OpenAI 兼容端点，行云挂上后即可用其整个免费目录并加入免费池轮询。需你自行部署 9Router。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
  { key: 'router9_base_url', label: '9Router 地址', tooltip: '9Router 的 OpenAI 兼容基地址，默认 http://localhost:20128/v1（与行云同机部署时）。填根地址会自动尝试 /v1/models。', placeholder: 'http://localhost:20128/v1', type: 'input', tag: '已生效' },
  { key: 'freellm_enabled', label: 'FreeLLMAPI 免费池', tooltip: '接入本地运行的 FreeLLMAPI（开源免费模型聚合代理，34 家上游 / 数百免费模型）。同 9Router，需你自行部署后填地址。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
  { key: 'freellm_base_url', label: 'FreeLLMAPI 地址', tooltip: 'FreeLLMAPI 的 OpenAI 兼容基地址，默认 http://localhost:8787/v1。', placeholder: 'http://localhost:8787/v1', type: 'input', tag: '已生效' },
  { key: 'keyed_enabled', label: '自有 API Key 渠道', tooltip: '填入你自己的上游 API Key，把该账号下全部模型并入统一入口。Key 加密存放，保存后不再明文回显。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
  { key: 'keyed_preset', label: 'API Key 预设', tooltip: '没手填地址时打哪儿。也可在「自定义渠道」里添加更多官方免费层渠道（Groq/Cerebras/魔搭/硅基流动等）。', placeholder: '智谱开放平台', type: 'select', options: [
    { label: '智谱开放平台', value: 'bigmodel' },
    { label: 'NVIDIA 免费端点（build.nvidia.com）', value: 'nvidia' },
    { label: 'B.AI（meta.ai 开放平台）', value: 'bai' },
  ], tag: '已生效' },
  { key: 'keyed_base_url', label: 'API Key 渠道地址', tooltip: 'OpenAI 兼容 /v1 基地址，填这里会覆盖预设；留空按预设走。', placeholder: '留空则使用所选预设的地址', type: 'input', tag: '已生效' },
  { key: 'keyed_api_key', label: 'API Key 渠道密钥', tooltip: '上游账号密钥，加密存放。框内是占位符不是密钥本身；清空并保存即删除。', placeholder: '粘贴上游 API Key', type: 'password', tag: '已生效' },
  { key: 'keyed_extra_models', label: 'API Key 渠道补录模型', tooltip: '上游 /models 不展示但仍能调用的模型，逗号或空格分隔。目录拉不到时以此为准。', placeholder: 'glm-4.7, glm-4-flash', type: 'input', tag: '已生效' },
  { key: 'route_failover_enabled', label: '限流自动切换', tooltip: '当前模型被限流/欠费/掉线时，按「模型列表」页排好的顺序自动换下一个可用模型。只在同一计费边界内切换：免费池、自有 Key、行云账号三者互不串。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
  { key: 'health_probe_enabled', label: '主动探测模型可用性', tooltip: '定期用一条最短对话敲一遍免费池/自有 Key 渠道的模型，提前发现 429 与掉线。注意会消耗真实额度。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
  { key: 'health_probe_interval_seconds', label: '探测间隔（秒）', tooltip: '两轮探测之间的等待时间，最小 30 秒。', placeholder: '300', type: 'number', suffix: '秒', tag: '已生效' },
];

const fieldLabel = (f: FieldConfig) => (
  <Space size={4}>
    {f.label}
    <Tooltip title={f.tooltip}><QuestionCircleOutlined style={{ color: '#bbb' }} /></Tooltip>
    {f.tag && <Tag color={f.tag === '已生效' ? 'success' : 'default'} style={{ marginLeft: 4, fontSize: 11 }}>{f.tag === '已生效' ? <CheckCircleOutlined /> : <InfoCircleOutlined />} {f.tag}</Tag>}
  </Space>
);

const ChannelSettings: React.FC = () => {
  const [form] = Form.useForm();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [customProviders, setCustomProviders] = useState<CustomProvider[]>([]);
  const [savingCustom, setSavingCustom] = useState(false);
  const [presets, setPresets] = useState<ChannelPreset[]>([]);
  const [blocklist, setBlocklist] = useState<string[]>([]);

  const load = async () => {
    setLoading(true);
    try {
      const data = await api.getSettings();
      const normalized: Record<string, unknown> = { ...data };
      for (const f of CHANNEL_FIELDS) {
        if (f.type !== 'switch') continue;
        const v = data[f.key];
        normalized[f.key] = f.defaultOff ? v === '1' || v === 'true' : v !== 'false' && v !== '0';
      }
      form.setFieldsValue(normalized);
      api.getCustomProviders().then(setCustomProviders).catch(() => {});
      api.getChannelPresets().then(setPresets).catch(() => {});
      api.getModelBlocklist().then(setBlocklist).catch(() => {});
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '加载渠道设置失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { load(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const save = async () => {
    setSaving(true);
    try {
      const values = form.getFieldsValue();
      const payload = Object.fromEntries(
        Object.entries(values).map(([k, v]) => [k, v == null ? '' : String(v)]),
      );
      await api.updateSettings(payload);
      message.success('渠道设置已保存');
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const saveCustom = async () => {
    setSavingCustom(true);
    try { await api.saveCustomProviders(customProviders); message.success('自定义渠道已保存'); }
    catch (e: unknown) { message.error(e instanceof Error ? e.message : '保存失败'); }
    finally { setSavingCustom(false); }
  };
  const addCustom = () => setCustomProviders((p) => [...p, { id: 'cp_' + Date.now(), name: '', base_url: '', api_key: '', enabled: true, free_only: false }]);
  const removeCustom = (id: string) => setCustomProviders((p) => p.filter((x) => x.id !== id));
  const updCustom = (id: string, field: string, value: string | boolean) => setCustomProviders((p) => p.map((x) => (x.id === id ? { ...x, [field]: value } : x)));
  const unblock = async (key: string) => {
    try { await api.setModelHidden(key, false); setBlocklist((prev) => prev.filter((k) => k !== key)); message.success('已恢复显示'); }
    catch (e: unknown) { message.error(e instanceof Error ? e.message : '恢复失败'); }
  };

  if (loading) return <Card size="small"><Skeleton active paragraph={{ rows: 6 }} /></Card>;

  return (
    <div>
      <Card
        size="small"
        style={{ marginBottom: 16 }}
        title={<span className="jc-section-title"><ApiOutlined />免费池与渠道</span>}
        extra={<Button size="small" type="primary" icon={<SaveOutlined />} loading={saving} onClick={save}>保存设置</Button>}
      >
        <div style={{ fontSize: 13, color: 'var(--jc-fg-muted)', lineHeight: 1.8, marginBottom: 12 }}>
          免费池（9Router / FreeLLMAPI / OpenCode）是免 Key 的公共模型源，开启即并入统一入口并参与免费池轮询；
          自有 API Key 渠道与自定义渠道花你自己的额度。各计费边界互不串。
        </div>
        <Form form={form} component={false}>
          <Row gutter={[24, 0]}>
            {CHANNEL_FIELDS.map((f) => (
              <Col xs={24} md={12} key={f.key}>
                {f.type === 'switch' && <Form.Item name={f.key} label={fieldLabel(f)} valuePropName="checked"><Switch /></Form.Item>}
                {f.type === 'number' && <Form.Item name={f.key} label={fieldLabel(f)}><InputNumber style={{ width: '100%' }} placeholder={f.placeholder} addonAfter={f.suffix} /></Form.Item>}
                {f.type === 'select' && <Form.Item name={f.key} label={fieldLabel(f)}><Select placeholder={f.placeholder} options={f.options} allowClear /></Form.Item>}
                {f.type === 'password' && <Form.Item name={f.key} label={fieldLabel(f)}><Input.Password placeholder={f.placeholder} visibilityToggle={false} /></Form.Item>}
                {f.type === 'input' && <Form.Item name={f.key} label={fieldLabel(f)}><Input placeholder={f.placeholder} /></Form.Item>}
              </Col>
            ))}
          </Row>
        </Form>
      </Card>

      <Card size="small" style={{ marginBottom: 16 }} title={<span className="jc-section-title"><ApiOutlined />自定义渠道</span>}>
        <div style={{ fontSize: 13, color: 'var(--jc-fg-muted)', lineHeight: 1.8, marginBottom: 12 }}>
          添加任意 OpenAI 兼容上游地址和 Key。填根地址即可（自动尝试 /v1/models）；开启「仅免费」后加入免费池轮询，付费墙模型自动隐藏。
        </div>
        {presets.length > 0 && (
          <div style={{ marginBottom: 12 }}>
            <span style={{ fontSize: 13, color: 'var(--jc-fg-muted)', marginRight: 8 }}>快速添加官方免费渠道：</span>
            <Select
              style={{ minWidth: 320 }}
              placeholder="选择预设（B.AI / NVIDIA / Groq / 魔搭 / 硅基流动…）"
              value={null}
              options={presets.map((p) => ({ label: `${p.name} — ${p.note}`, value: p.id }))}
              onChange={(id) => {
                const p = presets.find((x) => x.id === id); if (!p) return;
                setCustomProviders((prev) => [...prev, { id: 'cp_' + Date.now(), name: p.name, base_url: p.base_url, api_key: '', enabled: true, free_only: true }]);
                message.info(`已添加 ${p.name} 预设，填入你的官方 Key 后保存即可`);
              }}
            />
          </div>
        )}
        {customProviders.map((cp, idx) => (
          <div key={cp.id} style={{ borderBottom: idx < customProviders.length - 1 ? '1px solid var(--jc-border)' : 'none', paddingBottom: 16, marginBottom: 16 }}>
            <Row gutter={[16, 8]} align="middle">
              <Col xs={24} md={6}><Form.Item label="渠道名称" style={{ marginBottom: 0 }}><Input placeholder="例如：OpenRouter" value={cp.name} onChange={(e) => updCustom(cp.id, 'name', e.target.value)} /></Form.Item></Col>
              <Col xs={24} md={9}><Form.Item label="Base URL" style={{ marginBottom: 0 }}><Input placeholder="https://api.openai.com/v1" value={cp.base_url} onChange={(e) => updCustom(cp.id, 'base_url', e.target.value)} /></Form.Item></Col>
              <Col xs={24} md={5}><Form.Item label="API Key" style={{ marginBottom: 0 }}><Input.Password placeholder="sk-..." value={cp.api_key} onChange={(e) => updCustom(cp.id, 'api_key', e.target.value)} visibilityToggle={false} /></Form.Item></Col>
              <Col xs={12} md={2}><Form.Item label="启用" style={{ marginBottom: 0 }}><Switch checked={cp.enabled} onChange={(v) => updCustom(cp.id, 'enabled', v)} /></Form.Item></Col>
              <Col xs={12} md={2}><Form.Item label="仅免费" style={{ marginBottom: 0 }}><Switch checked={!!cp.free_only} onChange={(v) => updCustom(cp.id, 'free_only', v)} /></Form.Item></Col>
            </Row>
            <div style={{ textAlign: 'right' }}><Button size="small" danger onClick={() => removeCustom(cp.id)}>删除</Button></div>
          </div>
        ))}
        <Space style={{ marginTop: 8 }}>
          <Button size="small" icon={<SaveOutlined />} onClick={saveCustom} loading={savingCustom}>保存渠道</Button>
          <Button size="small" icon={<ReloadOutlined />} onClick={() => api.getCustomProviders().then(setCustomProviders).catch(() => {})}>刷新</Button>
          <Button size="small" type="dashed" icon={<PlusOutlined />} onClick={addCustom}>添加渠道</Button>
        </Space>
      </Card>

      <Card
        size="small"
        title={<span className="jc-section-title"><EyeInvisibleOutlined />隐藏的模型（{blocklist.length}）</span>}
        extra={<Tooltip title="在「模型列表」页点隐藏按钮加入这里；被隐藏的模型不出现在模型列表与自动切换中。"><QuestionCircleOutlined style={{ color: '#bbb' }} /></Tooltip>}
      >
        {blocklist.length === 0 ? (
          <div style={{ fontSize: 13, color: 'var(--jc-fg-muted)' }}>暂无隐藏的模型。付费/不可用模型可在「模型列表」页一键隐藏。</div>
        ) : (
          <Space size={[8, 8]} wrap>
            {blocklist.map((k) => <Tag key={k} closable onClose={() => unblock(k)} style={{ fontSize: 13 }}>{k}</Tag>)}
          </Space>
        )}
      </Card>
    </div>
  );
};

export default ChannelSettings;
