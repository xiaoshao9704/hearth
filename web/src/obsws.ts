// obs-websocket 5.x 客户端：握手、认证、请求配对全交给官方库 obs-websocket-js（json 子路径，
// 浏览器里走原生 WebSocket，不打包 msgpack 与 node 的 ws）。本模块只补库不管的三件事：
// 地址白名单、错误与超时收口成中文、hearth 用到的请求形状。
import OBSWebSocket, { OBSWebSocketError } from 'obs-websocket-js/json';
import type { OBSRequestTypes, OBSResponseTypes } from 'obs-websocket-js/json';

export const OBS_WS_DEFAULT_URL = 'ws://localhost:4455';
export const OBS_RPC_VERSION = 1;
// WHIP 输出是 OBS 30 才有的；低于此版本配了也推不动，提前说清楚
export const OBS_WHIP_MIN_MAJOR = 30;

const CONNECT_TIMEOUT_MS = 8000;
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
          throw new Error(callText(String(requestType), e));
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

// 建连并等握手完成；地址白名单与超时都收口在这里
export async function connectObs(
  url: string,
  password: string,
  allowPrivate = false,
  timeoutMs = CONNECT_TIMEOUT_MS,
): Promise<ObsConn> {
  const bad = checkObsWsUrl(url, allowPrivate);
  if (bad) throw new Error(bad);
  const ws = new OBSWebSocket();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, rej) => {
    timer = setTimeout(
      () => rej(new Error('连接 OBS 超时：确认 OBS 已开启「工具 → WebSocket 服务器设置」并勾选启用')),
      timeoutMs,
    );
  });
  timeout.catch(() => {});
  try {
    await Promise.race([ws.connect(url.trim(), password, { rpcVersion: OBS_RPC_VERSION }), timeout]);
  } catch (e) {
    void ws.disconnect().catch(() => {});
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
