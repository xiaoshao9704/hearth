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

export interface ChatMessage {
  id: number;
  channel_id: number;
  uid: number; // 发送者 user_id（右键菜单的操作目标）
  username: string; // 纯展示
  kind: ChatKind;
  content: string;
  file?: ChatFileMeta; // kind=file 时有值
  created_at: string;
}

// 取历史：after=0 取最近 limit 条，after=<最大已知 id> 取增量；一律时间正序
export async function fetchMessages(channel: string, after = 0, limit = 50): Promise<ChatMessage[]> {
  const list = await apiRequest<ChatMessage[] | null>(
    `/api/channels/${encodeURIComponent(channel)}/messages?after=${after}&limit=${limit}`,
  );
  return list ?? [];
}

export type PostBody = { content: string } | { kind: 'file'; file: ChatFileMeta };

// 发消息：落库成功才算发出（禁言 403、文件超限 413、文本超长 400），返回带 id 的整条消息
export function postMessage(channel: string, body: PostBody): Promise<ChatMessage> {
  return apiRequest<ChatMessage>(`/api/channels/${encodeURIComponent(channel)}/messages`, {
    method: 'POST',
    body,
  });
}
