// 「通过 OBS 投屏」的共享件：OBS 联动的本机凭证存取，以及选源分区。
// 分区同时挂在两处——桌面壳的原生选源面板里多一节，浏览器里点「投屏」弹的面板里多一节；
// 两处都只是多一个选择，浏览器 getDisplayMedia 与原生 SCK 那两条原路径一行不动。
import { createSignal, For, onMount, Show } from 'solid-js';
import { inShell } from '../../bridge';
import {
  connectObs,
  OBS_WHIP_MIN_MAJOR,
  OBS_WS_DEFAULT_URL,
  obsMajor,
  obsPlatform,
  prepareObsCapture,
  type ObsConn,
  type ObsPlatform,
  type ObsTarget,
  type ObsWinMode,
} from '../../obsws';
import { el, icon } from '../../ui';

export const LS_OBS_URL = 'hearth_obsws_url';
export const LS_OBS_PASSWORD = 'hearth_obsws_password';
// 「这台设备上的 OBS 联动确实连通过」：房间页据此才去自动连一条，
// 否则每个开房间的人都会去敲一遍 localhost:4455。
export const LS_OBS_READY = 'hearth_obsws_ready';

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
  if (platform === 'other') return '快速配置采集源目前只支持 Windows 与 macOS 上的 OBS。';
  return '';
}

const TARGET_HINT: Record<ObsTarget['kind'], string> = { app: '整个应用', window: '单个窗口' };

/**
 * 选源分区：进来就向 OBS 要一次清单（这一步会在 OBS 里建出 Hearth 场景与画面源，
 * 但不切场景——用户中途关掉最多留一个静止的场景）。选中即交给上层去开播。
 */
export const ObsCaptureSection = (p: {
  conn: ObsConn;
  obsVersion: string;
  platform: string;
  busy: boolean;
  // 进来就取清单。点「投屏」弹出的面板给 true（用户已经表达了要投屏的意思）；
  // 设置面板里给 false，别让「测试连接」顺手在 OBS 里建出场景。
  auto?: boolean;
  onPick: (target: ObsTarget, mode: ObsWinMode) => void;
}) => {
  const platform = () => obsPlatform(p.platform);
  const blocked = () => obsCaptureBlocker(platform(), p.obsVersion);
  const [mode, setMode] = createSignal<ObsWinMode>('game');
  const [targets, setTargets] = createSignal<ObsTarget[] | null>(null);
  const [err, setErr] = createSignal('');
  const [loading, setLoading] = createSignal(false);

  const load = () => {
    if (blocked() || loading()) return;
    setLoading(true);
    setErr('');
    void prepareObsCapture(p.conn, platform(), mode())
      .then(setTargets)
      .catch((e: unknown) => {
        setTargets(null);
        setErr(
          platform() === 'macos'
            ? `${(e as Error).message}（macOS 还要在「系统设置 → 隐私与安全性 → 屏幕录制」里放行 OBS）`
            : (e as Error).message,
        );
      })
      .finally(() => setLoading(false));
  };
  onMount(() => {
    if (p.auto) load();
  });

  const switchMode = (next: ObsWinMode) => {
    if (next === mode()) return;
    setMode(next);
    load(); // 换捕获方式等于换 inputKind，画面源要重建，清单也重取
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

        <Show when={loading()}>
          <div class="ig-tip">正在向 OBS 要窗口列表…</div>
        </Show>
        <Show when={!loading() && !targets() && !err()}>
          <div class="ig-tip">点下面的按钮让 OBS 报一遍当前能采集的窗口与应用。</div>
        </Show>
        <Show when={targets()?.length === 0}>
          <div class="ig-tip">OBS 没报出任何可采集的窗口或应用，先把要投的程序打开再刷新。</div>
        </Show>
        <Show when={targets()?.length}>
          <div class="src-list obs-cap-list">
            <For each={targets()!}>
              {(t) => (
                <button
                  type="button"
                  class="hit src-item obs-cap-item"
                  disabled={p.busy}
                  onClick={() => p.onPick(t, mode())}
                >
                  <span class="src-copy">
                    <span class="src-name">{t.label}</span>
                    <span class="src-meta">{TARGET_HINT[t.kind]}</span>
                  </span>
                </button>
              )}
            </For>
          </div>
        </Show>

        <div class="obs-cap-foot">
          <button type="button" class="hit btn btn-sm" disabled={loading() || p.busy} onClick={load}>
            {el(icon('reset', 13, 'currentColor', 1.8))} {targets() ? '刷新列表' : '列出 OBS 里的窗口'}
          </button>
          <span class="ig-tip">
            会在 OBS 里建一个固定的「Hearth 投屏」场景（画面 + 声音两个源），你自己的场景不受影响。
          </span>
        </div>
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
  busy: boolean;
  onBrowser: () => void;
  onPick: (target: ObsTarget, mode: ObsWinMode) => void;
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
        <button type="button" class="hit btn btn-sm btn-primary obs-cap-browser" disabled={p.busy} onClick={p.onBrowser}>
          {el(icon('screen', 13, 'var(--on-ember)', 1.8))} 用浏览器选窗口共享
        </button>
        <div class="ig-tip">浏览器自带的共享选择器，和以前一样。</div>
      </div>

      <div class="ig-sep" />
      <ObsCaptureSection
        conn={p.conn}
        obsVersion={p.obsVersion}
        platform={p.platform}
        busy={p.busy}
        auto
        onPick={p.onPick}
      />
    </div>
  </div>
);
