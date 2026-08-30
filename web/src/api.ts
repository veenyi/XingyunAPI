export interface Account {
  user_id: string;
  nickname: string;
  remark: string;
  api_token: string;
  is_default: boolean;
  default_model: string;
  created_at?: string;
  display_order: number;
  active_sessions: number;
  total_requests: number;
  today_requests: number;
  total_tokens: number;
  today_tokens: number;
  credential_valid: number; // -1=unknown, 0=expired, 1=valid
  credential_checked_at?: string;
  credential_refreshed_at?: string;
  credential_error?: string;
}

export function accountDisplayName(a: { nickname?: string; remark?: string; user_id: string }): string {
  if (a.remark) return a.remark;
  if (a.nickname) return a.nickname;
  return a.user_id;
}

export interface ModelInfo {
  id: string;
  name: string;
}

export interface Stats {
  total_requests: number;
  total_input_tokens: number;
  total_output_tokens: number;
  accounts_count: number;
  avg_latency_ms: number;
  error_count: number;
  stream_count: number;
  success_count: number;
  by_model: { model: string; count: number }[];
  by_account: { user_id: string; nickname: string; remark: string; count: number }[];
  all_time: {
    total_requests: number;
    total_input_tokens: number;
    total_output_tokens: number;
    error_count: number;
  };
  hourly: {
    hour: string;
    count: number;
    input_tokens: number;
    output_tokens: number;
    errors: number;
  }[];
}

export interface CustomProvider {
  id: string;
  name: string;
  base_url: string;
  api_key?: string;
  enabled: boolean;
  free_only?: boolean;
}

// --- 签到中心 ---

export interface CheckinAccount {
  id: string;
  platform: 'workbuddy' | 'traework';
  name: string;
  uid: string;
  access_token?: string;
  refresh_token?: string;
  enterprise_id?: string;
  domain?: string;
  device_id?: string;
  machine_id?: string;
  api_host?: string;
  enabled: boolean;
  last_checkin_at?: string;
  last_checkin_ok?: boolean;
  last_result?: string;
  credits?: number;
  credits_total?: number;
  last_refresh_at?: string;
  last_refresh_ok?: boolean;
}

export interface CheckinResult {
  id: string;
  name: string;
  platform: string;
  ok: boolean;
  message: string;
  credits?: number;
}

export interface Settings {
  [key: string]: string;
}

export interface AccountStats {
  user_id: string;
  nickname: string;
  remark: string;
  total_requests: number;
  total_input_tokens: number;
  total_output_tokens: number;
  success_count: number;
  stream_count: number;
  by_model: { model: string; count: number }[];
  by_endpoint: { endpoint: string; count: number }[];
  avg_latency_ms: number;
  error_count: number;
  all_time?: {
    total_requests: number;
    total_input_tokens: number;
    total_output_tokens: number;
    error_count: number;
  };
  hourly: {
    hour: string;
    count: number;
    input_tokens: number;
    output_tokens: number;
    errors: number;
  }[];
}

export interface RequestLog {
  id: number;
  user_id: string;
  model: string;
  endpoint: string;
  stream: boolean;
  status_code: number;
  latency_ms: number;
  error_message: string;
  input_tokens: number;
  output_tokens: number;
  created_at: string;
}

// 模型健康状态：一次派单候选（渠道 + 模型）此刻的可用性。
export interface ModelStatus {
  provider: string;
  model: string;
  status: 'ok' | 'cooling';
  class?: string;
  reason?: string;
  failures?: number;
  until?: string;
  ranked: boolean;
}

// 自动切换顺序里的一项。
export interface ModelRank {
  provider: string;
  model: string;
}

const TOKEN_KEY = 'joycode_jwt';

function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY);
}

export function isAuthenticated(): boolean {
  return !!getToken();
}

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) {
    headers['Authorization'] = `Bearer ${token}`;
  }
  const resp = await fetch(path, {
    headers,
    ...options,
  });
  if (resp.status === 401) {
    clearToken();
    window.location.href = '/login';
    throw new Error('Unauthorized');
  }
  if (!resp.ok) {
    const err = await resp.json().catch(() => ({ detail: resp.statusText }));
    throw new Error(err.detail || `HTTP ${resp.status}`);
  }
  return resp.json();
}

async function authRequest<T>(path: string, options?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  });
  if (!resp.ok) {
    const err = await resp.json().catch(() => ({ detail: resp.statusText }));
    throw new Error(err.detail || `HTTP ${resp.status}`);
  }
  return resp.json();
}

export const authApi = {
  status: () => authRequest<{ initialized: boolean; exe_path?: string }>('/api/auth/status'),
  setup: (password: string) =>
    authRequest<{ ok: boolean; token: string }>('/api/auth/setup', {
      method: 'POST',
      body: JSON.stringify({ password }),
    }),
  login: (password: string) =>
    authRequest<{ ok: boolean; token: string }>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ password }),
    }),
  changePassword: (oldPassword: string, newPassword: string) =>
    request<{ ok: boolean }>('/api/auth/change-password', {
      method: 'POST',
      body: JSON.stringify({ old_password: oldPassword, new_password: newPassword }),
    }),
};

export const api = {
  listAccounts: () => request<{ accounts: Account[] }>('/api/accounts').then(r => r.accounts),
  addAccount: (data: { user_id: string; pt_key: string; nickname?: string; is_default?: boolean; default_model?: string }) =>
    request<{ ok: boolean }>('/api/accounts', { method: 'POST', body: JSON.stringify(data) }),
  removeAccount: (userId: string) =>
    request<{ ok: boolean }>(`/api/accounts/${encodeURIComponent(userId)}`, { method: 'DELETE' }),
  setDefault: (userId: string) =>
    request<{ ok: boolean }>(`/api/accounts/${encodeURIComponent(userId)}/default`, { method: 'PUT' }),
  validateAccount: (userId: string) =>
    request<{ valid: boolean }>(`/api/accounts/${encodeURIComponent(userId)}/validate`, { method: 'POST' }),
  listModels: () => request<{ models: ModelInfo[] }>('/api/models').then(r => r.models),
  getChatHistory: (userId?: string) => request<{ messages: any[] }>(`/api/chat-history${userId ? `?user_id=${encodeURIComponent(userId)}` : ''}`),
  saveChatHistory: (messages: any[], userId?: string) =>
    request<{ ok: boolean }>('/api/chat-history', { method: 'POST', body: JSON.stringify({ messages, user_id: userId || 'root' }) }),
  chatStream: (
    params: { messages: { role: string; content: string; images?: string[] }[]; model?: string; mode?: string; web_search?: boolean; user_id?: string },
    onEvent: (e: any) => void,
  ): Promise<void> => {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    const token = getToken();
    if (token) headers['Authorization'] = `Bearer ${token}`;
    return fetch('/api/chat', {
      method: 'POST',
      headers,
      body: JSON.stringify(params),
    }).then(async (res) => {
      if (res.status === 401) {
        clearToken();
        window.location.href = '/login';
        throw new Error('登录已过期，请重新登录');
      }
      if (!res.ok || !res.body) throw new Error('请求失败，请检查服务状态');
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx: number;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const raw = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          for (const line of raw.split('\n')) {
            if (!line.startsWith('data:')) continue;
            const d = line.slice(5).trim();
            if (!d || d.startsWith(':')) continue;
            try { onEvent(JSON.parse(d)); } catch { /* ignore */ }
          }
        }
      }
    });
  },
  listAccountModels: (userId: string) =>
    request<{ models: ModelInfo[] }>(`/api/accounts/${encodeURIComponent(userId)}/models`).then(r => r.models),
  getStats: () => request<Stats>('/api/stats'),
  getSettings: () => request<{ settings: Settings }>('/api/settings').then(r => r.settings),
  updateSettings: (data: Settings) =>
    request<{ ok: boolean }>('/api/settings', { method: 'PUT', body: JSON.stringify(data) }),
  listModelStatus: () =>
    request<{ models: ModelStatus[] }>('/api/model-status').then(r => r.models ?? []),
  updateModelRanking: (ranking: ModelRank[]) =>
    request<{ ok: boolean; count: number }>('/api/model-ranking', {
      method: 'PUT',
      body: JSON.stringify({ ranking }),
    }),
  rotateAggregateKey: () =>
    request<{ ok: boolean; key: string }>('/api/rotate-aggregate-key', { method: 'POST' }),
  getCustomProviders: () =>
    request<{ providers: CustomProvider[] }>('/api/custom_providers').then(r => r.providers ?? []),
  saveCustomProviders: (providers: CustomProvider[]) =>
    request<{ ok: boolean }>('/api/custom_providers', { method: 'PUT', body: JSON.stringify(providers) }),
  refreshChannels: () =>
    request<{ ok: boolean; results: Record<string, unknown> }>('/api/channels/refresh', { method: 'POST' }),
  getModelBlocklist: () =>
    request<{ blocked: string[] }>('/api/model-blocklist').then(r => r.blocked ?? []),
  setModelHidden: (key: string, hidden: boolean) =>
    request<{ ok: boolean }>('/api/model-blocklist', { method: 'POST', body: JSON.stringify({ key, hidden }) }),
  // --- 签到中心 ---
  listCheckin: () =>
    request<{ accounts: CheckinAccount[]; times: string[] }>('/api/checkin/accounts'),
  saveCheckinAccounts: (accounts: CheckinAccount[]) =>
    request<{ ok: boolean }>('/api/checkin/accounts', { method: 'PUT', body: JSON.stringify({ accounts }) }),
  removeCheckinAccount: (id: string) =>
    request<{ ok: boolean }>(`/api/checkin/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  runCheckin: (opts: { id?: string; all?: boolean }) =>
    request<{ results: CheckinResult[] }>('/api/checkin/run', { method: 'POST', body: JSON.stringify(opts) }),
  getCheckinConfig: () =>
    request<{ times: string[] }>('/api/checkin/config').then(r => r.times),
  saveCheckinConfig: (times: string[]) =>
    request<{ ok: boolean; times: string[] }>('/api/checkin/config', { method: 'PUT', body: JSON.stringify({ times }) }),
  getHealth: () => request<{ status: string; accounts: number }>('/api/health'),
  updateAccountModel: (userId: string, defaultModel: string) =>
    request<{ ok: boolean }>(`/api/accounts/${encodeURIComponent(userId)}/model`, {
      method: 'PUT',
      body: JSON.stringify({ default_model: defaultModel }),
    }),
  getAccountStats: (userId: string) =>
    request<AccountStats>(`/api/accounts/${encodeURIComponent(userId)}/stats`),
  getAccountPoint: (userId: string) =>
    request<any>(`/api/accounts/${encodeURIComponent(userId)}/point`),
  getAccountLogs: (userId: string, limit = 200) =>
    request<{ logs: RequestLog[]; total: number }>(`/api/accounts/${encodeURIComponent(userId)}/logs?limit=${limit}`),
  renewToken: (userId: string) =>
    request<{ ok: boolean; api_token: string }>(`/api/accounts/${encodeURIComponent(userId)}/renew-token`, { method: 'POST' }),
  autoLogin: () =>
    request<{ ok: boolean; user_id: string; nickname: string; real_name: string; is_default: boolean }>('/api/accounts-auto-login', { method: 'POST' }),
  qrLoginInit: () =>
    request<{ ok: boolean; session_id: string; qr_image: string }>('/api/qr-login/init', { method: 'POST' }),
  jdHptLoginInit: () =>
    request<{ ok: boolean; session_id: string; login_url: string }>('/api/jdhgpt-login/init', { method: 'POST' }),
  jdHptLoginStatus: (session: string) =>
    request<{ status: string; ok?: boolean; user_id?: string; nickname?: string; erp?: string; message?: string }>(
      `/api/jdhgpt-login/status?session=${encodeURIComponent(session)}`,
    ),
  qrLoginStatus: (sessionId: string) =>
    request<{ status: string; ok?: boolean; user_id?: string; nickname?: string; real_name?: string; message?: string; verify_url?: string; risk_code?: number }>(`/api/qr-login/status?session=${encodeURIComponent(sessionId)}`),
  browserLogin: () =>
    request<{ ok: boolean; url: string; token: string }>('/api/browser-login', { method: 'POST' }),
  oauthSubmit: (ptKey: string) =>
    request<{ ok: boolean; user_id: string; nickname: string }>('/api/oauth-submit', { method: 'POST', body: JSON.stringify({ pt_key: ptKey }) }),
  getRecentErrors: (limit = 50) =>
    request<{ errors: RequestLog[]; total: number }>(`/api/errors?limit=${limit}`),
  getGitHubStars: () =>
    request<{ stars: number }>('/api/github-stars').then(r => r.stars),
  clearAllAccounts: () =>
    request<{ ok: boolean; count: number }>('/api/accounts-clear-all', { method: 'POST' }),
  clearJoyCodeSession: () =>
    request<{ ok: boolean; message: string }>('/api/clear-joycode-session', { method: 'POST' }),
  updateRemark: (userId: string, remark: string) =>
    request<{ ok: boolean }>(`/api/accounts/${encodeURIComponent(userId)}/remark`, { method: 'PUT', body: JSON.stringify({ remark }) }),
  reorderAccounts: (userIds: string[]) =>
    request<{ ok: boolean }>('/api/accounts/reorder', { method: 'PUT', body: JSON.stringify({ user_ids: userIds }) }),
  exportAccounts: () =>
    request<{ ok: boolean; accounts: Array<{ user_id: string; nickname: string; remark: string; pt_key: string; is_default: boolean; default_model: string; display_order: number }>; count: number }>('/api/accounts-export'),
  importAccounts: (accounts: Array<{ user_id: string; nickname: string; remark: string; pt_key: string; is_default: boolean; default_model: string; display_order: number }>) =>
    request<{ ok: boolean; added: number; updated: number; total: number }>('/api/accounts-import', { method: 'POST', body: JSON.stringify({ accounts }) }),
};
