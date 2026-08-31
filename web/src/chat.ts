// 聊天 WebSocket 客户端：连接 server 的 /api/chat，收历史与实时消息。
import { getToken, wsBase } from './api';

export interface ChatMessage {
  id: number;
  channel_id: number;
  username: string;
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
  onClose: () => void;
}

// connectChat 连接频道聊天，返回发送函数；关闭时调用返回的 close。
export function connectChat(channel: string, handlers: ChatHandlers): { send: (content: string) => void; close: () => void } {
  const url = `${wsBase()}/api/chat?channel=${encodeURIComponent(channel)}&token=${getToken()}`;
  const ws = new WebSocket(url);

  ws.onmessage = (ev) => {
    const data = JSON.parse(ev.data as string) as ServerMsg;
    if (data.type === 'history') {
      handlers.onHistory(data.messages ?? []);
    } else if (data.type === 'message' && data.message) {
      handlers.onMessage(data.message);
    }
  };
  ws.onclose = () => handlers.onClose();

  return {
    send: (content: string) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ content }));
      }
    },
    close: () => ws.close(),
  };
}
