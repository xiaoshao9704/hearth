// 色码编解码的单测：node 自带 test runner，跑 `npm test`（先把 code.ts 单独编到 test/tmp）。
// web 没有测试框架，只这一处纯函数值得单测，不为它引 vitest。
import assert from 'node:assert/strict';
import test from 'node:test';
import { BLOCKS, decode, encode } from './tmp/code.js';

// 把编码结果变成亮度采样（黑 = 16，白 = 240），模拟从视频帧读到的一行
const toSamples = (bits, black = 16, white = 240) => bits.map((b) => (b ? white : black));

test('往返：编码再解码回原值', () => {
  for (const ts of [0, 1, 1000, 0x7fffffff, 0xfffffffe, 0xffffffff, 1757000000000 % 0x100000000]) {
    const bits = encode(ts);
    assert.equal(bits.length, BLOCKS);
    assert.equal(decode(toSamples(bits)), ts);
  }
});

test('时间戳取模 2^32（回绕），小数与负数先规整', () => {
  assert.equal(decode(toSamples(encode(0x100000000 + 123))), 123);
  assert.equal(decode(toSamples(encode(123.7))), 123);
  assert.equal(decode(toSamples(encode(-1))), 0xffffffff);
});

test('亮度阈值自适应：整体偏暗、偏亮、低对比都能判读', () => {
  const ts = 0x0a5b3c7d;
  const bits = encode(ts);
  assert.equal(decode(toSamples(bits, 0, 60)), ts, '偏暗');
  assert.equal(decode(toSamples(bits, 190, 255)), ts, '偏亮');
  assert.equal(decode(toSamples(bits, 120, 152)), ts, '低对比但仍有 32 的差');
});

test('单块噪声：任一块翻转都被校验挡下（返回 null）', () => {
  const bits = encode(0x12345678);
  for (let i = 0; i < BLOCKS; i++) {
    const flipped = bits.slice();
    flipped[i] = !flipped[i];
    assert.equal(decode(toSamples(flipped)), null, `第 ${i} 块翻转应解不出`);
  }
});

test('没有标尺：纯色画面、对比不足、块数不对一律 null', () => {
  assert.equal(decode(new Array(BLOCKS).fill(128)), null, '纯色');
  assert.equal(decode(toSamples(encode(42), 100, 118)), null, '对比不足');
  assert.equal(decode(new Array(BLOCKS - 1).fill(0)), null, '块数不对');
  assert.equal(decode(toSamples(encode(42)).map((v, i) => (i === 0 ? NaN : v))), null, 'NaN');
});
