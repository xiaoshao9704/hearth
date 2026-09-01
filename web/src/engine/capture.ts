// 麦克风采集（按 prefs：设备、回声消除/增益、RNNoise/浏览器降噪、音乐模式）。
// 给不自带采集封装的引擎用（pion-voice）；LiveKit 引擎沿用 SDK 内置路径。
import { RnnoisePipeline } from '../audio';
import { loadPrefs } from '../prefs';

export interface MicCapture {
  track: MediaStreamTrack; // 发布用（可能是 RNNoise 处理后的轨）
  raw: MediaStreamTrack; // 原始设备轨（监听 ended 用）
  stop(): Promise<void>;
}

let rnnoiseBroken = false;

export function micConstraints(): MediaTrackConstraints {
  const p = loadPrefs();
  const music = p.musicMode;
  return {
    ...(p.micDeviceId ? { deviceId: { ideal: p.micDeviceId } } : {}),
    echoCancellation: music ? false : p.echoCancellation,
    noiseSuppression: music ? false : p.denoise === 'browser',
    autoGainControl: music ? false : p.autoGainControl,
  };
}

export async function captureMic(): Promise<MicCapture> {
  const p = loadPrefs();
  const raw = await navigator.mediaDevices.getUserMedia({ audio: micConstraints() });
  const rawTrack = raw.getAudioTracks()[0];
  if (p.denoise === 'rnnoise' && !p.musicMode && !rnnoiseBroken) {
    const pipe = new RnnoisePipeline();
    try {
      const processed = await pipe.start(raw);
      const resume = () => void pipe.resume();
      document.addEventListener('pointerdown', resume, false);
      return {
        track: processed,
        raw: rawTrack,
        stop: async () => {
          document.removeEventListener('pointerdown', resume, false);
          raw.getTracks().forEach((t) => t.stop());
          await pipe.stop();
        },
      };
    } catch (err) {
      console.warn('RNNoise 不可用，回退浏览器内置处理:', err);
      rnnoiseBroken = true;
      await pipe.stop();
    }
  }
  return {
    track: rawTrack,
    raw: rawTrack,
    stop: async () => {
      raw.getTracks().forEach((t) => t.stop());
    },
  };
}
