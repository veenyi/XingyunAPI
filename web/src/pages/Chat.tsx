import React, { useEffect, useRef, useState } from 'react';
import {
  Card, Typography, Segmented, Select, Switch, Input, Button, Space, Spin, Empty, Tooltip,
} from 'antd';
import {
  SendOutlined, DeleteOutlined, GlobalOutlined, BulbOutlined,
} from '@ant-design/icons';
import { api } from '../api';

interface Msg {
  role: 'user' | 'assistant';
  content: string;
  reasoning: string;   // 深度思考内容（可折叠）
  tools: { tool: string; query: string; status: 'running' | 'done' }[];
  error?: string;
}

const { Text } = Typography;

const FALLBACK_MODELS = ["JoyAI-Code-1.5", "MiniMax-M3", "MiniMax-M2.7", "Kimi-K2.6", "GLM-5.1", "GLM-5", "DeepSeek-V4-Pro", "Doubao-Seed-2.0-pro"];

// 聊天历史持久化（localStorage，刷新后保留）
const HISTORY_KEY = 'joycode_chat_history';
const HISTORY_LIMIT = 100; // 最多保留 100 条消息

const loadHistory = (): Msg[] => {
  try {
    const raw = localStorage.getItem(HISTORY_KEY);
    if (!raw) return [];
    const arr = JSON.parse(raw);
    return Array.isArray(arr) ? arr.filter((m) => m && m.role) : [];
  } catch { return []; }
};

const Chat: React.FC = () => {
  const [messages, setMessages] = useState<Msg[]>(loadHistory);
  const [input, setInput] = useState('');
  const [model, setModel] = useState<string>('GLM-5.1');
  const [mode, setMode] = useState<string>('qa');
  const [webSearch, setWebSearch] = useState(true);
  const [models, setModels] = useState<string[]>(FALLBACK_MODELS);
  const [sending, setSending] = useState(false);
  const listRef = useRef<HTMLDivElement>(null);

  // 消息变化时自动保存（最多保留 HISTORY_LIMIT 条）
  useEffect(() => {
    try {
      const toSave = messages.slice(-HISTORY_LIMIT);
      localStorage.setItem(HISTORY_KEY, JSON.stringify(toSave));
    } catch { /* 存储满时忽略 */ }
  }, [messages]);

  useEffect(() => {
    api.listModels().then((ms) => {
      const names = ms.map((m) => m.name).filter(Boolean);
      if (names.length > 0) {
        setModels(names);
        if (!names.includes(model)) setModel(names[0]);
      }
    }).catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (listRef.current) listRef.current.scrollTop = listRef.current.scrollHeight;
  }, [messages, sending]);

  const send = async () => {
    const text = input.trim();
    if (!text || sending) return;
    setInput('');
    setMessages((prev) => [...prev, { role: 'user', content: text, reasoning: '', tools: [] }]);
    // 追加一个空的 assistant 消息用于流式填充
    setMessages((prev) => [...prev, { role: 'assistant', content: '', reasoning: '', tools: [] }]);
    setSending(true);

    // 历史消息（去掉最后的空 assistant）
    const history = messages.filter((m) => m.role === 'user' || (m.role === 'assistant' && m.content)).map((m) => ({
      role: m.role,
      content: m.content || '',
    }));
    history.push({ role: 'user', content: text });

    let reasoningBuf = '';
    let contentBuf = '';
    const toolsRun: { tool: string; query: string; status: 'running' | 'done' }[] = [];

    const patchLast = (fn: (m: Msg) => Msg) => {
      setMessages((prev) => {
        const next = [...prev];
        const last = next[next.length - 1];
        if (last && last.role === 'assistant') next[next.length - 1] = fn(last);
        return next;
      });
    };

    try {
      await api.chatStream(
        { messages: history, model, mode, web_search: webSearch },
        (e: any) => {
          switch (e.type) {
            case 'reasoning':
              reasoningBuf += e.content || '';
              patchLast((m) => ({ ...m, reasoning: reasoningBuf }));
              break;
            case 'text':
              contentBuf += e.content || '';
              patchLast((m) => ({ ...m, content: contentBuf }));
              break;
            case 'tool_start':
              toolsRun.push({ tool: e.tool || 'web_search', query: e.query || '', status: 'running' });
              patchLast((m) => ({ ...m, tools: [...toolsRun] }));
              break;
            case 'tool_result':
              if (toolsRun.length > 0) toolsRun[toolsRun.length - 1].status = 'done';
              patchLast((m) => ({ ...m, tools: [...toolsRun] }));
              break;
            case 'error':
              patchLast((m) => ({ ...m, error: e.message || '请求失败' }));
              break;
            default:
              break;
          }
        },
      );
    } catch (err: any) {
      patchLast((m) => ({ ...m, error: err?.message || '请求失败' }));
    } finally {
      setSending(false);
    }
  };

  const clearAll = () => {
    setMessages([]);
    try { localStorage.removeItem(HISTORY_KEY); } catch { /* ignore */ }
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: 'calc(100vh - 140px)', minHeight: 480, overflow: 'hidden' }}>
      {/* 顶栏 */}
      <Card size="small" style={{ marginBottom: 12 }}>
        <Space wrap style={{ width: '100%', justifyContent: 'space-between' }}>
          <Space wrap>
            <Text strong style={{ fontSize: 15 }}>AI 聊天</Text>
            <Segmented
              value={mode}
              onChange={(v) => setMode(String(v))}
              options={[
                { label: '问答', value: 'qa' },
                { label: '编程', value: 'coding' },
              ]}
            />
            <Select
              size="small"
              value={model}
              onChange={setModel}
              options={models.map((m) => ({ label: m, value: m }))}
              style={{ width: 170 }}
              popupMatchSelectWidth={false}
            />
            <Tooltip title="联网搜索：模型需要实时信息时自动搜索网页">
              <span style={{ fontSize: 12, color: 'var(--jc-fg-muted)', display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                <GlobalOutlined /> 联网
                <Switch size="small" checked={webSearch} onChange={setWebSearch} />
              </span>
            </Tooltip>
          </Space>
          <Button size="small" icon={<DeleteOutlined />} onClick={clearAll} disabled={messages.length === 0}>
            清空
          </Button>
        </Space>
      </Card>

      {/* 消息列表 */}
      <Card size="small" style={{ flex: 1, overflow: 'hidden', display: 'flex', flexDirection: 'column', minHeight: 0 }}>
        <div ref={listRef} style={{ flex: 1, overflowY: 'auto', paddingRight: 4 }}>
          {messages.length === 0 && (
            <div style={{ display: 'flex', height: '100%', alignItems: 'center', justifyContent: 'center' }}>
              <Empty
                image={Empty.PRESENTED_IMAGE_SIMPLE}
                description={<span style={{ color: 'var(--jc-fg-muted)' }}>向 JoyCode 模型提问，支持联网搜索与深度思考</span>}
              />
            </div>
          )}
          {messages.map((m, i) => (
            <div key={i} style={{ display: 'flex', justifyContent: m.role === 'user' ? 'flex-end' : 'flex-start', marginBottom: 14 }}>
              <div
                style={{
                  maxWidth: '88%',
                  padding: '10px 14px',
                  borderRadius: 12,
                  background: m.role === 'user' ? 'rgba(34,197,94,0.15)' : 'var(--jc-card-bg)',
                  border: m.role === 'user' ? '1px solid rgba(34,197,94,0.3)' : '1px solid var(--jc-card-border)',
                  color: 'var(--jc-fg)',
                  fontSize: 14,
                  lineHeight: 1.7,
                  wordBreak: 'break-word',
                }}
              >
                {/* 工具提示 */}
                {m.tools.length > 0 && (
                  <div style={{ marginBottom: 8 }}>
                    {m.tools.map((t, ti) => (
                      <div key={ti} style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, color: '#22C55E', marginBottom: 2 }}>
                        <GlobalOutlined />
                        <span>{t.status === 'running' ? '正在搜索：' : '已搜索：'}{t.query}</span>
                        {t.status === 'running' && <Spin size="small" />}
                      </div>
                    ))}
                  </div>
                )}
                {/* 深度思考（可折叠） */}
                {m.reasoning && (
                  <details style={{ marginBottom: 8, fontSize: 12, color: 'var(--jc-fg-muted)' }}>
                    <summary style={{ cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 4 }}>
                      <BulbOutlined /> 已深度思考
                    </summary>
                    <div style={{ marginTop: 6, padding: '8px 10px', background: 'rgba(59,130,246,0.08)', borderRadius: 8, whiteSpace: 'pre-wrap', lineHeight: 1.6 }}>
                      {m.reasoning}
                    </div>
                  </details>
                )}
                {/* 正文 / 错误 */}
                {m.error ? (
                  <div style={{ color: '#EF4444', fontSize: 13 }}>{m.error}</div>
                ) : (
                  <div style={{ whiteSpace: 'pre-wrap' }}>{m.content || (sending && i === messages.length - 1 ? (
                    <span style={{ color: 'var(--jc-fg-muted)' }}><Spin size="small" /> 思考中...</span>
                  ) : '')}</div>
                )}
              </div>
            </div>
          ))}
        </div>

        {/* 输入区 */}
        <div style={{ borderTop: '1px solid var(--jc-card-border)', paddingTop: 12, marginTop: 8, flexShrink: 0 }}>
          <Input.TextArea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="输入问题，Ctrl+Enter 发送"
            autoSize={{ minRows: 2, maxRows: 6 }}
            style={{ minHeight: 52 }}
            onPressEnter={(e) => { if (!e.shiftKey) { e.preventDefault(); send(); } }}
          />
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: 8 }}>
            <Text style={{ fontSize: 12, color: 'var(--jc-fg-muted)' }}>
              {mode === 'qa' ? '问答模式 · 简洁直接回答' : '编程模式 · 面向开发任务'}
            </Text>
            <Button type="primary" icon={<SendOutlined />} loading={sending} onClick={send}>
              发送
            </Button>
          </div>
        </div>
      </Card>
    </div>
  );
};

export default Chat;
