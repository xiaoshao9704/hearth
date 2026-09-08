// 账户菜单：账户设置 / 外观 / 管理后台（admin+）/ 退出登录。
// 侧栏用户栏与房间、大厅顶栏的账户入口共用这一份（手机上侧栏是抽屉，顶栏那个才是能看见的入口）。
import { getUser, logout } from './api';
import { cycleTheme, getTheme, THEME_ICONS, type Theme } from './theme';
import { confirmDialog, icon, toast } from './ui';
import { openSettings } from './views/settings';

const THEME_LABELS: Record<Theme, string> = { light: '浅色', dark: '深色', auto: '跟随系统' };

let menu: HTMLDivElement | null = null;
let openAnchor: HTMLElement | null = null;

export function closeAccountMenu() {
  if (!menu) return;
  menu.remove();
  menu = null;
  openAnchor?.setAttribute('aria-expanded', 'false');
  openAnchor = null;
  document.removeEventListener('pointerdown', onOutside, true);
  document.removeEventListener('keydown', onKeydown, true);
}

function onOutside(ev: Event) {
  // 点触发器不算「点外面」：那一下由它的 click 走 openAccountMenu 的收起分支
  if (menu && !menu.contains(ev.target as Node) && !openAnchor?.contains(ev.target as Node)) closeAccountMenu();
}

function onKeydown(ev: KeyboardEvent) {
  if (ev.key === 'Escape') closeAccountMenu();
}

async function doLogout() {
  closeAccountMenu();
  const ok = await confirmDialog({ title: '退出登录？', body: '只退这台设备', danger: true, confirmText: '退出登录' });
  if (!ok) return;
  await logout().catch(() => {});
  location.replace('#/login');
}

// anchor 就是触发器本身：菜单贴着它出，再点它一次即收起
export function openAccountMenu(anchor: HTMLElement) {
  if (openAnchor === anchor) {
    closeAccountMenu();
    return;
  }
  closeAccountMenu();
  // 「管理后台」按服务端下发的 role 派生的 is_admin 显隐，前端不推导权限
  const isAdmin = getUser()?.is_admin === true;
  const themeIcon = THEME_ICONS[getTheme()];
  const box = document.createElement('div');
  box.className = 'acct-menu';
  box.setAttribute('role', 'menu');
  box.innerHTML = `
    <button type="button" class="hit am-item" role="menuitem" data-act="account">${icon('user', 14, 'currentColor', 1.6)}<span>账户设置</span></button>
    <button type="button" class="hit am-item" role="menuitem" data-act="theme">${icon(themeIcon, 14, 'currentColor', 1.6)}<span>外观</span></button>
    ${isAdmin ? `<button type="button" class="hit am-item" role="menuitem" data-act="admin">${icon('gauge', 14, 'currentColor', 1.6)}<span>管理后台</span></button>` : ''}
    <button type="button" class="hit am-item danger" role="menuitem" data-act="logout">${icon('leave', 14, 'var(--red)', 1.6)}<span>退出登录</span></button>
  `;
  document.body.appendChild(box);
  menu = box;
  openAnchor = anchor;
  anchor.setAttribute('aria-expanded', 'true');

  const r = anchor.getBoundingClientRect();
  const w = box.offsetWidth;
  const h = box.offsetHeight;
  box.style.left = `${Math.max(8, Math.min(r.right - w, window.innerWidth - w - 8))}px`;
  const below = r.bottom + 6;
  // 装不下就翻到触发器上方（侧栏用户栏贴着窗口底）
  box.style.top = `${below + h > window.innerHeight - 8 ? Math.max(8, r.top - h - 6) : below}px`;

  box.querySelector('[data-act="account"]')!.addEventListener('click', () => {
    closeAccountMenu();
    openSettings('account');
  });
  box.querySelector('[data-act="theme"]')!.addEventListener('click', () => {
    closeAccountMenu();
    const next = cycleTheme();
    toast(`外观：${THEME_LABELS[next]}`);
  });
  box.querySelector('[data-act="admin"]')?.addEventListener('click', () => {
    closeAccountMenu();
    // 房间里开的入口：管理后台走新标签页，别把正在通话的房间顶掉（与设置浮层同一口径）
    if (location.hash.startsWith('#/room/')) window.open('#/admin', '_blank', 'noopener');
    else location.hash = '#/admin';
  });
  box.querySelector('[data-act="logout"]')!.addEventListener('click', () => void doLogout());

  // 延后注册：触发菜单的那次 pointerdown 还没冒泡完，立刻注册会自己把自己关掉
  setTimeout(() => document.addEventListener('pointerdown', onOutside, true));
  document.addEventListener('keydown', onKeydown, true);
}
