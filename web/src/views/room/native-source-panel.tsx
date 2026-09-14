// 原生投屏的采集源选择：桌面壳里点「投屏」先挑一块显示器或一个窗口。
// 浏览器的 getDisplayMedia 自带系统选择器，这一层只有原生流程需要。
import { createMemo, For, Show } from 'solid-js';
import type { NativeSource } from '../../bridge';
import { el, icon } from '../../ui';

export const NativeSourcePanel = (p: {
  sources: NativeSource[];
  appAudio: boolean;
  onPick: (source: NativeSource) => void;
  onClose: () => void;
}) => {
  const displays = createMemo(() => p.sources.filter((s) => s.kind === 'display'));
  const windows = createMemo(() => p.sources.filter((s) => s.kind === 'window'));

  const row = (s: NativeSource) => (
    <button type="button" class="hit src-item" onClick={() => p.onPick(s)}>
      {el(icon(s.kind === 'display' ? 'monitor' : 'grid', 15, 'var(--text-2)', 1.7))}
      <span class="src-name">{s.title}</span>
      <Show when={s.app}>
        <span class="src-app">{s.app}</span>
      </Show>
    </button>
  );

  return (
    <div class="ingest-scrim" onClick={p.onClose}>
      <div class="ingest-panel card" onClick={(ev) => ev.stopPropagation()}>
        <header class="ig-head">
          {el(icon('screen', 16, 'var(--ember)', 1.7))}
          <div class="ig-title">选择要共享的画面</div>
          <button type="button" class="hit btn btn-icon" aria-label="关闭" onClick={p.onClose}>
            {el(icon('close', 15, 'var(--text-1)', 1.8))}
          </button>
        </header>

        <Show when={displays().length}>
          <div class="ig-field">
            <div class="section-label">整个屏幕</div>
            <div class="src-list">
              <For each={displays()}>{row}</For>
            </div>
          </div>
        </Show>
        <Show when={windows().length}>
          <div class="ig-field">
            <div class="section-label">窗口</div>
            <div class="src-list">
              <For each={windows()}>{row}</For>
            </div>
          </div>
        </Show>

        <div class="ig-tip">
          {p.appAudio
            ? '共享画面与所属应用的声音；你自己的麦克风仍由通话那边负责。'
            : '只共享画面，不带声音。'}
        </div>
      </div>
    </div>
  );
};
