import assert from 'node:assert/strict';
import test from 'node:test';
import { addWatch, diffWatch, emptyWatchTotals, shouldReportWatch, watchLevel, watchTrouble } from './tmp/watchdiag.js';

const base = {
  at: 1000,
  framesDecoded: 100,
  keyFramesDecoded: 2,
  freezeCount: 0,
  totalFreezesDuration: 0,
  packetsLost: 10,
  packetsReceived: 990,
  pliCount: 1,
  nackCount: 3,
  jitter: 0.004,
  frameWidth: 1920,
  frameHeight: 1080,
  bytesReceived: 100000,
  jitterBufferDelay: 1,
  jitterBufferEmittedCount: 100,
};

test('增量：帧率/码率按区间时长换算，冻结与丢包取差分', () => {
  const s = diffWatch(base, {
    ...base,
    at: 6000, // 5 秒
    framesDecoded: 250, // 150 帧 → 30fps
    keyFramesDecoded: 4,
    freezeCount: 2,
    totalFreezesDuration: 1.25,
    packetsLost: 15,
    packetsReceived: 1490,
    pliCount: 3,
    nackCount: 9,
    bytesReceived: 100000 + 625000, // 5MBit / 5s → 1000 kbps
    jitterBufferDelay: 1 + 15,
    jitterBufferEmittedCount: 250,
  });
  assert.equal(s.ms, 5000);
  assert.equal(s.fps, 30);
  assert.equal(s.freezes, 2);
  assert.equal(s.freeze_ms, 1250);
  assert.equal(s.keyframes, 2);
  assert.equal(s.lost, 5);
  assert.equal(s.pli, 2);
  assert.equal(s.nack, 6);
  assert.equal(s.kbps, 1000);
  assert.equal(s.loss_pct, 1); // 5 丢 / (5 丢 + 495 收)
  assert.equal(s.jitter_ms, 4);
  assert.equal(s.jb_ms, 100); // 15s / 150 帧
  assert.equal(s.width, 1920);
  assert.equal(s.height, 1080);
});

test('时间没走返回 null，重订阅导致计数器归零按 0 增量', () => {
  assert.equal(diffWatch(base, { ...base }), null);
  assert.equal(diffWatch(base, { ...base, at: 500 }), null);
  const s = diffWatch(base, { ...base, at: 6000, framesDecoded: 5, packetsLost: 0, packetsReceived: 5 });
  assert.equal(s.freezes, 0);
  assert.equal(s.lost, 0);
  assert.equal(s.fps, 0); // 计数器归零那一次不产出虚假帧率
});

test('浏览器缺字段时不报 NaN，缺失项按 0 / 不给值', () => {
  const s = diffWatch({ at: 1000 }, { at: 6000 });
  assert.equal(s.fps, 0);
  assert.equal(s.freezes, 0);
  assert.equal(s.kbps, 0);
  assert.equal(s.loss_pct, undefined);
  assert.equal(s.jitter_ms, undefined);
  assert.equal(s.jb_ms, undefined);
  assert.equal(s.rtt_ms, undefined);
});

test('候选只带协议与类型，RTT 秒转毫秒', () => {
  const s = diffWatch(base, { ...base, at: 6000, rtt: 0.123, local: 'udp/srflx', remote: 'udp/prflx' });
  assert.equal(s.rtt_ms, 123);
  assert.equal(s.local, 'udp/srflx');
  assert.equal(s.remote, 'udp/prflx');
});

const sample = (over = {}) => ({
  ms: 5000, fps: 30, freezes: 0, freeze_ms: 0, keyframes: 0, lost: 0, pli: 0, nack: 0,
  kbps: 1000, width: 1920, height: 1080, ...over,
});

test('节流：出事立刻发，平稳时满 30 秒才发', () => {
  assert.equal(shouldReportWatch(sample(), 5000), false);
  assert.equal(shouldReportWatch(sample(), 29999), false);
  assert.equal(shouldReportWatch(sample(), 30000), true);
  assert.equal(shouldReportWatch(sample({ freezes: 1 }), 0), true);
  assert.equal(shouldReportWatch(sample({ lost: 1 }), 0), true);
  assert.equal(shouldReportWatch(sample({ pli: 1 }), 0), true);
  // 解码到关键帧本身不算事故（开局/换层都会有），不触发上报
  assert.equal(shouldReportWatch(sample({ keyframes: 3 }), 0), false);
});

test('级别：有冻结或丢包 warn，其余 info', () => {
  assert.equal(watchLevel(sample()), 'info');
  assert.equal(watchLevel(sample({ pli: 2 })), 'info');
  assert.equal(watchLevel(sample({ freezes: 1 })), 'warn');
  assert.equal(watchLevel(sample({ lost: 4 })), 'warn');
  assert.equal(watchTrouble(sample({ pli: 1 })), true);
  assert.equal(watchTrouble(sample()), false);
});

test('累计：本地面板的总数逐次相加', () => {
  let t = emptyWatchTotals();
  t = addWatch(t, sample({ freezes: 1, freeze_ms: 300, keyframes: 2, lost: 5, pli: 1, nack: 2 }));
  t = addWatch(t, sample({ freezes: 2, freeze_ms: 700, keyframes: 1, lost: 3, pli: 0, nack: 1 }));
  assert.deepEqual(t, { samples: 2, freezes: 3, freeze_ms: 1000, keyframes: 3, lost: 8, pli: 1, nack: 3 });
});
