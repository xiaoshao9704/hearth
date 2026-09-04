// 聊天 WebSocket 客户端：连接 server 的 /api/chat，收历史与实时消息。
// 断线自动重连（指数退避，页面回前台/网络恢复立即重试）；重连成功后服务端会重发历史。
// 被移出频道（1008）/ 频道删除（1001）经关闭码识别为终态，不再重试。
import { getToken, wsBase } from './api';

export interface ChatMessage {
  id: number;
  channel_id: number;
  uid: number; // 发送者 user_id（右键菜单的操作目标）
  username: string; // 纯展示
  content: string;
  created_at: string;
}

interface ServerMsg {
  type: 'history' | 'message';
  messages?: ChatMessage[];
  message?: ChatMessage;
}

export interface ChatHandlers {
  onHistory: (messages: ChatMessage[]) => void;
  onMessage: (message: ChatMessage) => void;
  // 终态关闭：1008 = 被移出频道（踢出/封禁），1001 = 频道已删除
  onKicked: (code: number) => void;
  onState?: (state: 'open' | 'reconnecting') => void;
}

// connectChat 连接频道聊天，返回发送函数；关闭时调用返回的 close。
export function connectChat(
  channel: string,
  handlers: ChatHandlers,
): { send: (content: string) => void; connected: () => boolean; close: () => void } {
  let ws: WebSocket | null = null;
  let closed = false;
  let attempts = 0;
  let timer = 0;

  const open = () => {
    if (closed || getToken() === null) return;
    const url = `${wsBase()}/api/chat?channel=${encodeURIComponent(channel)}&token=${getToken()}`;
    ws = new WebSocket(url);
    ws.onopen = () => {
      attempts = 0;
      handlers.onState?.('open');
    };
    ws.onmessage = (ev) => {
      const data = JSON.parse(ev.data as string) as ServerMsg;
      if (data.type === 'history') {
        handlers.onHistory(data.messages ?? []);
      } else if (data.type === 'message' && data.message) {
        handlers.onMessage(data.message);
      }
    };
    ws.onclose = (ev) => {
      ws = null;
      if (closed) return;
      if (ev.code === 1008 || ev.code === 1001) {
        closed = true;
        handlers.onKicked(ev.code);
        return;
      }
      handlers.onState?.('reconnecting');
      schedule();
    };
  };

  const schedule = () => {
    clearTimeout(timer);
    const delay = Math.min(15000, 1000 * 2 ** Math.min(attempts++, 4)) * (0.7 + Math.random() * 0.6);
    timer = window.setTimeout(open, delay);
  };

  // 回前台 / 网络恢复：跳过退避立即重试
  const retryNow = () => {
    if (!closed && (!ws || ws.readyState === WebSocket.CLOSED)) {
      clearTimeout(timer);
      attempts = 0;
      open();
    }
  };
  const onVisible = () => {
    if (document.visibilityState === 'visible') retryNow();
  };
  document.addEventListener('visibilitychange', onVisible);
  window.addEventListener('online', retryNow);

  open();

  return {
    send: (content: string) => {
      if (ws?.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ content }));
      }
    },
    connected: () => ws?.readyState === WebSocket.OPEN,
    close: () => {
      closed = true;
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('online', retryNow);
      ws?.close();
    },
  };
}
