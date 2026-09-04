// hash 路由：#/login、#/join/<code>、#/lobby、#/room/<频道名>、#/manage/<频道名>、#/admin/<tab>、#/setup。
import './style.css';
import { initScreenCodecAuto } from './prefs';
import { fetchMe, getToken, siteInfo } from './api';
import { renderAdmin } from './views/admin';
import { renderJoin } from './views/join';
import { isLobbyHash, renderLobby } from './views/lobby';
import { renderLogin } from './views/login';
import { renderManage } from './views/manage';
import { renderRoom } from './views/room';
import { closeSettings } from './views/settings';
import { renderSetup } from './views/setup';

// 登录后回跳用：main.ts 判定未登录时记，login.ts 登录成功后读取并清除（同一把 key）
const NEXT_KEY = 'hearth_next';

// 每次 route() 自增的代际号：异步渲染函数在每个 await 之后拿 alive() 校验，
// 过期（页面已经切走）就不再碰 DOM，替代原来「渲染一半页面已经不在了」的裸奔写法
let gen = 0;

async function route() {
  gen++;
  const myGen = gen;
  const alive = () => gen === myGen;

  const app = document.getElementById('app')!;
  const hash = location.hash || '#/lobby';
  const authed = getToken() !== null;

  closeSettings(); // 换路由时收掉设置浮层

  if (!app.firstElementChild) {
    app.innerHTML = '<div class="auth-page"><div class="auth-card" aria-live="polite">正在连接服务器…</div></div>';
  }

  // 首启向导：users 表为空时一切路由都让位给 #/setup；走完（needs_setup 变 false）后向导页本身即失效
  let needsSetup = false;
  try {
    needsSetup = (await siteInfo()).needs_setup;
  } catch {
    // 连不上服务器时按「不需要向导」继续，各页面自己有连接失败态
  }
  if (gen !== myGen) return;
  if (needsSetup && !hash.startsWith('#/setup')) {
    location.replace('#/setup');
    return;
  }
  if (!needsSetup && hash.startsWith('#/setup')) {
    location.replace(authed ? '#/lobby' : '#/login');
    return;
  }
  if (hash.startsWith('#/setup')) {
    document.title = '初始设置 · Hearth';
    renderSetup(app, alive);
    return;
  }

  if (hash.startsWith('#/join/')) {
    document.title = '加入 Hearth';
    void renderJoin(app, decodeURIComponent(hash.slice('#/join/'.length)), alive);
    return;
  }

  if (!authed && !hash.startsWith('#/login')) {
    sessionStorage.setItem(NEXT_KEY, hash);
    document.title = '登录 · Hearth';
    location.replace('#/login'); // replace 而非 push：避免返回键在登录页与目标页之间死循环
    return;
  }
  if (authed && hash.startsWith('#/login')) {
    location.replace('#/lobby');
    return;
  }

  if (hash.startsWith('#/room/')) {
    const channel = decodeURIComponent(hash.slice('#/room/'.length));
    document.title = `${channel} · Hearth`;
    void renderRoom(app, channel);
  } else if (hash.startsWith('#/manage/')) {
    document.title = '频道管理 · Hearth';
    void renderManage(app, decodeURIComponent(hash.slice('#/manage/'.length)));
  } else if (hash.startsWith('#/admin')) {
    document.title = '管理后台 · Hearth';
    const tab = hash.slice('#/admin'.length).replace(/^\//, '') || 'status';
    void renderAdmin(app, tab as Parameters<typeof renderAdmin>[1]);
  } else if (hash.startsWith('#/login')) {
    document.title = '登录 · Hearth';
    renderLogin(app);
  } else if (isLobbyHash(hash)) {
    document.title = '大厅 · Hearth';
    void renderLobby(app, alive);
  } else {
    location.replace('#/lobby'); // 未知 hash：不再静默当大厅渲染
  }
}

window.addEventListener('hashchange', () => void route());
// 先画首屏，再后台校准用户信息（拿 is_admin / 改名后的新名字），失败静默——
// 401 已由 api.ts 统一处理（记回跳、toast、跳登录页），这里不用再兜底
void route();
if (getToken()) {
  void fetchMe()
    .then(() => window.dispatchEvent(new Event('hearth:user')))
    .catch(() => {});
}

// 投屏编码默认值按本机能力自动选择（硬编优先；用户手选过则不动）
void initScreenCodecAuto();
