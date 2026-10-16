// 鉴权 / 会话 / 账户 / 通行密钥 / 站点信息 / 注册邀请 / 客户端诊断上报。
import {
  ApiError,
  apiRequest as req,
  clearToken,
  getToken,
  persistToken,
  REQ_TIMEOUT_MS,
  SERVER_URL,
  statusMessage,
  trimSlash,
} from './http';
import { deviceId } from './channel';

const USER_KEY = 'hearth_user';

// 系统角色阶梯：guest < user < power < admin < super（前端不推导，只做显隐）
export type Role = 'guest' | 'user' | 'power' | 'admin' | 'super';

export interface User {
  id: number;
  username: string;
  role: Role;
  expires_at: string | null; // 仅访客有值
  is_admin: boolean; // 派生只读（role ≥ admin），过渡一个版本后删
  passkey_count?: number; // 只有 /api/me 带；登录后推荐卡片据此判断「还没有通行密钥」
  can_claim?: boolean; // 服务端下发：访客能否转正（是访客 + 站点开了 guest_claim），转正入口的唯一依据
}

// 能不能发注册邀请（建频道同此档）：power 及以上
export function canInvite(u: User | null): boolean {
  return u?.role === 'power' || u?.role === 'admin' || u?.role === 'super';
}

// 访客身份：无密码、有过期时间、会话绑定这台浏览器；能力显隐按服务端下发的 role
export function isGuest(u: User | null): boolean {
  return u?.role === 'guest';
}

// 访客还剩多久（提示条与账号 pane 共用的说法）：粗到天/小时/分钟就够，不做秒级倒计时
export function guestTimeLeft(u: User | null): string {
  const ms = u?.expires_at ? new Date(u.expires_at).getTime() - Date.now() : 0;
  if (ms <= 0) return '即将过期';
  const min = Math.round(ms / 60000);
  if (min >= 1440) return `${Math.floor(min / 1440)} 天后过期`;
  if (min >= 60) return `${Math.floor(min / 60)} 小时后过期`;
  return `${Math.max(1, min)} 分钟后过期`;
}

export function getUser(): User | null {
  const raw = localStorage.getItem(USER_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as User;
  } catch {
    // 存档损坏：当作未登录，别让每次读用户都抛
    clearSession();
    return null;
  }
}

function saveSession(token: string, user: User) {
  persistToken(token);
  localStorage.setItem(USER_KEY, JSON.stringify(user));
}

export function clearSession() {
  clearToken();
  localStorage.removeItem(USER_KEY);
}

export async function register(username: string, password: string, invite?: string): Promise<User> {
  const data = await req<{ token: string; user: User }>('/api/register', {
    method: 'POST',
    body: { username, password, invite },
  });
  saveSession(data.token, data.user);
  return data.user;
}

// 会话过期后本地信息可能陈旧，进应用时拉一次 /api/me 校准
export async function fetchMe(): Promise<User> {
  const me = await req<User>('/api/me');
  localStorage.setItem(USER_KEY, JSON.stringify(me));
  return me;
}

export async function login(username: string, password: string): Promise<User> {
  const data = await req<{ token: string; user: User }>('/api/login', {
    method: 'POST',
    body: { username, password },
  });
  saveSession(data.token, data.user);
  return data.user;
}

// 指定地址登录并存会话：桌面壳「在本机运行服务器」刚建好账号时用。
// SERVER_URL 在模块加载时就定死了，这一刻还是旧地址，只能显式给 origin。
export async function loginTo(origin: string, username: string, password: string): Promise<void> {
  const res = await fetch(`${trimSlash(origin)}/api/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
    signal: AbortSignal.timeout(REQ_TIMEOUT_MS),
  });
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null;
    throw new ApiError(res.status, data?.error ?? statusMessage(res.status));
  }
  const data = (await res.json()) as { token: string; user: User };
  saveSession(data.token, data.user);
}

export async function logout(): Promise<void> {
  try {
    await req('/api/logout', { method: 'POST' });
  } finally {
    clearSession();
  }
}

// ---- 前端诊断 ----

export interface ClientLogEntry {
  level: 'info' | 'warn' | 'error';
  event: string;
  session: string;
  channel?: string;
  role?: 'voice' | 'stage';
  engine?: string;
  endpoint?: string;
  attempt?: number;
  elapsed_ms?: number;
  state?: string;
  detail?: string;
  reason?: string;
  error?: unknown;
}

function redactDiagnosticText(value: string, max: number): string {
  return value
    .replace(/\bBearer\s+\S+/gi, 'Bearer <redacted>')
    .replace(/\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_-]{10,})?\b/g, '<token>')
    .replace(/\b((?:https?|wss?):\/\/[^\s?]+)\?[^\s]+/gi, '$1?<redacted>')
    .replace(/((?:access[_-]?token|token|secret|signature|authorization|api[_-]?key|key)=)[^&\s]+/gi, '$1<redacted>')
    .replace(/[\u0000-\u001f\u007f]+/g, ' ')
    .trim()
    .slice(0, max);
}

function diagnosticNetwork(): string {
  const c = (navigator as Navigator & {
    connection?: { type?: string; effectiveType?: string; saveData?: boolean; downlink?: number; rtt?: number };
  }).connection;
  if (!c) return '';
  return [c.type, c.effectiveType, c.saveData ? 'save-data' : '', c.downlink == null ? '' : `${c.downlink}mbps`, c.rtt == null ? '' : `${c.rtt}ms`]
    .filter(Boolean)
    .join(',');
}

// 诊断上报必须完全旁路业务请求：失败静默、短超时，不触发 401 跳转或 toast。
export function reportClientLog(entry: ClientLogEntry): void {
  const token = getToken();
  if (!token) return;
  let errorName = '';
  let errorMessage = '';
  if (entry.error != null) {
    if (entry.error instanceof Error || entry.error instanceof DOMException) {
      errorName = entry.error.name;
      errorMessage = entry.error.message || String(entry.error);
    } else {
      errorName = typeof entry.error;
      errorMessage = String(entry.error);
    }
  }
  const payload = {
    level: entry.level,
    event: entry.event,
    session: redactDiagnosticText(entry.session, 40),
    channel: redactDiagnosticText(entry.channel ?? '', 120),
    role: entry.role,
    engine: redactDiagnosticText(entry.engine ?? '', 40),
    endpoint: redactDiagnosticText(entry.endpoint ?? '', 200),
    attempt: entry.attempt ?? 0,
    elapsed_ms: Math.max(0, Math.round(entry.elapsed_ms ?? 0)),
    online: navigator.onLine,
    visibility: document.visibilityState,
    network: diagnosticNetwork(),
    state: redactDiagnosticText(entry.state ?? '', 80),
    detail: redactDiagnosticText(entry.detail ?? '', 2000),
    reason: redactDiagnosticText(entry.reason ?? '', 120),
    error_name: redactDiagnosticText(errorName, 80),
    error_message: redactDiagnosticText(errorMessage, 600),
  };
  const controller = new AbortController();
  const timer = window.setTimeout(() => controller.abort(), 5000);
  void fetch(`${SERVER_URL}/api/client-log`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
    signal: controller.signal,
    keepalive: true,
  })
    .catch(() => {})
    .finally(() => clearTimeout(timer));
}

// ---- 账户 ----

export async function updateUsername(username: string): Promise<User> {
  const u = await req<User>('/api/account/username', { method: 'POST', body: { username } });
  localStorage.setItem(USER_KEY, JSON.stringify(u));
  window.dispatchEvent(new CustomEvent('hearth:user'));
  return u;
}

export function updatePassword(current: string, next: string): Promise<void> {
  return req('/api/account/password', { method: 'POST', body: { current, new: next } });
}

export interface DeviceRecord {
  device_id: string;
  tag: string;
  first_seen: string;
  last_seen: string;
}

export async function listMyDevices(): Promise<DeviceRecord[]> {
  const data = await req<{ devices: DeviceRecord[] | null }>('/api/account/devices');
  return data.devices ?? [];
}

export function deleteMyDevice(deviceID: string): Promise<void> {
  return req(`/api/account/devices/${encodeURIComponent(deviceID)}`, { method: 'DELETE' });
}

// 登录会话（"我的登录状态"）：id 是服务端给的 token 指纹，token 本身不下发
export interface SessionRecord {
  id: string;
  user_agent: string;
  created_at: string | null;
  last_seen: string | null;
  expires_at: string;
  current: boolean;
}

export async function listMySessions(): Promise<SessionRecord[]> {
  const data = await req<{ sessions: SessionRecord[] | null }>('/api/account/sessions');
  return data.sessions ?? [];
}

export function deleteMySession(id: string): Promise<void> {
  return req(`/api/account/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

// ---- 通行密钥（WebAuthn）----
// 这里只负责收发；挑战与凭证的格式转换在 passkey.ts。
// options 是服务端直出的 PublicKeyCredential*OptionsJSON，前端不改字段，只做 base64url 解码。

export interface PasskeyRecord {
  id: number;
  name: string;
  created_at: string;
  last_used_at: string | null;
  backup_eligible: boolean;
  backup_state: boolean; // 已同步/已备份到账号（换设备也还在）
}

export type PasskeyOptions = Record<string, unknown>;

export interface PasskeyCeremony {
  ceremony_id: string;
  options: PasskeyOptions;
}

export function passkeyLoginBegin(): Promise<PasskeyCeremony> {
  return req<PasskeyCeremony>('/api/auth/passkey/login/begin', { method: 'POST' });
}

// 与密码登录同一条签发路径，成功后本地会话按同样方式落地
export async function passkeyLoginFinish(ceremonyId: string, credential: unknown): Promise<User> {
  const data = await req<{ token: string; user: User }>('/api/auth/passkey/login/finish', {
    method: 'POST',
    body: { ceremony_id: ceremonyId, credential },
  });
  saveSession(data.token, data.user);
  return data.user;
}

// ---- 桌面端浏览器跳转登录 ----
// 壳把系统浏览器指向 #/device-auth，用户在浏览器里登录并点「允许」后签一次性码；
// 壳收到 hearth:// 深链后拿码 + 自己手上的 verifier 换会话。

export async function deviceApprove(challenge: string): Promise<string> {
  const data = await req<{ code: string }>('/api/auth/device/approve', { method: 'POST', body: { challenge } });
  return data.code;
}

// 与密码登录同一条签发路径，成功后本地会话按同样方式落地
export async function deviceExchange(code: string, verifier: string): Promise<User> {
  const data = await req<{ token: string; user: User }>('/api/auth/device/exchange', {
    method: 'POST',
    body: { code, verifier },
  });
  saveSession(data.token, data.user);
  return data.user;
}

export async function listPasskeys(): Promise<PasskeyRecord[]> {
  const data = await req<{ passkeys: PasskeyRecord[] | null }>('/api/account/passkeys');
  return data.passkeys ?? [];
}

export function passkeyRegisterBegin(): Promise<PasskeyCeremony> {
  return req<PasskeyCeremony>('/api/account/passkeys/begin', { method: 'POST' });
}

export function passkeyRegisterFinish(ceremonyId: string, credential: unknown, name?: string): Promise<PasskeyRecord> {
  return req<PasskeyRecord>('/api/account/passkeys/finish', {
    method: 'POST',
    body: { ceremony_id: ceremonyId, credential, name },
  });
}

export function renamePasskey(id: number, name: string): Promise<void> {
  return req(`/api/account/passkeys/${id}`, { method: 'PATCH', body: { name } });
}

export function deletePasskey(id: number): Promise<void> {
  return req(`/api/account/passkeys/${id}`, { method: 'DELETE' });
}

// ---- 邀请（注册类）----

export interface InviteInfo {
  inviter: string;
  expires_at: string;
  alive: boolean;
  kind: string; // register / guest
  channel_name: string; // guest 类授予的频道
  allow_guest: boolean; // register 类是否允许「先以访客进入」
  guest_ttl_sec: number; // guest 类产出访客的寿命
  role: string; // register 类产出档（空 = 跟随注册默认档）
}

export function inviteInfo(code: string): Promise<InviteInfo> {
  return req(`/api/invites/${encodeURIComponent(code)}`);
}

// 访客入场：产出的会话绑定这台浏览器的 device_id，返回目标频道（guest 类才有）
export async function guestEntry(code: string, username: string): Promise<{ user: User; channel: string }> {
  const data = await req<{ token: string; user: User; channel: string }>(
    `/api/invites/${encodeURIComponent(code)}/guest`,
    { method: 'POST', body: { username, device_id: deviceId() } },
  );
  saveSession(data.token, data.user);
  return { user: data.user, channel: data.channel };
}

// 访客转正：user_id 不变，当前会话继续有效（服务端解除设备绑定），本地用户信息换成新角色
export async function claimAccount(username: string, password: string): Promise<User> {
  const u = await req<User>('/api/account/claim', { method: 'POST', body: { username, password } });
  localStorage.setItem(USER_KEY, JSON.stringify(u));
  window.dispatchEvent(new CustomEvent('hearth:user'));
  return u;
}

// 注册邀请管理（power+）：发链接、列自己发的（admin+ 列全部）、撤销
export interface Invite {
  id: number;
  code: string;
  kind: string; // register / guest（guest 类由频道管理出，阶段二）
  channel_id: number | null;
  channel_name: string;
  role: string; // register 类产出档（空 = 跟随注册默认档）
  guest_ttl_sec: number;
  allow_guest: boolean;
  note: string;
  max_uses: number;
  used: number;
  revoked: boolean;
  created_by: string;
  created_at: string;
  expires_at: string;
}

export async function listInvites(): Promise<{ invites: Invite[]; base: string }> {
  const data = await req<{ invites: Invite[] | null; base: string }>('/api/invites');
  return { invites: data.invites ?? [], base: data.base };
}

// role 仅 admin+ 可指定（user/power）；空 = 跟随注册默认档。
// allowGuest = 对方可以「先以访客进入」再注册转正
export function createInvite(
  note: string,
  maxUses: number,
  ttl: string,
  role = '',
  allowGuest = false,
): Promise<{ invite: Invite; url: string }> {
  return req('/api/invites', { method: 'POST', body: { note, max_uses: maxUses, ttl, role, allow_guest: allowGuest } });
}

export function deleteInvite(id: number): Promise<void> {
  return req(`/api/invites/${id}`, { method: 'DELETE' });
}

export interface SiteInfo {
  name: string;
  policy: string;
  tls_source: 'off' | 'self' | 'file' | 'upload'; // 当前生效的证书来源
  http_port: number; // ADDR 的端口号；合并模式下与页面端口相同
  watch_diag: boolean; // 观看诊断总开关（服务端 cfg_watch_diag），客户端只听这一份
}

// 登录页与邀请页只需要站点名和注册策略；首启流程不在本发布线。
export function siteInfo(): Promise<SiteInfo> {
  return req('/api/site');
}

// 房间里多处要站点配置（观看诊断开关、拼 WHIP 地址），共用同一次请求。
// 取舍：一次页面生命周期内只取一次，管理员在后台改完要刷新或重新进房才生效——
// 为一个排查用的开关加轮询或推送不值当。失败不留缓存，下次调用重试。
let sitePromise: Promise<SiteInfo> | null = null;
export function siteInfoCached(): Promise<SiteInfo> {
  if (!sitePromise) {
    sitePromise = siteInfo().catch((e) => {
      sitePromise = null;
      throw e;
    });
  }
  return sitePromise;
}
