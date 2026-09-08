// 聊天的 REST 客户端：文本与文件「卡片」由 hearth 落库（权威在 store），实时到达走
// LiveKit 数据通道（见 views/room.tsx 的数据线），文件字节完全不经服务端。
// 因此这里只剩取历史与发消息两件事——没有长连接、没有重连、没有心跳。
import { apiRequest } from './api';

export type ChatKind = 'text' | 'file';

// 文件卡片的元数据（服务端只存这些，字节经 SFU 扇出）
export interface ChatFileMeta {
  name: string;
  mime: string;
  size: number;
}

// 某个表情的反应聚合：uids 是点过的人（计数取长度，自己点没点看 uid 在不在里面）
export interface ChatReaction {
  emoji: string;
  uids: number[];
}

export interface ChatMessage {
  id: number;
  channel_id: number;
  uid: number; // 发送者 user_id（右键菜单的操作目标）
  username: string; // 纯展示
  kind: ChatKind;
  content: string;
  file?: ChatFileMeta; // kind=file 时有值
  created_at: string;
  reply_to?: number | null; // 引用回复指向的消息 id
  deleted?: boolean; // true = 已撤回，content/file 已被服务端清空，只渲染占位
  reactions?: ChatReaction[]; // 服务端聚合；旧服务端不带这个字段，按空处理
}

// 取历史：after=0 取最近 limit 条，after=<最大已知 id> 取增量；一律时间正序
export async function fetchMessages(channel: string, after = 0, limit = 50): Promise<ChatMessage[]> {
  const list = await apiRequest<ChatMessage[] | null>(
    `/api/channels/${encodeURIComponent(channel)}/messages?after=${after}&limit=${limit}`,
  );
  return list ?? [];
}

// mentions：客户端按名册算出的被@用户 uid（服务端逐个校验后用于离线推送，见 api/push.go）
export type PostBody = ({ content: string } | { kind: 'file'; file: ChatFileMeta }) & {
  reply_to?: number;
  mentions?: number[];
};

// 发消息：落库成功才算发出（禁言 403、文件超限 413、文本超长 400），返回带 id 的整条消息
export function postMessage(channel: string, body: PostBody): Promise<ChatMessage> {
  return apiRequest<ChatMessage>(`/api/channels/${encodeURIComponent(channel)}/messages`, {
    method: 'POST',
    body,
  });
}

// 撤回/删除一条消息：作者本人或频道管理员，服务端软删（历史里留占位）
export function deleteMessage(channel: string, id: number): Promise<void> {
  return apiRequest<void>(`/api/channels/${encodeURIComponent(channel)}/messages/${id}`, { method: 'DELETE' });
}

// 清空频道聊天记录（仅频道主/系统管理员），返回删掉的条数
export function clearMessages(channel: string): Promise<{ deleted: number }> {
  return apiRequest<{ deleted: number }>(`/api/channels/${encodeURIComponent(channel)}/messages`, { method: 'DELETE' });
}

// 加/取消一个表情反应，返回该消息最新的聚合结果（权威在服务端，广播只是让对端早点看到）
export function setReaction(
  channel: string,
  id: number,
  emoji: string,
  on: boolean,
): Promise<{ id: number; reactions: ChatReaction[] }> {
  const path = `/api/channels/${encodeURIComponent(channel)}/messages/${id}/reactions/${encodeURIComponent(emoji)}`;
  return apiRequest<{ id: number; reactions: ChatReaction[] }>(path, { method: on ? 'PUT' : 'DELETE' });
}
