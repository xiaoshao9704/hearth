// 应用壳：左侧栏（频道导航 / 连接状态 / 用户栏）+ 内容区。大厅与房间共用。
import { getUser, listChannels, logout } from './api';
import type { Channel } from './api';
import { loadPrefs, savePrefs, notifyPrefsChanged, prefsBus } from './prefs';
import { avatarHtml, confirmDialog, esc, flameLogo, icon, micIcon } from './ui';
import { wireThemeButton } from './theme';
import { openSettings } from './views/settings';

const CHANNELS_POLL_MS = 15000;

export interface ShellOptions {
  activeChannel?: string; // 当前所在频道（高亮 + 连接状态）
  connMeta?: string; // 连接状态副标题
}

export interface Shell {
  content: HTMLElement;
  setConn(live: boolean, meta: string): void;
  refreshChannels(): Promise<void>;
  destroy(): void;
}

export function renderShell(root: HTMLElement, opts: ShellOptions = {}): Shell {
  const user = getUser();
  const prefs = loadPrefs();

  root.innerHTML = `
    <div class="app-frame">
      <div class="nav-scrim"></div>
      <aside class="sidebar" id="app-sidebar">
        <div class="sidebar-head">
          <a class="hit" href="#/lobby" style="display:flex;align-items:center;gap:10px;min-width:0;color:inherit;text-decoration:none" title="回大厅">
            ${flameLogo()}
            <div style="display:flex;flex-direction:column;gap:1px;min-width:0">
              <div class="brand">HEARTH</div>
              <div class="host mono">${esc(location.host || 'localhost')}</div>
            </div>
          </a>
        </div>
        <div class="sidebar-body">
          <div>
            <div class="side-section-title">语音频道</div>
            <div id="side-channels"><div class="side-row muted">加载中…</div></div>
          </div>
        </div>
        <div class="conn-box" id="conn-box">
          <div class="conn-title"><span class="dot"></span><span id="conn-title">未连接语音</span></div>
          <div class="conn-meta mono" id="conn-meta">进入频道后自动协商</div>
        </div>
        <div class="user-bar" id="user-bar">
          <button type="button" class="hit user-bar-trigger" id="acct-trigger" aria-haspopup="menu" aria-expanded="false">
            <div style="position:relative" id="side-avatar-wrap">
              ${avatarHtml(user?.username ?? '?', 'avatar')}
              <div class="presence-dot"></div>
            </div>
            <div class="who">
              <div class="name" id="side-name">${esc(user?.username ?? '')}</div>
              <div class="meta mono">本机</div>
            </div>
          </button>
          <button class="hit mini-btn" id="side-mic" title="麦克风偏好" aria-label="麦克风偏好" style="${opts.activeChannel ? 'display:none' : ''}"></button>
          <button class="hit mini-btn" id="side-theme" aria-label="外观"></button>
          <button class="hit mini-btn boxed" id="side-gear" title="设置" aria-label="设置">${icon('gear', 16, 'var(--text-1)', 1.6)}</button>
        </div>
      </aside>
      <div class="content" id="shell-content"></div>
    </div>
  `;

  const content = root.querySelector<HTMLDivElement>('#shell-content')!;
  const channelsEl = root.querySelector<HTMLDivElement>('#side-channels')!;
  const connBox = root.querySelector<HTMLDivElement>('#conn-box')!;
  const connTitle = root.querySelector<HTMLSpanElement>('#conn-title')!;
  const connMeta = root.querySelector<HTMLDivElement>('#conn-meta')!;
  const micBtn = root.querySelector<HTMLButtonElement>('#side-mic')!;
  const avatarWrap = root.querySelector<HTMLDivElement>('#side-avatar-wrap')!;
  const nameEl = root.querySelector<HTMLDivElement>('#side-name')!;
  const userBar = root.querySelector<HTMLDivElement>('#user-bar')!;
  const acctTrigger = root.querySelector<HTMLButtonElement>('#acct-trigger')!;
  const themeBtn = root.querySelector<HTMLButtonElement>('#side-theme')!;

  wireThemeButton(themeBtn);

  // 改用户名后立刻重画侧栏名字/头像
  const onUser = () => {
    const u = getUser();
    nameEl.textContent = u?.username ?? '';
    avatarWrap.innerHTML = `${avatarHtml(u?.username ?? '?', 'avatar')}<div class="presence-dot"></div>`;
  };
  window.addEventListener('hearth:user', onUser);

  // 麦克风偏好快捷开关：只决定进房时是否自动开麦（房内开关麦不回写，房内隐藏本按钮）
  function paintMic() {
    const p = loadPrefs();
    micBtn.innerHTML = micIcon(16, !p.mic, p.mic ? 'var(--text-1)' : 'var(--red)');
  }
  paintMic();
  micBtn.addEventListener('click', () => {
    const p = loadPrefs();
    p.mic = !p.mic;
    savePrefs(p);
    notifyPrefsChanged('mic');
    paintMic();
  });
  const onPrefs = () => paintMic();
  prefsBus.addEventListener('prefs', onPrefs);

  root.querySelector('#side-gear')!.addEventListener('click', () => {
    openSettings('av', { channel: opts.activeChannel });
  });

  // ---- 用户栏小菜单：点头像/名字，账户设置 / 外观 / 退出登录 ----
  let acctMenu: HTMLDivElement | null = null;

  function closeAcctMenu() {
    if (!acctMenu) return;
    acctMenu.remove();
    acctMenu = null;
    acctTrigger.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', onOutsidePointer, true);
    document.removeEventListener('keydown', onAcctKeydown, true);
  }
  function onOutsidePointer(ev: PointerEvent) {
    if (acctMenu && !acctMenu.contains(ev.target as Node) && !acctTrigger.contains(ev.target as Node)) closeAcctMenu();
  }
  function onAcctKeydown(ev: KeyboardEvent) {
    if (ev.key === 'Escape') closeAcctMenu();
  }
  async function doLogout() {
    closeAcctMenu();
    const ok = await confirmDialog({ title: '退出登录？', body: '只退这台设备', danger: true, confirmText: '退出登录' });
    if (!ok) return;
    await logout().catch(() => {});
    location.replace('#/login');
  }
  function openAcctMenu() {
    const menu = document.createElement('div');
    menu.className = 'acct-menu';
    menu.setAttribute('role', 'menu');
    menu.innerHTML = `
      <button type="button" class="hit am-item" role="menuitem" data-act="account">${icon('user', 14, 'currentColor', 1.6)}<span>账户设置</span></button>
      <button type="button" class="hit am-item" role="menuitem" data-act="theme">${icon('sun', 14, 'currentColor', 1.6)}<span>外观</span></button>
      <button type="button" class="hit am-item danger" role="menuitem" data-act="logout">${icon('leave', 14, 'var(--red)', 1.6)}<span>退出登录</span></button>
    `;
    userBar.appendChild(menu);
    acctMenu = menu;
    acctTrigger.setAttribute('aria-expanded', 'true');
    menu.querySelector('[data-act="account"]')!.addEventListener('click', () => {
      closeAcctMenu();
      openSettings('account');
    });
    menu.querySelector('[data-act="theme"]')!.addEventListener('click', () => {
      closeAcctMenu();
      themeBtn.click(); // 复用主题按钮已有的切换 + 图标 + toast 逻辑
    });
    menu.querySelector('[data-act="logout"]')!.addEventListener('click', () => void doLogout());
    document.addEventListener('pointerdown', onOutsidePointer, true);
    document.addEventListener('keydown', onAcctKeydown, true);
  }
  acctTrigger.addEventListener('click', () => {
    if (acctMenu) closeAcctMenu();
    else openAcctMenu();
  });

  function paintChannels(channels: Channel[]) {
    if (channels.length === 0) {
      channelsEl.innerHTML = '<div class="side-row muted">还没有频道</div>';
      return;
    }
    channelsEl.innerHTML = channels
      .map((c) => {
        const on = c.name === opts.activeChannel;
        return `
          <a class="hit side-row ${on ? 'on' : ''}" href="#/room/${encodeURIComponent(c.name)}">
            ${icon('volume', 16, on ? 'var(--ember)' : 'var(--text-2)', 1.6)}
            <span style="flex-grow:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(c.name)}</span>
            ${c.invite_only ? `<span title="邀请制">${icon('shield', 12, 'var(--text-3)', 1.6)}</span>` : ''}
            <span class="count mono">${c.online > 0 ? c.online : ''}</span>
          </a>`;
      })
      .join('');
  }

  function paintChannelsError() {
    channelsEl.innerHTML = `<button type="button" class="hit side-row" id="side-channels-retry" style="width:100%">${icon('reset', 14, 'var(--text-2)', 1.7)}<span>加载失败，点击重试</span></button>`;
    channelsEl.querySelector('#side-channels-retry')!.addEventListener('click', () => void refreshChannels());
  }

  let channelsLoaded = false;
  async function refreshChannels() {
    try {
      const channels = await listChannels();
      channelsLoaded = true;
      paintChannels(channels);
    } catch {
      // 首次拉取失败给出重试入口；已有数据的轮询失败保持现状，等下一轮
      if (!channelsLoaded) paintChannelsError();
    }
  }
  void refreshChannels();

  // 频道列表 + 在线数自管轮询（房间内的侧栏也靠这个刷新，不再是进房即冻结的快照）
  const pollTimer = window.setInterval(() => {
    if (document.visibilityState === 'visible') void refreshChannels();
  }, CHANNELS_POLL_MS);
  const onVisible = () => {
    if (document.visibilityState === 'visible') void refreshChannels();
  };
  document.addEventListener('visibilitychange', onVisible);

  // 进房自动开麦偏好展示
  if (prefs.mic) paintMic();

  return {
    content,
    setConn(live, meta) {
      connBox.classList.toggle('live', live);
      connTitle.textContent = live ? '语音已连接' : '未连接语音';
      connMeta.innerHTML = meta; // 内容可信：调用方只传固定文案或 esc 过的引擎名
    },
    refreshChannels,
    destroy() {
      clearInterval(pollTimer);
      document.removeEventListener('visibilitychange', onVisible);
      prefsBus.removeEventListener('prefs', onPrefs);
      window.removeEventListener('hearth:user', onUser);
      closeAcctMenu();
    },
  };
}
