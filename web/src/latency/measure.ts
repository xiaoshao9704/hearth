// 从一路视频里读端到端延迟：每 150 ms 把画面上部的标尺行画进离屏 canvas，
// 解出色码里的时间戳，与自己校准过的时钟相减，得到采集 → 编码 → 服务器 → 解码 → 渲染的全程。
import { BLOCKS, decode } from './code';
import { ensureClock, syncQuality, syncedNow } from './clock';

export interface LatencyResult {
  median_ms: number;
  p95_ms: number;
  samples: number;
  clock_ms: number; // 时钟对齐的误差界（校准往返的一半）
}

const SAMPLE_MS = 150;
const PX_PER_BLOCK = 5; // 每块横向取 5 px，只用中间 3 px（中心 60%），躲开块边界的缩放混色
const ROWS = 3;
const WRAP = 0x1_0000_0000;
const MAX_DELAY_MS = 60_000; // 超过一分钟的读数只可能是解错/回绕，丢掉

// 标尺占页面顶部 1/3，取其中线附近（画面高度 1/6 处）一条窄带
const BAND_CENTER = 1 / 6;
const BAND_HEIGHT = 1 / 30;

function waitReady(video: HTMLVideoElement, timeoutMs = 5000): Promise<boolean> {
  if (video.readyState >= 2 && video.videoWidth > 0) return Promise.resolve(true);
  return new Promise((resolve) => {
    const deadline = Date.now() + timeoutMs;
    const tick = () => {
      if (video.readyState >= 2 && video.videoWidth > 0) return resolve(true);
      if (Date.now() > deadline) return resolve(false);
      window.setTimeout(tick, 100);
    };
    tick();
  });
}

// 把窄带里每个块的中心区域压成一个亮度值（0~255）
function readRow(ctx: CanvasRenderingContext2D): number[] {
  const { data } = ctx.getImageData(0, 0, BLOCKS * PX_PER_BLOCK, ROWS);
  const out: number[] = [];
  for (let b = 0; b < BLOCKS; b++) {
    let sum = 0;
    let n = 0;
    for (let row = 0; row < ROWS; row++) {
      for (let dx = 1; dx <= 3; dx++) {
        const x = b * PX_PER_BLOCK + dx;
        const i = (row * BLOCKS * PX_PER_BLOCK + x) * 4;
        sum += 0.299 * data[i] + 0.587 * data[i + 1] + 0.114 * data[i + 2];
        n++;
      }
    }
    out.push(sum / n);
  }
  return out;
}

// 单次采样：解得出色码就返回延迟（ms），否则 null
function sampleOnce(video: HTMLVideoElement, ctx: CanvasRenderingContext2D): number | null {
  const vw = video.videoWidth;
  const vh = video.videoHeight;
  if (!vw || !vh) return null;
  const bandH = Math.max(1, Math.round(vh * BAND_HEIGHT));
  const bandY = Math.max(0, Math.min(vh - bandH, Math.round(vh * BAND_CENTER - bandH / 2)));
  ctx.drawImage(video, 0, bandY, vw, bandH, 0, 0, BLOCKS * PX_PER_BLOCK, ROWS);
  const ts = decode(readRow(ctx));
  if (ts === null) return null;
  let delay = (syncedNow() % WRAP) - ts;
  if (delay < 0) delay += WRAP; // 时间戳只有 32 位，跨越 2^32 ms 边界时补回来
  if (delay > MAX_DELAY_MS) return null;
  return delay;
}

function quantile(sorted: number[], q: number): number {
  if (sorted.length === 1) return sorted[0];
  const pos = (sorted.length - 1) * q;
  const lo = Math.floor(pos);
  const hi = Math.ceil(pos);
  return sorted[lo] + (sorted[hi] - sorted[lo]) * (pos - lo);
}

// 跑 seconds 秒，返回中位数/p95/样本数；一个都没解出（画面里没有标尺）返回 null。
// onSample 每次采样都回调（null = 这一帧没解出），给 UI 显示进度
export async function measure(
  video: HTMLVideoElement,
  seconds = 10,
  onSample?: (delayMs: number | null) => void,
): Promise<LatencyResult | null> {
  await ensureClock();
  if (!(await waitReady(video))) return null;
  const canvas = document.createElement('canvas');
  canvas.width = BLOCKS * PX_PER_BLOCK;
  canvas.height = ROWS;
  // willReadFrequently：每 150 ms 一次 getImageData，别让浏览器把画布留在 GPU 上来回搬
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  if (!ctx) return null;

  const delays: number[] = [];
  const deadline = Date.now() + seconds * 1000;
  while (Date.now() < deadline) {
    let d: number | null = null;
    try {
      d = sampleOnce(video, ctx);
    } catch {
      // 跨源画面会让 drawImage/getImageData 抛安全错误：当作解不出，别把整次测量炸掉
      d = null;
    }
    if (d !== null) delays.push(d);
    onSample?.(d);
    await new Promise((r) => window.setTimeout(r, SAMPLE_MS));
  }
  if (delays.length === 0) return null;
  const sorted = [...delays].sort((a, b) => a - b);
  return {
    median_ms: Math.round(quantile(sorted, 0.5)),
    p95_ms: Math.round(quantile(sorted, 0.95)),
    samples: sorted.length,
    clock_ms: Math.round((syncQuality() ?? 0) / 2),
  };
}
