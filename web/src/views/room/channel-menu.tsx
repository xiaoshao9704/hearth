// 频道菜单：「我在本频道」这一层的入口（静音、OBS 推流地址、频道链接、频道管理、离开）。
// 房间顶栏的频道名与大厅卡片的「…」共用这一份；大厅没有房间上下文，不传 onIngest/onLeave 就不出那两项。
// 命令式入口：内部 render 到自建宿主 div、关闭时 dispose（与设置浮层同一套做法），
// vanilla 的大厅与 Solid 的房间页都能调。外观复用 .user-menu 的浮层壳。
import { onCleanup, onMount, Show } from 'solid-js';
import { render } from 'solid-js/web';
import { getUser, isGuest, setChannelMuted } from '../../api';
import type { ChannelRole } from '../../api';
import { copyText, el, icon, slashIcon, toast } from '../../ui';
import { openSettings } from '../settings';

export interface ChannelMenuOpts {
  muted?: boolean; // 服务端下发的静音态（调用方给，菜单不自己查、不记第二份）
  myRole?: ChannelRole; // 服务端下发的我在该频道的角色：owner/moderator 才出「频道管理…」
  onIngest?: () => void; // 房间页：打开 OBS 推流面板
  onLeave?: () => void; // 房间页：离开频道（走调用方现成的离开确认）
}

let dispose: (() => void) | null = null;
let openAnchor: HTMLElement | null = null;

export function closeChannelMenu() {
  dispose?.();
  dispose = null;
  openAnchor = null;
}

// anchor 就是触发器本身：菜单贴着它出，再点它一次即收起
export function openChannelMenu(anchor: HTMLElement, channel: string, opts: ChannelMenuOpts = {}) {
  if (openAnchor === anchor) {
    closeChannelMenu();
    return;
  }
  closeChannelMenu();
  document.querySelector('.user-menu')?.remove(); // 与用户/消息菜单互斥（那两个是纯命令式浮层）
  const host = document.createElement('div');
  document.body.appendChild(host);
  const d = render(() => <ChannelMenu anchor={anchor} channel={channel} opts={opts} />, host);
  openAnchor = anchor;
  dispose = () => {
    d();
    host.remove();
  };
}

function ChannelMenu(p: { anchor: HTMLElement; channel: string; opts: ChannelMenuOpts }) {
  let box!: HTMLDivElement;
  const guest = isGuest(getUser());
  const canManage = p.opts.myRole === 'owner' || p.opts.myRole === 'moderator';
  const muted = p.opts.muted === true;

  onMount(() => {
    const r = p.anchor.getBoundingClientRect();
    const w = box.offsetWidth;
    const h = box.offsetHeight;
    box.style.left = `${Math.max(8, Math.min(r.left, window.innerWidth - w - 8))}px`;
    const below = r.bottom + 6;
    // 装不下就翻到触发器上方（大厅卡片在页面底部时）
    box.style.top = `${below + h > window.innerHeight - 8 ? Math.max(8, r.top - h - 6) : below}px`;
  });

  const onDoc = (ev: Event) => {
    // 点触发器不算「点外面」：那一下由它的 click 走 openChannelMenu 的收起分支
    if (!box.contains(ev.target as Node) && !p.anchor.contains(ev.target as Node)) closeChannelMenu();
  };
  const onKey = (ev: KeyboardEvent) => {
    if (ev.key === 'Escape') closeChannelMenu();
  };
  // 延后注册：触发菜单的那次 pointerdown 还没冒泡完，立刻注册会自己把自己关掉
  const armTimer = setTimeout(() => document.addEventListener('pointerdown', onDoc, true));
  document.addEventListener('keydown', onKey, true);
  onCleanup(() => {
    clearTimeout(armTimer);
    document.removeEventListener('pointerdown', onDoc, true);
    document.removeEventListener('keydown', onKey, true);
  });

  // 静音：落库即生效（setChannelMuted 自己派 hearth:channels，房间页与侧栏据此重取）
  const toggleMute = async () => {
    closeChannelMenu();
    try {
      await setChannelMuted(p.channel, !muted);
      toast(muted ? `已恢复「${p.channel}」的提醒` : `已静音「${p.channel}」`, 'ok');
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  };

  const copyLink = async () => {
    closeChannelMenu();
    const url = `${location.origin}${location.pathname}#/room/${encodeURIComponent(p.channel)}`;
    if (await copyText(url)) toast('已复制频道链接', 'ok', 1400);
    else toast('复制失败，请手动复制地址栏', 'bad');
  };

  return (
    <div ref={box} class="user-menu channel-menu" role="menu">
      <div class="um-title">#{p.channel}</div>
      <button type="button" class="hit um-item" role="menuitem" data-act="mute" onClick={() => void toggleMute()}>
        {el(slashIcon('bell', 14, !muted, 'currentColor'))}
        <span>{muted ? '取消静音' : '静音本频道'}</span>
      </button>
      <Show when={p.opts.onIngest && !guest}>
        <button
          type="button"
          class="hit um-item"
          role="menuitem"
          data-act="ingest"
          onClick={() => {
            closeChannelMenu();
            p.opts.onIngest!();
          }}
        >
          {el(icon('stream', 14, 'currentColor'))}
          <span>OBS 推流地址…</span>
        </button>
      </Show>
      <Show when={!guest}>
        <button type="button" class="hit um-item" role="menuitem" data-act="invite" onClick={() => void copyLink()}>
          {el(icon('copy', 14, 'currentColor'))}
          <span>复制邀请链接</span>
        </button>
      </Show>
      <Show when={canManage}>
        <div class="um-sep"></div>
        <button
          type="button"
          class="hit um-item"
          role="menuitem"
          data-act="manage"
          onClick={() => {
            closeChannelMenu();
            openSettings('channel', { channel: p.channel, backLabel: `返回 ${p.channel}` });
          }}
        >
          {el(icon('shield', 14, 'currentColor'))}
          <span>频道管理…</span>
        </button>
      </Show>
      <Show when={p.opts.onLeave}>
        <div class="um-sep"></div>
        <button
          type="button"
          class="hit um-item danger"
          role="menuitem"
          data-act="leave"
          onClick={() => {
            closeChannelMenu();
            p.opts.onLeave!();
          }}
        >
          {el(icon('leave', 14, 'var(--red)'))}
          <span>离开频道</span>
        </button>
      </Show>
    </div>
  );
}
