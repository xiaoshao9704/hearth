// 与 server 交互的 REST 客户端，会话 token 存 localStorage（MVP 简化处理）。
import { toast } from './ui';

const SERVER_URL: string = import.meta.env.VITE_SERVER_URL ?? 'http://localhost:8080';
export const LIVEKIT_URL_FALLBACK: string =
  import.meta.env.VITE_LIVEKIT_URL ?? 'ws://localhost:7880';

const TOKEN_KEY = 'hearth_token';
const USER_KEY = 'hearth_user';

// 系统角色阶梯：guest < user < power < admin < super（前端不推导，只做显隐）
export type Role = 'guest' | 'user' | 'power' | 'admin' | 'super';
// 频道角色：owner / moderator / member，空串 = 无角色
export type ChannelRole = 'owner' | 'moderator' | 'member' | '';

export interface User {
  id: number;
  username: string;
  role: Role;
  expires_at: string | null; // 仅访客有值
  is_admin: boolean; // 派生只读（role ≥ admin），过渡一个版本后删
}

export interface Channel {
  id: number;
  name: string;
  created_by: string;
  created_at: string;
  invite_only: boolean;
  my_role: ChannelRole; // 当前用户在该频道的角色（服务端下发）
  online: number;
}

// 能不能发注册邀请（建频道同此档）：power 及以上
export function canInvite(u: User | null): boolean {
  return u?.role === 'power' || u?.role === 'admin' || u?.role === 'super';
}

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
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
  localStorage.setItem(TOKEN_KEY, token);
  localStorage.setItem(USER_KEY, JSON.stringify(user));
}

export function clearSession() {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
}

export function wsBase(): string {
  return SERVER_URL.replace(/^http/, 'ws');
}

// 请求失败的统一错误类型：status 是 HTTP 状态码，0 表示网络层失败（连不上 / 超时）。
// 调用方据此区分「服务器拒绝」与「压根没到服务器」，message 一律是可直接展示的中文。
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// 服务端没给 error 字段时的兜底文案
function statusMessage(status: number): string {
  if (status === 403) return '没有权限执行此操作';
  if (status === 404) return '资源不存在';
  if (status === 429) return '操作太频繁，稍后再试';
  if (status >= 500) return '服务器出错了，稍后再试';
  return `请求失败 (${status})`;
}

// 登录后回跳用：401 时记下当前 hash，登录页取用后自行清除
const NEXT_KEY = 'hearth_next';
const REQ_TIMEOUT_MS = 15000;

async function req<T>(path: string, options: { method?: string; body?: unknown } = {}): Promise<T> {
  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  let res: Response;
  try {
    res = await fetch(`${SERVER_URL}${path}`, {
      method: options.method ?? 'GET',
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: AbortSignal.timeout(REQ_TIMEOUT_MS),
    });
  } catch (e) {
    // fetch 只在网络层失败时 reject（HTTP 错误码走下面的 res.ok 分支）
    const timeout = (e as { name?: string } | null)?.name === 'TimeoutError';
    throw new ApiError(0, timeout ? '服务器响应超时，请稍后重试' : '连不上服务器，请检查网络或服务器地址');
  }
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null;
    // 带着 token 却 401 说明会话已失效（区别于登录页密码错误——那时本地无 token）：
    // 清掉本地会话并跳回登录页，仍然 throw 让调用方停止后续逻辑
    if (res.status === 401 && token) {
      if (!location.hash.startsWith('#/login')) sessionStorage.setItem(NEXT_KEY, location.hash);
      clearSession();
      toast('登录已失效，请重新登录', 'bad');
      // replace 而非 push：否则用户按返回键会回到失效页再被踢回来，死循环
      location.replace('#/login');
    }
    throw new ApiError(res.status, data?.error ?? statusMessage(res.status));
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
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

export async function logout(): Promise<void> {
  try {
    await req('/api/logout', { method: 'POST' });
  } finally {
    clearSession();
  }
}

export async function listChannels(): Promise<Channel[]> {
  const data = await req<{ channels: Channel[] | null }>('/api/channels');
  return data.channels ?? [];
}

export async function createChannel(name: string): Promise<Channel> {
  return req<Channel>('/api/channels', { method: 'POST', body: { name } });
}

export interface EngineCred {
  engine: string; // 客户端引擎名（engine/index.ts 注册表的键）
  url: string;
  token: string;
}

// 双线进房凭证：语音线必有；舞台线（投屏/摄像头）可缺席；
// combined = 两线同一内核，前端用一条连接承担两种角色
export interface JoinCredentials {
  voice: EngineCred;
  stage?: EngineCred | null;
  combined: boolean;
}

// 持久设备 ID：首次访问生成并存 localStorage，用于区分同一账号的多设备
export function deviceId(): string {
  let id = localStorage.getItem('hearth_device_id');
  if (!id) {
    const rand = new Uint8Array(4);
    const c = globalThis.crypto as Crypto | undefined;
    if (c?.getRandomValues) {
      c.getRandomValues(rand);
    } else {
      for (let i = 0; i < rand.length; i++) rand[i] = Math.floor(Math.random() * 256);
    }
    id = Array.from(rand, (b) => b.toString(16).padStart(2, '0')).join('');
    localStorage.setItem('hearth_device_id', id);
  }
  return id;
}

export function fetchJoinCredentials(channel: string): Promise<JoinCredentials> {
  return req<JoinCredentials>('/api/token', {
    method: 'POST',
    body: { channel, device_id: deviceId() },
  });
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

// ---- WHIP 推流令牌（每用户一把，房间在 URL 里）----

export interface IngestTokenInfo {
  token: string;
  tag: string; // 推流设备标签（identity = {用户名}-{标签}）
  base: string; // 同源 WHIP 基地址（/providers/{alias}/w/ 绝对地址），拼上频道名即完整服务器地址
  enabled: boolean; // 推流入口当前是否可用（false 时地址照给，但推起来会被拒）
}

// 获取（首次自动创建）当前用户的推流令牌
export function getIngestToken(): Promise<IngestTokenInfo> {
  return req<IngestTokenInfo>('/api/ingest/token');
}

// 重置令牌（旧令牌立即失效，进行中的推流会话全部掐断）
export function resetIngestToken(): Promise<IngestTokenInfo> {
  return req<IngestTokenInfo>('/api/ingest/token/reset', { method: 'POST' });
}

// 改推流设备标签（下次推流生效，进行中的会话不掐）
export function setIngestTag(tag: string): Promise<IngestTokenInfo> {
  return req<IngestTokenInfo>('/api/ingest/token', { method: 'PUT', body: { tag } });
}

// ---- 频道管理（房主）----

// 管理操作的目标一律是 user_id：用户名可改、改后旧名即释放，拿它当目标会在
// 改名/重注册后打到别人身上。uid 从参与者元数据（EPart.uid / RoomParticipant.uid）
// 或名单条目（UserRef.id）来，前端不解析 identity、也不按用户名反查。

// identity 非空时只踢该设备（须归属该 uid）；空则踢全部设备
export function kickUser(channel: string, uid: number, identity?: string): Promise<{ kicked: number }> {
  return req(`/api/channels/${encodeURIComponent(channel)}/kick`, {
    method: 'POST',
    body: { user_id: uid, identity: identity ?? '' },
  });
}

export function banUser(channel: string, uid: number): Promise<void> {
  return req(`/api/channels/${encodeURIComponent(channel)}/ban`, { method: 'POST', body: { user_id: uid } });
}

export function muteUser(channel: string, uid: number, muted: boolean): Promise<unknown> {
  // 落库为权威：目标不在房也能禁言/解禁（下次进房生效）
  return req(`/api/channels/${encodeURIComponent(channel)}/${muted ? 'mute' : 'unmute'}`, {
    method: 'POST',
    body: { user_id: uid },
  });
}

export function unbanUser(channel: string, uid: number): Promise<void> {
  return req(`/api/channels/${encodeURIComponent(channel)}/unban`, { method: 'POST', body: { user_id: uid } });
}

// UserRef 名单条目：id 是操作目标，username 只用于展示；role 是该条目在频道里的角色（members 列表带）
export interface UserRef {
  id: number;
  username: string;
  role?: ChannelRole;
}

export async function listBans(channel: string): Promise<UserRef[]> {
  const data = await req<{ bans: UserRef[] | null }>(`/api/channels/${encodeURIComponent(channel)}/bans`);
  return data.bans ?? [];
}

export function setInviteOnly(channel: string, enabled: boolean): Promise<{ invite_only: boolean }> {
  return req(`/api/channels/${encodeURIComponent(channel)}/invite-only`, {
    method: 'POST',
    body: { enabled },
  });
}

export async function listMembers(channel: string): Promise<UserRef[]> {
  const data = await req<{ members: UserRef[] | null }>(
    `/api/channels/${encodeURIComponent(channel)}/members`,
  );
  return data.members ?? [];
}

// 加白名单按用户名收：目标是还没进过房的人，房主手输名字、界面上没有 uid 可选，
// 与登录同属「名字 → 用户」的一次查找（服务端查到后立即换成 user_id 落库）
export function addMember(channel: string, username: string): Promise<void> {
  return req(`/api/channels/${encodeURIComponent(channel)}/members`, {
    method: 'POST',
    body: { username },
  });
}

export function removeMember(channel: string, uid: number): Promise<void> {
  return req(`/api/channels/${encodeURIComponent(channel)}/members`, {
    method: 'DELETE',
    body: { user_id: uid },
  });
}

// ---- 频道归属（仅频道主）----

export function transferChannel(channel: string, uid: number): Promise<{ owner: string }> {
  return req(`/api/channels/${encodeURIComponent(channel)}/transfer`, {
    method: 'POST',
    body: { user_id: uid },
  });
}

export async function listModerators(channel: string): Promise<UserRef[]> {
  const data = await req<{ moderators: UserRef[] | null }>(
    `/api/channels/${encodeURIComponent(channel)}/moderators`,
  );
  return data.moderators ?? [];
}

export function addModerator(channel: string, uid: number): Promise<void> {
  return req(`/api/channels/${encodeURIComponent(channel)}/moderators`, {
    method: 'POST',
    body: { user_id: uid },
  });
}

export function removeModerator(channel: string, uid: number): Promise<void> {
  return req(`/api/channels/${encodeURIComponent(channel)}/moderators`, {
    method: 'DELETE',
    body: { user_id: uid },
  });
}

export interface RoomParticipant {
  identity: string;
  uid: number; // 归属用户 id（内核元数据透传；管理操作的目标）
  username: string; // 归属用户名（纯展示）
  name: string; // 内核侧显示名
  joined_at: number;
  kind?: string; // 参与者类别（omitempty；ingest = 推流设备）
  tag?: string; // 设备标签
}

export async function listParticipants(channel: string): Promise<RoomParticipant[]> {
  const data = await req<{ participants: RoomParticipant[] | null }>(
    `/api/channels/${encodeURIComponent(channel)}/participants`,
  );
  return data.participants ?? [];
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

// ---- 邀请 ----

export interface InviteInfo {
  inviter: string;
  expires_at: string;
  alive: boolean;
}

export function inviteInfo(code: string): Promise<InviteInfo> {
  return req(`/api/invites/${encodeURIComponent(code)}`);
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

// role 仅 admin+ 可指定（user/power）；空 = 跟随注册默认档
export function createInvite(note: string, maxUses: number, ttl: string, role = ''): Promise<{ invite: Invite; url: string }> {
  return req('/api/invites', { method: 'POST', body: { note, max_uses: maxUses, ttl, role } });
}

export function deleteInvite(id: number): Promise<void> {
  return req(`/api/invites/${id}`, { method: 'DELETE' });
}

export interface SiteInfo {
  name: string;
  policy: string;
}

// 登录页与邀请页只需要站点名和注册策略；首启流程不在本发布线。
export function siteInfo(): Promise<SiteInfo> {
  return req('/api/site');
}

// ---- 管理后台 ----

export interface AdminOverview {
  users: number;
  channels: number;
  online: number;
  uptime_seconds: number;
  go_version: string;
  policy: string;
  services: Record<string, { name?: string; ok: boolean; url: string }>;
  resources: {
    load: number | null;
    cpus: number;
    mem_used_mb: number | null;
    mem_total_mb: number | null;
    temp_c: number | null;
  };
}

export function adminOverview(): Promise<AdminOverview> {
  return req('/api/admin/overview');
}

export interface AdminUser {
  id: number;
  username: string;
  role: Role;
  is_admin: boolean; // 派生只读（role ≥ admin），过渡一个版本后删
  disabled: boolean;
  expires_at: string | null; // 访客行：过期时间
  invite: string; // 访客行：来源邀请码（无则空）
  created_at: string;
  devices: number;
  last_seen: string | null;
  can_set_roles: Role[] | null; // 当前管理员可授予的角色候选（服务端按阶梯算好；null/空 = 不可操作）
}

export async function adminListUsers(): Promise<AdminUser[]> {
  const data = await req<{ users: AdminUser[] | null }>('/api/admin/users');
  return data.users ?? [];
}

export function adminSetUserDisabled(id: number, disabled: boolean): Promise<void> {
  return req(`/api/admin/users/${id}/${disabled ? 'disable' : 'enable'}`, { method: 'POST', body: {} });
}

// 降级时若对方名下还有频道，owned_channels 带回数量（频道不跟着动，提示用）
export function adminSetUserRole(id: number, role: string): Promise<{ role: string; owned_channels: number }> {
  return req(`/api/admin/users/${id}/role`, { method: 'POST', body: { role } });
}

// 名下频道过户给执行删除的管理员，adopted_channels 是过户数量
export function adminDeleteUser(id: number): Promise<{ adopted_channels: number }> {
  return req(`/api/admin/users/${id}`, { method: 'DELETE' });
}

export function adminDeleteChannel(id: number): Promise<void> {
  return req(`/api/admin/channels/${id}`, { method: 'DELETE' });
}

export interface ConfigItem {
  name: string;
  env: string;
  label: string;
  hint: string;
  secret: boolean;
  group: string;
  options?: string[]; // 枚举可选值（渲染选择框）；缺省 = 自由文本
  value: string;
  set: boolean;
  locked: boolean;
}

export async function adminGetConfig(): Promise<ConfigItem[]> {
  const data = await req<{ items: ConfigItem[] | null }>('/api/admin/config');
  return data.items ?? [];
}

export function adminSetConfig(values: Record<string, string>): Promise<void> {
  return req('/api/admin/config', { method: 'POST', body: { values } });
}

// ---- 服务实例（内核注册表）----

// 注册表单的字段模式（rtc.ConfigKey 形状）；当前三类可注册实例的字段均为自由文本
export interface ProviderField {
  name: string;
  label: string;
  hint: string;
  secret: boolean;
}

export interface ProviderInstance {
  alias: string;
  type: string;
  caps: string[]; // 槽位能力：voice / stage / ingest
  locked: boolean; // 环境变量锁定，只读
  builtin: boolean; // 内建（lkembed），只读
  params: Record<string, string>; // Secret 字段掩码为空串
  params_set: Record<string, boolean>; // Secret 字段是否已设置
}

export interface ProviderType {
  type: string;
  label: string;
  fields: ProviderField[];
}

export async function adminListProviders(): Promise<{ instances: ProviderInstance[]; types: ProviderType[] }> {
  const data = await req<{ instances: ProviderInstance[] | null; types: ProviderType[] | null }>('/api/admin/providers');
  return { instances: data.instances ?? [], types: data.types ?? [] };
}

export function adminCreateProvider(body: { type: string; alias: string; params: Record<string, string> }): Promise<void> {
  return req('/api/admin/providers', { method: 'POST', body });
}

// 全量替换语义：params 须含该类型全部字段；Secret 字段空串 = 保留旧值（livekit_url 空 = 清除）
export function adminUpdateProvider(alias: string, params: Record<string, string>): Promise<void> {
  return req(`/api/admin/providers/${encodeURIComponent(alias)}`, { method: 'PUT', body: { params } });
}

export function adminDeleteProvider(alias: string): Promise<void> {
  return req(`/api/admin/providers/${encodeURIComponent(alias)}`, { method: 'DELETE' });
}

export function adminGetPolicy(): Promise<{ policy: string; default_role: string }> {
  return req('/api/admin/policy');
}

export function adminSetPolicy(policy: string, defaultRole: string): Promise<{ policy: string; default_role: string }> {
  return req('/api/admin/policy', { method: 'POST', body: { policy, default_role: defaultRole } });
}
