// 麦克风采集/发布参数与 RNNoise 发布路径（LiveKitEngine.setMic 调用）
import { Track } from 'livekit-client';
import type { AudioCaptureOptions, TrackPublishOptions } from 'livekit-client';
import type { RnnoisePipeline } from '../audio';
import { loadPrefs } from '../prefs';

export function micCaptureOptions(): AudioCaptureOptions {
  const p = loadPrefs();
  const music = p.musicMode;
  return {
    deviceId: p.micDeviceId ? { ideal: p.micDeviceId } : undefined,
    channelCount: { ideal: 1 }, // 语音单声道：立体声麦克风若只有一路有声，发布出去就成了单耳
    echoCancellation: music ? false : p.echoCancellation,
    noiseSuppression: music ? false : p.denoise === 'browser',
    autoGainControl: music ? false : p.autoGainControl,
  };
}

export function micPublishOptions(): TrackPublishOptions {
  return { audioPreset: { maxBitrate: loadPrefs().voiceBitrate } };
}

/** 经 RNNoise 管线发布麦克风；成功返回 true，失败时已清理 raw 并 stop 管线，调用方回退浏览器路径 */
export async function tryPublishRnnoiseMic(
  raw: MediaStream,
  rnnoise: RnnoisePipeline,
  publishTrack: (track: MediaStreamTrack, opts: TrackPublishOptions) => Promise<unknown>,
  publishOpts: TrackPublishOptions = micPublishOptions(),
): Promise<boolean> {
  try {
    const processed = await rnnoise.start(raw);
    await publishTrack(processed, {
      ...publishOpts,
      source: Track.Source.Microphone,
    });
    return true;
  } catch (err) {
    console.warn('RNNoise 不可用，回退浏览器内置处理:', err);
    raw.getTracks().forEach((t) => t.stop());
    await rnnoise.stop();
    return false;
  }
}
