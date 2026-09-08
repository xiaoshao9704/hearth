// 端到端延迟标尺 `#/latency`：把服务器对齐过的时间画成一行黑白色码，
// 推流方用 OBS 抓这个窗口、或在浏览器投屏里共享这个标签页；观众在那路画面上点「测延迟」，
// 从帧里读回时间戳与自己的时钟相减，就得到全程延迟。不需要登录（只读一个时间接口）。
import { encode } from '../latency/code';
import { syncClock, syncQuality, syncedNow } from '../latency/clock';
import { esc } from '../ui';

// 色码行占顶部 1/3 高（.lat-bar 的 flex 基准，见 style.css）：
// 观众端按画面高度 1/6 处取样，正落在这一行的中间

function fmtTime(ms: number): string {
  const d = new Date(ms);
  const p = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

export async function renderLatency(root: HTMLElement, alive: () => boolean) {
  root.innerHTML = `
    <div class="latency-page">
      <canvas class="lat-bar" id="lat-bar"></canvas>
      <div class="lat-body">
        <div class="lat-clock mono" id="lat-clock">校准中…</div>
        <div class="lat-sub" id="lat-sub">正在与服务器校准时钟…</div>
        <div class="lat-note">
          用 OBS 抓这个窗口，或在浏览器投屏里共享这个标签页；观众在那路画面上点「测延迟」。<br />
          色码是这台机器对齐到服务器后的时间，两端不必同机。
        </div>
      </div>
    </div>`;

  const canvas = root.querySelector<HTMLCanvasElement>('#lat-bar')!;
  const clockEl = root.querySelector<HTMLElement>('#lat-clock')!;
  const subEl = root.querySelector<HTMLElement>('#lat-sub')!;

  try {
    await syncClock();
  } catch (err) {
    if (!alive()) return;
    subEl.innerHTML = `校准失败：${esc((err as Error).message)}——刷新页面重试`;
    return;
  }
  if (!alive()) return;

  const ctx = canvas.getContext('2d');
  if (!ctx) {
    subEl.textContent = '这个浏览器画不出色码（canvas 不可用）';
    return;
  }

  const paint = () => {
    // 页面已切走（视图被换掉）就停：vanilla 视图没有 dispose 钩子，靠元素还在不在文档里判断
    if (!canvas.isConnected) return;
    // 不看 document.hidden：页面不渲染时 rAF 自己就不回调（这就是「隐藏时停」），
    // 而标签页被采集时 hidden 也是 true——那时再停就会让观众读到一个冻住的时间戳
    const dpr = window.devicePixelRatio || 1;
    const w = Math.max(40, Math.round(canvas.clientWidth * dpr));
    const h = Math.max(1, Math.round(canvas.clientHeight * dpr));
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
    }
    const now = syncedNow();
    const bits = encode(now);
    const bw = w / bits.length;
    for (let i = 0; i < bits.length; i++) {
      ctx.fillStyle = bits[i] ? '#ffffff' : '#000000';
      // 块边界取整到像素，避免相邻块之间留下半透明缝（缝会被缩放混色，观众端就判不准）
      const x0 = Math.round(i * bw);
      const x1 = Math.round((i + 1) * bw);
      ctx.fillRect(x0, 0, x1 - x0, h);
    }
    clockEl.textContent = fmtTime(now);
    const q = syncQuality();
    subEl.textContent = q === null ? '' : `时钟校准 ±${Math.round(q / 2)} ms`;
    requestAnimationFrame(paint);
  };
  requestAnimationFrame(paint);
}
