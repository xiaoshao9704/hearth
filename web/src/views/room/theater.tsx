// 剧场模式：画面占满内容区，侧栏/名册卡片/聊天抽屉让位，顶栏与控制栏改成浮层，
// 无操作 3 秒淡出、动一下就回来（偏好 theaterAutoHide 可关掉自动隐藏）。
//
// 「淡出」只动透明度与 pointer-events，DOM 不卸载：所有既有控件在剧场里仍然存在，
// 移动鼠标/触摸就能点到——布局改变不许让任何一个按钮变得够不着。
//
// 全屏是独立开关（快捷键 F），但两者常一起用：只有「由剧场发起的那次全屏」被退出时
// 才顺带退出剧场，别人（卡片全屏）退出全屏不受影响。
import { createSignal, Show } from 'solid-js';
import { loadPrefs, savePrefs } from '../../prefs';
import { el } from '../../ui';
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

// 两个只在本批用到的图标：不进 ui.ts 的公共表，避免和别的批次改同一张表撞车
function theaterIcon(size = 17): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 4.5h15v11h-15z"/><path d="M6.5 4.5v11M13.5 4.5v11"/></svg>`;
}

function pipIcon(size = 17): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 4h15v12h-15z"/><path d="M10 9.5h6V15h-6z"/></svg>`;
}

// 控制栏里的舞台视图开关：剧场 / 全屏 / 画中画（+ 剧场内的「固定控制栏」）。
// 放控制栏是刻意的：三种布局下控制栏都在，按钮因此始终可达。
export function StageViewButtons(props: { theater: TheaterCtl; pip: PipCtl }) {
  return (
    <>
      <button
        class="hit ctl-square"
        classList={{ on: props.theater.on() }}
        title={props.theater.on() ? '退出剧场模式（T）' : '剧场模式（T）'}
        aria-label={props.theater.on() ? '退出剧场模式' : '剧场模式'}
        onClick={() => props.theater.toggle()}
      >
        {el(theaterIcon())}
        <span class="ctl-mobile-label">剧场</span>
      </button>
      <button
        class="hit ctl-square"
        classList={{ on: props.theater.fullscreen() }}
        title={props.theater.fullscreen() ? '退出全屏（F）' : '全屏（F）'}
        aria-label={props.theater.fullscreen() ? '退出全屏' : '全屏'}
        onClick={() => props.theater.toggleFullscreen()}
      >
        {el(`<svg width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.5 7.5v-4h4M12.5 3.5h4v4M16.5 12.5v4h-4M7.5 16.5h-4v-4"/></svg>`)}
        <span class="ctl-mobile-label">全屏</span>
      </button>
      <Show when={props.pip.supported()}>
        <button
          class="hit ctl-square"
          classList={{ on: props.pip.active() }}
          title={props.pip.active() ? '收回画中画' : '画中画'}
          aria-label={props.pip.active() ? '收回画中画' : '画中画'}
          onClick={() => void props.pip.toggle()}
        >
          {el(pipIcon())}
          <span class="ctl-mobile-label">画中画</span>
        </button>
      </Show>
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
