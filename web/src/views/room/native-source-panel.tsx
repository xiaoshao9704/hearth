// 原生投屏的采集源选择：桌面壳里点「投屏」先挑一块显示器或一个窗口。
// 浏览器的 getDisplayMedia 自带系统选择器，这一层只有原生流程需要。
import { createEffect, createMemo, createSignal, For, onCleanup, Show } from 'solid-js';
import { sourcePreview } from '../../bridge';
import type { NativeSource } from '../../bridge';
import { el, icon } from '../../ui';

export const NativeSourcePanel = (p: {
  sources: NativeSource[];
  appAudio: boolean;
  screenAudio: boolean;
  onConfirm: (source: NativeSource, audio: boolean) => void;
  onClose: () => void;
}) => {
  const displays = createMemo(() => p.sources.filter((s) => s.kind === 'display'));
  const windows = createMemo(() => p.sources.filter((s) => s.kind === 'window'));
  const [selected, setSelected] = createSignal<NativeSource | null>(null);
  const [previews, setPreviews] = createSignal<Record<string, string | null>>({});
  const [audio, setAudio] = createSignal(p.screenAudio);
  let disposed = false;
  const previewPending = new Set<string>();
  const previewQueued = new Set<string>();
  const previewQueue: Array<{ source: NativeSource; retry: number }> = [];
  let previewActive = false;
  let previewDelayTimer = 0;

  const audioScope = () => selected()?.audio_scope ?? 'none';
  const canAudio = () => p.appAudio && audioScope() !== 'none';
  const audioLabel = () => {
    const source = selected();
    if (!p.appAudio) return '当前设备不支持原生投屏音频';
    if (!source || audioScope() === 'none') return '这个画面不能共享声音';
    return audioScope() === 'system' ? '共享整屏系统声音' : `共享 ${source.app || '所属应用'} 的声音`;
  };
  const loadPreview = async (source: NativeSource, retry = 0) => {
    previewPending.add(source.id);
    previewActive = true;
    try {
      const preview = await sourcePreview(source.id);
      if (!disposed) setPreviews((all) => ({ ...all, [source.id]: preview }));
      // Rust 会为忙态排队，但采集源短暂切换时仍可能没有帧；初始缩略图再试两次。
      if (!disposed && preview === null && retry < 2) queuePreview(source, false, retry + 1);
    } catch {
      // 缩略图只是辅助选择；壳较旧或暂时取不到时仍可正常共享。
      if (!disposed) setPreviews((all) => ({ ...all, [source.id]: null }));
      if (!disposed && retry < 2) queuePreview(source, false, retry + 1);
    } finally {
      previewPending.delete(source.id);
      previewActive = false;
      if (!disposed) previewDelayTimer = window.setTimeout(pumpPreviews, 550);
    }
  };

  // Rust 的预览采集本身是全局单路；这里也只发一路，选中项插队，初始任务不会被忙态吞掉。
  const pumpPreviews = () => {
    previewDelayTimer = 0;
    if (disposed || previewActive || !previewQueue.length) return;
    const task = previewQueue.shift()!;
    previewQueued.delete(task.source.id);
    if (!previewPending.has(task.source.id)) void loadPreview(task.source, task.retry);
    else pumpPreviews();
  };
  const queuePreview = (source: NativeSource, priority = false, retry = 0) => {
    if (disposed || previewPending.has(source.id)) return;
    const queuedAt = previewQueue.findIndex((item) => item.source.id === source.id);
    if (queuedAt >= 0) {
      if (priority && queuedAt > 0) previewQueue.unshift(previewQueue.splice(queuedAt, 1)[0]);
    } else {
      previewQueued.add(source.id);
      const task = { source, retry };
      if (priority) previewQueue.unshift(task);
      else previewQueue.push(task);
    }
    if (!previewDelayTimer) pumpPreviews();
  };
  p.sources.forEach((source) => queuePreview(source));
  createEffect(() => {
    const source = selected();
    if (!source) return;
    queuePreview(source, true);
    const timer = window.setInterval(() => queuePreview(source, true), 1000);
    onCleanup(() => window.clearInterval(timer));
  });
  onCleanup(() => {
    disposed = true;
    previewQueue.length = 0;
    window.clearTimeout(previewDelayTimer);
  });

  const row = (s: NativeSource) => (
    <button type="button" class="hit src-item" classList={{ selected: selected()?.id === s.id }} onClick={() => setSelected(s)}>
      <span class="src-preview">
        <Show when={previews()[s.id]} fallback={el(icon(s.kind === 'display' ? 'monitor' : 'grid', 20, 'var(--text-2)', 1.7))}>
          <img src={previews()[s.id] ?? ''} alt="" />
        </Show>
      </span>
      <span class="src-copy">
        <span class="src-name">{s.title}</span>
        <span class="src-meta">{s.kind === 'display' ? '整个屏幕' : s.app || '窗口'}</span>
      </span>
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

        <div class="native-share-options">
          <label class="native-audio-option" classList={{ disabled: !canAudio() }}>
            <input type="checkbox" checked={audio()} disabled={!canAudio()} onChange={(ev) => setAudio(ev.currentTarget.checked)} />
            <span>{audioLabel()}</span>
          </label>
          <div class="ig-tip">麦克风仍由通话控制；请选择画面后再确认共享。</div>
        </div>
        <footer class="native-share-actions">
          <button type="button" class="hit btn" onClick={p.onClose}>取消</button>
          <button type="button" class="hit btn primary" disabled={!selected()} onClick={() => {
            const source = selected();
            if (source) p.onConfirm(source, canAudio() && audio());
          }}>共享</button>
        </footer>
      </div>
    </div>
  );
};
