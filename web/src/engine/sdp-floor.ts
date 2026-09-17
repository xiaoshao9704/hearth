// 给投屏的会话描述补一个码率下限（x-google-min-bitrate）。
//
// 为什么需要：libwebrtc 的带宽估计只测「发布端 → SFU」这一段，跟观众侧无关；但它在跨网路径上
// 被一次抖动或零星丢包压下去之后没有底，会一路掉到几百 kbps 再也爬不回来——表现就是「投屏码率
// 上不去、内网却没事，同一条链路 OBS 固定码率很稳」。LiveKit 只写 x-google-start-bitrate
// （投屏不封顶，取目标的 90%），整个客户端里没有任何地方设下限。
//
// 下限是多少由用户定（设置里的「码率下限」），不是这里算出来的比例：顶得太高，真拥堵时就从
// 「画面变糊」变成「丢包花屏」，这个取舍只能交给看得见自己链路的人。未设定（0）时一个字节都不改。
//
// 只改投屏、不碰摄像头：LiveKit 给摄像头的起始码率封顶 1000 kbps，投屏那条不封顶，所以
// 「起始码率 > 1000」就是一个干净的判据，不必去猜哪条媒体段是投屏。
//
// 这是会话描述改写，属于绕过 SDK 的手段：一旦 LiveKit 自己支持设下限就该删掉这个文件。

/** 摄像头的起始码率被 LiveKit 封在 1000，超过这个数的只可能是投屏。 */
const SCREEN_START_MIN = 1000;

let installed = false;
let floorKbps = 0;

/**
 * 设定下一次协商要注入的下限（kbps）。0 或非法值 = 不注入，行为与没装这个补丁一致。
 * 只在发布投屏之前设：写进去的是会话描述，改完要重新协商才生效。
 */
export function setScreenBitrateFloorKbps(kbps: number): void {
  floorKbps = Number.isFinite(kbps) && kbps > 0 ? Math.round(kbps) : 0;
}

/** 把下限写进 fmtp 行；返回改写后的描述文本，没有可改的就原样返回。 */
export function addScreenBitrateFloor(sdp: string): string {
  if (floorKbps <= 0) return sdp;
  let touched = false;
  const out = sdp.split(/\r?\n/).map((line) => {
    if (!line.startsWith('a=fmtp:')) return line;
    const m = /x-google-start-bitrate=(\d+)/.exec(line);
    if (!m) return line;
    if (line.includes('x-google-min-bitrate')) return line;
    const start = Number(m[1]);
    if (!Number.isFinite(start) || start <= SCREEN_START_MIN) return line;
    touched = true;
    return `${line};x-google-min-bitrate=${floorKbps}`;
  });
  return touched ? out.join('\r\n') : sdp;
}

/**
 * 装一次全局补丁：LiveKit 自己建 RTCPeerConnection、自己调 setLocalDescription，
 * 没有留改写会话描述的口子，只能从原型上截。判据严格（见上），不会误伤别的连接。
 */
export function installScreenBitrateFloor(): void {
  if (installed || typeof RTCPeerConnection === 'undefined') return;
  installed = true;
  // 老式回调签名（success/failure）也要透传，所以用 rest 参数而不是固定形参
  const orig = RTCPeerConnection.prototype.setLocalDescription as (
    this: RTCPeerConnection,
    ...args: unknown[]
  ) => Promise<void>;
  const patched = function (this: RTCPeerConnection, ...args: unknown[]) {
    const desc = args[0] as RTCLocalSessionDescriptionInit | undefined;
    if (desc?.sdp) {
      const next = addScreenBitrateFloor(desc.sdp);
      if (next !== desc.sdp) args[0] = { ...desc, sdp: next };
    }
    return orig.apply(this, args);
  };
  RTCPeerConnection.prototype.setLocalDescription = patched as typeof RTCPeerConnection.prototype.setLocalDescription;
}
