// 投屏采集/发布参数（LiveKitEngine.setScreen / republishScreen / applyScreenPrefs 调用）
import type { ScreenShareCaptureOptions, TrackPublishOptions, VideoCodec } from 'livekit-client';
import { RES_DIMS } from '../prefs';
import type { RoomPrefs, ScreenContent } from '../prefs';

// 带宽不够时编码器往哪边让：文字要看清（丢帧保分辨率），游戏要动起来（缩分辨率保帧率）。
// 真正决定降级方向的是 sender 的 degradationPreference，contentHint 只是给编码器的提示
export const degradationFor = (c: ScreenContent): RTCDegradationPreference =>
  c === 'game' ? 'maintain-framerate' : 'maintain-resolution';

// SVC 投屏的 contentHint 只能是 motion：Chrome 走投屏专用路径时 L1T3 限到 5fps，
// SDK 发布时会强制改成 motion 绕开，这里跟着它，免得热改把画面按回 5fps
export const contentHintFor = (p: RoomPrefs): 'detail' | 'motion' =>
  p.screenContent === 'game' || p.screenCodec === 'vp9' || p.screenCodec === 'av1' ? 'motion' : 'detail';

export function buildScreenOptions(p: RoomPrefs): { capture: ScreenShareCaptureOptions; publish: TrackPublishOptions } {
  const d = RES_DIMS[p.res];
  // restrictOwnAudio 还没普及：浏览器不认的约束一律不传，免得整个 getDisplayMedia 直接 TypeError
  const supported = navigator.mediaDevices?.getSupportedConstraints?.() as
    | (MediaTrackSupportedConstraints & { restrictOwnAudio?: boolean })
    | undefined;
  const capture: ScreenShareCaptureOptions = {
    resolution: { width: d.width, height: d.height, frameRate: p.fps },
    contentHint: contentHintFor(p),
    systemAudio: 'include', // 让浏览器把系统声音摆进可选源；不支持的浏览器忽略
    selfBrowserSurface: 'exclude', // 别把 hearth 自己这个标签页列为候选（选中就成了镜中镜）
    preferCurrentTab: false,
    // 音频约束一律传：要不要带声音交给浏览器自己的选择框（Chrome 只有共享标签页时
    // 才给勾选框），拿到的流里没有音轨就只发画面，不算失败。
    // 系统声音是音乐/游戏音效，不是人声：回声消除/降噪/自动增益会把它嚼烂，
    // 声道数也和麦克风相反——麦克风降到单声道避免单耳，这里要保住左右声场。
    // restrictOwnAudio 把本页面自己播放的声音（也就是别人的语音）剔出采集，避免回音
    audio: {
      echoCancellation: false,
      noiseSuppression: false,
      autoGainControl: false,
      channelCount: 2,
      ...(supported?.restrictOwnAudio ? { restrictOwnAudio: true } : {}),
    },
  };
  const encoding = { maxBitrate: Math.round(p.bitrateMax * 1e6), maxFramerate: p.fps };
  let publish: TrackPublishOptions;
  if (p.screenCodec === 'h264') {
    // 单层：H.264 无 SVC；simulcast 双编码会把软编 CPU 拖垮，维持单层
    publish = { videoCodec: 'h264', screenShareEncoding: encoding, screenShareSimulcastLayers: [] };
  } else if (p.screenCodec === 'h265') {
    // HEVC 单层（SDK 的 SVC 只认 vp9/av1）：发送端平台硬编、观众端硬解，
    // 同码率观感约为 H.264 的 1.5 倍；不支持 h265 的订阅端触发 h264 备份编码
    publish = {
      videoCodec: 'h265' as VideoCodec,
      screenShareEncoding: encoding,
      screenShareSimulcastLayers: [],
      backupCodec: { codec: 'h264' },
    };
  } else {
    // SVC：单编码器产分层码流，SFU 按观众带宽逐层转发；
    // 不支持该编码的订阅端会触发 h264 备份编码（按需才多一路编码）
    publish = {
      videoCodec: p.screenCodec as VideoCodec,
      scalabilityMode: 'L2T2_KEY',
      screenShareEncoding: encoding,
      backupCodec: { codec: 'h264' },
    };
  }
  // 这次发布连带的投屏音轨（有就发）：128k 立体声，关 DTX 免得静音段断流吞掉音乐尾音，
  // 关 RED 免得为冗余多占上行。只作用于本次投屏发布，麦克风走 micPublishOptions
  return {
    capture,
    publish: {
      ...publish,
      degradationPreference: degradationFor(p.screenContent),
      audioPreset: { maxBitrate: 128_000 },
      dtx: false,
      red: false,
    },
  };
}
