// 原生投屏的采集源选择：桌面壳里点「投屏」先挑一块显示器或一个窗口。
// 浏览器的 getDisplayMedia 自带系统选择器，这一层只有原生流程需要。
import { createEffect, createMemo, createSignal, For, onCleanup, Show } from 'solid-js';
import { sourcePreview } from '../../bridge';
import type { NativeSource } from '../../bridge';
import type { ObsConn } from '../../obsws';
import { ObsCaptureSection } from './obs-capture';
import { el, icon } from '../../ui';

/** 本机 OBS 连上了才传：面板里多一节「通过 OBS 投屏」，原生那条路一行不变 */
export type ObsShareOption = {
  conn: ObsConn;
  obsVersion: string;
  platform: string;
  start: () => Promise<void>;
  onReady: () => void;
  onStarted: () => void;
};

export const NativeSourcePanel = (p: {
  sources: NativeSource[];
  appAudio: boolean;
  encoder: string; // 这次投屏会用的编码器，空串表示壳没有原生能力
  screenAudio: boolean;
  obs?: ObsShareOption | null;
  onConfirm: (source: NativeSource, audio: boolean) => void;
  onClose: () => void;
}) => {
  const displays = createMemo(() => p.sources.filter((s) => s.kind === 'display'));
  const windows = createMemo(() => p.sources.filter((s) => s.kind === 'window'));
  const [selected, setSelected] = createSignal<NativeSource | null>(null);
  const [previews, setPreviews] = createSignal<Record<string, string | null>>({});
  const [audio, setAudio] = createSignal(p.screenAudio);
  let disposed = false;
  // 预览按需取：壳里每张缩略图都是一条真的采集管线，几十个源全预取要等几十秒。
  // 列表先画出来，只给视口内、悬停、选中的源排队，优先级 0 选中 > 1 悬停 > 2 视口内。
  const order = new Map(p.sources.map((s, i) => [s.id, i] as const));
  const queue = new Map<string, number>();
  let active = false;
  let pumpTimer = 0;

  const audioScope = () => selected()?.audio_scope ?? 'none';
  const canAudio = () => p.appAudio && audioScope() !== 'none';
  const audioLabel = () => {
    const source = selected();
    if (!p.appAudio) return '当前设备不支持原生投屏音频';
    if (!source || audioScope() === 'none') return '这个画面不能共享声音';
    return audioScope() === 'system' ? '共享整屏系统声音' : `共享 ${source.app || '所属应用'} 的声音`;
  };
  // 壳里的预览采集是全局单路，这里也只发一路；空了再等一小会儿，别把壳打满。
  const pump = () => {
    pumpTimer = 0;
    if (disposed || active || !queue.size) return;
    let id = '';
    let best = Infinity;
    for (const [candidate, priority] of queue) {
      const rank = priority * 1e6 + (order.get(candidate) ?? 0);
      if (rank < best) {
        best = rank;
        id = candidate;
      }
    }
    queue.delete(id);
    active = true;
    void sourcePreview(id)
      .then((img) => {
        if (!disposed) setPreviews((all) => ({ ...all, [id]: img }));
      })
      .catch(() => {
        // 缩略图只是辅助选择；取不到就留占位图，不影响共享。
        if (!disposed) setPreviews((all) => ({ ...all, [id]: all[id] ?? null }));
      })
      .finally(() => {
        active = false;
        if (!disposed) pumpTimer = window.setTimeout(pump, 120);
      });
  };
  const request = (id: string, priority: number) => {
    if (disposed) return;
    const queued = queue.get(id);
    if (queued !== undefined && queued <= priority) return;
    queue.set(id, priority);
    if (!active && !pumpTimer) pump();
  };
  // 视口内才取：列表可以很长，滚到哪儿取哪儿。
  const observer = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        const id = (e.target as HTMLElement).dataset.sourceId;
        if (e.isIntersecting && id) request(id, 2);
      }
    },
    { rootMargin: '120px' },
  );
  createEffect(() => {
    const source = selected();
    if (!source) return;
    request(source.id, 0);
    // 选中项保持刷新（准备共享的那一个要看得见动静），但不抢在别人前面重复排队。
    const timer = window.setInterval(() => {
      if (!active && !queue.size) request(source.id, 0);
    }, 1500);
    onCleanup(() => window.clearInterval(timer));
  });
  onCleanup(() => {
    disposed = true;
    queue.clear();
    observer.disconnect();
    window.clearTimeout(pumpTimer);
  });

  const row = (s: NativeSource) => (
    <button
      type="button"
      class="hit src-item"
      classList={{ selected: selected()?.id === s.id }}
      data-source-id={s.id}
      ref={(node) => observer.observe(node)}
      onMouseEnter={() => request(s.id, 1)}
      onClick={() => setSelected(s)}
    >
      <span class="src-preview">
        <Show when={previews()[s.id]} fallback={el(icon(s.kind === 'display' ? 'monitor' : 'grid', 20, 'var(--text-2)', 1.7))}>
          <img src={previews()[s.id] ?? ''} alt="" />
        </Show>
      </span>
      {/* 窗口按应用名认人：标题五花八门，「是哪个程序」才是挑源时先看的那一行 */}
      <span class="src-copy">
        <span class="src-name">{s.kind === 'display' ? s.title : s.app || s.title}</span>
        <span class="src-meta">{s.kind === 'display' ? '整个屏幕' : s.title}</span>
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

        <Show when={p.obs}>
          <div class="ig-sep" />
          <ObsCaptureSection
            conn={p.obs!.conn}
            obsVersion={p.obs!.obsVersion}
            platform={p.obs!.platform}
            start={p.obs!.start}
            onReady={p.obs!.onReady}
            onStarted={p.obs!.onStarted}
          />
          <div class="ig-sep" />
        </Show>

        <div class="native-share-options">
          <label class="native-audio-option" classList={{ disabled: !canAudio() }}>
            <input type="checkbox" checked={audio()} disabled={!canAudio()} onChange={(ev) => setAudio(ev.currentTarget.checked)} />
            <span>{audioLabel()}</span>
          </label>
          <div class="ig-tip">麦克风仍由通话控制；请选择画面后再确认共享。</div>
          <Show when={p.encoder}>
            <div class="ig-tip">编码器：{p.encoder}</div>
          </Show>
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
