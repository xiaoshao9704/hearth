// 「通过 OBS 投屏」的共享件：OBS 联动的本机凭证存取，以及在 OBS 里建采集源的分区。
// 分区同时挂在两处——桌面壳的原生选源面板里多一节，浏览器里点「投屏」弹的面板里多一节；
// 两处都只是多一个选择，浏览器 getDisplayMedia 与原生 SCK 那两条原路径一行不动。
import { createSignal, Show } from 'solid-js';
import { inShell } from '../../bridge';
import {
  connectObs,
  OBS_AUDIO_INPUT,
  OBS_WHIP_MIN_MAJOR,
  OBS_WS_DEFAULT_URL,
  obsMajor,
  obsPlatform,
  openObsInputProperties,
  setupObsCapture,
  type ObsConn,
  type ObsPlatform,
  type ObsWinMode,
} from '../../obsws';
import { el, icon } from '../../ui';

export const LS_OBS_URL = 'hearth_obsws_url';
export const LS_OBS_PASSWORD = 'hearth_obsws_password';
// 「这台设备上的 OBS 联动确实连通过」：房间页据此才去自动连一条，
// 否则每个开房间的人都会去敲一遍 localhost:4455。
export const LS_OBS_READY = 'hearth_obsws_ready';

/** 建完源之后该怎么办：两处入口说的是同一句话 */
export const OBS_SETUP_HINT = '已在 OBS 里打开源属性窗口：选好要投的应用后回来点「配置并开始推流」。';

export const obsLsGet = (k: string, dflt = ''): string => {
  try {
    return localStorage.getItem(k) ?? dflt;
  } catch {
    return dflt;
  }
};
export const obsLsSet = (k: string, v: string): void => {
  try {
    localStorage.setItem(k, v);
  } catch {
    /* 隐私模式写不了就只在本次会话里有效 */
  }
};

/** 本机存过的 OBS 联动凭证连一条；没配过就返回 null（不去试探默认地址） */
export async function connectStoredObs(): Promise<ObsConn | null> {
  if (obsLsGet(LS_OBS_READY) !== '1') return null;
  return connectObs(obsLsGet(LS_OBS_URL) || OBS_WS_DEFAULT_URL, obsLsGet(LS_OBS_PASSWORD), inShell());
}

/** 版本/平台够不够用；空串 = 能用 */
export function obsCaptureBlocker(platform: ObsPlatform, obsVersion: string): string {
  if (obsMajor(obsVersion) && obsMajor(obsVersion) < OBS_WHIP_MIN_MAJOR)
    return `OBS ${obsVersion} 没有 WHIP 输出，需要 OBS ${OBS_WHIP_MIN_MAJOR} 或更新的版本。`;
  if (platform === 'other') return '在 OBS 里建采集源目前只支持 Windows 与 macOS 上的 OBS。';
  return '';
}

/**
 * 建源分区：点一下就在 OBS 里建好「Hearth 投屏」场景与画面/声音两个源、切成当前场景，
 * 再弹 OBS 自己的源属性窗口让用户在 OBS 里挑要投的应用——hearth 不枚举窗口清单
 * （obs-websocket 5.7.3 枚举 macOS screen_capture 会让 OBS 段错误）。这一步不开播。
 */
export const ObsCaptureSection = (p: {
  conn: ObsConn;
  obsVersion: string;
  platform: string;
  /** 外层正忙（例如「OBS 联动」面板在写配置）时连带禁用，自身的进行中状态在内部管 */
  busy?: boolean;
  /** 建完源（属性窗口已弹或已给出替代说明）时通知外层，用来补一句提示 */
  onReady?: () => void;
}) => {
  const platform = () => obsPlatform(p.platform);
  const blocked = () => obsCaptureBlocker(platform(), p.obsVersion);
  const [mode, setMode] = createSignal<ObsWinMode>('game');
  const [ready, setReady] = createSignal(false);
  const [note, setNote] = createSignal('');
  const [err, setErr] = createSignal('');
  const [loading, setLoading] = createSignal(false);

  const macTip = (e: unknown) =>
    platform() === 'macos'
      ? `${(e as Error).message}（macOS 还要在「系统设置 → 隐私与安全性 → 屏幕录制」里放行 OBS）`
      : (e as Error).message;

  const setup = () => {
    if (blocked() || loading()) return;
    setLoading(true);
    setErr('');
    void setupObsCapture(p.conn, platform(), mode())
      .then((r) => {
        setNote(r.note);
        setReady(true);
        p.onReady?.();
      })
      .catch((e: unknown) => {
        setReady(false);
        setNote('');
        setErr(macTip(e));
      })
      .finally(() => setLoading(false));
  };

  // 声音源的目标（抓哪个应用的声）同样在 OBS 里选，按钮只负责把那个窗口弹出来
  const openAudio = () => {
    if (loading()) return;
    setLoading(true);
    setErr('');
    void openObsInputProperties(p.conn, OBS_AUDIO_INPUT)
      .catch((e: unknown) => setErr(macTip(e)))
      .finally(() => setLoading(false));
  };

  const switchMode = (next: ObsWinMode) => {
    if (next === mode()) return;
    setMode(next);
    setReady(false); // 换捕获方式等于换 inputKind，画面源要重建
  };

  return (
    <div class="ig-field obs-cap">
      <div class="section-label">通过 OBS 投屏（更流畅）</div>
      <Show when={!blocked()} fallback={<div class="ig-tip">{blocked()}</div>}>
        <Show when={platform() === 'windows'}>
          <div class="obs-cap-modes">
            <button
              type="button"
              class="hit btn btn-sm"
              classList={{ 'btn-primary': mode() === 'game' }}
              disabled={loading() || p.busy}
              onClick={() => switchMode('game')}
            >
              游戏捕获
            </button>
            <button
              type="button"
              class="hit btn btn-sm"
              classList={{ 'btn-primary': mode() === 'window' }}
              disabled={loading() || p.busy}
              onClick={() => switchMode('window')}
            >
              窗口捕获
            </button>
            <span class="ig-tip obs-cap-modetip">
              {mode() === 'game' ? '独占全屏的游戏只有游戏捕获抓得到' : '抓不到画面时换这个（WGC）'}
            </span>
          </div>
        </Show>

        <Show when={err()}>
          <div class="notice-bad">
            <span class="ig-note">{err()}</span>
          </div>
        </Show>

        <div class="obs-cap-foot">
          <button
            type="button"
            class="hit btn btn-sm"
            classList={{ 'btn-primary': !ready(), loading: loading() }}
            disabled={loading() || p.busy}
            onClick={setup}
          >
            {el(icon('screen', 13, 'currentColor', 1.8))} {ready() ? '重新建采集源' : '在 OBS 里建采集源'}
          </button>
          <Show when={ready()}>
            <button type="button" class="hit btn btn-sm" disabled={loading() || p.busy} onClick={openAudio}>
              选声音来源
            </button>
          </Show>
        </div>

        <Show
          when={ready()}
          fallback={
            <div class="ig-tip">
              会在 OBS 里建一个固定的「Hearth 投屏」场景（画面 + 声音两个源）并切过去，
              然后弹出 OBS 自己的源属性窗口让你选要投的应用；你原有的场景与源不受影响。
            </div>
          }
        >
          <div class="ig-tip">{OBS_SETUP_HINT}</div>
        </Show>
        <Show when={note()}>
          <div class="ig-tip">{note()}</div>
        </Show>
      </Show>
    </div>
  );
};

/**
 * 浏览器里点「投屏」弹的面板：原来的浏览器共享仍是第一选项，OBS 只是多一节。
 * 桌面壳走的是原生选源面板，那边把同一个分区接进去。
 */
export const ObsScreenPanel = (p: {
  conn: ObsConn;
  obsVersion: string;
  platform: string;
  onBrowser: () => void;
  onReady?: () => void;
  onClose: () => void;
}) => (
  <div class="ingest-scrim" onClick={p.onClose}>
    <div class="ingest-panel card" onClick={(ev) => ev.stopPropagation()}>
      <header class="ig-head">
        {el(icon('screen', 16, 'var(--ember)', 1.7))}
        <div class="ig-title">选择要共享的画面</div>
        <button type="button" class="hit btn btn-icon" aria-label="关闭" onClick={p.onClose}>
          {el(icon('close', 15, 'var(--text-1)', 1.8))}
        </button>
      </header>

      <div class="ig-field">
        <div class="section-label">本机浏览器</div>
        <button type="button" class="hit btn btn-sm btn-primary obs-cap-browser" onClick={p.onBrowser}>
          {el(icon('screen', 13, 'var(--on-ember)', 1.8))} 用浏览器选窗口共享
        </button>
        <div class="ig-tip">浏览器自带的共享选择器，和以前一样。</div>
      </div>

      <div class="ig-sep" />
      <ObsCaptureSection conn={p.conn} obsVersion={p.obsVersion} platform={p.platform} onReady={p.onReady} />
    </div>
  </div>
);
