// 选中即开播
// 目标已经从清单里选好（壳的 listObsTargets 或 listObsWindows），下面把它写进源就能直接开播，
// 不用再弹 OBS 的属性窗口。枚举属性清单只许打 `window` 这一个属性，理由见 listObsWindows。
import type { ObsConn, ObsPlatform } from './connection';
import { whipServiceSettings } from './connection';
import {
  OBS_AUDIO_INPUT,
  OBS_CODE_NOT_FOUND,
  OBS_SCENE,
  OBS_VIDEO_INPUT,
  macDisplayUuid,
  obsErrorCode,
  obsVideoSpec,
  setupObsCapture,
  type ObsInputSpec,
  type ObsTarget,
  type ObsWinMode,
} from './capture';

/** 声音源的 kind 与设置；null = 这个平台不用单独的声音源（见 obsAudioSetupSpec） */
export function obsAudioSpec(platform: ObsPlatform, target: ObsTarget): ObsInputSpec | null {
  if (platform === 'windows') {
    return { inputKind: 'wasapi_process_output_capture', inputSettings: { window: String(target.value) } };
  }
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
