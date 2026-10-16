// 管理后台：概览 / 用户 / 配置 / TLS / 审计 / 服务实例 / 注册策略。
import { apiRequest as req } from './http';
import type { Role } from './session';

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

// ---- TLS ----

export interface TlsCertInfo {
  subject: string;
  sans: string[];
  not_after: string;
  fingerprint_sha256: string;
}

export interface TlsCaInfo {
  fingerprint_sha256: string;
  not_after: string;
  constraint_stale: boolean; // tls_self_hosts 加了根约束里没有的 DNS 名，根证书需要重新生成
}

export interface TlsExternalInfo {
  addresses: string[];
  probed_at: string;
}

export interface TlsPinhole {
  proto: string;
  port: number;
  gua: string;
  method: string;
}

export interface TlsPortmapInfo {
  mode: 'auto' | 'off'; // portmap_mode 当前值
  diagnosis: string; // portmap.Diagnosis：ok/off/no_gateway/disabled_by_gateway/upstream_nat/port_conflict/host_firewall/error
  detail: string;
  v6_detail: string;
  pinholes: TlsPinhole[];
}

export interface TlsStatus {
  source: 'off' | 'self' | 'file' | 'upload';
  mode: 'merged' | 'split';
  http_addr: string;
  https_addr: string;
  cert: TlsCertInfo | null; // 来源 off 或当前没有可用证书时为 null
  ca: TlsCaInfo | null; // 来源不是 self 时为 null
  cert_file: string;
  key_file: string;
  external: TlsExternalInfo;
  portmap: TlsPortmapInfo;
}

export function getTls(): Promise<TlsStatus> {
  return req('/api/admin/tls');
}

export function uploadTls(cert: File, key: File): Promise<TlsStatus> {
  const form = new FormData();
  form.append('cert', cert);
  form.append('key', key);
  return req('/api/admin/tls/upload', { method: 'POST', body: form });
}

export function rotateTlsCA(): Promise<TlsStatus> {
  return req('/api/admin/tls/ca/rotate', { method: 'POST', body: {} });
}

// ---- 审计日志 ----

export interface AuditEntry {
  id: number;
  at: string;
  actor_uid: number;
  actor_name: string;
  action: string;
  target_uid: number; // 0 = 不针对某人
  target_name: string;
  channel_id: number; // 0 = 不属于某频道
  channel_name: string;
  detail: string;
}

export interface AuditQuery {
  channel?: number;
  actor?: number;
  action?: string;
  after?: number; // 游标：只取比这一条更早的记录
  limit?: number;
}

// next 是继续往更早翻的游标，0 = 没有更多
export async function adminAudit(q: AuditQuery = {}): Promise<{ entries: AuditEntry[]; next: number; actions: string[] }> {
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(q)) {
    if (v !== undefined && v !== '' && v !== 0) params.set(k, String(v));
  }
  const qs = params.toString();
  const data = await req<{ entries: AuditEntry[] | null; next: number; actions: string[] | null }>(
    `/api/admin/audit${qs ? `?${qs}` : ''}`,
  );
  return { entries: data.entries ?? [], next: data.next ?? 0, actions: data.actions ?? [] };
}

// ---- 服务实例（内核注册表）----

// 注册表单的字段模式（rtc.ConfigKey 形状）；optional/pattern 由服务端下发，页面不复刻校验规则
export interface ProviderField {
  name: string;
  label: string;
  hint: string;
  secret: boolean;
  optional?: boolean;
  pattern?: string;
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

export async function adminListProviders(): Promise<{
  instances: ProviderInstance[];
  types: ProviderType[];
  alias_pattern: string;
}> {
  const data = await req<{
    instances: ProviderInstance[] | null;
    types: ProviderType[] | null;
    alias_pattern?: string;
  }>('/api/admin/providers');
  return {
    instances: data.instances ?? [],
    types: data.types ?? [],
    // 不在客户端写死正则：接口没下发时留给页面按空串拒绝提交
    alias_pattern: data.alias_pattern ?? '',
  };
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
