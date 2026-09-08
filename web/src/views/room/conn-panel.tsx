// 顶栏 conn-chip 点开的连接读数面板：每条线一行 RTT · 抖动 · 丢包 · 传输方式。
// 数据取引擎的 transport 快照（引擎自己 5 秒刷一次），这里只是按同样的节奏重读，
// 不另开 getStats；面板关着的时候整个组件不存在，也就不读。
import { createMemo, createSignal, onCleanup, For, Show } from 'solid-js';
import type { LineStats } from '../../engine/types';
import { el, icon } from '../../ui';

export interface ConnRow {
  label: string;
  stats: () => LineStats | null;
}

const TRANSPORT_TEXT: Record<string, string> = {
  udp: 'UDP',
  tcp: 'TCP 兜底',
  relay: '中继兜底',
  unknown: '未知',
};

function fmt(s: LineStats | null): string {
  if (!s) return '未连接';
  const bits: string[] = [];
  bits.push(s.rtt_ms === undefined ? 'RTT —' : `RTT ${s.rtt_ms} ms`);
  if (s.jitter_ms !== undefined) bits.push(`抖动 ${s.jitter_ms} ms`);
  if (s.loss_pct !== undefined) bits.push(`丢包 ${s.loss_pct}%`);
  bits.push(TRANSPORT_TEXT[s.transport ?? 'unknown'] ?? '未知');
  return bits.join(' · ');
}

export const ConnPanel = (p: { rows: () => ConnRow[]; anchor: HTMLElement | null; onClose: () => void }) => {
  const [tick, setTick] = createSignal(0);
  const timer = window.setInterval(() => setTick((t) => t + 1), 5000);
  onCleanup(() => window.clearInterval(timer));

  // tick 只为让读数随时间重算：LineStats 本身不是信号，靠这一下把 memo 拉脏
  const rows = createMemo(() => {
    tick();
    return p.rows().map((r) => ({ label: r.label, text: fmt(r.stats()), remote: r.stats()?.remote ?? '' }));
  });

  let panelEl!: HTMLDivElement;
  const onDoc = (ev: Event) => {
    const t = ev.target as Node;
    if (!panelEl.contains(t) && !p.anchor?.contains(t)) p.onClose();
  };
  // 延后注册：开面板那次 pointerdown 还没冒泡完，立刻注册会自己把自己关掉
  const arm = window.setTimeout(() => document.addEventListener('pointerdown', onDoc, true));
  onCleanup(() => {
    window.clearTimeout(arm);
    document.removeEventListener('pointerdown', onDoc, true);
  });

  const rect = p.anchor?.getBoundingClientRect();
  const left = Math.max(8, Math.min(rect?.left ?? 8, window.innerWidth - 268));
  const top = (rect?.bottom ?? 56) + 6;

  return (
    <div id="conn-panel" class="conn-panel" ref={panelEl} style={`left:${left}px;top:${top}px`}>
      <div class="cp-title">连接质量</div>
      <For each={rows()}>
        {(r) => (
          <div class="cp-row">
            <span class="cp-line">{r.label}</span>
            <span class="cp-val mono" title={r.remote}>
              {r.text}
            </span>
          </div>
        )}
      </For>
      <Show when={rows().length === 0}>
        <div class="cp-row">
          <span class="cp-val">尚未连接</span>
        </div>
      </Show>
      <div class="cp-note">RTT 是你到服务器的往返；抖动与丢包按你实际收到的包统计。</div>
    </div>
  );
};

// 名册里的连接质量标记：只在内核判定 poor/lost 时出现（excellent/good 与未回报都不出图标，
// 一切正常时名册应当是干净的）。数据来自参与者快照的 quality，不另外算
export const QualityMark = (p: { quality: () => string | undefined }) => (
  <Show when={p.quality() === 'poor' || p.quality() === 'lost'}>
    <span
      class="quality-mark"
      classList={{ lost: p.quality() === 'lost' }}
      title={p.quality() === 'lost' ? '与服务器失联' : '连接质量差'}
      aria-label={p.quality() === 'lost' ? '与服务器失联' : '连接质量差'}
    >
      {el(icon('signalLow', 13, 'currentColor', 1.7))}
    </span>
  </Show>
);
