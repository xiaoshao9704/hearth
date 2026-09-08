// 最小 service worker：**不缓存任何资源**，只做两件事——让浏览器认这是可安装的 PWA，
// 以及承接通知（页面通知与离线推送都从这里弹）。
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

// 同类通知互相替换：连来十条只在桌面上留最后一条（与页面侧 notify.ts 的 tag 同名）
const TAG_CHAT = 'hearth-chat';

// 图标角标的计数只活在 worker 里：推送到达 +1，回到页面由页面清零（页面拿的是真未读数）。
let badge = 0;

function setBadge(n) {
  try {
    if (n > 0) self.navigator.setAppBadge?.(n);
    else self.navigator.clearAppBadge?.();
  } catch {
    // 不支持角标的平台（多数桌面浏览器与未安装的 PWA）：忽略
  }
}

// visibleClient 有没有一个正开着且可见的页面：有就不弹通知，页内提示音/角标已经在响
async function hasVisibleClient() {
  const list = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  return list.some((c) => c.visibilityState === 'visible');
}

// push 离线推送到达：载荷形状见 server/internal/api/push.go 的 pushPayload
self.addEventListener('push', (event) => {
  event.waitUntil(
    (async () => {
      let data = {};
      try {
        data = event.data ? event.data.json() : {};
      } catch {
        // 载荷不是我们发的 JSON：当空对象处理，仍然弹一条泛化提示
      }
      if (await hasVisibleClient()) return;
      const who = data.from || '有人';
      const title = data.kind === 'reply' ? `${who} 回复了你` : `${who} 提到了你`;
      badge += 1;
      setBadge(badge);
      await self.registration.showNotification(title, {
        body: data.preview || '',
        tag: TAG_CHAT,
        icon: '/icons/icon-192.png',
        badge: '/icons/icon-192.png',
        data: { channel: data.channel || '' },
      });
    })(),
  );
});

// notificationclick 点通知直达该频道：先找已有窗口聚焦并让它切频道，没有才新开一个
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const channel = (event.notification.data && event.notification.data.channel) || '';
  event.waitUntil(
    (async () => {
      badge = 0;
      setBadge(0);
      const list = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
      if (list.length) {
        const client = list[0];
        await client.focus().catch(() => {});
        client.postMessage({ type: 'open-channel', channel });
        return;
      }
      if (channel) await self.clients.openWindow('/#/room/' + encodeURIComponent(channel));
      else await self.clients.openWindow('/');
    })(),
  );
});
