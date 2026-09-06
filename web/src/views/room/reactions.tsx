// 消息卡片下方的反应条：每个表情一个 chip，显示计数，自己点过的高亮（再点是取消）。
import { For, Show } from 'solid-js';
import type { ChatReaction } from '../../chat';

// 服务端白名单的同一份（api/chat_reactions.go 的 reactionEmojis）：
// 两边都写死是刻意的——多一个"查可用表情"的接口不值得，超集/子集不一致时服务端 400 兜底。
export const REACTION_EMOJIS = ['👍', '❤️', '😂', '😮', '😢', '🔥', '👀', '🎉'] as const;

// mergeReaction 把"某人点了/取消了某个表情"合进聚合结果，返回新数组（不改原对象，Solid 才看得见变化）。
// 三处共用同一规则：自己点击的乐观更新、失败回滚、对端广播到达——不然本地与服务端会算出不同的计数。
export function mergeReaction(
  list: ChatReaction[] | undefined,
  emoji: string,
  uid: number,
  on: boolean,
): ChatReaction[] {
  const out = (list ?? []).map((r) => ({ emoji: r.emoji, uids: [...r.uids] }));
  const hit = out.find((r) => r.emoji === emoji);
  if (on) {
    if (!hit) out.push({ emoji, uids: [uid] });
    else if (!hit.uids.includes(uid)) hit.uids.push(uid);
    return out;
  }
  if (!hit) return out;
  hit.uids = hit.uids.filter((x) => x !== uid);
  return out.filter((r) => r.uids.length > 0);
}

export function ReactionBar(p: {
  reactions: ChatReaction[] | undefined;
  myUid: number;
  onToggle: (emoji: string, on: boolean) => void;
}) {
  const list = () => (p.reactions ?? []).filter((r) => r.uids.length > 0);
  return (
    <Show when={list().length > 0}>
      <div class="msg-reactions">
        <For each={list()}>
          {(r) => {
            const mine = () => r.uids.includes(p.myUid);
            return (
              <button
                type="button"
                class="hit reaction-chip"
                classList={{ on: mine() }}
                title={mine() ? '取消我的反应' : '加上我的反应'}
                onClick={() => p.onToggle(r.emoji, !mine())}
              >
                <span class="re-emoji">{r.emoji}</span>
                <span class="re-n mono">{r.uids.length}</span>
              </button>
            );
          }}
        </For>
      </div>
    </Show>
  );
}
