// 名册里的「推流中」徽标：OBS 推的是一路投屏轨，实测数据直接借用那张卡片已有的
// 2 秒轮询（stats 由房间页按 identity 取），不再为名册另开一路 getStats。
import { createSignal } from 'solid-js';
import type { VideoStats } from '../../engine/types';

export const IngestBadge = (p: { stats: () => VideoStats | null }) => {
  const [open, setOpen] = createSignal(false);
  const s = () => p.stats();
  const line = () => {
    const v = s();
    if (!v) return '尚未收到画面';
    const loss = v.loss === undefined ? '' : ` · 丢包 ${v.loss.toFixed(1)}%`;
    return `${v.width}×${v.height} · ${Math.round(v.fps)}fps · ${(v.kbps / 1000).toFixed(1)}Mbps${loss}`;
  };
  return (
    <span class="ingest-badge" classList={{ open: open() }}>
      <button
        type="button"
        class="hit ingest-chip"
        title={line()}
        aria-label={`推流中：${line()}`}
        onClick={(ev) => {
          ev.stopPropagation();
          setOpen((o) => !o);
        }}
      >
        推流中
      </button>
      <span class="ingest-pop mono">{line()}</span>
    </span>
  );
};
