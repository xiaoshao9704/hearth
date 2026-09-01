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

// 按 bpp 模型推导默认码率（宽×高×帧率×0.07）
export function autoBitrate(res: string, fps: number): number {
  const d = RES_DIMS[res] ?? RES_DIMS['1080p'];
  return Math.round(((d.width * d.height * fps * 0.07) / 1e6) * 10) / 10;
}

export type DenoiseMode = 'rnnoise' | 'browser' | 'off';
export type ScreenCodec = 'h264' | 'h265' | 'vp9' | 'av1';

export interface RoomPrefs {
  mic: boolean;
  camera: boolean;
  layout: 'grid' | 'spotlight';
  res: string;
  fps: number;
  bitrate: number; // Mbps
  bitrateAuto: boolean;
  screenCodec: ScreenCodec; // 投屏编码：h264 单层 / vp9·av1 走 SVC 分层
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
}

const PREFS_KEY = 'hearth_room_prefs';

export function defaultPrefs(): RoomPrefs {
  return {
    mic: false,
    camera: false,
    layout: 'grid',
    res: '1080p',
    fps: 60,
    bitrate: autoBitrate('1080p', 60),
    bitrateAuto: true,
    screenCodec: 'vp9',
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
  };
}

interface LegacyPrefs {
  rnnoise?: boolean;
  noiseSuppression?: boolean;
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
    return {
      mic: p.mic === true,
      camera: p.camera === true,
      layout: p.layout === 'spotlight' ? 'spotlight' : 'grid',
      res: RES_DIMS[p.res ?? ''] ? (p.res as string) : def.res,
      fps: (FPS_BY_RES[p.res ?? '1080p'] ?? [15, 30, 60]).includes(p.fps as number) ? (p.fps as number) : def.fps,
      bitrate: typeof p.bitrate === 'number' && p.bitrate >= 1 && p.bitrate <= 15 ? p.bitrate : def.bitrate,
      bitrateAuto: p.bitrateAuto !== false,
      screenCodec: p.screenCodec === 'h264' || p.screenCodec === 'h265' || p.screenCodec === 'av1' ? p.screenCodec : 'vp9',
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
        bitrate: Math.round(p.bitrate * 1e6),
        ...(codec === 'h264' || codec === 'h265' ? {} : { scalabilityMode: 'L2T2_KEY' }),
      },
    });
    if (!info.supported) return null;
    return info.powerEfficient;
  } catch {
    return null;
  }
}
