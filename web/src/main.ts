// 极简 hash 路由：#/login、#/channels、#/room/<频道名>。
import './style.css';
import { getToken } from './api';
import { renderLogin } from './views/login';
import { renderChannels } from './views/channels';
import { renderRoom } from './views/room';

export function navigate(path: string) {
  location.hash = path;
}

function route() {
  const app = document.getElementById('app')!;
  const hash = location.hash || '#/channels';
  const authed = getToken() !== null;

  if (!authed && !hash.startsWith('#/login')) {
    navigate('#/login');
    return;
  }
  if (authed && hash.startsWith('#/login')) {
    navigate('#/channels');
    return;
  }

  if (hash.startsWith('#/room/')) {
    const name = decodeURIComponent(hash.slice('#/room/'.length));
    renderRoom(app, name);
  } else if (hash.startsWith('#/login')) {
    renderLogin(app);
  } else {
    renderChannels(app);
  }
}

window.addEventListener('hashchange', route);
route();
