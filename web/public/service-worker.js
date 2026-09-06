// 最小 service worker：只为让浏览器认这是可安装的 PWA，**不缓存任何资源**。
// 缓存前端产物会让升级后的旧页面顶着新服务端跑（版本头探测也救不回来），
// 因此这里连 fetch 事件都不监听——所有请求照常走网络。
// 纯 JS 放在 public/ 而不是 src/：service worker 必须以正确 MIME 从站点根路径提供，
// 经打包器输出的 .ts 产物做不到（见 web/src/sw.ts 的注册侧注释）。
self.addEventListener('install', () => {
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});
