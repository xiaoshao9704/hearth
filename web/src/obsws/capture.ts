// 在 OBS 里建采集源
// 「投哪个应用/窗口」有三条路，优先级与可见条件写在 obs-capture.tsx 的 ObsCaptureSection：
// 桌面壳给的清单（listObsTargets，应用级）、问 OBS 要窗口清单（listObsWindows，只有 macOS 安全）、
// 以及退路「建好源后弹 OBS 自己的属性窗口，让用户在 OBS 里选」。
// 只认这三个固定名字：建、复用、清理都只针对它们，用户自己的场景与源一概不碰。
import type { ObsConn, ObsPlatform } from './connection';

export const OBS_SCENE = 'Hearth 投屏';
export const OBS_VIDEO_INPUT = 'Hearth 画面';
export const OBS_AUDIO_INPUT = 'Hearth 声音';

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
export async function macDisplayUuid(c: ObsConn, platform: ObsPlatform): Promise<string> {
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
