// 无投屏时的「聊天为主」布局顶栏：一行频道用户（头像 + 名字 + 说话高亮）。
// 舞台区这时没有任何画面可看，与其留一屏空卡片，不如把在场的人压成一条，把地方让给聊天。
import { createEffect, createMemo, on, untrack, For, Show } from 'solid-js';
import type { EPart } from '../../engine/types';
import { avatarHtml, el, icon, micIcon } from '../../ui';

export interface ChatFirstProps {
  parts: () => EPart[];
  speaking: () => Set<string>;
  onMenu?: (x: number, y: number, p: EPart) => void;
}

interface UserChip {
  key: string;
  name: string;
  parts: EPart[];
}

export function ChatFirstBar(props: ChatFirstProps) {
  const chips = createMemo<UserChip[]>(() => {
    const map = new Map<string, UserChip>();
    for (const p of [...props.parts()].sort((a, b) => a.username.localeCompare(b.username) || a.uid - b.uid)) {
      const key = p.uid > 0 ? `u${p.uid}` : p.identity;
      const ex = map.get(key);
      if (ex) ex.parts.push(p);
      else map.set(key, { key, name: p.username || p.display || '—', parts: [p] });
    }
    return [...map.values()];
  });

  const isSpeaking = (c: UserChip) => c.parts.some((p) => props.speaking().has(p.identity));
  const micOn = (c: UserChip) => c.parts.some((p) => p.micOn);

  return (
    <div class="chat-first-bar">
      <For each={chips()}>
        {(c) => (
          <div
            class="cf-chip"
            classList={{ speaking: isSpeaking(c) }}
            title={c.name}
            onContextMenu={(ev) => {
              if (!props.onMenu) return;
              ev.preventDefault();
              props.onMenu(ev.clientX, ev.clientY, c.parts[0]);
            }}
          >
            {el(avatarHtml(c.name, 'avatar avatar-sm'))}
            <span class="cf-name">{c.name}</span>
            <Show when={!micOn(c)}>
              <span class="cf-mute">{el(micIcon(12, true, 'currentColor'))}</span>
            </Show>
          </div>
        )}
      </For>
      <Show when={chips().length === 0}>
        <span class="cf-empty">房间里还没有别人</span>
      </Show>
      <span class="spacer"></span>
      <span class="cf-note">
        {el(icon('screen', 13, 'currentColor', 1.7))}
        <span>没有人在投屏，先聊着</span>
      </span>
    </div>
  );
}

// 「聊天为主」时聊天区常驻可见：未读清零、滚动跟随、发送按钮都挂在 panel() 上，
// 所以把它归位到 chat，而不是另立一个"聊天是否可见"的布尔。离开该布局时还原成
// 进入前的选择——有人开始投屏时抽屉不该继续挡着画面。
export function syncPanelWithChatFirst(
  layoutMode: () => 'chat' | 'stage' | 'theater',
  panel: () => 'members' | 'chat' | '',
  switchPanel: (p: 'members' | 'chat' | '') => void,
) {
  let before: 'members' | 'chat' | '' | null = null;
  createEffect(
    on(layoutMode, (mode, prev) => {
      if (mode === 'chat') {
        if (prev === 'chat') return;
        before = untrack(panel);
        if (before !== 'chat') switchPanel('chat');
      } else if (prev === 'chat') {
        if (before !== null && untrack(panel) === 'chat') switchPanel(before);
        before = null;
      }
    }),
  );
}
