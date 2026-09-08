// 卡片上的「测延迟」：菜单（命令式浮层，与 msg-menu 同一套做法）+ 结果面板 + 测量执行。
// 房间页只负责把菜单点开、把状态放进一个信号，测量本身在这里跑。
import { Show } from 'solid-js';
import { measure } from '../../latency/measure';
import { esc, icon } from '../../ui';

// 同一时刻只测一路（一次测量 10 秒占着 CPU 解像素，多路并行没有意义）
export interface LatencyState {
  key: string; // 卡片 key，决定结果面板贴在哪张卡上
  text: string;
  running: boolean;
}

// 卡片右上角「更多操作」：目前只有测延迟一项，后续卡片级操作也挂这里
export function showTileMenu(x: number, y: number, onMeasure: () => void) {
  document.querySelector('.user-menu')?.remove();
  const menu = document.createElement('div');
  menu.className = 'user-menu tile-menu';
  menu.innerHTML = `<button class="hit um-item" data-act="latency">${icon('clock', 14, 'currentColor')}<span>测延迟</span></button>
    <div class="um-title">需要推流方打开 ${esc('#/latency')} 标尺页</div>`;
  document.body.appendChild(menu);
  menu.style.left = `${Math.max(8, Math.min(x, window.innerWidth - menu.offsetWidth - 8))}px`;
  menu.style.top = `${Math.max(8, Math.min(y, window.innerHeight - menu.offsetHeight - 8))}px`;

  const close = () => {
    menu.remove();
    document.removeEventListener('pointerdown', onDoc, true);
  };
  const onDoc = (ev: Event) => {
    if (!menu.contains(ev.target as Node)) close();
  };
  // 延后注册：触发菜单的那次 pointerdown 还没冒泡完，立刻注册会自己把自己关掉
  setTimeout(() => document.addEventListener('pointerdown', onDoc, true));
  menu.querySelector('[data-act="latency"]')?.addEventListener('click', () => {
    onMeasure();
    close();
  });
}

// 跑一次测量并把过程与结果写进调用方的信号
export async function measureTile(key: string, video: HTMLVideoElement, set: (s: LatencyState | null) => void) {
  let got = 0;
  set({ key, text: '正在读画面里的标尺…', running: true });
  try {
    const r = await measure(video, 10, (d) => {
      if (d !== null) got++;
      set({ key, text: got ? `正在测…已取到 ${got} 个样本` : '正在读画面里的标尺…', running: true });
    });
    if (!r) {
      set({ key, text: '画面里没有标尺：让推流方打开 #/latency 页并共享/推流那个画面', running: false });
      return;
    }
    set({
      key,
      text: `端到端 ${r.median_ms} ms（p95 ${r.p95_ms}，${r.samples} 样本，时钟 ±${r.clock_ms} ms）`,
      running: false,
    });
  } catch (err) {
    set({ key, text: `测不了：${(err as Error).message}`, running: false });
  }
}

export const LatencyResult = (p: { state: () => LatencyState; onClose: () => void }) => (
  <div class="tile-latency">
    <span class="tl-text mono">{p.state().text}</span>
    <Show when={!p.state().running}>
      <button class="hit tl-close" title="关闭" onClick={() => p.onClose()}>
        ×
      </button>
    </Show>
  </div>
);
