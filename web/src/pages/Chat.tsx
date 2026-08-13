import React, { useEffect, useRef, useState } from 'react';
import {
  Card, Typography, Segmented, Select, Switch, Input, Button, Space, Spin, Empty, Tooltip, Upload, message,
} from 'antd';
import {
  SendOutlined, DeleteOutlined, GlobalOutlined, BulbOutlined, PaperClipOutlined, UserOutlined, DownloadOutlined,
} from '@ant-design/icons';
import { api } from '../api';

interface Msg {
  role: 'user' | 'assistant';
  content: string;
  reasoning: string;   // 深度思考内容（可折叠）
  tools: { tool: string; query: string; status: 'running' | 'done' }[];
  images?: string[];   // 图片 data URL
  error?: string;
}

const { Text } = Typography;

const FALLBACK_MODELS = ["JoyAI-Code-1.5", "MiniMax-M3", "MiniMax-M2.7", "Kimi-K2.6", "GLM-5.1", "GLM-5", "DeepSeek-V4-Pro", "Doubao-Seed-2.0-pro"];

const HISTORY_LIMIT = 200; // 最多保留 200 条消息
const MAX_IMG_SIZE = 5 * 1024 * 1024; // 单图 5MB

const Chat: React.FC = () => {
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState('');
  const [model, setModel] = useState<string>('GLM-5.1');
  const [mode, setMode] = useState<string>('qa');
  const [webSearch, setWebSearch] = useState(true);
  const [models, setModels] = useState<string[]>(FALLBACK_MODELS);
  const [sending, setSending] = useState(false);
  const [historyLoaded, setHistoryLoaded] = useState(false);
  const [accounts, setAccounts] = useState<{ label: string; value: string }[]>([]);
  const [accountId, setAccountId] = useState<string>('');
  const [pendingImages, setPendingImages] = useState<string[]>([]);
  const listRef = useRef<HTMLDivElement>(null);

  // 加载账号列表（聊天账号切换下拉）
  useEffect(() => {
    api.listAccounts()
      .then((list) => {
        const opts = (list || []).map((a: any) => ({
          label: a.nickname || a.remark || a.user_id,
          value: a.user_id,
        }));
        setAccounts(opts);
        if (opts.length > 0 && !accountId) {
          const def = (list || []).find((a: any) => a.is_default);
          setAccountId(def ? def.user_id : opts[0].value);
        }
      })
      .catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 从服务器加载聊天历史（按账号隔离，切换账号自动加载对应会话）
  useEffect(() => {
    if (!accountId) return;
    setHistoryLoaded(false);
    setMessages([]);
    api.getChatHistory(accountId)
      .then((res) => {
        if (Array.isArray(res.messages)) {
          const valid = res.messages.filter((m) => m && m.role);
          setMessages(valid);
        }
      })
      .catch(() => {})
      .finally(() => setHistoryLoaded(true));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [accountId]);

  // 消息变化时保存到服务器（按账号隔离）
  useEffect(() => {
    if (!historyLoaded) return;
    const toSave = messages.slice(-HISTORY_LIMIT);
    api.saveChatHistory(toSave, accountId).catch(() => {});
  }, [messages, historyLoaded, accountId]);

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
    if ((!text && pendingImages.length === 0) || sending) return;
    const imgs = [...pendingImages];
    setInput('');
    setPendingImages([]);
    setMessages((prev) => [...prev, { role: 'user', content: text, reasoning: '', tools: [], images: imgs }]);
    // 追加一个空的 assistant 消息用于流式填充
    setMessages((prev) => [...prev, { role: 'assistant', content: '', reasoning: '', tools: [] }]);
    setSending(true);

    // 历史消息（去掉最后的空 assistant）
    const history = messages.filter((m) => m.role === 'user' || (m.role === 'assistant' && m.content)).map((m) => ({
      role: m.role,
      content: m.content || '',
      ...(m.images && m.images.length > 0 ? { images: m.images } : {}),
    }));
    history.push({ role: 'user', content: text, ...(imgs.length > 0 ? { images: imgs } : {}) });

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
        { messages: history, model, mode, web_search: webSearch, user_id: accountId || undefined },
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
    api.saveChatHistory([], accountId).catch(() => {});
  };

  // 导出当前会话为 Markdown
  const exportChat = () => {
    if (messages.length === 0) {
      message.info('当前没有可导出的聊天记录');
      return;
    }
    const md = messages.map((m) => {
      const head = m.role === 'user' ? '## 我' : '## 助手';
      let body = m.content || '';
      if (m.reasoning) body += `\n\n<details><summary>深度思考</summary>\n\n${m.reasoning}\n\n</details>`;
      return `${head}\n\n${body}`;
    }).join('\n\n---\n\n');
    const blob = new Blob([md], { type: 'text/markdown;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `行云聊天-${accountId || 'root'}-${new Date().toISOString().slice(0, 10)}.md`;
    a.click();
    URL.revokeObjectURL(url);
  };

  // 通用附件：读取文本/代码文件内容拼接到输入
  const addTextAttachments = (files: FileList | File[]) => {
    Array.from(files).forEach((f) => {
      if (f.size > 2 * 1024 * 1024) {
        message.error(`文件「${f.name}」超过 2MB，请自行粘贴内容`);
        return;
      }
      const reader = new FileReader();
      reader.onload = () => {
        const text = String(reader.result || '');
        setInput((prev) => `${prev}\n\n【附件：${f.name}】\n${text}`.trim());
      };
      reader.readAsText(f);
    });
  };

  // 把图片文件转 data URL 加入待发列表
  const addImages = (files: FileList | File[]) => {
    const list = Array.from(files);
    list.forEach((f) => {
      if (!f.type.startsWith('image/')) {
        message.error('仅支持图片文件');
        return;
      }
      if (f.size > MAX_IMG_SIZE) {
        message.error('单张图片不能超过 5MB');
        return;
      }
      const reader = new FileReader();
      reader.onload = () => setPendingImages((prev) => [...prev, reader.result as string]);
      reader.readAsDataURL(f);
    });
  };

  // 粘贴图片
  const onPaste = (e: React.ClipboardEvent) => {
    const items = e.clipboardData?.items;
    if (!items) return;
    const files: File[] = [];
    for (const item of items) {
      if (item.type.startsWith('image/')) {
        const f = item.getAsFile();
        if (f) files.push(f);
      }
    }
    if (files.length > 0) {
      e.preventDefault();
      addImages(files);
    }
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0, overflow: 'hidden' }}>
      {/* 顶栏 */}
      <Card size="small" style={{ marginBottom: 12 }}>
        <Space wrap style={{ width: '100%', justifyContent: 'space-between' }}>
          <Space wrap>
            <Text strong style={{ fontSize: 15 }}>AI 聊天</Text>
            <Select
              size="small"
              value={accountId || undefined}
              onChange={(v) => setAccountId(v)}
              placeholder="选择账号"
              options={accounts}
              suffixIcon={<UserOutlined />}
              style={{ width: 160 }}
              popupMatchSelectWidth={false}
            />
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
          <Space>
            <Button size="small" icon={<DownloadOutlined />} onClick={exportChat} disabled={messages.length === 0}>
              导出
            </Button>
            <Button size="small" icon={<DeleteOutlined />} onClick={clearAll} disabled={messages.length === 0}>
              清空
            </Button>
          </Space>
        </Space>
      </Card>

      {/* 消息列表 — 普通 div（antd Card 的 body 不收缩会导致列表无法滚动） */}
      <div
        style={{
          flex: 1,
          minHeight: 0,
          overflow: 'hidden',
          display: 'flex',
          flexDirection: 'column',
          background: 'var(--jc-bg-elevated)',
          border: '1px solid var(--jc-card-border)',
          borderRadius: 12,
          padding: '12px',
        }}
      >
        <div ref={listRef} style={{ flex: 1, overflowY: 'auto', paddingRight: 4, minHeight: 0 }}>
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
                {/* 联网搜索过程（折叠） */}
                {m.tools.length > 0 && (
                  <details style={{ marginBottom: 8, fontSize: 12, color: '#22C55E' }}>
                    <summary style={{ cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 4 }}>
                      <GlobalOutlined /> 联网搜索 · {m.tools.length} 次
                    </summary>
                    <div style={{ marginTop: 6, padding: '6px 10px', background: 'rgba(34,197,94,0.08)', borderRadius: 8 }}>
                      {m.tools.map((t, ti) => (
                        <div key={ti} style={{ display: 'flex', alignItems: 'center', gap: 6, marginBottom: 2 }}>
                          <span>{t.status === 'running' ? '正在搜索' : '已搜索'}：{t.query}</span>
                          {t.status === 'running' && <Spin size="small" />}
                        </div>
                      ))}
                    </div>
                  </details>
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
                {/* 用户图片 */}
                {m.images && m.images.length > 0 && (
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 8 }}>
                    {m.images.map((img, ii) => (
                      <img key={ii} src={img} alt="附件" style={{ maxWidth: 200, maxHeight: 200, borderRadius: 8, border: '1px solid var(--jc-card-border)' }} />
                    ))}
                  </div>
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
      </div>

      {/* 输入区 — 固定在页面底部（独立于聊天卡片，永不随内容滚动/下移） */}
      <div
        style={{
          flexShrink: 0,
          marginTop: 12,
          background: 'var(--jc-bg-elevated)',
          border: '1px solid var(--jc-card-border)',
          borderRadius: 12,
          padding: '12px 14px',
        }}
      >
        <Input.TextArea
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="输入问题，Ctrl+Enter 发送；可直接粘贴或上传图片"
          autoSize={{ minRows: 2, maxRows: 6 }}
          style={{ minHeight: 52 }}
          onPaste={onPaste}
          onPressEnter={(e) => { if (!e.shiftKey) { e.preventDefault(); send(); } }}
        />
        {pendingImages.length > 0 && (
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginTop: 8 }}>
            {pendingImages.map((img, i) => (
              <div key={i} style={{ position: 'relative' }}>
                <img src={img} alt="待发送" style={{ width: 64, height: 64, objectFit: 'cover', borderRadius: 6, border: '1px solid var(--jc-card-border)' }} />
                <Button
                  size="small" type="text" danger
                  style={{ position: 'absolute', top: -8, right: -8, fontSize: 12, padding: 0, minWidth: 18, height: 18, borderRadius: 9, background: 'var(--jc-bg-elevated)' }}
                  onClick={() => setPendingImages((prev) => prev.filter((_, idx) => idx !== i))}
                >
                  ×
                </Button>
              </div>
            ))}
          </div>
        )}
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: 8 }}>
          <Space size={12}>
            <Upload
              multiple
              showUploadList={false}
              beforeUpload={(file) => {
                if (file.type.startsWith('image/')) addImages([file]);
                else addTextAttachments([file]);
                return false;
              }}
            >
              <Button size="small" icon={<PaperClipOutlined />}>附件</Button>
            </Upload>
            <Text style={{ fontSize: 12, color: 'var(--jc-fg-muted)' }}>
              {mode === 'qa' ? '问答模式 · 简洁直接回答' : '编程模式 · 面向开发任务'}
            </Text>
          </Space>
          <Button type="primary" icon={<SendOutlined />} loading={sending} onClick={send}>
            发送
          </Button>
        </div>
      </div>
    </div>
  );
};

export default Chat;
