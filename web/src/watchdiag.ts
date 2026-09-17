// 观看诊断（默认关）：观众侧投屏轨的秒级采样，用来定位「画面阶段性 0 帧、卡一下再恢复」。
// 60 秒一条的 line_stats 采样太粗，几秒的冻结落不进任何一条读数里。
// 开关在设置的「投屏画质」，关着时房间一次 getStats 都不多调。
//
// 本文件只做「累计值 → 区间增量」与上报节流判定，不碰 DOM（除两个 localStorage 存取），
// 纯函数部分有 node 单测。
import type { WatchCounters } from './engine/types';

const WATCH_DIAG_KEY = 'hearth_watch_diag';

export function watchDiagOn(): boolean {
  try {
    return localStorage.getItem(WATCH_DIAG_KEY) === '1';
  } catch {
    return false; // 隐私模式下读不到：按关处理
  }
}

export function setWatchDiag(on: boolean): void {
  try {
    if (on) localStorage.setItem(WATCH_DIAG_KEY, '1');
    else localStorage.removeItem(WATCH_DIAG_KEY);
  } catch {
    // 存不下就只在本次会话生效，不值得打扰用户
  }
}

// 相邻两次采样的差分结果：上报与本地面板共用这一份，不各算一遍
export interface WatchSample {
  ms: number; // 采样区间时长
  fps: number; // 区间解码帧率（framesDecoded 差分）
  freezes: number; // 区间冻结次数
  freeze_ms: number; // 区间冻结总时长
  keyframes: number; // 区间解码的关键帧数
  lost: number; // 区间丢包数
  pli: number; // 区间关键帧请求数
  nack: number; // 区间重传请求数
  kbps: number; // 区间码率
  width: number;
  height: number;
  loss_pct?: number; // 区间丢包率：区间内既没收到也没丢时无意义，不给值
  jitter_ms?: number; // 瞬时值，不差分
  jb_ms?: number; // 区间平均抖动缓冲驻留（累计值除累计帧会被开局那几帧长期拖住）
  rtt_ms?: number;
  local?: string; // 选中候选对的本地端点，只有 `协议/候选类型`——按隐私要求不带地址
  remote?: string;
}

// 进房以来的累计：本地面板显示这一份，让用户不必等服务端日志
export interface WatchTotals {
  samples: number;
  freezes: number;
  freeze_ms: number;
  keyframes: number;
  lost: number;
  pli: number;
  nack: number;
}

export function emptyWatchTotals(): WatchTotals {
  return { samples: 0, freezes: 0, freeze_ms: 0, keyframes: 0, lost: 0, pli: 0, nack: 0 };
}

const delta = (cur: number | undefined, prev: number | undefined): number => {
  if (cur === undefined || prev === undefined) return 0;
  return Math.max(0, cur - prev); // 轨重订阅后计数器会归零，负增量按 0 计
};

const round1 = (v: number): number => Math.round(v * 10) / 10;

// 两次采样求区间增量；时间没走（同一份快照、或时钟没动）返回 null，交给调用方跳过本次
export function diffWatch(prev: WatchCounters, cur: WatchCounters): WatchSample | null {
  const ms = cur.at - prev.at;
  if (!(ms > 0)) return null;
  const frames = delta(cur.framesDecoded, prev.framesDecoded);
  const lost = delta(cur.packetsLost, prev.packetsLost);
  const recv = delta(cur.packetsReceived, prev.packetsReceived);
  const jbDelay = delta(cur.jitterBufferDelay, prev.jitterBufferDelay);
  const jbCount = delta(cur.jitterBufferEmittedCount, prev.jitterBufferEmittedCount);
  const out: WatchSample = {
    ms: Math.round(ms),
    fps: round1((frames * 1000) / ms),
    freezes: delta(cur.freezeCount, prev.freezeCount),
    freeze_ms: Math.round(delta(cur.totalFreezesDuration, prev.totalFreezesDuration) * 1000),
    keyframes: delta(cur.keyFramesDecoded, prev.keyFramesDecoded),
    lost,
    pli: delta(cur.pliCount, prev.pliCount),
    nack: delta(cur.nackCount, prev.nackCount),
    kbps: Math.round((delta(cur.bytesReceived, prev.bytesReceived) * 8) / ms),
    width: cur.frameWidth ?? 0,
    height: cur.frameHeight ?? 0,
  };
  if (lost + recv > 0) out.loss_pct = round1((lost / (lost + recv)) * 100);
  if (cur.jitter !== undefined) out.jitter_ms = round1(cur.jitter * 1000);
  if (jbCount > 0) out.jb_ms = round1((jbDelay / jbCount) * 1000);
  if (cur.rtt !== undefined) out.rtt_ms = Math.round(cur.rtt * 1000);
  if (cur.local) out.local = cur.local;
  if (cur.remote) out.remote = cur.remote;
  return out;
}

export function addWatch(t: WatchTotals, s: WatchSample): WatchTotals {
  return {
    samples: t.samples + 1,
    freezes: t.freezes + s.freezes,
    freeze_ms: t.freeze_ms + s.freeze_ms,
    keyframes: t.keyframes + s.keyframes,
    lost: t.lost + s.lost,
    pli: t.pli + s.pli,
    nack: t.nack + s.nack,
  };
}

// 值得留痕的一次采样：冻结、丢包、或对端被要了关键帧（PLI）——三者都是卡顿的直接证据
export function watchTrouble(s: WatchSample): boolean {
  return s.freezes > 0 || s.lost > 0 || s.pli > 0;
}

export function watchLevel(s: WatchSample): 'info' | 'warn' {
  return s.freezes > 0 || s.lost > 0 ? 'warn' : 'info';
}

export const WATCH_REPORT_INTERVAL_MS = 30000;

// 上报节流：出事就立刻发，平稳时每 30 秒发一条当基线，其余采样只在本地累计
export function shouldReportWatch(s: WatchSample, sinceLastReportMs: number, intervalMs = WATCH_REPORT_INTERVAL_MS): boolean {
  return watchTrouble(s) || sinceLastReportMs >= intervalMs;
}
