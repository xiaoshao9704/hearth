// 「通过 OBS 投屏」的共享件：OBS 联动的本机凭证存取，以及在 OBS 里建采集源的分区。
// 分区同时挂在两处——桌面壳的原生选源面板里多一节，浏览器里点「投屏」弹的面板里多一节；
// 两处都只是多一个选择，浏览器 getDisplayMedia 与原生 SCK 那两条原路径一行不动。
import { createMemo, createSignal, For, Show } from 'solid-js';
import { capabilities, inShell, listObsTargets } from '../../bridge';
import type { IngestTokenInfo, SiteInfo } from '../../api';
import {
  connectObs,
  listObsWindows,
  obsCaptureToTarget,
  OBS_AUDIO_INPUT,
  OBS_WHIP_MIN_MAJOR,
  OBS_WS_DEFAULT_URL,
  obsAudioSetupSpec,
  obsMajor,
  obsPlatform,
  openObsInputProperties,
  setupObsCapture,
  splitObsWindowLabel,
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
// 「OBS 联动」刚连通（LS_OBS_READY 刚写上）时派给 window：房间页据此补连一条，
// 否则首次配好的人得退出重进才看得到「通过 OBS 投屏」。
export const OBS_READY_EVENT = 'hearth:obs-ready';

/** 建完源之后该怎么办：两处入口说的是同一句话（只有弹了 OBS 属性窗口那条路用得上） */
export const OBS_SETUP_HINT = '已在 OBS 里打开源属性窗口：选好要投的应用后回来点「配置并开始推流」。';

// 自签证书 OBS 不认：页面是 https 且证书来源是 self 时，把地址换成同主机的 http 端口
// （path 与查询串不变，只动 scheme 与 host:port）。「OBS 推流」面板与房间页的
// 「通过 OBS 投屏」同用这一份。
export function whipServer(info: IngestTokenInfo | null, site: SiteInfo | null, channelId: number): string {
  if (!info || channelId <= 0) return '';
  const full = `${info.base}${channelId}`;
  if (!site || site.tls_source !== 'self' || location.protocol !== 'https:') return full;
  try {
    return `http://${location.hostname}:${site.http_port}${new URL(full).pathname}`;
  } catch {
    return full;
  }
}

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
 * 建源分区。选源三条路，优先级从高到低；最后一条始终摆着，前两条都不可用时它就是唯一入口：
 * 1) 桌面壳给的清单（capabilities().obs_targets + listObsTargets）：应用级目标，进面板就有，
 *    选中即建源 + 写目标 + 开播，OBS 不跳到前台，体验最好；
 * 2) 问 OBS 要窗口清单（listObsWindows）：只有 macOS 走得通（别的平台枚举属性清单不安全，
 *    理由见 obsws.ts），而且枚举要打扰 OBS，所以不自动拉，点「列出窗口」才拉；
 * 3) 「在 OBS 里选采集源」：只建好「Hearth 投屏」场景与画面（Windows 另加声音）源、切成当前场景，
 *    再弹 OBS 自己的源属性窗口让用户在 OBS 里挑。这一步不开播，用户选完回来手动点开播。
 */
export const ObsCaptureSection = (p: {
  conn: ObsConn;
  obsVersion: string;
  platform: string;
  /** 外层正忙（例如「OBS 联动」面板在写配置）时连带禁用，自身的进行中状态在内部管 */
  busy?: boolean;
  /** 写 WHIP 配置并开播；给了才有「从清单里选中即开播」那两条路 */
  start?: () => Promise<void>;
  /** 建完源（属性窗口已弹或已给出替代说明）时通知外层，用来补一句提示 */
  onReady?: () => void;
  /** 选中即开播那条路真的开播了：外层据此收掉选源面板 */
  onStarted?: () => void;
}) => {
  const platform = () => obsPlatform(p.platform);
  const blocked = () => obsCaptureBlocker(platform(), p.obsVersion);
  const [mode, setMode] = createSignal<ObsWinMode>('game');
  const [ready, setReady] = createSignal(false);
  const [note, setNote] = createSignal('');
  const [err, setErr] = createSignal('');
  const [loading, setLoading] = createSignal(false);
  // null = 壳这条路走不通（浏览器里，或壳太老没有这条命令）
  const [targets, setTargets] = createSignal<ObsTarget[] | null>(null);
  // 壳的能力检测还没出结果：这期间先不摆「列出窗口」，免得壳里闪一下又被壳的清单顶掉
  const [probing, setProbing] = createSignal(false);
  // null = 还没列过；空数组是「列过了但什么都没有」，那是另一回事（多半是授权失效），要单独提示
  const [windows, setWindows] = createSignal<ObsTarget[] | null>(null);
  const [query, setQuery] = createSignal('');

  if (p.start) {
    setProbing(inShell());
    void capabilities()
      .then((caps) => (caps.obs_targets ? listObsTargets() : null))
      .then((list) => list && setTargets(list))
      .catch(() => {
        /* 壳报不出清单就退到后两条路，不打扰用户 */
      })
      .finally(() => setProbing(false));
  }

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

  // 从清单里选：目标直接写进源，接着就开播——这会儿投的是什么已经确定了
  const pick = (t: ObsTarget) => {
    if (blocked() || loading() || !p.start) return;
    setLoading(true);
    setErr('');
    void obsCaptureToTarget(p.conn, platform(), mode(), t)
      .then((n) => {
        setNote(n);
        return p.start!();
      })
      .then(() => p.onStarted?.())
      .catch((e: unknown) => setErr(macTip(e)))
      .finally(() => setLoading(false));
  };

  // 壳的清单排在前面：它是应用级的，比窗口好选。壳给不出来（浏览器里）才问 OBS 要窗口。
  const canListWindows = () => !!p.start && platform() === 'macos' && !targets() && !probing();
  const listWindows = () => {
    if (blocked() || loading()) return;
    setLoading(true);
    setErr('');
    void listObsWindows(p.conn, platform())
      .then(setWindows)
      .catch((e: unknown) => setErr(macTip(e)))
      .finally(() => setLoading(false));
  };

  const list = () => targets() ?? windows();

  /**
   * 按应用名分组：macOS 的窗口条目名是「[应用名] 窗口标题」，同一应用开了多个窗口要一眼可辨。
   * 认不出前缀的条目（壳给的应用级目标就是这样）app 为空串，归进无组标题那一组、原样列出，不丢。
   * 搜索匹配完整 label——应用名与窗口标题都在里面，一次匹配两样。
   */
  const groups = createMemo(() => {
    const q = query().trim().toLowerCase();
    const out: Array<{ app: string; items: Array<{ t: ObsTarget; title: string }> }> = [];
    const at = new Map<string, number>();
    for (const t of list() ?? []) {
      if (q && !t.label.toLowerCase().includes(q)) continue;
      const { app, title } = splitObsWindowLabel(t.label);
      let i = at.get(app);
      if (i === undefined) {
        i = out.length;
        at.set(app, i);
        out.push({ app, items: [] });
      }
      out[i].items.push({ t, title });
    }
    return out;
  });

  const emptyTip = () => {
    if (query().trim()) return '没有匹配的应用或窗口。';
    if (windows() && !targets())
      return 'OBS 现在只看得到自己的窗口，多半是屏幕录制授权失效了，去系统设置里重新授权后重启 OBS 再试。';
    return '没有可选的应用或窗口。';
  };

  // Windows 的声音是另一个源，抓哪个应用同样在 OBS 里选，按钮只负责把那个窗口弹出来；
  // macOS 没有这个源（画面源自带所属应用的声音），按钮也就不摆。
  const hasAudioInput = () => obsAudioSetupSpec(platform()) !== null;
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

        <Show when={list() || canListWindows()}>
          <div class="obs-cap-way">
            <Show when={canListWindows()}>
              <div class="obs-cap-foot">
                <button
                  type="button"
                  class="hit btn btn-sm"
                  classList={{ 'btn-primary': !windows(), loading: loading() }}
                  disabled={loading() || p.busy}
                  onClick={listWindows}
                >
                  {el(icon('screen', 13, 'currentColor', 1.8))} {windows() ? '重新列出窗口' : '列出 OBS 看得到的窗口'}
                </button>
                <Show when={!windows()}>
                  <span class="ig-tip obs-cap-modetip">列出来点一个就开播，不用切到 OBS。</span>
                </Show>
              </div>
            </Show>
            <Show when={list()}>
              <div class="field obs-cap-search">
                <input
                  value={query()}
                  placeholder="搜索应用或窗口"
                  autocomplete="off"
                  spellcheck={false}
                  aria-label="搜索要投的应用或窗口"
                  onInput={(ev) => setQuery(ev.currentTarget.value)}
                />
              </div>
              <div class="obs-cap-list">
                <For each={groups()} fallback={<div class="ig-tip">{emptyTip()}</div>}>
                  {(g) => (
                    <>
                      <Show when={g.app}>
                        <div class="obs-cap-group">{g.app}</div>
                      </Show>
                      <For each={g.items}>
                        {(it) => (
                          <button
                            type="button"
                            class="hit obs-cap-item"
                            classList={{ 'obs-cap-item-sub': !!g.app }}
                            disabled={loading() || p.busy}
                            onClick={() => pick(it.t)}
                          >
                            <span class="obs-cap-item-name">{it.title}</span>
                            <span class="obs-cap-item-kind">{it.t.kind === 'app' ? '应用' : '窗口'}</span>
                          </button>
                        )}
                      </For>
                    </>
                  )}
                </For>
              </div>
              <div class="ig-tip">
                选中就在 OBS 里建好「Hearth 投屏」场景、指到它并开始推流；你原有的场景与源不受影响。
                {hasAudioInput() ? '声音取自所选程序。' : '投出去的声音是该窗口所属应用的声音，不是单个窗口的。'}
              </div>
            </Show>
          </div>
        </Show>

        <div class="obs-cap-way">
          <div class="obs-cap-foot">
            <button
              type="button"
              class="hit btn btn-sm"
              classList={{ 'btn-primary': !ready() && !list(), loading: loading() }}
              disabled={loading() || p.busy}
              onClick={setup}
            >
              {el(icon('screen', 13, 'currentColor', 1.8))} {ready() ? '重新建采集源' : '在 OBS 里选采集源'}
            </button>
            <Show when={ready() && hasAudioInput()}>
              <button type="button" class="hit btn btn-sm" disabled={loading() || p.busy} onClick={openAudio}>
                选声音来源
              </button>
            </Show>
          </div>
          <Show
            when={ready()}
            fallback={
              <div class="ig-tip">
                想自己在 OBS 里挑就走这条：会建一个固定的「Hearth 投屏」场景（
                {hasAudioInput() ? '画面 + 声音两个源' : '一个画面源，画面与所属应用声音一起采集'}
                ）并切过去，然后弹出 OBS 自己的源属性窗口让你选要投的画面；你原有的场景与源不受影响。
              </div>
            }
          >
            <div class="ig-tip">{OBS_SETUP_HINT}</div>
          </Show>
        </div>

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
  start?: () => Promise<void>;
  onReady?: () => void;
  onStarted?: () => void;
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
      <ObsCaptureSection
        conn={p.conn}
        obsVersion={p.obsVersion}
        platform={p.platform}
        start={p.start}
        onReady={p.onReady}
        onStarted={p.onStarted}
      />
    </div>
  </div>
);
