// 频道列表 / 进房凭证 / 推流令牌 / 频道管理 / 频道访客邀请。
import { apiRequest as req } from './http';
import type { Invite } from './session';

// 频道角色：owner / moderator / member，空串 = 无角色
export type ChannelRole = 'owner' | 'moderator' | 'member' | '';

export interface Channel {
  id: number;
  name: string;
  created_by: string;
  created_at: string;
  invite_only: boolean;
  my_role: ChannelRole; // 当前用户在该频道的角色（服务端下发）
  online: number;
  banned?: boolean; // 当前用户是否被该频道封禁（服务端下发，封禁不影响是否出现在列表里）
  muted?: boolean; // 当前用户是否静音了该频道的提醒（服务端下发，每人每频道一个开关）
  hidden?: boolean; // 仅 super 能看到的非成员邀请制频道（普通人根本收不到这条，服务端下发）
}

export async function listChannels(): Promise<Channel[]> {
  const data = await req<{ channels: Channel[] | null }>('/api/channels');
  return data.channels ?? [];
}

export async function createChannel(name: string): Promise<Channel> {
  return req<Channel>('/api/channels', { method: 'POST', body: { name } });
}

// 静音/取消静音一个频道的提醒（每人每频道一个开关，落库即生效）。
// 改完派 hearth:channels：房间页与大厅重取频道列表，不各存一份布尔
export async function setChannelMuted(channel: string, muted: boolean): Promise<void> {
  await req<void>(`/api/channels/${encodeURIComponent(channel)}/mute`, { method: muted ? 'PUT' : 'DELETE' });
  window.dispatchEvent(new Event('hearth:channels'));
}

export interface EngineCred {
  engine: string; // 客户端引擎名（engine/index.ts 注册表的键）
  url: string;
  token: string;
}

// 聊天与文件走哪条线的数据通道（服务端 chat_data_line 下发，前端不猜拓扑）：
// auto = 有舞台线走舞台线、否则语音线；voice/stage = 强制
export type DataLine = 'auto' | 'voice' | 'stage';

// 双线进房凭证：语音线必有；舞台线（投屏/摄像头）可缺席；
// combined = 两线同一内核，前端用一条连接承担两种角色。
// stage_error 有值 = 配了独立舞台但签发失败（与缺席的「没配舞台」区分开）
export interface JoinCredentials {
  voice: EngineCred;
  stage?: EngineCred | null;
  stage_error?: string;
  combined: boolean;
  data_line?: DataLine;
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

// channelID 是频道寻址往 id 靠的一环：给了就按 id 判定，拿不到（频道列表还没回来）服务端按名字兜底
export function fetchJoinCredentials(channel: string, channelID?: number): Promise<JoinCredentials> {
  return req<JoinCredentials>('/api/token', {
    method: 'POST',
    body: { channel, ...(channelID ? { channel_id: channelID } : {}), device_id: deviceId() },
  });
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

// ---- 桌面端原生投屏的设备票（短时效，绑本频道与本设备）----

export interface CastTicketInfo {
  ticket: string; // 只能推这个频道、这台设备，10 分钟到期
  base: string; // 同源 WHIP 基地址（/providers/{alias}/w/），拼上频道 id 即完整端点
  expires_in: number;
}

// 点投屏时现取：服务端当场判定能否发布，与 OBS 的账号级推流令牌互不干扰
export function getCastTicket(channelId: number, deviceId: string): Promise<CastTicketInfo> {
  return req<CastTicketInfo>(`/api/channels/${channelId}/cast-ticket`, {
    method: 'POST',
    body: { device_id: deviceId },
  });
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

// 频道访客邀请（频道主与频道管理员）：产出绑在本频道的访客，不产出注册账号
export function createGuestInvite(
  channel: string,
  ttl: string,
  guestTTL: string,
  maxUses: number,
  note = '',
): Promise<{ invite: Invite; url: string }> {
  return req(`/api/channels/${encodeURIComponent(channel)}/invites`, {
    method: 'POST',
    body: { ttl, guest_ttl: guestTTL, max_uses: maxUses, note },
  });
}

export async function listGuestInvites(channel: string): Promise<{ invites: Invite[]; base: string }> {
  const data = await req<{ invites: Invite[] | null; base: string }>(`/api/channels/${encodeURIComponent(channel)}/invites`);
  return { invites: data.invites ?? [], base: data.base };
}

export function deleteGuestInvite(channel: string, id: number): Promise<void> {
  return req(`/api/channels/${encodeURIComponent(channel)}/invites/${id}`, { method: 'DELETE' });
}
