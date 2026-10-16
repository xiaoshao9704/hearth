// obs-websocket 5.x 客户端：握手、认证、请求配对全交给官方库 obs-websocket-js（json 子路径，
// 浏览器里走原生 WebSocket，不打包 msgpack 与 node 的 ws）。本模块只补库不管的三件事：
// 地址白名单、错误与超时收口成中文、hearth 用到的请求形状。
import OBSWebSocket, { OBSWebSocketError } from 'obs-websocket-js/json';
import type { OBSRequestTypes, OBSResponseTypes } from 'obs-websocket-js/json';

export const OBS_WS_DEFAULT_URL = 'ws://localhost:4455';
export const OBS_RPC_VERSION = 1;
// WHIP 输出是 OBS 30 才有的；低于此版本配了也推不动，提前说清楚
export const OBS_WHIP_MIN_MAJOR = 30;

export const CONNECT_TIMEOUT_MS = 8000;
// 浏览器的「允许本站访问本地网络」提示还挂着时的宽限超时：断开在途连接会把那个提示一起收走，
// 8 秒到点就断，用户看到「点允许后重试」时提示已经没了。挂住这条连接，点了允许握手会自己继续。
export const CONNECT_PROMPT_TIMEOUT_MS = 60000;
// 库在连接断开时不会拒掉在途请求，靠这条超时兜底（也兜 OBS 卡住不回的情况）
const REQUEST_TIMEOUT_MS = 10000;

// 只放行本机明文与 TLS：https 页面连非回环的 ws:// 会被混合内容拦掉，
// 而把 obs-websocket 的密码送去任意主机本身就越了「配置本机 OBS」的边界。
// 私网地址（10/8、172.16/12、192.168/16）：只有桌面壳允许明文连过去——浏览器里 https 页面
// 连局域网 ws:// 会被当混合内容拦下，放行了也连不上，不如直接说清楚。
const PRIVATE_V4 = /^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/;

export function checkObsWsUrl(raw: string, allowPrivate = false): string {
  const s = raw.trim();
  if (!s) return '请填写 obs-websocket 地址';
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return '地址格式不对，应形如 ws://localhost:4455';
  }
  if (u.protocol === 'wss:') return '';
  if (u.protocol !== 'ws:') return '地址只能以 ws:// 或 wss:// 开头';
  if (u.hostname === 'localhost' || u.hostname === '127.0.0.1') return '';
  if (allowPrivate && PRIVATE_V4.test(u.hostname)) return '';
  return allowPrivate
    ? '明文 ws:// 只允许连本机或局域网私网地址；连公网机器请用 wss://'
    : '浏览器里明文 ws:// 只允许连本机（localhost 或 127.0.0.1）；局域网里别的机器请用桌面端';
}

// 码值取自 obs-websocket 的 WebSocketCloseCode，只挑用户能据此行动的几个翻译。
// hasPassword 只影响 4009 的措辞：没填密码时说「要填密码」，填了才说「密码不对」。
const closeText = (code: number, reason: string, hasPassword: boolean): string => {
  if (code === 4009) return hasPassword ? 'OBS 拒绝认证：密码不对' : 'OBS 的 WebSocket 服务器开了鉴权，请填写密码';
  if (code === 4010) return 'OBS 不支持该 RPC 版本，请升级 OBS 或 obs-websocket';
  if (code === 4011) return 'OBS 端注销了这个会话';
  if (code === 1006) return '连不上 OBS：地址不通，或「工具 → WebSocket 服务器设置」没启用';
  return `OBS 连接已断开（code ${code}${reason ? ` ${reason}` : ''}）`;
};

const connectText = (e: unknown, hasPassword: boolean): string => {
  if (e instanceof OBSWebSocketError) return closeText(e.code, e.message, hasPassword);
  const msg = (e as Error | undefined)?.message ?? '';
  return msg || '连不上 OBS：检查地址，以及 obs-websocket 是否已启用';
};

// 请求失败：库把 requestStatus 的 code/comment 原样塞进 OBSWebSocketError，补上请求名给用户定位
const callText = (what: string, e: unknown): string => {
  if (e instanceof OBSWebSocketError) return `OBS 拒绝了 ${what}（code ${e.code}）${e.message ? `：${e.message}` : ''}`;
  return `${what} 失败：${(e as Error | undefined)?.message ?? '未知错误'}`;
};

// Chrome 142+ 把 https 页面访问 localhost/私网纳入 Local Network Access 权限：用户没点「允许」
// 之前 WebSocket 会一直停在 CONNECTING，表现就是我们这边超时。此时把人引去查 OBS 的设置是错的，
// 所以超时文案要按权限态分流。权限名是新的，不认识它的浏览器（以及 node）query 会抛，一律当 unknown。
export type LnaState = 'granted' | 'denied' | 'prompt' | 'unknown';

export async function localNetworkState(): Promise<LnaState> {
  try {
    const st = await navigator.permissions.query({ name: 'local-network-access' as PermissionName });
    return st.state === 'granted' || st.state === 'denied' || st.state === 'prompt' ? st.state : 'unknown';
  } catch {
    return 'unknown';
  }
}

const TIMEOUT_OBS = '连接 OBS 超时：确认 OBS 已开启「工具 → WebSocket 服务器设置」并勾选启用';

export function connectTimeoutText(lna: LnaState): string {
  if (lna === 'prompt')
    return '连接 OBS 超时：Chrome 正在询问是否允许本站访问本地网络，请在地址栏旁的提示里点「允许」后重试';
  if (lna === 'denied')
    return '连接 OBS 超时：Chrome 已拒绝本站访问本地网络，请在地址栏的站点设置里把「本地网络访问」改为允许后重试';
  return TIMEOUT_OBS;
}

export type ObsVersion = OBSResponseTypes['GetVersion'];
export type ObsStreamStatus = OBSResponseTypes['GetStreamStatus'];
export type ObsVideoSettings = OBSResponseTypes['GetVideoSettings'];

export class ObsConn {
  private readonly ws: OBSWebSocket;
  private readonly hasPassword: boolean;
  private dead: Error | null = null;
  // 在途请求的 reject：连接断了立刻拒掉，不等各自的超时
  private readonly waiters = new Set<(e: Error) => void>();
  /** 非主动关闭的断开回调，只触发一次 */
  onLost: (() => void) | null = null;

  constructor(ws: OBSWebSocket, hasPassword: boolean) {
    this.ws = ws;
    this.hasPassword = hasPassword;
    ws.on('ConnectionClosed', (e) => this.die(new Error(closeText(e.code, e.message, this.hasPassword))));
  }

  get alive(): boolean {
    return this.dead === null;
  }

  private die(e: Error): void {
    if (this.dead) return;
    this.dead = e;
    for (const bad of this.waiters) bad(e);
    this.waiters.clear();
    const cb = this.onLost;
    this.onLost = null;
    cb?.();
  }

  async request<T extends keyof OBSRequestTypes>(
    requestType: T,
    requestData?: OBSRequestTypes[T],
  ): Promise<OBSResponseTypes[T]> {
    if (this.dead) throw this.dead;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let bad!: (e: Error) => void;
    const aborted = new Promise<never>((_, rej) => {
      bad = rej;
      this.waiters.add(rej);
      timer = setTimeout(() => rej(new Error(`OBS 迟迟没有回应 ${String(requestType)}，请检查 OBS 是否卡住`)), REQUEST_TIMEOUT_MS);
    });
    aborted.catch(() => {}); // race 输掉的那一侧没人接，先兜住免得成未处理拒绝
    try {
      return await Promise.race([
        this.ws.call(requestType, requestData).catch((e: unknown) => {
          const err = new Error(callText(String(requestType), e)) as Error & { code?: number };
          err.code = (e as { code?: number })?.code; // 调用方按 OBS 的状态码分流（如 600 = 源不存在）
          throw err;
        }),
        aborted,
      ]);
    } finally {
      clearTimeout(timer);
      this.waiters.delete(bad);
    }
  }

  close(): void {
    this.onLost = null;
    this.die(new Error('连接已关闭'));
    void this.ws.disconnect().catch(() => {
      /* 已经断了就算了 */
    });
  }
}

export type ConnectObsOpts = {
  /** 常规超时；本地网络权限提示还挂着时改用 promptTimeoutMs */
  timeoutMs?: number;
  promptTimeoutMs?: number;
  /** 正在等用户点浏览器的权限提示（这期间不会主动断开）：UI 据此说清楚在等什么 */
  onWaiting?: (why: 'local-network') => void;
};

// 建连并等握手完成；地址白名单与超时都收口在这里
export async function connectObs(
  url: string,
  password: string,
  allowPrivate = false,
  opts: ConnectObsOpts = {},
): Promise<ObsConn> {
  const bad = checkObsWsUrl(url, allowPrivate);
  if (bad) throw new Error(bad);
  const ws = new OBSWebSocket();
  // 连接前先记一次权限态：发起连接本身会让 Chrome 弹提示，事后再查分不清「浏览器不认识这个权限」
  // 与「认识但还没批」。取不到新鲜结果时就用这一份兜底；它同时决定这次等多久。
  const before = await localNetworkState();
  const prompting = before === 'prompt';
  const limit = prompting ? (opts.promptTimeoutMs ?? CONNECT_PROMPT_TIMEOUT_MS) : (opts.timeoutMs ?? CONNECT_TIMEOUT_MS);
  if (prompting) opts.onWaiting?.('local-network');
  let timer: ReturnType<typeof setTimeout> | undefined;
  let timedOut = false;
  const timeout = new Promise<never>((_, rej) => {
    timer = setTimeout(() => {
      timedOut = true;
      rej(new Error(TIMEOUT_OBS));
    }, limit);
  });
  timeout.catch(() => {});
  try {
    await Promise.race([ws.connect(url.trim(), password, { rpcVersion: OBS_RPC_VERSION }), timeout]);
  } catch (e) {
    void ws.disconnect().catch(() => {});
    if (timedOut) {
      const now = await localNetworkState();
      throw new Error(connectTimeoutText(now === 'unknown' ? before : now));
    }
    throw new Error(connectText(e, password !== ''));
  } finally {
    clearTimeout(timer);
  }
  return new ObsConn(ws, password !== '');
}

// "30.2.3" → 30；取不到主版本号返回 0（当作未知，不拿它去报「不支持」）
export function obsMajor(version: string): number {
  const n = Number.parseInt(String(version).split('.')[0] ?? '', 10);
  return Number.isFinite(n) ? n : 0;
}

// hearth 的 WHIP 入口喂给 OBS 的直播服务设置
export function whipServiceSettings(server: string, token: string): OBSRequestTypes['SetStreamServiceSettings'] {
  return {
    streamServiceType: 'whip_custom',
    streamServiceSettings: { server, bearer_token: token },
  };
}

/**
 * 画质预设要写的 OBS 视频设置：画布（base）与输出（output）一起设成同一个分辨率。
 * 只改 output 的话，画布若比它小就是把小画面放大到大分辨率再编码，白费码率。
 * 手动改分辨率那条路仍只动 output——用户可能在画布上摆了自己的布局。
 */
export function obsPresetVideoSettings(width: number, height: number, fps: number): OBSRequestTypes['SetVideoSettings'] {
  return {
    baseWidth: width,
    baseHeight: height,
    outputWidth: width,
    outputHeight: height,
    fpsNumerator: fps,
    fpsDenominator: 1,
  };
}

// OBS 配置里的编码器 id → 人话；认不出的原样显示，别把没见过的编码器说成「未知」
export function encoderLabel(raw: string): string {
  const id = raw.trim();
  if (!id) return '';
  const low = id.toLowerCase();
  if (low === 'x264' || low === 'obs_x264') return '软件 H.264（x264）';
  if (low.includes('videotoolbox') || low.startsWith('apple_')) return 'VideoToolbox（Apple 硬件编码）';
  if (low.includes('nvenc')) return 'NVENC（NVIDIA 硬件编码）';
  if (low.includes('qsv')) return 'Intel QSV（硬件编码）';
  if (low.includes('amd') || low.includes('amf')) return 'AMD（硬件编码）';
  return id;
}

export type ObsPlatform = 'windows' | 'macos' | 'other';

// GetVersion.platform 文档写明不保证是这几个值，认不出的一律当 other（功能不可用）
export function obsPlatform(raw: string | undefined): ObsPlatform {
  const s = String(raw ?? '').toLowerCase();
  return s === 'windows' || s === 'macos' ? s : 'other';
}

/**
 * 简单输出模式下可选的流编码器（OBS 的 SimpleOutput.StreamEncoder 取值）。
 * OBS 没有「列出本机可用编码器」的请求，所以按平台给一份固定候选；机器没有对应显卡时
 * OBS 会在开播那一步报错，我们把它的原话显示出来，而不是在这里假装知道支不支持。
 */
export function obsEncoderChoices(platform: ObsPlatform): Array<{ value: string; label: string }> {
  if (platform === 'macos') {
    return [
      { value: 'apple_hevc', label: 'HEVC · VideoToolbox 硬件编码' },
      { value: 'apple_h264', label: 'H.264 · VideoToolbox 硬件编码' },
      { value: 'x264', label: 'H.264 · 软件编码（x264）' },
    ];
  }
  if (platform === 'windows') {
    return [
      { value: 'nvenc_hevc', label: 'HEVC · NVENC（NVIDIA）' },
      { value: 'nvenc', label: 'H.264 · NVENC（NVIDIA）' },
      { value: 'qsv_hevc', label: 'HEVC · Intel QSV' },
      { value: 'qsv', label: 'H.264 · Intel QSV' },
      { value: 'amd_hevc', label: 'HEVC · AMD' },
      { value: 'amd', label: 'H.264 · AMD' },
      { value: 'x264', label: 'H.264 · 软件编码（x264）' },
    ];
  }
  return [];
}
