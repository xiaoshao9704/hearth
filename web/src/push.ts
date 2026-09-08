// 离线推送（Web Push）的订阅侧。
//
// 两条约束：
//  1. **开关状态以浏览器为准**：`reg.pushManager.getSubscription()` 是唯一真相，
//     本地不存布尔副本——浏览器会自己轮换/撤销订阅，本地副本必然对不上。
//  2. **不支持就如实说明**：iPhone 上只有"添加到主屏幕"后的 PWA 才有推送权限，
//     普通 Safari 标签页里连 PushManager 都没有，这时该给出理由而不是让开关静默失效。
import { apiRequest } from './api';

// 用户在设置里主动关过离线推送时写 '1'：自动订阅路径（授权瞬间、页面启动补订阅）看到就不再打扰。
// 手动打开时清掉——这两个是本模块唯一的写点，键名不对外暴露。
const OFF_KEY = 'hearth_push_off';

function isOptedOut(): boolean {
  try {
    return localStorage.getItem(OFF_KEY) === '1';
  } catch {
    return false; // 存储不可用（隐私模式等）时退化为"每次都可自动订阅"
  }
}

// setPushOptOut 手动开关的落点：关闭时 off=true，打开时 off=false。
export function setPushOptOut(off: boolean): void {
  try {
    if (off) localStorage.setItem(OFF_KEY, '1');
    else localStorage.removeItem(OFF_KEY);
  } catch {
    // 忽略：存储不可用不影响当次订阅/退订本身
  }
}

// urlBase64ToUint8Array VAPID 公钥（base64url）转 applicationServerKey 要的字节
function urlBase64ToUint8Array(base64: string): Uint8Array {
  const padded = base64.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (base64.length % 4)) % 4);
  const raw = atob(padded);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

export function isSupported(): boolean {
  return 'serviceWorker' in navigator && 'PushManager' in window && typeof Notification !== 'undefined';
}

// iOS 上没装成 PWA：Safari 标签页里拿不到推送权限，文案要点明先添加到主屏幕
export function needsInstall(): boolean {
  const ua = navigator.userAgent;
  const ios = /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1);
  const standalone = (navigator as unknown as { standalone?: boolean }).standalone;
  return ios && standalone === false && !isSupported();
}

// unsupportedReason 开关关不上时给用户的说法（null = 支持）
export function unsupportedReason(): string | null {
  if (isSupported()) return null;
  if (needsInstall()) return '在 iPhone / iPad 上要先把网页「添加到主屏幕」，从那个图标打开后才能开离线推送。';
  if (!('serviceWorker' in navigator)) return '这个浏览器（或当前的 http 访问方式）不支持 Service Worker，收不到离线推送。';
  return '这个浏览器不支持 Web Push，收不到离线推送。';
}

async function registration(): Promise<ServiceWorkerRegistration | null> {
  if (!isSupported()) return null;
  try {
    return (await navigator.serviceWorker.ready) ?? null;
  } catch {
    return null;
  }
}

// state 当前是否已订阅（问浏览器，不问本地）
export async function state(): Promise<boolean> {
  const reg = await registration();
  if (!reg) return false;
  try {
    return (await reg.pushManager.getSubscription()) !== null;
  } catch {
    return false;
  }
}

function report(sub: PushSubscription): Promise<void> {
  const json = sub.toJSON() as { endpoint?: string; keys?: { p256dh?: string; auth?: string } };
  return apiRequest<void>('/api/push/subscribe', {
    method: 'POST',
    body: { endpoint: json.endpoint, keys: { p256dh: json.keys?.p256dh, auth: json.keys?.auth } },
  });
}

// subscribe 申请权限 → 向浏览器订阅 → 报给服务端。失败抛错，调用方给 toast。
export async function subscribe(): Promise<void> {
  const reason = unsupportedReason();
  if (reason) throw new Error(reason);
  const reg = await registration();
  if (!reg) throw new Error('Service Worker 还没准备好，刷新页面再试一次。');
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') throw new Error('浏览器没有给通知权限，离线推送开不了。');
  const existing = await reg.pushManager.getSubscription();
  if (existing) {
    await report(existing); // 已有订阅：只是把它（可能已轮换的地址）报一遍
    return;
  }
  const { public_key: key } = await apiRequest<{ public_key: string }>('/api/push/vapid');
  if (!key) throw new Error('服务端还没有推送密钥。');
  const sub = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(key) as BufferSource,
  });
  try {
    await report(sub);
  } catch (err) {
    await sub.unsubscribe().catch(() => {}); // 服务端没记下就别在浏览器留个孤儿订阅
    throw err;
  }
}

// unsubscribe 退订：浏览器与服务端各退一次（服务端失败不影响本机已退）
export async function unsubscribe(): Promise<void> {
  const reg = await registration();
  const sub = reg ? await reg.pushManager.getSubscription() : null;
  if (!sub) return;
  const endpoint = sub.endpoint;
  await sub.unsubscribe().catch(() => {});
  await apiRequest<void>('/api/push/subscribe', { method: 'DELETE', body: { endpoint } }).catch(() => {});
}

// syncSubscription 页面启动时的静默对账：浏览器里还有订阅就报一次
// （地址可能被浏览器轮换过，服务端那条就成了死地址）。没订阅什么也不做。
export async function syncSubscription(): Promise<void> {
  const reg = await registration();
  if (!reg) return;
  try {
    const sub = await reg.pushManager.getSubscription();
    if (sub) await report(sub);
  } catch {
    // 对账失败不影响任何功能：下次启动或用户手动开关时再来
  }
}

// autoSubscribeIfAllowed 两处调用的自动订阅：通知权限刚变 granted 的那一刻、
// 以及页面启动时补一次（覆盖已授权但还没订阅的老用户）。失败静默——
// 自动路径不该弹错误打扰用户，手动开关那条路径才提示。
export async function autoSubscribeIfAllowed(): Promise<void> {
  if (!isSupported() || isOptedOut()) return;
  if (Notification.permission !== 'granted') return;
  if (await state()) return; // 已订阅：不重复走一遍
  try {
    await subscribe();
  } catch {
    // 静默
  }
}
