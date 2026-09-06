// AFK（离开）判定：页面切到后台，或指针/键盘在 afkMinutes 分钟内没有任何动静，
// 就算「离开」，由房间页广播成参与者属性。这里只做判定，不碰引擎、不碰视图。
//
// 判定是纯本地的礼貌提示，不是权限状态：任何一次输入都立刻解除，回到前台也立刻解除。
import { loadPrefs, prefsBus } from './prefs';

export interface AfkWatch {
  afk(): boolean;
  dispose(): void;
}

// 活动事件之间的最小重算间隔：鼠标移动是高频事件，不能每一下都重排定时器
const THROTTLE_MS = 1000;

export function startAfkWatch(onChange: (afk: boolean) => void): AfkWatch {
  let minutes = loadPrefs().afkMinutes;
  let afk = false;
  let lastActive = Date.now();
  let timer = 0;

  const set = (next: boolean) => {
    if (next === afk) return;
    afk = next;
    onChange(afk);
  };

  // 只在需要的时刻醒一次：离开态无需定时器（等下一次输入），在场态按剩余时长排一次
  const schedule = () => {
    clearTimeout(timer);
    if (minutes <= 0 || afk) return;
    const due = lastActive + minutes * 60_000 - Date.now();
    timer = window.setTimeout(() => {
      if (document.visibilityState !== 'visible' || Date.now() - lastActive >= minutes * 60_000) set(true);
      else schedule(); // 期间有过输入：按新的起点再排
    }, Math.max(due, 1000));
  };

  const active = () => {
    const now = Date.now();
    if (!afk && now - lastActive < THROTTLE_MS) return;
    lastActive = now;
    set(false);
    schedule();
  };

  const onVisibility = () => {
    // 后台即离开（手机锁屏、切到别的 app）；回到前台算一次活动
    if (document.visibilityState === 'visible') active();
    else if (minutes > 0) set(true);
  };

  const onPrefs = (ev: Event) => {
    if ((ev as CustomEvent).detail !== 'afk') return;
    minutes = loadPrefs().afkMinutes;
    if (minutes <= 0) set(false);
    else if (document.visibilityState !== 'visible') set(true);
    else {
      lastActive = Date.now();
      set(false);
    }
    schedule();
  };

  const opts = { passive: true } as const;
  document.addEventListener('pointerdown', active, opts);
  document.addEventListener('pointermove', active, opts);
  document.addEventListener('keydown', active, opts);
  document.addEventListener('wheel', active, opts);
  document.addEventListener('touchstart', active, opts);
  document.addEventListener('visibilitychange', onVisibility);
  prefsBus.addEventListener('prefs', onPrefs);
  schedule();

  return {
    afk: () => afk,
    dispose() {
      clearTimeout(timer);
      document.removeEventListener('pointerdown', active);
      document.removeEventListener('pointermove', active);
      document.removeEventListener('keydown', active);
      document.removeEventListener('wheel', active);
      document.removeEventListener('touchstart', active);
      document.removeEventListener('visibilitychange', onVisibility);
      prefsBus.removeEventListener('prefs', onPrefs);
    },
  };
}
