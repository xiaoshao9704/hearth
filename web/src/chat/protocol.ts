// 聊天数据通道的载荷信封。topic 仍是 chat，但载荷从"裸的一条 Message"升级成带类型的信封：
// 撤回与表情反应也要实时到达，而它们不是一条新消息。
//
// 兼容旧版页面：解析到不带 t 的载荷按 message 处理（升级过渡期对端可能还是旧前端）。
// 反过来旧页面收到新信封会当成一条 id 非法的消息丢掉——不会错乱，只是看不到实时的撤回/反应，
// 重连或下次拉历史时服务端会把状态补齐（权威始终在库里）。
import type { ChatMessage } from '../chat';

export type ChatEnvelope =
  | { t: 'message'; m: ChatMessage }
  | { t: 'delete'; id: number; by: number }
  | { t: 'reaction'; id: number; emoji: string; uid: number; on: boolean };

export function encodeMessage(m: ChatMessage): string {
  return JSON.stringify({ t: 'message', m } satisfies ChatEnvelope);
}

export function encodeDelete(id: number, by: number): string {
  return JSON.stringify({ t: 'delete', id, by } satisfies ChatEnvelope);
}

export function encodeReaction(id: number, emoji: string, uid: number, on: boolean): string {
  return JSON.stringify({ t: 'reaction', id, emoji, uid, on } satisfies ChatEnvelope);
}

function validMessage(m: unknown): m is ChatMessage {
  const x = m as ChatMessage | null;
  return !!x && typeof x.id === 'number' && x.id > 0;
}

// parseEnvelope 解析一条数据线载荷；形状不对一律返回 null（丢掉即可，重连时 after= 会补齐）
export function parseEnvelope(text: string): ChatEnvelope | null {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return null;
  }
  if (!raw || typeof raw !== 'object') return null;
  const env = raw as Record<string, unknown>;
  switch (env.t) {
    case undefined:
      // 旧载荷：整条 Message 直接躺在顶层
      return validMessage(raw) ? { t: 'message', m: raw } : null;
    case 'message':
      return validMessage(env.m) ? { t: 'message', m: env.m } : null;
    case 'delete':
      return typeof env.id === 'number' && env.id > 0 && typeof env.by === 'number'
        ? { t: 'delete', id: env.id, by: env.by }
        : null;
    case 'reaction':
      return typeof env.id === 'number' &&
        env.id > 0 &&
        typeof env.emoji === 'string' &&
        env.emoji !== '' &&
        typeof env.uid === 'number' &&
        typeof env.on === 'boolean'
        ? { t: 'reaction', id: env.id, emoji: env.emoji, uid: env.uid, on: env.on }
        : null;
    default:
      return null;
  }
}
