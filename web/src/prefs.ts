// 房间与音视频偏好：持久化到 localStorage，设置页与房间页共用。
export const RES_DIMS: Record<string, { width: number; height: number }> = {
  '1080p': { width: 1920, height: 1080 },
  '720p': { width: 1280, height: 720 },
};
export const FPS_BY_RES: Record<string, number[]> = {
  '720p': [15, 30, 60],
  '1080p': [15, 30, 60],
};
// 码率建议区间（Mbps）：与原型一致的档位联动
export const BR_LIMITS: Record<string, { min: number; max: number }> = {
  '720p': { min: 1, max: 6 },
  '1080p': { min: 2.5, max: 15 },
};
export const VOICE_BITRATES = [32000, 64000, 96000, 128000]; // bps

// 按 bpp 模型推导默认码率上限（宽×高×帧率×0.07）
export function autoBitrate(res: string, fps: number): number {
  const d = RES_DIMS[res] ?? RES_DIMS['1080p'];
  return Math.round(((d.width * d.height * fps * 0.07) / 1e6) * 10) / 10;
}

/** 下限的绝对下界（Mbps）：再低就不是「保住画面」而是留一片马赛克。 */
export const BITRATE_FLOOR = 0.5;
/** 下限最多到上限的这个比例：留出 25% 余量，不给带宽估计留一段无处可降的死区。 */
export const BITRATE_MIN_RATIO = 0.8;
/** 自动下限取上限的这个比例。 */
const AUTO_MIN_RATIO = 0.4;

const round1 = (n: number) => Math.round(n * 10) / 10;
// 推挤后的值按 0.1 取整：被推的一侧总往「更宽松」的方向取，免得取整后又踩回禁区
const floor1 = (n: number) => Math.floor(n * 10) / 10;
const ceil1 = (n: number) => Math.ceil(n * 10) / 10;

/** 自动模式下由上限推下限。 */
export function autoBitrateMin(max: number): number {
  return round1(Math.max(BITRATE_FLOOR, max * AUTO_MIN_RATIO));
}

/**
 * 把「下限 + 上限」收进合法区间：下限不低于绝对下界，且不高于上限的 BITRATE_MIN_RATIO。
 * anchor 是用户刚拖动的那一侧（保持不动），另一侧被推开——所以拖高下限会顶高上限，
 * 拖低上限会压低下限，两个滑块都不会出现「拖不动」的手感。
 */
export function clampBitrateRange(min: number, max: number, anchor: 'min' | 'max' = 'max'): { min: number; max: number } {
  let lo = round1(Math.max(BITRATE_FLOOR, min));
  let hi = round1(Math.max(ceil1(BITRATE_FLOOR / BITRATE_MIN_RATIO), max));
  if (lo > hi * BITRATE_MIN_RATIO) {
    if (anchor === 'min') hi = ceil1(lo / BITRATE_MIN_RATIO);
    else lo = floor1(hi * BITRATE_MIN_RATIO);
  }
  return { min: lo, max: hi };
}

// 剧场浮动名册的停靠角：左上/右上/左下/右下
export type TheaterCorner = 'tl' | 'tr' | 'bl' | 'br';
export const THEATER_CORNERS: TheaterCorner[] = ['tl', 'tr', 'bl', 'br'];

export type DenoiseMode = 'rnnoise' | 'browser' | 'off';
export type ScreenCodec = 'h264' | 'h265' | 'vp9' | 'av1';
// 投屏内容类型：决定带宽不够时牺牲清晰度还是牺牲帧率
export type ScreenContent = 'text' | 'game';

export interface RoomPrefs {
  mic: boolean;
  camera: boolean;
  layout: 'grid' | 'spotlight';
  res: string;
  fps: number;
  bitrateMax: number; // Mbps，网络好时最多发多少
  bitrateMin: number; // Mbps，网络差时最少也要发多少（见 engine/sdp-floor.ts）
  bitrateAuto: boolean; // 用户没手动拖过码率，两端都跟着分辨率/帧率自动算
  screenCodec: ScreenCodec; // 投屏编码：h264/h265 单层 / vp9·av1 走 SVC 分层
  screenCodecAuto: boolean; // true = 按本机能力自动选（硬编优先）；用户手选后置 false
  screenContent: ScreenContent; // text = 保清晰度丢帧（文字/界面）；game = 保帧率缩分辨率（游戏/视频）
  denoise: DenoiseMode; // 三选一：RNNoise / 浏览器自带 / 不降噪
  echoCancellation: boolean;
  autoGainControl: boolean;
  musicMode: boolean; // 开启后旁路全部处理 + 语音 128k
  micDeviceId: string;
  camDeviceId: string;
  speakerId: string;
  volume: number; // 0-100 输出音量
  mirror: boolean; // 摄像头预览镜像（仅本地）
  voiceBitrate: number; // bps
  joinCue: boolean; // 他人进出房间时的短提示音
  chatCue: boolean; // 他人实时发来聊天消息时的短提示音
  afkMinutes: number; // 无操作多少分钟后向房间广播「离开」；0 = 不广播
  mentionCue: boolean; // 被 @ 提到时的短提示音（比普通消息更醒目，且不受消息提示音节流影响）
  theaterAutoHide: boolean; // 剧场模式无操作 3 秒后隐藏顶栏与控制栏
  theaterCorner: TheaterCorner; // 剧场浮动名册停靠的角落
  theaterRosterFold: boolean; // 剧场浮动名册折叠成小按钮
  notifyMessages: boolean; // 页面在后台时，新消息发系统通知
  notifyMentions: boolean; // 页面在后台时，被 @ 发系统通知
  notifyJoins: boolean; // 页面在后台时，有人进房发系统通知
}

const PREFS_KEY = 'hearth_room_prefs';

export function defaultPrefs(): RoomPrefs {
  return {
    mic: false,
    camera: false,
    layout: 'grid',
    res: '1080p',
    fps: 60,
    bitrateMax: autoBitrate('1080p', 60),
    bitrateMin: autoBitrateMin(autoBitrate('1080p', 60)),
    bitrateAuto: true,
    screenCodec: 'vp9',
    screenCodecAuto: true,
    screenContent: 'game', // 默认保帧率：实测高熵画面下「保清晰度」会把帧率压到个位数，游戏/视频场景是主用途
    denoise: 'rnnoise',
    echoCancellation: true,
    autoGainControl: true,
    musicMode: false,
    micDeviceId: '',
    camDeviceId: '',
    speakerId: '',
    volume: 100,
    mirror: true,
    voiceBitrate: 64000,
    joinCue: true,
    chatCue: true,
    afkMinutes: 10,
    mentionCue: true,
    theaterAutoHide: true,
    theaterCorner: 'tr',
    theaterRosterFold: false,
    notifyMessages: true,
    notifyMentions: true,
    notifyJoins: false,
  };
}

interface LegacyPrefs {
  rnnoise?: boolean;
  noiseSuppression?: boolean;
  bitrate?: number; // 旧存档里的单值码率 = 现在的上限
}

export function loadPrefs(): RoomPrefs {
  const def = defaultPrefs();
  try {
    const raw = localStorage.getItem(PREFS_KEY);
    if (!raw) return def;
    const p = JSON.parse(raw) as Partial<RoomPrefs> & LegacyPrefs;
    // 旧字段迁移：rnnoise/noiseSuppression 两个布尔 → denoise 三选一
    let denoise: DenoiseMode = def.denoise;
    if (p.denoise === 'rnnoise' || p.denoise === 'browser' || p.denoise === 'off') {
      denoise = p.denoise;
    } else if (typeof p.rnnoise === 'boolean') {
      denoise = p.rnnoise ? 'rnnoise' : p.noiseSuppression !== false ? 'browser' : 'off';
    }
    // 旧存档只有单值 bitrate：读成上限，下限按比例补出来
    const rawMax =
      typeof p.bitrateMax === 'number' && p.bitrateMax >= 1 && p.bitrateMax <= 15
        ? p.bitrateMax
        : typeof p.bitrate === 'number' && p.bitrate >= 1 && p.bitrate <= 15
          ? p.bitrate
          : def.bitrateMax;
    const rawMin =
      typeof p.bitrateMin === 'number' && p.bitrateMin >= BITRATE_FLOOR && p.bitrateMin <= 15 ? p.bitrateMin : autoBitrateMin(rawMax);
    const br = clampBitrateRange(rawMin, rawMax);
    return {
      mic: p.mic === true,
      camera: p.camera === true,
      layout: p.layout === 'spotlight' ? 'spotlight' : 'grid',
      res: RES_DIMS[p.res ?? ''] ? (p.res as string) : def.res,
      fps: (FPS_BY_RES[p.res ?? '1080p'] ?? [15, 30, 60]).includes(p.fps as number) ? (p.fps as number) : def.fps,
      bitrateMax: br.max,
      bitrateMin: br.min,
      bitrateAuto: p.bitrateAuto !== false,
      screenCodec: p.screenCodec === 'h264' || p.screenCodec === 'h265' || p.screenCodec === 'av1' ? p.screenCodec : 'vp9',
      screenCodecAuto: p.screenCodecAuto !== false,
      screenContent: p.screenContent === 'text' || p.screenContent === 'game' ? p.screenContent : def.screenContent,
      denoise,
      echoCancellation: p.echoCancellation !== false,
      autoGainControl: p.autoGainControl !== false,
      musicMode: p.musicMode === true,
      micDeviceId: typeof p.micDeviceId === 'string' ? p.micDeviceId : '',
      camDeviceId: typeof p.camDeviceId === 'string' ? p.camDeviceId : '',
      speakerId: typeof p.speakerId === 'string' ? p.speakerId : '',
      volume: typeof p.volume === 'number' && p.volume >= 0 && p.volume <= 100 ? p.volume : def.volume,
      mirror: p.mirror !== false,
      voiceBitrate: VOICE_BITRATES.includes(p.voiceBitrate as number) ? (p.voiceBitrate as number) : def.voiceBitrate,
      joinCue: p.joinCue !== false,
      chatCue: p.chatCue !== false,
      afkMinutes:
        typeof p.afkMinutes === 'number' && p.afkMinutes >= 0 && p.afkMinutes <= 240 ? Math.round(p.afkMinutes) : def.afkMinutes,
      mentionCue: p.mentionCue !== false,
      theaterAutoHide: p.theaterAutoHide !== false,
      theaterCorner: THEATER_CORNERS.includes(p.theaterCorner as TheaterCorner) ? (p.theaterCorner as TheaterCorner) : def.theaterCorner,
      theaterRosterFold: p.theaterRosterFold === true,
      notifyMessages: p.notifyMessages !== false,
      notifyMentions: p.notifyMentions !== false,
      notifyJoins: p.notifyJoins === true,
    };
  } catch {
    return def;
  }
}

export function savePrefs(prefs: RoomPrefs) {
  localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
}

// 设置页改了偏好，通知已打开的房间视图热应用
export const prefsBus = new EventTarget();

export function notifyPrefsChanged(what: string) {
  prefsBus.dispatchEvent(new CustomEvent('prefs', { detail: what }));
}

// 房间视图在浏览器投屏进行中登记「重开投屏」的动作，设置浮层关闭时据此让新的码率范围生效
// （码率范围只能重开发布才生效）。两边没有直接引用关系，这个模块是它们唯一的共同依赖。
let screenRepublish: (() => void) | null = null;

export function setScreenRepublish(fn: (() => void) | null) {
  screenRepublish = fn;
}

export function screenRepublishHook(): (() => void) | null {
  return screenRepublish;
}


// ---- 投屏编码的软/硬编探测 ----

// 运行时真值解读：优先浏览器上报的 powerEfficientEncoder，旧版本缺字段时按实现名兜底
export function encoderIsHw(info: { impl: string; hw: boolean | null }): boolean | null {
  if (info.hw !== null) return info.hw;
  if (/libvpx|libaom|OpenH264/i.test(info.impl)) return false;
  if (/External|VideoToolbox|MediaFoundation|Hardware|VAAPI/i.test(info.impl)) return true;
  return null;
}

// 事前预测：MediaCapabilities 按当前档位问浏览器"这么编走不走硬件"
export async function probeHwEncode(codec: ScreenCodec): Promise<boolean | null> {
  try {
    const mc = navigator.mediaCapabilities as {
      encodingInfo?: (c: unknown) => Promise<{ supported: boolean; powerEfficient: boolean }>;
    };
    if (!mc?.encodingInfo) return null;
    const p = loadPrefs();
    const d = RES_DIMS[p.res] ?? RES_DIMS['1080p'];
    const info = await mc.encodingInfo({
      type: 'webrtc',
      video: {
        contentType:
          codec === 'h264' ? 'video/H264' : codec === 'h265' ? 'video/H265' : codec === 'vp9' ? 'video/VP9' : 'video/AV1',
        width: d.width,
        height: d.height,
        framerate: p.fps,
        bitrate: Math.round(p.bitrateMax * 1e6),
        ...(codec === 'h264' || codec === 'h265' ? {} : { scalabilityMode: 'L2T2_KEY' }),
      },
    });
    if (!info.supported) return null;
    return info.powerEfficient;
  } catch {
    return null;
  }
}

// ---- 投屏编码自动默认 ----

// pickBestScreenCodec 按本机真实能力选默认编码：
// 硬编优先，同硬编按体验排序——SVC 分层档（av1/vp9，硬编 SVC 存在即最优）
// 排在高效单层档（h265）前，h264 兜底；全软编时选 vp9（SVC 平衡，
// av1 软编吃 CPU 伤帧率不算"体验最好"）。
export async function pickBestScreenCodec(): Promise<ScreenCodec> {
  for (const c of ['av1', 'vp9', 'h265', 'h264'] as ScreenCodec[]) {
    if ((await probeHwEncode(c)) === true) return c;
  }
  return 'vp9';
}

// initScreenCodecAuto 启动时执行一次：仅在用户未手选（screenCodecAuto）时更新默认值。
export async function initScreenCodecAuto() {
  const p = loadPrefs();
  if (!p.screenCodecAuto) return;
  try {
    const best = await pickBestScreenCodec();
    if (best !== p.screenCodec) {
      const cur = loadPrefs(); // 重取，避免覆盖探测期间的其他改动
      cur.screenCodec = best;
      savePrefs(cur);
    }
  } catch {
    // 探测失败维持现值
  }
}
