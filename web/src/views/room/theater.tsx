// 剧场模式：画面占满内容区，侧栏/名册卡片/聊天抽屉让位，顶栏与控制栏改成浮层，
// 无操作 3 秒淡出、动一下就回来（偏好 theaterAutoHide 可关掉自动隐藏）。
//
// 「淡出」只动透明度与 pointer-events，DOM 不卸载：所有既有控件在剧场里仍然存在，
// 移动鼠标/触摸就能点到——布局改变不许让任何一个按钮变得够不着。
//
// 全屏是独立开关（快捷键 F），但两者常一起用：只有「由剧场发起的那次全屏」被退出时
// 才顺带退出剧场，别人（卡片全屏）退出全屏不受影响。
//
// 控制栏里这三件事（剧场/全屏/画中画）对用户是一个「观看模式」选择器，见文件末尾
// 的 ViewModeControl：四态互斥，状态由这些 ctl 派生。
import { createSignal, For, onCleanup, onMount, Show } from 'solid-js';
import { Portal } from 'solid-js/web';
import { loadPrefs, savePrefs } from '../../prefs';
import { el, icon } from '../../ui';
import type { PipCtl } from './pip';

const IDLE_MS = 3000;

export interface TheaterOpts {
  hasStage: () => boolean; // 有画面可看才允许进剧场
  onNotice?: (msg: string) => void;
}

export interface TheaterCtl {
  on: () => boolean;
  fullscreen: () => boolean;
  autoHide: () => boolean;
  chromeHidden: () => boolean; // 剧场里顶栏/控制栏当前是否淡出
  toggle: () => void;
  exit: () => void;
  toggleFullscreen: () => void;
  toggleAutoHide: () => void;
  dispose: () => void;
}

export function createTheaterCtl(opts: TheaterOpts): TheaterCtl {
  const [on, setOn] = createSignal(false);
  const [idle, setIdle] = createSignal(false);
  const [fullscreen, setFullscreen] = createSignal(!!document.fullscreenElement);
  const [autoHide, setAutoHide] = createSignal(loadPrefs().theaterAutoHide);
  let idleTimer = 0;
  let fsFromTheater = false; // 这次全屏是剧场发起的：退出全屏时才连带退出剧场

  function wake() {
    clearTimeout(idleTimer);
    setIdle(false);
    if (on() && autoHide()) idleTimer = window.setTimeout(() => setIdle(true), IDLE_MS);
  }

  function exit() {
    clearTimeout(idleTimer);
    setIdle(false);
    setOn(false);
    if (fsFromTheater && document.fullscreenElement) void document.exitFullscreen().catch(() => {});
    fsFromTheater = false;
  }

  function toggle() {
    if (on()) {
      exit();
      return;
    }
    if (!opts.hasStage()) {
      opts.onNotice?.('先开始投屏或打开摄像头，才有画面可放大');
      return;
    }
    setOn(true);
    wake();
  }

  function toggleFullscreen() {
    if (document.fullscreenElement) {
      void document.exitFullscreen().catch(() => {});
      return;
    }
    fsFromTheater = on();
    const refused = () => {
      opts.onNotice?.('浏览器拒绝了全屏请求');
      fsFromTheater = false;
    };
    // requestFullscreen 被权限策略挡住时是**同步**抛 TypeError（不是 reject 的 promise），
    // 只挂 .catch 会漏成未捕获错误，还会把 fsFromTheater 留在 true
    try {
      const p = document.documentElement.requestFullscreen?.();
      if (p) void p.catch(refused);
      else refused();
    } catch {
      refused();
    }
  }

  function toggleAutoHide() {
    const v = !autoHide();
    setAutoHide(v);
    const p = loadPrefs();
    p.theaterAutoHide = v;
    savePrefs(p);
    wake();
  }

  const onActivity = () => {
    if (on()) wake();
  };
  const onFullscreenChange = () => {
    const fs = !!document.fullscreenElement;
    setFullscreen(fs);
    if (!fs && fsFromTheater) {
      fsFromTheater = false;
      exit(); // 「退出全屏自动退出剧场」
    }
  };
  document.addEventListener('pointermove', onActivity, { passive: true });
  document.addEventListener('pointerdown', onActivity, { passive: true });
  document.addEventListener('touchstart', onActivity, { passive: true });
  document.addEventListener('keydown', onActivity);
  document.addEventListener('wheel', onActivity, { passive: true });
  document.addEventListener('fullscreenchange', onFullscreenChange);

  return {
    on,
    fullscreen,
    autoHide,
    chromeHidden: () => on() && autoHide() && idle(),
    toggle,
    exit,
    toggleFullscreen,
    toggleAutoHide,
    dispose() {
      clearTimeout(idleTimer);
      document.removeEventListener('pointermove', onActivity);
      document.removeEventListener('pointerdown', onActivity);
      document.removeEventListener('touchstart', onActivity);
      document.removeEventListener('keydown', onActivity);
      document.removeEventListener('wheel', onActivity);
      document.removeEventListener('fullscreenchange', onFullscreenChange);
      if (fsFromTheater && document.fullscreenElement) void document.exitFullscreen().catch(() => {});
      fsFromTheater = false;
    },
  };
}

// 三个只在这里用到的图标：不进 ui.ts 的公共表，避免和别的批次改同一张表撞车
function layoutIcon(size = 17): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 3.5h15v13h-15z"/><path d="M2.5 7.5h15M12.5 7.5v9"/></svg>`;
}

function theaterIcon(size = 17): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 4.5h15v11h-15z"/><path d="M6.5 4.5v11M13.5 4.5v11"/></svg>`;
}

function pipIcon(size = 17): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 4h15v12h-15z"/><path d="M10 9.5h6V15h-6z"/></svg>`;
}

// ---- 观看模式：普通 / 剧场 / 全屏 / 画中画 四态互斥 ----
// 三个独立开关（剧场、全屏、画中画）各占控制栏一格、彼此还能叠加，用户得自己推理
// 「现在是什么状态」。收成一个控件后状态是**派生**的：三个 ctl 的现有 accessor 按
// 优先级组出当前模式，不新建信号，也不记第二份。
export type ViewMode = 'normal' | 'theater' | 'fullscreen' | 'pip';

interface ModeItem {
  mode: ViewMode;
  label: string; // 菜单项文字
  btn: string; // 控制栏按钮上的文字
  svg: () => string;
}

const MODES: ModeItem[] = [
  { mode: 'normal', label: '普通', btn: '观看', svg: layoutIcon },
  { mode: 'theater', label: '剧场', btn: '剧场', svg: theaterIcon },
  { mode: 'fullscreen', label: '全屏', btn: '全屏', svg: () => icon('fullscreen', 17, 'currentColor') },
  { mode: 'pip', label: '画中画', btn: '画中画', svg: pipIcon },
];

// 控制栏里的观看模式控件（+ 剧场内的「固定控制栏」）。
// 放控制栏是刻意的：四种布局下控制栏都在，入口因此始终可达。
export function ViewModeControl(props: { theater: TheaterCtl; pip: PipCtl; hasStage: () => boolean }) {
  const [open, setOpen] = createSignal(false);
  let btnEl!: HTMLButtonElement;

  // 派生态：画中画 > 全屏 > 剧场 > 普通
  const mode = (): ViewMode =>
    props.pip.active() ? 'pip' : props.theater.fullscreen() ? 'fullscreen' : props.theater.on() ? 'theater' : 'normal';
  const cur = () => MODES.find((m) => m.mode === mode()) ?? MODES[0];
  // 没有画面时剧场/画中画无从谈起：菜单里置灰（快捷键仍走各自 ctl 的提示路径）
  const usable = (m: ViewMode) => m === 'normal' || m === 'fullscreen' || props.hasStage();

  // 互斥：切进某个模式前先把别的退掉。全屏是剧场的上一档——进全屏顺带开剧场，于是
  // toggleFullscreen 记下的 fsFromTheater 为真，退出全屏会连剧场一起退回普通。
  function exitOthers(keep: ViewMode) {
    if (keep !== 'pip' && props.pip.active()) props.pip.close();
    if (keep !== 'theater' && keep !== 'fullscreen' && props.theater.on()) props.theater.exit();
    if (keep !== 'fullscreen' && props.theater.fullscreen()) props.theater.toggleFullscreen();
  }

  async function setMode(next: ViewMode) {
    setOpen(false);
    if (next === mode()) return;
    exitOthers(next);
    if (next === 'theater' && !props.theater.on()) props.theater.toggle();
    if (next === 'fullscreen') {
      if (props.hasStage() && !props.theater.on()) props.theater.toggle();
      if (!props.theater.fullscreen()) props.theater.toggleFullscreen();
    }
    if (next === 'pip') await props.pip.toggle();
  }

  return (
    <>
      <div class="view-mode-wrap">
        <button
          ref={btnEl}
          id="view-mode-btn"
          class="hit ctl-pill"
          classList={{ on: mode() !== 'normal' }}
          aria-haspopup="menu"
          aria-expanded={open()}
          title={`观看模式：${cur().label}`}
          aria-label={`观看模式：${cur().label}`}
          onClick={() => setOpen((o) => !o)}
        >
          {el(cur().svg())}
          <span class="pill-label">{cur().btn}</span>
        </button>
        {/* 浮层必须挂到 body：剧场里控制栏会整条淡出（opacity + pointer-events），
            菜单作为它的子节点会跟着淡掉，开着的菜单就点不动了 */}
        <Show when={open()}>
          <Portal>
            <ViewModeMenu
              anchor={btnEl}
              mode={mode}
              usable={usable}
              pipSupported={() => !!props.pip.supported()}
              onPick={(m) => void setMode(m)}
              onClose={() => setOpen(false)}
            />
          </Portal>
        </Show>
      </div>
      <Show when={props.theater.on()}>
        <button
          class="hit ctl-square"
          classList={{ on: !props.theater.autoHide() }}
          title={props.theater.autoHide() ? '固定控制栏（不再自动隐藏）' : '控制栏恢复自动隐藏'}
          aria-label="控制栏自动隐藏"
          onClick={() => props.theater.toggleAutoHide()}
        >
          {el(
            `<svg width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10 3v9M6.5 8.5L10 12l3.5-3.5M4 16.5h12"/></svg>`,
          )}
          <span class="ctl-mobile-label">{props.theater.autoHide() ? '固定栏' : '自动隐藏'}</span>
        </button>
      </Show>
    </>
  );
}

// 浮层壳复用 .user-menu；位置按触发器算并夹进视口（控制栏在底部，默认往上开）
function ViewModeMenu(p: {
  anchor: HTMLElement;
  mode: () => ViewMode;
  usable: (m: ViewMode) => boolean;
  pipSupported: () => boolean;
  onPick: (m: ViewMode) => void;
  onClose: () => void;
}) {
  let box!: HTMLDivElement;

  onMount(() => {
    const r = p.anchor.getBoundingClientRect();
    const w = box.offsetWidth;
    const h = box.offsetHeight;
    box.style.left = `${Math.max(8, Math.min(r.left, window.innerWidth - w - 8))}px`;
    const above = r.top - h - 6;
    box.style.top = `${above < 8 ? Math.min(r.bottom + 6, window.innerHeight - h - 8) : above}px`;
  });

  const onDoc = (ev: Event) => {
    if (!box.contains(ev.target as Node) && !p.anchor.contains(ev.target as Node)) p.onClose();
  };
  const onKey = (ev: KeyboardEvent) => {
    // 吞掉这次 Escape：房间页的快捷键也听 Escape（退剧场），关菜单不该顺带退出模式
    if (ev.key !== 'Escape') return;
    ev.stopPropagation();
    p.onClose();
  };
  // 延后注册：开菜单那次 pointerdown 还没冒泡完，立刻注册会自己把自己关掉
  const armTimer = setTimeout(() => document.addEventListener('pointerdown', onDoc, true));
  document.addEventListener('keydown', onKey, true);
  onCleanup(() => {
    clearTimeout(armTimer);
    document.removeEventListener('pointerdown', onDoc, true);
    document.removeEventListener('keydown', onKey, true);
  });

  return (
    <div ref={box} id="view-mode-menu" class="user-menu" role="menu">
      <div class="um-title">观看模式</div>
      <For each={MODES.filter((m) => m.mode !== 'pip' || p.pipSupported())}>
        {(m) => (
          <button
            type="button"
            class="hit um-item vm-item"
            role="menuitemradio"
            data-mode={m.mode}
            aria-checked={p.mode() === m.mode}
            classList={{ cur: p.mode() === m.mode, disabled: !p.usable(m.mode) }}
            disabled={!p.usable(m.mode)}
            onClick={() => p.onPick(m.mode)}
          >
            {el(m.svg())}
            <span class="vm-name">{m.label}</span>
            <Show when={p.mode() === m.mode}>{el(icon('check', 13, 'var(--ember)', 2))}</Show>
          </button>
        )}
      </For>
    </div>
  );
}

