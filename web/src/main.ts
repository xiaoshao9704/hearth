// hash 路由：#/login、#/join/<code>、#/lobby、#/room/<频道名>、#/manage/<频道名>、#/admin/<tab>。
import './style.css';
import { fetchMe, getToken } from './api';
import { renderAdmin } from './views/admin';
import { renderJoin } from './views/join';
import { renderLobby } from './views/lobby';
import { renderLogin } from './views/login';
import { renderManage } from './views/manage';
import { renderRoom } from './views/room';
import { closeSettings } from './views/settings';

export function navigate(path: string) {
  location.hash = path;
}

function route() {
  const app = document.getElementById('app')!;
  const hash = location.hash || '#/lobby';
  const authed = getToken() !== null;

  closeSettings(); // 换路由时收掉设置浮层

  if (hash.startsWith('#/join/')) {
    void renderJoin(app, decodeURIComponent(hash.slice('#/join/'.length)));
    return;
  }
  if (!authed && !hash.startsWith('#/login')) {
    navigate('#/login');
    return;
  }
  if (authed && hash.startsWith('#/login')) {
    navigate('#/lobby');
    return;
  }

  if (hash.startsWith('#/room/')) {
    void renderRoom(app, decodeURIComponent(hash.slice('#/room/'.length)));
  } else if (hash.startsWith('#/manage/')) {
    void renderManage(app, decodeURIComponent(hash.slice('#/manage/'.length)));
  } else if (hash.startsWith('#/admin')) {
    const tab = hash.slice('#/admin'.length).replace(/^\//, '') || 'status';
    void renderAdmin(app, tab as Parameters<typeof renderAdmin>[1]);
  } else if (hash.startsWith('#/login')) {
    renderLogin(app);
  } else {
    void renderLobby(app);
  }
}

window.addEventListener('hashchange', route);
// 进应用先校准一次本地用户信息（拿 is_admin / 改名后的新名字）
if (getToken()) {
  void fetchMe()
    .catch(() => {})
    .finally(route);
} else {
  route();
}
