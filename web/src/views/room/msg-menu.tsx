// 消息的右键/长按菜单：快捷反应一行 + 回复 + 撤回/删除。
// 与名册的 user-menu 同一套命令式做法（临时浮层，开一次用一次，不进 Solid 的响应式图）——
// 它的内容在弹出那一刻就定死了，做成组件反而要为一个瞬时浮层维护一份信号。
import { esc, icon } from '../../ui';
import { REACTION_EMOJIS } from './reactions';

export interface MsgMenuOpts {
  x: number;
  y: number;
  mine: boolean; // 是不是自己发的（决定菜单里写"撤回"还是"删除"）
  canDelete: boolean; // 自己的，或者我是频道管理员
  deleted: boolean; // 已撤回的只剩关闭，不给再操作
  myEmojis: string[]; // 我已经点过的表情（再点是取消，菜单里高亮）
  onReply: () => void;
  onDelete: () => void;
  onReact: (emoji: string, on: boolean) => void;
}

export function showMsgMenu(o: MsgMenuOpts) {
  document.querySelector('.user-menu')?.remove();
  const menu = document.createElement('div');
  menu.className = 'user-menu msg-menu';
  const reactRow = o.deleted
    ? ''
    : `<div class="mm-react">${REACTION_EMOJIS.map(
        (e) => `<button class="hit mm-emoji${o.myEmojis.includes(e) ? ' on' : ''}" data-emoji="${esc(e)}">${e}</button>`,
      ).join('')}</div>`;
  const replyBtn = o.deleted
    ? ''
    : `<button class="hit um-item" data-act="reply">${icon('back', 14, 'currentColor')}<span>回复</span></button>`;
  const delBtn =
    o.canDelete && !o.deleted
      ? `<button class="hit um-item danger" data-act="delete">${icon('trash', 14, 'var(--red)')}<span>${o.mine ? '撤回' : '删除这条消息'}</span></button>`
      : '';
  menu.innerHTML = reactRow + replyBtn + delBtn;
  if (!menu.firstChild) return;
  document.body.appendChild(menu);
  menu.style.left = `${Math.max(8, Math.min(o.x, window.innerWidth - menu.offsetWidth - 8))}px`;
  menu.style.top = `${Math.max(8, Math.min(o.y, window.innerHeight - menu.offsetHeight - 8))}px`;

  const close = () => {
    menu.remove();
    document.removeEventListener('pointerdown', onDoc, true);
  };
  const onDoc = (ev: Event) => {
    if (!menu.contains(ev.target as Node)) close();
  };
  // 延后注册：触发菜单的那次 pointerdown 还没冒泡完，立刻注册会自己把自己关掉
  setTimeout(() => document.addEventListener('pointerdown', onDoc, true));

  menu.querySelectorAll<HTMLButtonElement>('.mm-emoji').forEach((btn) => {
    btn.addEventListener('click', () => {
      const emoji = btn.dataset.emoji ?? '';
      o.onReact(emoji, !o.myEmojis.includes(emoji));
      close();
    });
  });
  menu.querySelector('[data-act="reply"]')?.addEventListener('click', () => {
    o.onReply();
    close();
  });
  menu.querySelector('[data-act="delete"]')?.addEventListener('click', () => {
    o.onDelete();
    close();
  });
}
