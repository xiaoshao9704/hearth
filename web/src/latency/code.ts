// 端到端延迟标尺的色码：把一个 32 位毫秒时间戳编成一行 40 个黑白块，
// 从视频帧里再解回来。纯函数、不碰 DOM，可单测（见 web/test/code.test.mjs）。
//
// 排布（左→右）：
//   0..3   同步标记 黑白黑白：解码方靠它定阈值（黑块均值与白块均值的中点），
//          于是缩放、亮度、编码色偏都不影响判读，只要还分得出黑白。
//   4..35  数据 32 位，高位在前（黑 = 0，白 = 1）。
//   36..39 校验 4 位：32 位数据按 8 位分成 4 组全部异或得 1 字节，再把高低两个
//          半字节异或折成 4 位，高位在前。单块翻转必然改变其中一位，故一定被抓到。

export const BLOCKS = 40;
const SYNC = [false, true, false, true];
const DATA_BITS = 32;
const CHECK_BITS = 4;
// 同步黑块与白块的亮度差下限：低于此认为画面里没有标尺（纯色、糊掉、根本不是标尺）
const MIN_CONTRAST = 24;

function checkNibble(bits: boolean[]): boolean[] {
  let byte = 0;
  for (let group = 0; group < 4; group++) {
    let v = 0;
    for (let i = 0; i < 8; i++) v = v * 2 + (bits[group * 8 + i] ? 1 : 0);
    byte ^= v;
  }
  const nibble = ((byte >> 4) ^ byte) & 0xf;
  return [8, 4, 2, 1].map((mask) => (nibble & mask) !== 0);
}

// 把 32 位时间戳编成 40 块（true = 白）。ts 取模 2^32，负数与小数一律先规整
export function encode(ts: number): boolean[] {
  let v = Math.floor(ts) % 0x1_0000_0000;
  if (v < 0) v += 0x1_0000_0000;
  const data: boolean[] = [];
  for (let i = DATA_BITS - 1; i >= 0; i--) data.push(Math.floor(v / 2 ** i) % 2 === 1);
  return [...SYNC, ...data, ...checkNibble(data)];
}

// 从 40 个亮度值（0~255）解回时间戳；同步块对比不足或校验不过返回 null
export function decode(samples: number[]): number | null {
  if (samples.length !== BLOCKS) return null;
  if (samples.some((v) => !Number.isFinite(v))) return null;
  const dark = (samples[0] + samples[2]) / 2;
  const bright = (samples[1] + samples[3]) / 2;
  if (bright - dark < MIN_CONTRAST) return null;
  const threshold = (dark + bright) / 2;
  const bits = samples.map((v) => v > threshold);
  // 同步块自己也要对得上：阈值是拿它算的，对不上说明这四块不是黑白黑白
  for (let i = 0; i < SYNC.length; i++) if (bits[i] !== SYNC[i]) return null;
  const data = bits.slice(SYNC.length, SYNC.length + DATA_BITS);
  const check = bits.slice(SYNC.length + DATA_BITS, SYNC.length + DATA_BITS + CHECK_BITS);
  const want = checkNibble(data);
  for (let i = 0; i < CHECK_BITS; i++) if (check[i] !== want[i]) return null;
  let ts = 0;
  for (const bit of data) ts = ts * 2 + (bit ? 1 : 0);
  return ts;
}
