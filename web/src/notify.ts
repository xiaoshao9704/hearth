// 系统通知：页面在后台时把新消息/被@/有人进房推到桌面。
// 两条约束决定了这里的做法：
//  1. 不在进页面时申请权限——那是最容易被永久拒掉的时机。第一条实时消息到达时才"备好"，
//     真正的 requestPermission 尽量落在紧接着的一次用户手势上（Firefox 只在手势里允许申请）。
//  2. 页面可见时一律不发通知：可见时该响的是提示音（audio.ts），通知只补"人不在这一页"的场景。
import { loadPrefs } from './prefs';
import { autoSubscribeIfAllowed } from './push';

// 通知的 tag：同类通知互相替换，连来十条消息桌面上只留最后一条，不堆成一摞
const TAG_CHAT = 'hearth-chat';
const TAG_JOIN = 'hearth-join';

const supported = typeof Notification !== 'undefined';
let armed = false; // 已经"备好"过申请（只做一次）
let gestureWired = false;

function permission(): NotificationPermission | 'unsupported' {
  return supported ? Notification.permission : 'unsupported';
}

function request() {
  if (!supported || Notification.permission !== 'default') return;
  try {
    // 权限从 default 变成 granted 的这一刻顺手订阅离线推送：默认开，用户不用再进设置打开一次
    void Notification.requestPermission().then((perm) => {
      if (perm === 'granted') void autoSubscribeIfAllowed();
    });
  } catch {
    // 老式回调签名的浏览器：忽略，下一次手势还会再试
  }
}

// armNotifyPermission 备好权限申请：先直接试一次（Chromium 系不要求手势），
// 同时挂一次性的手势监听兜底（Firefox 等要求手势的浏览器靠它）。
export function armNotifyPermission() {
  if (!supported || armed || Notification.permission !== 'default') return;
  armed = true;
  request();
  if (gestureWired) return;
  gestureWired = true;
  const once = () => {
    request();
    window.removeEventListener('pointerdown', once, true);
    window.removeEventListener('keydown', once, true);
  };
  window.addEventListener('pointerdown', once, true);
  window.addEventListener('keydown', once, true);
}

// show 发一条通知。优先走 Service Worker 的 showNotification：Android Chrome 只认这条路
// （页面里直接 new Notification 会抛），而且点通知的处置在 worker 侧统一（见
// public/service-worker.js 的 notificationclick：聚焦已有窗口并让它切到该频道）。
// 没有 worker（未注册成功、http 非 localhost）才回落页面通知，点击时执行 onOpen。
function show(tag: string, title: string, body: string, channel: string, onOpen: () => void) {
  if (permission() !== 'granted') return;
  const opts: NotificationOptions = { body, tag, icon: '/icons/icon-192.png', data: { channel } };
  const reg = navigator.serviceWorker?.ready;
  if (reg) {
    void reg
      .then((r) => r.showNotification(title, opts))
      .catch(() => showInPage(tag, title, body, onOpen));
    return;
  }
  showInPage(tag, title, body, onOpen);
}

function showInPage(tag: string, title: string, body: string, onOpen: () => void) {
  let n: Notification;
  try {
    n = new Notification(title, { body, tag, icon: '/icons/icon-192.png' });
  } catch {
    // 老 Android Chrome 等要求 ServiceWorkerRegistration.showNotification 的浏览器：放弃
    return;
  }
  n.onclick = () => {
    window.focus();
    onOpen();
    n.close();
  };
}

interface NotifiableMessage {
  username: string;
  content: string;
  kind: string;
}

// notifyMessage 实时到达的他人消息：页面在后台且对应开关开着时发通知。
// 无论开关如何都会"备好"权限申请——第一条消息就是最合适的申请时机。
// mentioned 由调用方按 chat/mentions.ts 判定（按名册 uid，不按用户名）并把"回复我的"
// 也算进去，保证提示音、通知与离线推送认的是同一件事。
// channel 随通知带上：点通知由 Service Worker 直达该频道。
export function notifyMessage(m: NotifiableMessage, mentioned: boolean, channel: string, onOpen: () => void) {
  armNotifyPermission();
  if (document.visibilityState === 'visible') return;
  const prefs = loadPrefs();
  if (mentioned ? !prefs.notifyMentions : !prefs.notifyMessages) return;
  const who = m.username || '有人';
  const body = m.kind === 'file' ? '发来一个文件' : m.content.slice(0, 120);
  show(TAG_CHAT, mentioned ? `${who} 提到了你` : who, body, channel, onOpen);
}

// notifyJoin 有人进房：默认关（人来人往比消息吵得多），开了才发。
export function notifyJoin(name: string, channel: string, onOpen: () => void) {
  if (document.visibilityState === 'visible') return;
  if (!loadPrefs().notifyJoins) return;
  show(TAG_JOIN, 'Hearth', `${name} 进入了房间`, channel, onOpen);
}

// notifyState 给设置面板显示当前权限状态。
export function notifyState(): NotificationPermission | 'unsupported' {
  return permission();
}
