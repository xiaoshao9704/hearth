// 应用壳：左侧栏（频道导航 / 连接状态 / 用户栏）+ 内容区。大厅与房间共用。
import { getUser, listChannels } from './api';
import type { Channel } from './api';
import { loadPrefs, savePrefs, notifyPrefsChanged, prefsBus } from './prefs';
import { avatarHtml, esc, flameLogo, icon, micIcon } from './ui';
import { openSettings } from './views/settings';

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
      <aside class="sidebar">
        <div class="sidebar-head">
          ${flameLogo()}
          <div style="display:flex;flex-direction:column;gap:1px">
            <div class="brand">HEARTH</div>
            <div class="host mono">${esc(location.host || 'localhost')}</div>
          </div>
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
        <div class="user-bar">
          <div style="position:relative" id="side-avatar-wrap">
            ${avatarHtml(user?.username ?? '?', 'avatar')}
            <div class="presence-dot"></div>
          </div>
          <div class="who">
            <div class="name" id="side-name">${esc(user?.username ?? '')}</div>
            <div class="meta mono">本机</div>
          </div>
          <button class="hit mini-btn" id="side-mic" title="麦克风偏好" style="${opts.activeChannel ? 'display:none' : ''}"></button>
          <button class="hit mini-btn boxed" id="side-gear" title="设置">${icon('gear', 16, 'var(--text-1)', 1.6)}</button>
        </div>
      </aside>
      <div class="content" id="shell-content"></div>
    </div>
  `;

  const frame = root.querySelector<HTMLDivElement>('.app-frame')!;
  const content = root.querySelector<HTMLDivElement>('#shell-content')!;
  const channelsEl = root.querySelector<HTMLDivElement>('#side-channels')!;
  const connBox = root.querySelector<HTMLDivElement>('#conn-box')!;
  const connTitle = root.querySelector<HTMLSpanElement>('#conn-title')!;
  const connMeta = root.querySelector<HTMLDivElement>('#conn-meta')!;
  const micBtn = root.querySelector<HTMLButtonElement>('#side-mic')!;
  const avatarWrap = root.querySelector<HTMLDivElement>('#side-avatar-wrap')!;
  const nameEl = root.querySelector<HTMLDivElement>('#side-name')!;

  root.querySelector('.nav-scrim')!.addEventListener('click', () => frame.classList.remove('nav-open'));

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

  async function refreshChannels() {
    try {
      paintChannels(await listChannels());
    } catch {
      // 列表拉取失败保持现状
    }
  }
  void refreshChannels();

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
      prefsBus.removeEventListener('prefs', onPrefs);
      window.removeEventListener('hearth:user', onUser);
    },
  };
}

// 顶栏里的移动端菜单按钮（各视图往 topbar 里塞）
export function menuButtonHtml(): string {
  return `<button class="hit btn btn-icon menu-btn" id="menu-btn">${icon('menu', 16, 'var(--text-1)')}</button>`;
}

export function wireMenuButton(root: HTMLElement) {
  root.querySelector('#menu-btn')?.addEventListener('click', () => {
    root.querySelector('.app-frame')?.classList.toggle('nav-open');
  });
}
