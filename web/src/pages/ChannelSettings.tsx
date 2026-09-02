import React, { useEffect, useState } from 'react';
import {
  Card, Form, InputNumber, Switch, Button, Space, Row, Col,
  Tag, Tooltip, message, Skeleton,
} from 'antd';
import {
  SaveOutlined, QuestionCircleOutlined, CheckCircleOutlined,
  InfoCircleOutlined, ApiOutlined,
} from '@ant-design/icons';
import { api } from '../api';

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

// 渠道与自动切换相关设置。
const CHANNEL_FIELDS: FieldConfig[] = [
  { key: 'route_failover_enabled', label: '限流自动切换', tooltip: '当前模型被限流/欠费/掉线时，按「模型列表」页排好的顺序自动换下一个可用模型。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
  { key: 'health_probe_enabled', label: '主动探测模型可用性', tooltip: '定期用一条最短对话敲一遍模型，提前发现 429 与掉线。注意会消耗真实额度。', placeholder: 'false', type: 'switch', defaultOff: true, tag: '已生效' },
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

  if (loading) return <Card size="small"><Skeleton active paragraph={{ rows: 3 }} /></Card>;

  return (
    <Card
      size="small"
      title={<span className="jc-section-title"><ApiOutlined />渠道设置</span>}
      extra={<Button size="small" type="primary" icon={<SaveOutlined />} loading={saving} onClick={save}>保存设置</Button>}
    >
      <div style={{ fontSize: 13, color: 'var(--jc-fg-muted)', lineHeight: 1.8, marginBottom: 12 }}>
        限流自动切换与模型可用性探测设置。免费模型请通过「自定义渠道」添加。
      </div>
      <Form form={form} component={false}>
        <Row gutter={[24, 0]}>
          {CHANNEL_FIELDS.map((f) => (
            <Col xs={24} md={12} key={f.key}>
              {f.type === 'switch' && <Form.Item name={f.key} label={fieldLabel(f)} valuePropName="checked"><Switch /></Form.Item>}
              {f.type === 'number' && <Form.Item name={f.key} label={fieldLabel(f)}><InputNumber style={{ width: '100%' }} placeholder={f.placeholder} addonAfter={f.suffix} /></Form.Item>}
            </Col>
          ))}
        </Row>
      </Form>
    </Card>
  );
};

export default ChannelSettings;
