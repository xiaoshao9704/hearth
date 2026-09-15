// obs-websocket 5.x 的最小客户端：只覆盖握手（op 0 Hello → op 1 Identify → op 2 Identified）
// 与请求-响应（op 6 / op 7）两条路径，事件（op 5）一律丢弃。不引第三方库。
// 传输由外部注入：浏览器给 browserSocket，单测给假 socket 直接喂帧。

export const OBS_WS_DEFAULT_URL = 'ws://localhost:4455';
export const OBS_RPC_VERSION = 1;
// WHIP 输出是 OBS 30 才有的；低于此版本配了也推不动，提前说清楚
export const OBS_WHIP_MIN_MAJOR = 30;

export type ObsData = Record<string, unknown>;

// WebSocket 的最小子集：回调收的是已解好的文本/关闭码，假实现好写，也不让协议层碰 DOM 类型
export interface ObsSocketLike {
  send(text: string): void;
  close(): void;
  onopen: (() => void) | null;
  onclose: ((code: number, reason: string) => void) | null;
  onerror: (() => void) | null;
  onmessage: ((text: string) => void) | null;
}

export type ObsSocketFactory = (url: string) => ObsSocketLike;

export const browserSocket: ObsSocketFactory = (url) => {
  const ws = new WebSocket(url);
  const s: ObsSocketLike = {
    send: (text) => ws.send(text),
    close: () => ws.close(),
    onopen: null,
    onclose: null,
    onerror: null,
    onmessage: null,
  };
  ws.onopen = () => s.onopen?.();
  ws.onclose = (ev) => s.onclose?.(ev.code, ev.reason);
  ws.onerror = () => s.onerror?.();
  ws.onmessage = (ev) => s.onmessage?.(typeof ev.data === 'string' ? ev.data : '');
  return s;
};

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

const b64 = (buf: ArrayBuffer): string => {
  const bytes = new Uint8Array(buf);
  let s = '';
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s);
};

const sha256b64 = async (text: string): Promise<string> =>
  b64(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text)));

// 协议规定：base64(sha256(base64(sha256(密码 + salt)) + challenge))
export async function obsAuthString(password: string, salt: string, challenge: string): Promise<string> {
  return sha256b64((await sha256b64(password + salt)) + challenge);
}

// 码值取自 obs-websocket 的 WebSocketCloseCode，只挑用户能据此行动的几个翻译
const closeReason = (code: number, reason: string): string => {
  if (code === 4009) return 'OBS 拒绝认证：密码不对';
  if (code === 4010) return 'OBS 不支持该 RPC 版本，请升级 OBS 或 obs-websocket';
  if (code === 4011) return 'OBS 端注销了这个会话';
  if (code === 1006) return '连不上 OBS：地址不通，或「工具 → WebSocket 服务器设置」没启用';
  return `OBS 连接已断开（code ${code}${reason ? ` ${reason}` : ''}）`;
};

export class ObsConn {
  private readonly sock: ObsSocketLike;
  private readonly password: string;
  private readonly pending = new Map<string, { ok: (d: ObsData) => void; bad: (e: Error) => void }>();
  private seq = 0;
  private dead: Error | null = null;
  private ready!: { ok: () => void; bad: (e: Error) => void };
  /** 握手完成；失败时带中文原因 reject */
  readonly identified: Promise<void>;
  /** 非主动关闭的断开回调，只触发一次 */
  onLost: (() => void) | null = null;

  constructor(url: string, password: string, factory: ObsSocketFactory = browserSocket) {
    this.password = password;
    this.identified = new Promise<void>((ok, bad) => {
      this.ready = { ok: () => ok(), bad };
    });
    this.identified.catch(() => {}); // 调用方可能只 await request()，这里先兜住免得成未处理拒绝
    this.sock = factory(url);
    this.sock.onmessage = (text) => void this.onFrame(text);
    this.sock.onclose = (code, reason) => this.die(new Error(closeReason(code, reason)));
    this.sock.onerror = () => this.die(new Error('连不上 OBS：检查地址，以及 obs-websocket 是否已启用'));
  }

  get alive(): boolean {
    return this.dead === null;
  }

  private send(msg: { op: number; d: ObsData }): void {
    this.sock.send(JSON.stringify(msg));
  }

  private die(e: Error): void {
    if (this.dead) return;
    this.dead = e;
    this.ready.bad(e);
    for (const p of this.pending.values()) p.bad(e);
    this.pending.clear();
    const cb = this.onLost;
    this.onLost = null;
    cb?.();
  }

  private async onFrame(text: string): Promise<void> {
    let msg: { op?: number; d?: ObsData };
    try {
      msg = JSON.parse(text) as { op?: number; d?: ObsData };
    } catch {
      return;
    }
    const d = msg.d ?? {};
    if (msg.op === 0) {
      const auth = d.authentication as { challenge?: string; salt?: string } | undefined;
      const payload: ObsData = { rpcVersion: OBS_RPC_VERSION };
      if (auth?.challenge && auth?.salt) {
        if (!this.password) {
          this.die(new Error('OBS 的 WebSocket 服务器开了鉴权，请填写密码'));
          return;
        }
        payload.authentication = await obsAuthString(this.password, auth.salt, auth.challenge);
      }
      if (this.dead) return;
      this.send({ op: 1, d: payload });
      return;
    }
    if (msg.op === 2) {
      this.ready.ok();
      return;
    }
    if (msg.op === 7) {
      const id = String(d.requestId ?? '');
      const p = this.pending.get(id);
      if (!p) return;
      this.pending.delete(id);
      const st = (d.requestStatus ?? {}) as { result?: boolean; code?: number; comment?: string };
      if (st.result) p.ok((d.responseData ?? {}) as ObsData);
      else p.bad(new Error(`OBS 拒绝了 ${String(d.requestType ?? '请求')}（code ${st.code ?? '?'}）${st.comment ? `：${st.comment}` : ''}`));
    }
  }

  async request<T extends ObsData = ObsData>(requestType: string, requestData?: ObsData): Promise<T> {
    await this.identified;
    if (this.dead) throw this.dead;
    const requestId = `h${++this.seq}`;
    return new Promise<T>((ok, bad) => {
      this.pending.set(requestId, { ok: ok as (d: ObsData) => void, bad });
      try {
        this.send({ op: 6, d: { requestType, requestId, requestData: requestData ?? {} } });
      } catch (e) {
        this.pending.delete(requestId);
        bad(new Error(`发给 OBS 失败：${(e as Error).message}`));
      }
    });
  }

  close(): void {
    if (!this.dead) {
      this.dead = new Error('连接已关闭');
      this.ready.bad(this.dead);
      for (const p of this.pending.values()) p.bad(this.dead);
      this.pending.clear();
    }
    this.onLost = null;
    try {
      this.sock.close();
    } catch {
      /* 已经断了就算了 */
    }
  }
}

// 建连并等握手完成；超时统一收口在这里（协议层不设表）
export async function connectObs(
  url: string,
  password: string,
  factory: ObsSocketFactory = browserSocket,
  timeoutMs = 8000,
  allowPrivate = false,
): Promise<ObsConn> {
  const bad = checkObsWsUrl(url, allowPrivate);
  if (bad) throw new Error(bad);
  let conn: ObsConn;
  try {
    conn = new ObsConn(url.trim(), password, factory);
  } catch (e) {
    throw new Error(`连不上 OBS：${(e as Error).message}`);
  }
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_, rej) => {
    timer = setTimeout(
      () => rej(new Error('连接 OBS 超时：确认 OBS 已开启「工具 → WebSocket 服务器设置」并勾选启用')),
      timeoutMs,
    );
  });
  try {
    await Promise.race([conn.identified, timeout]);
  } catch (e) {
    conn.close();
    throw e as Error;
  } finally {
    clearTimeout(timer);
  }
  return conn;
}

export type ObsVersion = {
  obsVersion: string;
  obsWebSocketVersion: string;
  rpcVersion: number;
  platformDescription: string;
};

export type ObsStreamStatus = {
  outputActive: boolean;
  outputReconnecting: boolean;
  outputDuration: number; // ms
  outputBytes: number;
};

// "30.2.3" → 30；取不到主版本号返回 0（当作未知，不拿它去报「不支持」）
export function obsMajor(version: string): number {
  const n = Number.parseInt(String(version).split('.')[0] ?? '', 10);
  return Number.isFinite(n) ? n : 0;
}

// hearth 的 WHIP 入口喂给 OBS 的直播服务设置
export function whipServiceSettings(server: string, token: string): ObsData {
  return {
    streamServiceType: 'whip_custom',
    streamServiceSettings: { server, bearer_token: token },
  };
}
