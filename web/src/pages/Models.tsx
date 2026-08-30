import React, { useEffect, useState } from 'react';
import {
  Alert, Button, Card, Space, Switch, Table, Tag, Tooltip, message,
} from 'antd';
import {
  HolderOutlined, ReloadOutlined, SaveOutlined, QuestionCircleOutlined,
  EyeInvisibleOutlined,
} from '@ant-design/icons';
import {
  DndContext, closestCenter, KeyboardSensor, PointerSensor,
  useSensor, useSensors, type DragEndEvent,
} from '@dnd-kit/core';
import {
  SortableContext, useSortable, verticalListSortingStrategy, arrayMove,
} from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { api } from '../api';
import type { ModelStatus } from '../api';

// 与后端 route.defaultMaxRotate 一致：排得再长，一次请求也只会试前 N 个候选。
const MAX_ROTATE = 3;

const keyOf = (m: { provider: string; model: string }) => `${m.provider}|${m.model}`;

const DraggableRow: React.FC<React.HTMLAttributes<HTMLTableRowElement> & { 'data-row-key': string }> = (props) => {
  const { attributes, setNodeRef, transform, transition, isDragging } = useSortable({
    id: props['data-row-key'],
  });
  const style: React.CSSProperties = {
    ...props.style,
    transform: CSS.Transform.toString(transform && { ...transform, scaleY: 1 }),
    transition,
    ...(isDragging ? { position: 'relative', zIndex: 9999 } : {}),
  };
  return <tr {...props} ref={setNodeRef} style={style} {...attributes} />;
};

const DragHandle: React.FC<{ id: string }> = ({ id }) => {
  const { listeners, setActivatorNodeRef } = useSortable({ id });
  return (
    <td ref={setActivatorNodeRef} {...listeners} style={{ cursor: 'grab', width: 40, textAlign: 'center' }}>
      <HolderOutlined style={{ color: '#999' }} />
    </td>
  );
};

// coolingUntil 把冷却结束时间说成人话："还有 42 秒"这种，而不是一个 ISO 串。
function remainingText(until?: string): string {
  if (!until) return '';
  const ms = new Date(until).getTime() - Date.now();
  if (!Number.isFinite(ms) || ms <= 0) return '';
  const sec = Math.ceil(ms / 1000);
  if (sec < 60) return `${sec} 秒后恢复`;
  if (sec < 3600) return `${Math.ceil(sec / 60)} 分钟后恢复`;
  return `${Math.ceil(sec / 3600)} 小时后恢复`;
}

const CLASS_LABELS: Record<string, string> = {
  rate_limited: '被限流',
  no_credit: '额度不足',
  auth_failed: '登录失效',
  not_found: '模型已下架',
  server_error: '上游故障',
  network: '网络不通',
};

const ModelsPage: React.FC = () => {
  const [rows, setRows] = useState<ModelStatus[]>([]);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [onlyAvailable, setOnlyAvailable] = useState(false);
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor),
  );

  const load = async () => {
    setLoading(true);
    try {
      const models = await api.listModelStatus();
      setRows(models);
      setDirty(false);
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '加载模型名单失败');
    } finally {
      setLoading(false);
    }
  };

  // 刷新：先强制重拉各渠道目录（免费名单随官方策略变动，可能已有新模型），
  // 再重新加载状态表。目录刷新失败不阻塞状态展示。
  const refresh = async () => {
    setLoading(true);
    try {
      await api.refreshChannels().catch(() => undefined);
      await load();
      message.success('已刷新渠道目录与模型状态');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { load(); }, []);

  const hideModel = async (record: ModelStatus) => {
    try {
      await api.setModelHidden(`${record.provider}|${record.model}`, true);
      message.success(`已隐藏 ${record.provider} / ${record.model}，可从系统设置的屏蔽名单恢复`);
      load();
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '隐藏失败');
    }
  };

  const handleDragEnd = ({ active, over }: DragEndEvent) => {
    if (!over || active.id === over.id) return;
    setRows((prev) => {
      const from = prev.findIndex((m) => keyOf(m) === active.id);
      const to = prev.findIndex((m) => keyOf(m) === over.id);
      if (from < 0 || to < 0) return prev;
      setDirty(true);
      return arrayMove(prev, from, to);
    });
  };

  const save = async () => {
    setSaving(true);
    try {
      await api.updateModelRanking(rows.map((m) => ({ provider: m.provider, model: m.model })));
      message.success('自动切换顺序已保存');
      setDirty(false);
      load();
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  // 重置 = 写入空顺序，后端回落到"渠道名单原序"兜底。
  const reset = async () => {
    setSaving(true);
    try {
      await api.updateModelRanking([]);
      message.success('已恢复默认顺序');
      load();
    } catch (e: unknown) {
      message.error(e instanceof Error ? e.message : '重置失败');
    } finally {
      setSaving(false);
    }
  };

  const coolingCount = rows.filter((m) => m.status === 'cooling').length;
  const providers = Array.from(new Set(rows.map((m) => m.provider)));
  const visibleRows = onlyAvailable ? rows.filter((m) => m.status !== 'cooling') : rows;

  const columns = [
    {
      key: 'drag',
      width: 40,
      render: (_: unknown, record: ModelStatus) => <DragHandle id={keyOf(record)} />,
    },
    {
      title: '#',
      key: 'index',
      width: 56,
      render: (_: unknown, __: ModelStatus, index: number) => (
        <span style={{ color: index < MAX_ROTATE ? undefined : '#94a3b8' }}>{index + 1}</span>
      ),
    },
    { title: '渠道', dataIndex: 'provider', key: 'provider', width: 150 },
    { title: '模型', dataIndex: 'model', key: 'model' },
    {
      title: '状态',
      key: 'status',
      width: 180,
      render: (_: unknown, record: ModelStatus) => {
        if (record.status !== 'cooling') return <Tag color="success">可用</Tag>;
        const label = CLASS_LABELS[record.class ?? ''] ?? record.class ?? '不可用';
        const left = remainingText(record.until);
        return (
          <Space size={4}>
            <Tag color="error">{label}</Tag>
            {left && <span style={{ fontSize: 12, color: '#94a3b8' }}>{left}</span>}
          </Space>
        );
      },
    },
    {
      title: '操作',
      key: 'actions',
      width: 60,
      render: (_: unknown, record: ModelStatus) => (
        <Tooltip title="从模型列表与自动切换中隐藏（付费/不可用模型可在此屏蔽）">
          <Button
            size="small"
            type="text"
            icon={<EyeInvisibleOutlined />}
            onClick={() => hideModel(record)}
          />
        </Tooltip>
      ),
    },
    {
      title: (
        <Space size={4}>
          说明
          <Tooltip title="冷却只影响派单顺序，不会永久禁用模型；到期后自动回到候选名单。">
            <QuestionCircleOutlined style={{ color: '#bbb' }} />
          </Tooltip>
        </Space>
      ),
      dataIndex: 'reason',
      key: 'reason',
      ellipsis: true,
      render: (v: string, record: ModelStatus) => (
        <span style={{ fontSize: 12, color: '#64748b' }}>
          {record.status === 'cooling' ? (v || '—') : (record.ranked ? '已计入自动切换顺序' : '尚未排序，按默认顺序兜底')}
        </span>
      ),
    },
  ];

  return (
    <div className="jc-page">
      <div className="jc-banner">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 12 }}>
          <div>
            <div className="jc-banner-sub">多渠道聚合 · 模型可用性</div>
            <div className="jc-banner-title">模型与渠道</div>
          </div>
          <Space>
            <Tooltip title="强制重拉各渠道模型目录（免费名单随官方策略变动），再加载状态">
              <Button icon={<ReloadOutlined />} onClick={refresh} loading={loading}>刷新</Button>
            </Tooltip>
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
              <Switch size="small" checked={onlyAvailable} onChange={setOnlyAvailable} />
              只看可用
            </span>
            <Button onClick={reset} disabled={saving || rows.length === 0}>恢复默认顺序</Button>
            <Button type="primary" icon={<SaveOutlined />} onClick={save} loading={saving} disabled={!dirty}>
              保存顺序
            </Button>
          </Space>
        </div>
      </div>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="这张名单就是自动切换的派单顺序"
        description={
          <span>
            拖动行左侧手柄调整顺序，保存后即生效（无需重启）。开启「系统设置 → 限流自动切换」后，
            某模型被限流、欠费或掉线时会自动往下一个可用候选派单；一次请求最多尝试前 {MAX_ROTATE} 个候选。
            当前渠道：{providers.join('、') || '仅 JoyCode'}
            {coolingCount > 0 ? `，其中 ${coolingCount} 个模型正在冷却` : ''}。
          </span>
        }
      />

      <Card size="small">
        <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={handleDragEnd}>
          <SortableContext items={rows.map(keyOf)} strategy={verticalListSortingStrategy}>
            <Table
              dataSource={visibleRows}
              columns={columns}
              rowKey={keyOf}
              loading={loading}
              pagination={false}
              size="small"
              scroll={{ x: 760 }}
              components={{ body: { row: DraggableRow } }}
              locale={{
                emptyText: (
                  <div style={{ padding: '24px 16px', color: '#64748b', lineHeight: 1.8, maxWidth: 520, margin: '0 auto' }}>
                    暂无可选渠道。在「系统设置 → 多渠道与自动切换」里打开免登录渠道或填入自带 Key 渠道的 API Key 后刷新本页。
                  </div>
                ),
              }}
            />
          </SortableContext>
        </DndContext>
      </Card>
    </div>
  );
};

export default ModelsPage;
