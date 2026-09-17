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

// ---- 在 OBS 里建采集源 ----
// 「投哪个应用/窗口」有三条路，优先级与可见条件写在 obs-capture.tsx 的 ObsCaptureSection：
// 桌面壳给的清单（listObsTargets，应用级）、问 OBS 要窗口清单（listObsWindows，只有 macOS 安全）、
// 以及退路「建好源后弹 OBS 自己的属性窗口，让用户在 OBS 里选」。
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
 * 一个可采集的目标。value 原样写进源设置：Windows 是 OBS 的 window 串，
 * macOS 应用是 bundle id、窗口是 CGWindowID（数字）。
 * 清单来自桌面壳（bridge 的 listObsTargets）或 OBS 自己（listObsWindows）。
 */
export type ObsTarget = { kind: 'app' | 'window'; label: string; value: string | number };

/** Windows 画面源两选一：独占全屏只有游戏捕获抓得到，抓不到的窗口退 WGC 窗口捕获 */
export type ObsWinMode = 'game' | 'window';

const WIN_VIDEO_GAME = 'game_capture';
const WIN_VIDEO_WINDOW = 'window_capture';
const WIN_AUDIO = 'wasapi_process_output_capture'; // OBS 28+ 的应用音频采集，按进程树取声
// OBS 30+ 的 macOS 屏幕采集（ScreenCaptureKit）；macOS 13+ 起它固定连所属应用的声音一起采
const MAC_VIDEO = 'screen_capture';
const MAC_DISPLAY_KIND = 'display_capture';
// screen_capture 的 type 枚举：0=显示器 1=窗口 2=应用
const MAC_TYPE_WINDOW = 1;
const MAC_TYPE_APP = 2;
// window_capture 的 method：2 = WGC（Windows 10 2004 起），比 BitBlt 抓得到的窗口多
const WIN_METHOD_WGC = 2;

// 值只会是串/数/布尔：OBS 的源设置就这几种标量，也正好落在库的 JsonObject 里
export type ObsInputSpec = { inputKind: string; inputSettings: Record<string, string | number | boolean> };


/** 画面源的 kind 与设置。target 为 null = 不预设目标，由用户在 OBS 的源属性窗口里选。 */
/**
 * macOS 的 screen_capture 即便是「按应用」采集，底层 SCStream 仍要绑一块显示器：
 * 源码里应用分支同样走 `display.displayID == sc->display` 的查找，display_uuid 留空
 * 就报 `init_screen_stream: Invalid target display ID: 0`，源一帧都不出（实测）。
 * OBS 自己的属性窗口会把它填上，我们直写设置就得自己带。
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
 * null = 这个平台不需要（或没有）单独的声音源，UI 据此不摆「选声音来源」。
 */
export function obsAudioSetupSpec(platform: ObsPlatform): ObsInputSpec | null {
  if (platform === 'windows') return { inputKind: WIN_AUDIO, inputSettings: {} };
  // macOS 13+ 的 screen_capture 固定把所属应用的声音一起采（OBS 源码里 setCapturesAudio:YES，
  // 没有开关键），再建一个 sck_audio_capture 等于把同一份声音采两遍。
  return null;
}

const strField = (o: unknown, k: string): string => {
  const v = (o as Record<string, unknown> | null)?.[k];
  return typeof v === 'string' ? v : '';
};

export const OBS_CODE_NOT_FOUND = 600;
const OBS_CODE_NAME_TAKEN = 601;
export const obsErrorCode = (e: unknown): number | undefined => (e as { code?: number } | null)?.code;

/** 建或复用 Hearth 场景（用户自己的场景一概不碰） */
export async function ensureObsScene(c: ObsConn): Promise<void> {
  const list = await c.request('GetSceneList');
  if (!list.scenes.some((s) => strField(s, 'sceneName') === OBS_SCENE)) {
    await c.request('CreateScene', { sceneName: OBS_SCENE });
  }
}

/** 按固定名删源，不存在（600）不算错 */
async function dropObsInput(c: ObsConn, inputName: string): Promise<void> {
  try {
    await c.request('RemoveInput', { inputName });
  } catch (e) {
    if (obsErrorCode(e) !== OBS_CODE_NOT_FOUND) throw e;
  }
}

/** 取源的 kind；源不存在返回空串 */
async function obsInputKind(c: ObsConn, inputName: string): Promise<string> {
  try {
    return (await c.request('GetInputSettings', { inputName })).inputKind;
  } catch (e) {
    if (obsErrorCode(e) !== OBS_CODE_NOT_FOUND) throw e;
    return '';
  }
}

/** 复用现成的同 kind 源：改设置、必要时挂回场景。挂不回去（死源）返回 false */
async function reuseObsInput(c: ObsConn, inputName: string, spec: ObsInputSpec): Promise<boolean> {
  // overlay=false：整份换掉，别让上一个目标的键留在里面
  await c.request('SetInputSettings', { inputName, inputSettings: spec.inputSettings, overlay: false });
  const items = await c.request('GetSceneItemList', { sceneName: OBS_SCENE });
  if (items.sceneItems.some((i) => strField(i, 'sourceName') === inputName)) return true;
  try {
    await c.request('CreateSceneItem', { sceneName: OBS_SCENE, sourceName: inputName });
    return true;
  } catch {
    return false;
  }
}

/** 名字被一个删不掉的死源占着时，把那个源改名让路（它在 OBS 界面里看不见，重启即消失） */
async function renameObsInputAway(c: ObsConn, inputName: string): Promise<void> {
  for (let i = 1; i <= 9; i++) {
    try {
      await c.request('SetInputName', { inputName, newInputName: `${inputName}-已失效${i}` });
      return;
    } catch (e) {
      if (obsErrorCode(e) !== OBS_CODE_NAME_TAKEN) throw e;
    }
  }
  throw new Error(`OBS 里「${inputName}」这个名字被一个已失效的源占着，腾不出来，请重启 OBS 后再试。`);
}

/**
 * 把一个固定名的源摆进 Hearth 场景：同 kind 的现成源直接复用（改设置 + 必要时挂回场景），
 * kind 变了（Windows 在游戏捕获/窗口捕获之间切）才删了重建。
 *
 * 为什么不一律删了重建：macOS 的 screen_capture 销毁是异步的（SCK 流要先停下来），实测
 * RemoveInput 返回后名字还占着好几秒、偶尔一直占着（那个源同时也挂不回场景了）。删了就建
 * 必撞 601，所以能复用就复用；真撞上了就把占名的死源改名让路。
 */
async function putObsInput(c: ObsConn, inputName: string, spec: ObsInputSpec): Promise<void> {
  const kind = await obsInputKind(c, inputName);
  if (kind === spec.inputKind && (await reuseObsInput(c, inputName, spec))) return;
  if (kind) await dropObsInput(c, inputName);
  const create = () =>
    c.request('CreateInput', {
      sceneName: OBS_SCENE,
      inputName,
      inputKind: spec.inputKind,
      inputSettings: spec.inputSettings,
    });
  try {
    await create();
  } catch (e) {
    if (obsErrorCode(e) !== OBS_CODE_NAME_TAKEN) throw e;
    await renameObsInputAway(c, inputName);
    await create();
  }
}

/** 弹出 OBS 自己的源属性窗口（OBS 会跳到前台） */
export function openObsInputProperties(c: ObsConn, inputName: string): Promise<unknown> {
  return c.request('OpenInputPropertiesDialog', { inputName });
}

/** dialog=false 表示属性窗口没弹出来，用户得自己在 OBS 里双击源；note 是给用户看的补充说明 */
export type ObsSetupResult = { dialog: boolean; note: string };

/**
 * 建好场景与源（Windows 画面 + 声音两个，macOS 只有画面）、切成当前场景。
 * openDialog 时再弹画面源的属性窗口让用户在 OBS 里选目标——浏览器里没有壳能列清单，
 * 只能这样；这一步不写直播服务设置、不开播，得等用户在 OBS 里选完才知道该不该推。
 */
/** 主显示器的 UUID：screen_capture 的默认设置里这项是空的，从 display_capture 的默认值借一个 */
async function macDisplayUuid(c: ObsConn, platform: ObsPlatform): Promise<string> {
  if (platform !== 'macos') return '';
  try {
    const r = await c.request('GetInputDefaultSettings', { inputKind: MAC_DISPLAY_KIND });
    const v = (r.defaultInputSettings as Record<string, unknown>).display_uuid;
    return typeof v === 'string' ? v : '';
  } catch {
    return ''; // 借不到就照旧写，至少不比现在差
  }
}

export async function setupObsCapture(
  c: ObsConn,
  platform: ObsPlatform,
  mode: ObsWinMode,
  openDialog = true,
): Promise<ObsSetupResult> {
  await ensureObsScene(c);
  const uuid = await macDisplayUuid(c, platform);
  await putObsInput(c, OBS_VIDEO_INPUT, obsVideoSpec(platform, mode, null, uuid));
  const audio = obsAudioSetupSpec(platform);
  let note = '';
  if (!audio) {
    // 老场景里可能留着上一版建的声音源，macOS 现在一个画面源就带声音，那个得清掉
    await dropObsInput(c, OBS_AUDIO_INPUT);
    if (platform !== 'macos') note = '这个平台没有能按应用取声的源，需要声音请在 OBS 里自己加一个音频采集。';
  } else {
    try {
      await putObsInput(c, OBS_AUDIO_INPUT, audio);
    } catch (e) {
      // 声音是附加项，建不出来也不该挡住画面那条主路
      note = `画面源建好了，声音源没建成（${(e as Error).message}），需要声音请在 OBS 里手动加一个应用音频采集。`;
    }
  }
  await c.request('SetCurrentProgramScene', { sceneName: OBS_SCENE });
  // 壳里目标是选好了才来的，弹属性窗口只会把 OBS 拉到前台挡住人
  if (!openDialog) return { dialog: false, note };
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

/**
 * macOS 的窗口条目名形如 `[应用名] 窗口标题`，拆出应用名好按应用分组：SCK 的声音是按应用过滤的，
 * 用户真正要选的是「投哪个应用」，窗口只是选中它的手段，同一应用多个窗口也该一眼可辨。
 * 认不出这个前缀的条目不猜：app 留空、标题原样返回，界面照原样列出来，不丢条目。
 */
export function splitObsWindowLabel(label: string): { app: string; title: string } {
  const m = /^\[([^\]]+)\]\s*(.*)$/.exec(label);
  const app = m ? m[1].trim() : '';
  if (!m || !app) return { app: '', title: label };
  return { app, title: m[2].trim() || '无标题窗口' };
}

/**
 * 问 OBS 要一份它自己看得到的窗口清单（只有 macOS 走这条，别的平台一律返回空数组）。
 *
 * 只枚举 screen_capture 的 `window` 属性：它是整数格式的列表，占位项是 `{itemName: ' ', itemValue: 0}`，
 * 实测安全（OBS 不崩；屏幕录制授权正常时能列出十几项，授权失效就只剩那个占位项）。
 * 同一个请求打到 `application` 或 `display_uuid` 会让 OBS 整个
 * 段错误——那两个是字符串格式的列表，占位项的值是空指针，obs-websocket 5.7.3/5.7.4 没判空
 * （已提上游 PR https://github.com/obsproject/obs-websocket/pull/1355）。**任何情况下都不要枚举那两个属性。**
 * Windows 的 window_capture/game_capture 的 `window` 同样是字符串格式的列表，含不含空指针占位项没验过，
 * 所以那边不走这条，退回壳的清单或「到 OBS 里自己选」。
 *
 * 这个请求走的是 obs_source_properties()，与打开源属性窗口同一条回调：macOS 上 SCK 的可共享内容列表
 * 就在这时重建，所以列过窗口之后写设置的源能出帧，不必再弹一次属性窗口（见 obsCaptureToTarget）。
 */
export async function listObsWindows(c: ObsConn, platform: ObsPlatform): Promise<ObsTarget[]> {
  if (platform !== 'macos') return [];
  // 属性清单挂在源上，源得先在；但列个清单不该把 OBS 拉到前台，所以不切场景、不弹窗。
  // mode 在 macOS 上不影响 spec（那是 Windows 的游戏/窗口捕获二选一），随便给一个。
  await ensureObsScene(c);
  await putObsInput(c, OBS_VIDEO_INPUT, obsVideoSpec(platform, 'game', null, await macDisplayUuid(c, platform)));
  const r = await c.request('GetInputPropertiesListPropertyItems', {
    inputName: OBS_VIDEO_INPUT,
    propertyName: 'window',
  });
  const out: ObsTarget[] = [];
  for (const raw of (r.propertyItems ?? []) as Array<Record<string, unknown> | null>) {
    if (raw?.itemEnabled === false) continue;
    const label = typeof raw?.itemName === 'string' ? raw.itemName.trim() : '';
    const value = Number(raw?.itemValue);
    // itemValue 为 0/非数字、或名字是空白的，都是 OBS 给「未选择」留的占位项，不给用户看
    if (!label || !Number.isFinite(value) || value === 0) continue;
    out.push({ kind: 'window', label, value });
  }
  return out;
}

// ---- 选中即开播 ----
// 目标已经从清单里选好（壳的 listObsTargets 或上面的 listObsWindows），下面把它写进源就能直接开播，
// 不用再弹 OBS 的属性窗口。枚举属性清单只许打 `window` 这一个属性，理由见 listObsWindows。

/** 声音源的 kind 与设置；null = 这个平台不用单独的声音源（见 obsAudioSetupSpec） */
export function obsAudioSpec(platform: ObsPlatform, target: ObsTarget): ObsInputSpec | null {
  if (platform === 'windows') return { inputKind: WIN_AUDIO, inputSettings: { window: String(target.value) } };
  return null;
}

/**
 * 建好源（不弹属性窗口）并把选中的目标写进去；Windows 的声音源跟画面指同一个目标。
 * 返回给用户看的补充说明（空串 = 没什么要交代的）。
 */
/**
 * 让 OBS 重建一次 ScreenCaptureKit 的可共享内容列表：那份列表是异步取回的，采集初始化只认它，
 * 而它只在 obs_source_properties() 被调时重建。壳里选应用走的是原生接口、不经 OBS 的属性回调，
 * 不补这一下可能写完设置源仍不出帧。只问 window（整数列表，安全），结果不要；失败不挡住选源。
 */
async function touchObsProperties(c: ObsConn): Promise<void> {
  try {
    await c.request('GetInputPropertiesListPropertyItems', { inputName: OBS_VIDEO_INPUT, propertyName: 'window' });
  } catch {
    /* 拿不到就算了，写设置照走 */
  }
}

export async function obsCaptureToTarget(
  c: ObsConn,
  platform: ObsPlatform,
  mode: ObsWinMode,
  target: ObsTarget,
): Promise<string> {
  const { note } = await setupObsCapture(c, platform, mode, false);
  if (platform === 'macos') await touchObsProperties(c);
  await c.request('SetInputSettings', {
    inputName: OBS_VIDEO_INPUT,
    inputSettings: obsVideoSpec(platform, mode, target, await macDisplayUuid(c, platform)).inputSettings,
  });
  // 目标定了才知道画面多大，适应画布只能排在这之后
  const fitNote = await fitObsSource(c).then(
    () => '',
    (e: unknown) => `画面没能自动适配 OBS 画布（${(e as Error).message}），可在 OBS 里选中源按 Ctrl+F。`,
  );
  const audio = obsAudioSpec(platform, target);
  if (audio) {
    try {
      await c.request('SetInputSettings', { inputName: OBS_AUDIO_INPUT, inputSettings: audio.inputSettings });
    } catch (e) {
      // 声音源没建成（600）上一步已经在 note 里说过了，别再拿同一件事挡住开播
      if (obsErrorCode(e) !== OBS_CODE_NOT_FOUND) throw e;
    }
  }
  // 不弹 OBS 的属性窗口：投什么已经替用户选好了，再把 OBS 拉到前台只会挡住人。
  // macOS 上「只写设置的源不出帧」是 SCK 的可共享内容列表没重建，那件事由列窗口那一步顺带做掉
  // （listObsWindows 与打开属性窗口同走 obs_source_properties()）。
  return [note, fitNote].filter(Boolean).join(' ');
}

/**
 * 把画面源「适应屏幕」：按画布尺寸给场景项设 bounds（等价 OBS 里的 Ctrl+F），
 * 保持宽高比、居中、放不下就留黑边而不是裁切。
 *
 * 不做这一步的话场景项是 1:1 摆在左上角：4K 屏进 1080p 画布，观众只看得到左上角四分之一。
 * 源的真实尺寸要等目标选定才知道，所以只能在源指到目标之后调。
 */
export async function fitObsSource(c: ObsConn): Promise<void> {
  const video = await c.request('GetVideoSettings');
  const { sceneItemId } = await c.request('GetSceneItemId', {
    sceneName: OBS_SCENE,
    sourceName: OBS_VIDEO_INPUT,
  });
  await c.request('SetSceneItemTransform', {
    sceneName: OBS_SCENE,
    sceneItemId,
    sceneItemTransform: {
      positionX: 0,
      positionY: 0,
      boundsType: 'OBS_BOUNDS_SCALE_INNER', // 按内接缩放 = 保持宽高比
      boundsAlignment: 0, // OBS_ALIGN_CENTER
      boundsWidth: video.baseWidth,
      boundsHeight: video.baseHeight,
    },
  });
}

/** 把 hearth 的 WHIP 地址与令牌写进 OBS 的直播服务设置并开播 */
export async function startObsStream(c: ObsConn, server: string, token: string): Promise<void> {
  // 浏览器那条路是用户在 OBS 的属性窗口里选完目标才回来点开播的，适配只能赶在这会儿做；
  // 场景/源不是本功能建的（用户用自己的场景推）就什么都别管，更不能挡住开播。
  await fitObsSource(c).catch(() => {});
  await c.request('SetStreamServiceSettings', whipServiceSettings(server, token));
  await c.request('StartStream');
}
