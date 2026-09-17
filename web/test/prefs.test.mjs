import assert from 'node:assert/strict';
import test from 'node:test';
import { autoBitrate, autoBitrateMin, clampBitrateRange, defaultPrefs, loadPrefs } from './tmp/prefs.js';

// loadPrefs 只在调用时读 localStorage，桩一份就够
function withStored(obj, fn) {
  const raw = obj === null ? null : JSON.stringify(obj);
  globalThis.localStorage = { getItem: () => raw, setItem: () => {} };
  try {
    return fn();
  } finally {
    delete globalThis.localStorage;
  }
}

test('合法区间原样返回', () => {
  assert.deepEqual(clampBitrateRange(3.5, 8.7), { min: 3.5, max: 8.7 });
});

test('拖高下限把上限顶高（留 25% 余量）', () => {
  assert.deepEqual(clampBitrateRange(8, 8, 'min'), { min: 8, max: 10 });
  assert.deepEqual(clampBitrateRange(6, 5, 'min'), { min: 6, max: 7.5 });
});

test('拖低上限把下限压低', () => {
  assert.deepEqual(clampBitrateRange(6, 5, 'max'), { min: 4, max: 5 });
  assert.deepEqual(clampBitrateRange(6, 5), { min: 4, max: 5 }); // 默认锚上限
});

test('下限有绝对下界，且推挤后不会踩回禁区', () => {
  assert.deepEqual(clampBitrateRange(0.1, 5), { min: 0.5, max: 5 });
  const r = clampBitrateRange(0.1, 0.1);
  assert.equal(r.min, 0.5);
  assert.ok(r.min <= r.max * 0.8, `${r.min} / ${r.max} 余量不足`);
});

test('自动下限取上限的四成，且不低于绝对下界', () => {
  assert.equal(autoBitrateMin(autoBitrate('1080p', 60)), 3.5);
  assert.equal(autoBitrateMin(1), 0.5);
});

test('默认值本身满足约束', () => {
  const d = defaultPrefs();
  assert.deepEqual(clampBitrateRange(d.bitrateMin, d.bitrateMax), { min: d.bitrateMin, max: d.bitrateMax });
});

test('旧存档只有 bitrate：读成上限并补出下限', () => {
  const p = withStored({ bitrate: 6, bitrateAuto: false }, loadPrefs);
  assert.equal(p.bitrateMax, 6);
  assert.equal(p.bitrateMin, 2.4);
});

test('存档里的非法下限被收进区间，缺字段回落默认', () => {
  const fixed = withStored({ bitrateMin: 9, bitrateMax: 10 }, loadPrefs);
  assert.equal(fixed.bitrateMin, 8);
  assert.equal(fixed.bitrateMax, 10);
  const def = defaultPrefs();
  const p = withStored({ bitrateMin: 'x', bitrateMax: 99 }, loadPrefs);
  assert.equal(p.bitrateMax, def.bitrateMax);
  assert.equal(p.bitrateMin, def.bitrateMin);
});
