import assert from 'node:assert/strict';
import test from 'node:test';
import { autoBitrate, autoBitrateMin, bitrateSliderBounds, clampBitrateRange, defaultPrefs, loadPrefs } from './tmp/prefs.js';

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

const LIM_1080 = { min: 2.5, max: 15 };
const LIM_720 = { min: 1, max: 6 };

test('滑块边界：下限的可拖上界随上限收紧，上限的可拖下界随下限抬高', () => {
  assert.deepEqual(bitrateSliderBounds(3.5, 8.7, LIM_1080), { minMax: 6.5, maxMin: 4.5 }); // 边界对齐步进
  assert.deepEqual(bitrateSliderBounds(0.5, 1, LIM_1080), { minMax: 0.5, maxMin: 2.5 }); // 下界不低于绝对下界与建议区间下沿
});

test('滑块边界落在步进刻度上（min 属性歪了整条刻度都会移位）', () => {
  for (let max = 1; max <= 15; max += 0.5)
    for (const lim of [LIM_1080, LIM_720]) {
      const b = bitrateSliderBounds(1.5, max, lim);
      assert.equal(b.minMax * 2, Math.round(b.minMax * 2), `${b.minMax} 不在 0.5 刻度上`);
      assert.equal(b.maxMin * 2, Math.round(b.maxMin * 2), `${b.maxMin} 不在 0.5 刻度上`);
    }
});

test('滑块边界不越出建议区间与绝对下界', () => {
  assert.deepEqual(bitrateSliderBounds(3.5, 8.7, LIM_720), { minMax: 6, maxMin: 4.5 }); // 上限的档位比 1080p 窄
  assert.deepEqual(bitrateSliderBounds(12, 15, LIM_1080), { minMax: 12, maxMin: 15 });
  assert.equal(bitrateSliderBounds(0.5, 0.5, LIM_1080).minMax, 0.5); // 再小也不低于绝对下界
});

test('拖到边界上的值本身就是合法区间（界面拦住的正是 clamp 会推挤的那一步）', () => {
  for (let max = 1; max <= 15; max += 0.5) {
    const b = bitrateSliderBounds(1, max, LIM_1080);
    assert.deepEqual(clampBitrateRange(b.minMax, max, 'min'), { min: b.minMax, max }, `上限 ${max} 处下限顶格仍被推挤`);
  }
  for (let min = 0.5; min <= 12; min += 0.5) {
    const b = bitrateSliderBounds(min, 15, LIM_1080);
    assert.deepEqual(clampBitrateRange(min, b.maxMin, 'max'), { min, max: b.maxMin }, `下限 ${min} 处上限见底仍被推挤`);
  }
});

test('系统声音交还浏览器后：旧存档里的 screenAudio 只是被忽略', () => {
  assert.equal('screenAudio' in defaultPrefs(), false);
  const p = withStored({ screenAudio: false, bitrateMax: 6 }, loadPrefs);
  assert.equal('screenAudio' in p, false);
  assert.equal(p.bitrateMax, 6); // 其余字段照常读出来
});
