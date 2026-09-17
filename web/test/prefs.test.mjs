import assert from 'node:assert/strict';
import test from 'node:test';
import {
  autoBitrate,
  autoBitrateMin,
  BITRATE_MIN_RATIO,
  bitrateSliderBounds,
  clampBitrateRange,
  defaultPrefs,
  loadPrefs,
} from './tmp/prefs.js';

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

test('自动推荐码率落在 0.5 的滑块步进上（否则 range 会把拇指夹到别处）', () => {
  // 1920*1080*60*0.07/1e6 ≈ 8.70912，最近的 0.5 刻度是 8.5
  assert.equal(autoBitrate('1080p', 60), 8.5);
  // 1280*720*30*0.07/1e6 ≈ 1.93536，最近的 0.5 刻度是 2
  assert.equal(autoBitrate('720p', 30), 2);
});

test('默认值本身满足约束', () => {
  const d = defaultPrefs();
  assert.deepEqual(clampBitrateRange(d.bitrateMin, d.bitrateMax), { min: d.bitrateMin, max: d.bitrateMax });
});

test('旧存档只有 bitrate：读成上限并补出下限', () => {
  const p = withStored({ bitrate: 6, bitrateAuto: false }, loadPrefs);
  assert.equal(p.bitrateMax, 6);
  // autoBitrateMin(6) = 6*0.4 = 2.4，对齐到 0.5 步进后是 2.5（不再是刻度外的 2.4）
  assert.equal(p.bitrateMin, 2.5);
});

test('旧存档里落在步进刻度外的值经 loadPrefs 后被对齐（下限下取整、上限上取整）', () => {
  const p = withStored({ bitrateMin: 3.4, bitrateMax: 8.7 }, loadPrefs);
  assert.equal(p.bitrateMin, 3);
  assert.equal(p.bitrateMax, 9);
  assert.equal(p.bitrateMin * 2, Math.round(p.bitrateMin * 2), `${p.bitrateMin} 不在 0.5 刻度上`);
  assert.equal(p.bitrateMax * 2, Math.round(p.bitrateMax * 2), `${p.bitrateMax} 不在 0.5 刻度上`);
  assert.ok(p.bitrateMin <= p.bitrateMax * BITRATE_MIN_RATIO, `${p.bitrateMin} / ${p.bitrateMax} 余量不足`);
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
  assert.deepEqual(bitrateSliderBounds(3.5, 8.7, LIM_1080), { minMax: 6.5, maxMin: 4.5, maxMinBy: 'range' }); // 边界对齐步进
  assert.deepEqual(bitrateSliderBounds(0.5, 1, LIM_1080), { minMax: 0.5, maxMin: 2.5, maxMinBy: 'floor' }); // 下界不低于绝对下界与建议区间下沿
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
  assert.deepEqual(bitrateSliderBounds(3.5, 8.7, LIM_720), { minMax: 6, maxMin: 4.5, maxMinBy: 'range' }); // 上限的档位比 1080p 窄
  assert.deepEqual(bitrateSliderBounds(12, 15, LIM_1080), { minMax: 12, maxMin: 15, maxMinBy: 'range' });
  assert.equal(bitrateSliderBounds(0.5, 0.5, LIM_1080).minMax, 0.5); // 再小也不低于绝对下界
});

test('上限见底的原因分得清：分辨率建议下界 vs 被下限顶住', () => {
  // 1080p 建议下界 2.5：下限已在绝对地板上，上限停在 2.5 与下限无关，提示得说是分辨率拦的
  const low = bitrateSliderBounds(0.5, 15, LIM_1080);
  assert.equal(low.maxMin, 2.5);
  assert.equal(low.maxMinBy, 'floor');
  // 下限抬到 4：上限见底 5 是下限除以 0.8 的结果，这时调低下限才有用
  const high = bitrateSliderBounds(4, 15, LIM_1080);
  assert.equal(high.maxMin, 5);
  assert.equal(high.maxMinBy, 'range');
  // 恰好相等（720p 建议下界 1，下限 0.5 → 0.5/0.8 进位也是 1）算「被下限顶住」
  const tie = bitrateSliderBounds(0.5, 6, LIM_720);
  assert.equal(tie.maxMin, 1);
  assert.equal(tie.maxMinBy, 'range');
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
