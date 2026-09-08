// 与服务器对齐时钟：标尺页与观众页各自校准到同一个时间源，两端不必同机也能相减。
// 连发几次 `GET /api/time`，只认往返最小的那次（排队与重传都会让 rtt 变大、偏移变糟）。
import { fetchServerTime } from '../api';

let offset = 0; // syncedNow() = Date.now() + offset
let rttUsed = Number.POSITIVE_INFINITY;
let pending: Promise<void> | null = null;

export async function syncClock(rounds = 5): Promise<void> {
  let best = Number.POSITIVE_INFINITY;
  let bestOffset = 0;
  let lastErr: unknown = null;
  for (let i = 0; i < rounds; i++) {
    const t0 = Date.now();
    try {
      const serverNow = await fetchServerTime();
      const t1 = Date.now();
      const rtt = t1 - t0;
      if (rtt < best) {
        best = rtt;
        bestOffset = serverNow + rtt / 2 - t1;
      }
    } catch (err) {
      lastErr = err;
    }
  }
  if (!Number.isFinite(best)) throw lastErr ?? new Error('校准时钟失败');
  offset = bestOffset;
  rttUsed = best;
}

// 校准过就直接返回（同一页里多次测延迟不重复打服务器）；并发调用共用同一次校准
export function ensureClock(): Promise<void> {
  if (Number.isFinite(rttUsed)) return Promise.resolve();
  if (!pending) {
    pending = syncClock().finally(() => {
      pending = null;
    });
  }
  return pending;
}

export function syncedNow(): number {
  return Date.now() + offset;
}

// 用来校准的那次往返（ms）；还没校准过返回 null。误差界按它的一半展示
export function syncQuality(): number | null {
  return Number.isFinite(rttUsed) ? Math.round(rttUsed) : null;
}
