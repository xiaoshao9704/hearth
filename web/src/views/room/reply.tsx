// 引用回复的两块 UI：卡片顶部的被引摘要（点击跳到原消息）与输入框上方的引用态条。
import { Show } from 'solid-js';
import type { ChatMessage } from '../../chat';
import { el, icon } from '../../ui';

const SUMMARY_MAX = 60;

// quoteSummary 被引消息的一行摘要：换行折成空格，超长截断；文件取文件名，撤回的给占位
export function quoteSummary(m: ChatMessage): string {
  if (m.deleted) return '消息已撤回';
  if (m.kind === 'file') return m.file?.name ?? '文件';
  const one = m.content.replace(/\s+/g, ' ').trim();
  return one.length > SUMMARY_MAX ? `${one.slice(0, SUMMARY_MAX)}…` : one;
}

// ReplyQuote 消息卡片顶部的引用行。target 为空 = 被引消息不在本次已加载的历史里
// （太早，或已被保留策略清掉）：如实说"原消息不在了"，不装作能跳过去。
export function ReplyQuote(p: { target: ChatMessage | undefined; onJump: (id: number) => void }) {
  return (
    <Show
      when={p.target}
      fallback={<div class="reply-quote missing">↩ 原消息已不在最近的记录里</div>}
    >
      {(t) => (
        <button type="button" class="hit reply-quote" onClick={() => p.onJump(t().id)}>
          <span class="rq-who">{t().username}</span>
          <span class="rq-text">{quoteSummary(t())}</span>
        </button>
      )}
    </Show>
  );
}

// ReplyComposer 输入框上方的引用态；取消即退出引用
export function ReplyComposer(p: { target: ChatMessage | undefined; onCancel: () => void }) {
  return (
    <Show when={p.target}>
      {(t) => (
        <div class="reply-bar">
          <span class="rb-label">回复</span>
          <span class="rq-who">{t().username}</span>
          <span class="rq-text">{quoteSummary(t())}</span>
          <button type="button" class="hit rb-close" aria-label="取消回复" onClick={p.onCancel}>
            {el(icon('close', 13, 'currentColor', 1.8))}
          </button>
        </div>
      )}
    </Show>
  );
}
