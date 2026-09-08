// PWA 的 service worker 注册侧。
// worker 本体是 public/service-worker.js（原样落到站点根路径）而不是这里的 .ts：
// service worker 必须以 JS 的 MIME 从根路径提供才能拿到全站 scope，
// 打包器对 .ts 只会输出带 hash 的资源（且扩展名不是 .js），注册会被浏览器拒。
// 这个 worker 不缓存任何东西，作用是让浏览器给出「安装」入口，并承接通知与离线推送。
import { getToken } from './api';
import { autoSubscribeIfAllowed, syncSubscription } from './push';

const SW_URL = '/service-worker.js';

export function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return;
  // 点通知打开/聚焦窗口时 worker 会 postMessage 过来：切到那个频道
  // （只挂一次，路由是 hash，切频道不重建页面）
  navigator.serviceWorker.addEventListener('message', (ev: MessageEvent) => {
    const data = ev.data as { type?: string; channel?: string } | null;
    if (data?.type !== 'open-channel' || !data.channel) return;
    location.hash = `#/room/${encodeURIComponent(data.channel)}`;
  });
  // http 非 localhost 下 SW 不可用（浏览器限制），注册失败只是没有安装入口，不影响功能
  window.addEventListener('load', () => {
    void navigator.serviceWorker
      .register(SW_URL, { scope: '/' })
      .then(() => {
        // 浏览器可能已经轮换过推送地址：已登录且本机还有订阅就静默报一次；
        // 已授权但还没订阅（老用户、没手动关过）则补一次自动订阅
        if (getToken()) {
          void syncSubscription();
          void autoSubscribeIfAllowed();
        }
      })
      .catch(() => {});
  });
}
