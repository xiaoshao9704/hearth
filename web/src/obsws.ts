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

// ---- 在 OBS 里建采集源 ----
// 「投哪个应用/窗口」不由 hearth 枚举再替用户选，而是把源建出来后弹 OBS 自己的属性窗口，
// 让用户在 OBS 里选：obs-websocket 5.7.3 枚举 macOS screen_capture 的 application/window
// 会让 OBS 整个段错误（实测 OBS 32.1.2 两次复现，崩在插件内的 _platform_strlen）。
// 只认这三个固定名字：建、复用、清理都只针对它们，用户自己的场景与源一概不碰。
export const OBS_SCENE = 'Hearth 投屏';
export const OBS_VIDEO_INPUT = 'Hearth 画面';
export const OBS_AUDIO_INPUT = 'Hearth 声音';

export type ObsPlatform = 'windows' | 'macos' | 'other';

// GetVersion.platform 文档写明不保证是这几个值，认不出的一律当 other（功能不可用）
export function obsPlatform(raw: string | undefined): ObsPlatform {
  const s = String(raw ?? '').toLowerCase();
  return s === 'windows' || s === 'macos' ? s : 'other';
}

/**
 * 一个可采集的目标。value 原样来自属性列表的 itemValue：Windows 是串，macOS 窗口是数字。
 * 目前没有 UI 用它：见文件末尾「枚举目标（未启用）」一节。
 */
export type ObsTarget = { kind: 'app' | 'window'; label: string; value: string | number };

/** Windows 画面源两选一：独占全屏只有游戏捕获抓得到，抓不到的窗口退 WGC 窗口捕获 */
export type ObsWinMode = 'game' | 'window';

const WIN_VIDEO_GAME = 'game_capture';
const WIN_VIDEO_WINDOW = 'window_capture';
const WIN_AUDIO = 'wasapi_process_output_capture'; // OBS 28+ 的应用音频采集，按进程树取声
const MAC_VIDEO = 'screen_capture'; // OBS 30+ 的 macOS 屏幕采集（ScreenCaptureKit）
const MAC_AUDIO = 'sck_audio_capture'; // 同一族的应用音频采集，老 OBS 没有这个 kind
// screen_capture / sck_audio_capture 共用的 type 枚举：0=显示器 1=窗口 2=应用
const MAC_TYPE_WINDOW = 1;
const MAC_TYPE_APP = 2;
// window_capture 的 method：2 = WGC（Windows 10 2004 起），比 BitBlt 抓得到的窗口多
const WIN_METHOD_WGC = 2;

// 值只会是串/数/布尔：OBS 的源设置就这几种标量，也正好落在库的 JsonObject 里
export type ObsInputSpec = { inputKind: string; inputSettings: Record<string, string | number | boolean> };


/**
 * 画面源的 kind 与设置。target 为 null = 不预设目标，由用户在 OBS 的源属性窗口里选。
 * displayUuid 只对 macOS 的枚举路径有意义（见文件末尾「枚举目标（未启用）」）。
 */
export function obsVideoSpec(
  platform: ObsPlatform,
  mode: ObsWinMode,
  target: ObsTarget | null,
  displayUuid = '',
): ObsInputSpec {
  if (platform === 'windows') {
    const window: Record<string, string> = target ? { window: String(target.value) } : {};
    return mode === 'game'
      ? { inputKind: WIN_VIDEO_GAME, inputSettings: { capture_mode: 'window', ...window } }
      : { inputKind: WIN_VIDEO_WINDOW, inputSettings: { method: WIN_METHOD_WGC, ...window } };
  }
  const display: Record<string, string> = displayUuid ? { display_uuid: displayUuid } : {};
  if (!target) return { inputKind: MAC_VIDEO, inputSettings: { type: MAC_TYPE_APP, ...display } };
  return target.kind === 'app'
    ? { inputKind: MAC_VIDEO, inputSettings: { type: MAC_TYPE_APP, application: String(target.value), ...display } }
    : { inputKind: MAC_VIDEO, inputSettings: { type: MAC_TYPE_WINDOW, window: Number(target.value), ...display } };
}

/**
 * 声音源的 kind：一律用这个 kind 的默认设置建，取哪个应用的声同样交给 OBS 的属性窗口。
 * null = 这个平台没有能按应用取声的源，只能让用户自己加。
 */
export function obsAudioSetupSpec(platform: ObsPlatform): ObsInputSpec | null {
  if (platform === 'windows') return { inputKind: WIN_AUDIO, inputSettings: {} };
  // macOS 的 screen_capture 默认设置里没有任何「采集音频」布尔键（实测 OBS 32.1.2），
  // 声音只能另起一个 sck_audio_capture。
  if (platform === 'macos') return { inputKind: MAC_AUDIO, inputSettings: {} };
  return null;
}

const strField = (o: unknown, k: string): string => {
  const v = (o as Record<string, unknown> | null)?.[k];
  return typeof v === 'string' ? v : '';
};

/** 建或复用 Hearth 场景，并清掉里面由本功能建的那两个源（用户后来自己加的留着） */
export const OBS_CODE_NOT_FOUND = 600;
export const obsErrorCode = (e: unknown): number | undefined => (e as { code?: number } | null)?.code;

export async function ensureObsScene(c: ObsConn): Promise<void> {
  // 源名在 OBS 里是全局的：上一次中途崩溃可能留下不挂在任何场景里的同名源，
  // 按场景项找不到它、再建就撞「已存在」。所以按固定名直接删，不存在（600）不算错。
  for (const name of [OBS_VIDEO_INPUT, OBS_AUDIO_INPUT]) {
    try {
      await c.request('RemoveInput', { inputName: name });
    } catch (e) {
      if (obsErrorCode(e) !== OBS_CODE_NOT_FOUND) throw e;
    }
  }
  const list = await c.request('GetSceneList');
  if (!list.scenes.some((s) => strField(s, 'sceneName') === OBS_SCENE)) {
    await c.request('CreateScene', { sceneName: OBS_SCENE });
  }
}

/** 弹出 OBS 自己的源属性窗口（OBS 会跳到前台） */
export function openObsInputProperties(c: ObsConn, inputName: string): Promise<unknown> {
  return c.request('OpenInputPropertiesDialog', { inputName });
}

/** dialog=false 表示属性窗口没弹出来，用户得自己在 OBS 里双击源；note 是给用户看的补充说明 */
export type ObsSetupResult = { dialog: boolean; note: string };

/**
 * 建好场景与画面/声音两个源、切成当前场景，然后弹画面源的属性窗口让用户在 OBS 里选目标。
 * 不写直播服务设置、不开播：选目标是 OBS 那边的交互，得等用户选完才知道该不该推。
 */
export async function setupObsCapture(c: ObsConn, platform: ObsPlatform, mode: ObsWinMode): Promise<ObsSetupResult> {
  await ensureObsScene(c);
  const video = obsVideoSpec(platform, mode, null);
  await c.request('CreateInput', {
    sceneName: OBS_SCENE,
    inputName: OBS_VIDEO_INPUT,
    inputKind: video.inputKind,
    inputSettings: video.inputSettings,
  });
  const audio = obsAudioSetupSpec(platform);
  let note = '';
  if (!audio) {
    note = '这个平台没有能按应用取声的源，需要声音请在 OBS 里自己加一个音频采集。';
  } else {
    try {
      await c.request('CreateInput', {
        sceneName: OBS_SCENE,
        inputName: OBS_AUDIO_INPUT,
        inputKind: audio.inputKind,
        inputSettings: audio.inputSettings,
      });
    } catch (e) {
      // 声音是附加项，建不出来也不该挡住画面那条主路
      note = `画面源建好了，声音源没建成（${(e as Error).message}），需要声音请在 OBS 里手动加一个应用音频采集。`;
    }
  }
  await c.request('SetCurrentProgramScene', { sceneName: OBS_SCENE });
  try {
    await openObsInputProperties(c, OBS_VIDEO_INPUT);
  } catch (e) {
    // 老版本 obs-websocket 没有 OpenInputPropertiesDialog。源已经建好并切了场景，
    // 回滚等于把用户刚拿到的东西又拆掉，只把话说清楚、让他在 OBS 里自己双击。
    const why = (e as Error).message;
    return {
      dialog: false,
      note: `${note ? `${note} ` : ''}没能替你打开源属性窗口（${why}），请在 OBS 的「${OBS_SCENE}」场景里双击「${OBS_VIDEO_INPUT}」自己选要投的画面。`,
    };
  }
  return { dialog: true, note };
}

// ---- 枚举目标（未启用） ----
// 下面几个是「由 hearth 列出可采集的窗口/应用、替用户选」的老路。
// obs-websocket 5.7.3 枚举 macOS screen_capture 的 application/window 会让 OBS 段错误
// （两次复现；给 screen_capture 补 display_uuid 无效，OBS 日志仍是 Invalid target display ID），
// 所以 UI 一概不调用；Windows 侧没有真机验过，验过之前同样不启用。

/** 声音源的 kind 与设置；null = 这个平台/目标没有能按目标取声的源 */
export function obsAudioSpec(platform: ObsPlatform, target: ObsTarget): ObsInputSpec | null {
  if (platform === 'windows') return { inputKind: WIN_AUDIO, inputSettings: { window: String(target.value) } };
  // sck_audio_capture 只认应用、不认单个窗口
  if (platform !== 'macos' || target.kind !== 'app') return null;
  return { inputKind: MAC_AUDIO, inputSettings: { type: MAC_TYPE_APP, application: String(target.value) } };
}

const listProp = async (c: ObsConn, propertyName: string, kind: ObsTarget['kind']): Promise<ObsTarget[]> => {
  const r = await c.request('GetInputPropertiesListPropertyItems', { inputName: OBS_VIDEO_INPUT, propertyName });
  const out: ObsTarget[] = [];
  for (const raw of r.propertyItems) {
    const item = raw as { itemName?: unknown; itemValue?: unknown; itemEnabled?: unknown };
    if (item.itemEnabled === false) continue; // OBS 自己都点不了的项别摆出来
    const value = item.itemValue;
    if (typeof value !== 'string' && typeof value !== 'number') continue;
    if (value === '' || value === 0) continue; // 属性列表里的空占位项
    out.push({ kind, value, label: String(item.itemName ?? '') || String(value) });
  }
  return out;
};

/** screen_capture 自己的默认值里 display_uuid 是空串，从老 kind display_capture 的默认值借一个主显示器 */
async function macDisplayUuid(c: ObsConn): Promise<string> {
  try {
    const d = await c.request('GetInputDefaultSettings', { inputKind: 'display_capture' });
    const v = (d.defaultInputSettings as Record<string, unknown>).display_uuid;
    return typeof v === 'string' ? v : '';
  } catch {
    return '';
  }
}

/** 建好场景与画面源（目标还没选），返回 OBS 自己给的可选清单 */
export async function prepareObsCapture(c: ObsConn, platform: ObsPlatform, mode: ObsWinMode): Promise<ObsTarget[]> {
  await ensureObsScene(c);
  const spec = obsVideoSpec(platform, mode, null, platform === 'macos' ? await macDisplayUuid(c) : '');
  await c.request('CreateInput', {
    sceneName: OBS_SCENE,
    inputName: OBS_VIDEO_INPUT,
    inputKind: spec.inputKind,
    inputSettings: spec.inputSettings,
  });
  if (platform === 'windows') return listProp(c, 'window', 'window');
  return [...(await listProp(c, 'application', 'app')), ...(await listProp(c, 'window', 'window'))];
}

/**
 * 把选中的目标写进画面源、按需补一个声音源，并切成当前场景。
 * 返回值是给用户看的补充说明（空串 = 画面与声音都配好了）。
 */
export async function applyObsCapture(
  c: ObsConn,
  platform: ObsPlatform,
  mode: ObsWinMode,
  target: ObsTarget,
): Promise<string> {
  await c.request('SetInputSettings', {
    inputName: OBS_VIDEO_INPUT,
    inputSettings: obsVideoSpec(platform, mode, target).inputSettings,
  });
  const audio = obsAudioSpec(platform, target);
  let note = '';
  if (!audio) {
    note = '这个目标没有能自动配的声音源，需要声音请在 OBS 里自己加一个音频采集。';
  } else {
    try {
      await c.request('CreateInput', {
        sceneName: OBS_SCENE,
        inputName: OBS_AUDIO_INPUT,
        inputKind: audio.inputKind,
        inputSettings: audio.inputSettings,
      });
    } catch (e) {
      note = `画面配好了，声音源没建成（${(e as Error).message}），请在 OBS 里手动加一个应用音频采集。`;
    }
  }
  await c.request('SetCurrentProgramScene', { sceneName: OBS_SCENE });
  return note;
}
