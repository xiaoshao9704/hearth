// PWA 的 service worker 注册侧。
// worker 本体是 public/service-worker.js（原样落到站点根路径）而不是这里的 .ts：
// service worker 必须以 JS 的 MIME 从根路径提供才能拿到全站 scope，
// 打包器对 .ts 只会输出带 hash 的资源（且扩展名不是 .js），注册会被浏览器拒。
// 这个 worker 不缓存任何东西，作用仅是让浏览器给出「安装」入口。
const SW_URL = '/service-worker.js';

export function registerServiceWorker() {
  if (!('serviceWorker' in navigator)) return;
  // http 非 localhost 下 SW 不可用（浏览器限制），注册失败只是没有安装入口，不影响功能
  window.addEventListener('load', () => {
    void navigator.serviceWorker.register(SW_URL, { scope: '/' }).catch(() => {});
  });
}
