// 安装为应用（PWA）：攒住 beforeinstallprompt 事件 + 各平台判定。
//
// 必须在 main.ts 顶部尽早 import——beforeinstallprompt 在页面加载早期触发，
// 模块加载晚了收不到（Safari/iOS 从不触发这个事件，只能给操作说明）。

interface BeforeInstallPromptEvent extends Event {
  prompt(): Promise<void>;
  userChoice: Promise<{ outcome: 'accepted' | 'dismissed' }>;
}

let deferred: BeforeInstallPromptEvent | null = null;
let installed = false;
const waiters: (() => void)[] = [];

window.addEventListener('beforeinstallprompt', (e) => {
  e.preventDefault();
  deferred = e as BeforeInstallPromptEvent;
  waiters.splice(0).forEach((cb) => cb());
});

window.addEventListener('appinstalled', () => {
  deferred = null;
  installed = true;
});

export function isStandalone(): boolean {
  return (
    matchMedia('(display-mode: standalone)').matches ||
    (navigator as unknown as { standalone?: boolean }).standalone === true
  );
}

export type InstallMode = 'prompt' | 'ios' | 'mac-safari' | 'manual' | 'none';

// installMode 当前该给用户看哪种安装引导：
// prompt = 浏览器已递来 beforeinstallprompt，可以直接弹系统安装对话框；
// ios/mac-safari = Safari 系不发这个事件，只能给操作说明；manual = 其余浏览器（Firefox 等）同样只能给说明。
export function installMode(): InstallMode {
  if (installed || isStandalone()) return 'none';
  if (deferred) return 'prompt';
  const ua = navigator.userAgent;
  const ios = /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1);
  if (ios) return 'ios';
  if (/Macintosh/.test(ua) && /Safari/.test(ua) && !/Chrome|Chromium|Edg/.test(ua)) return 'mac-safari';
  return 'manual';
}

// promptInstall 弹系统安装对话框，读用户的选择。deferred 用一次就废（浏览器规则），
// 调用前先摘掉，避免后续误用同一个已经 prompt() 过的事件。
export async function promptInstall(): Promise<'accepted' | 'dismissed' | 'unavailable'> {
  if (!deferred) return 'unavailable';
  const e = deferred;
  deferred = null;
  await e.prompt();
  const { outcome } = await e.userChoice;
  if (outcome === 'accepted') installed = true; // appinstalled 事件通常紧随其后，这里先乐观置位给 UI 用
  return outcome;
}

// onInstallAvailable：deferred 到达时回调一次。卡片可能先渲染、事件后到，
// 已经到达就直接同步回调，不必等下一轮事件。
export function onInstallAvailable(cb: () => void): void {
  if (deferred) {
    cb();
    return;
  }
  waiters.push(cb);
}
