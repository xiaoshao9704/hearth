// 未读定位与分割线：打开聊天抽屉时把第一条未读滚到视野，并在它上方画一条
// 「以下为新消息」。分割线只是一次性的路标——滚到底或 30 秒后自撤，不是持久状态。
import { createSignal, Show } from 'solid-js';
import type { JSX } from 'solid-js';

const AUTO_HIDE_MS = 30_000;
// 定位刚做完时会立刻来一个 scroll 事件，短暂忽略「到底」判定，否则未读就在底部附近时分割线一闪而过
const SETTLE_MS = 1200;

export interface UnreadMarker {
  note(id: number): void; // 计入未读的那一刻调用：只记第一条
  reveal(): void; // 抽屉打开：定位到第一条未读并开始倒计时
  atBottom(): void; // 用户滚到底：读完了，撤线
  Divider: (p: { id: number }) => JSX.Element;
}

export function createUnreadMarker(log: () => HTMLElement): UnreadMarker {
  const [markerId, setMarkerId] = createSignal(0);
  let timer = 0;
  let revealedAt = 0;

  const clear = () => {
    clearTimeout(timer);
    timer = 0;
    revealedAt = 0;
    setMarkerId(0);
  };

  return {
    note(id) {
      if (markerId() === 0) setMarkerId(id);
    },
    reveal() {
      if (markerId() === 0) return;
      revealedAt = Date.now();
      clearTimeout(timer);
      timer = window.setTimeout(clear, AUTO_HIDE_MS);
      // 排在抽屉自己的「滚到底」之后：面板刚从 hidden 变可见，DOM 尺寸这一拍才算数
      queueMicrotask(() => {
        const line = log().querySelector('.chat-unread-line');
        line?.scrollIntoView({ block: 'start' });
      });
    },
    atBottom() {
      if (markerId() === 0 || Date.now() - revealedAt < SETTLE_MS) return;
      clear();
    },
    Divider: (p) => (
      <Show when={markerId() === p.id}>
        <div class="chat-unread-line">
          <span>以下为新消息</span>
        </div>
      </Show>
    ),
  };
}
