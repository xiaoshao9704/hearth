// 管理后台（服务器级，仅管理员）：服务状态 / 服务参数 / 用户 / 房间 / 审计。
// Solid 渲染：每个配置组与实例表单各持一份局部输入状态，保存一处或展开表单不再重建整页，
// 别处未提交的输入不会被清掉；配置组按「当前输入 vs 已保存值」自己算脏，脏时才允许保存并拦截切 tab。
import { createEffect, createSignal, For, onCleanup, Show } from 'solid-js';
import { render } from 'solid-js/web';
import { adminOverview, getUser } from '../../api';
import type { AdminOverview } from '../../api';
import { avatarHtml, el, icon, menuButtonHtml, wireMenuButton } from '../../ui';
import { AuditTab } from './audit';
import { ConfigTab } from './config-tab';
import { RoomsTab } from './rooms-tab';
import { StatusTab } from './status-tab';
import { UsersTab } from './users-tab';

export type Tab = 'status' | 'config' | 'users' | 'rooms' | 'audit';

const NAV: { id: Tab; label: string; icon: string; sub: string }[] = [
  { id: 'status', label: '服务状态', icon: 'pulse', sub: '常驻进程与宿主资源' },
  { id: 'config', label: '服务参数', icon: 'gear', sub: '组件地址与注册策略' },
  { id: 'users', label: '用户', icon: 'users', sub: '账号、角色与启停' },
  { id: 'rooms', label: '房间', icon: 'volume', sub: '频道、房主与可见性' },
  { id: 'audit', label: '审计', icon: 'shield', sub: '管制动作的流水记录' },
];

export async function renderAdmin(root: HTMLElement, tab: Tab) {
  const me = getUser();
  if (!me?.is_admin) {
    location.hash = '#/lobby';
    return;
  }
  if (!NAV.some((n) => n.id === tab)) tab = 'status';
  const meta = NAV.find((n) => n.id === tab)!;

  const [dirtyGroups, setDirtyGroups] = createSignal<string[]>([]);
  const [pendingNav, setPendingNav] = createSignal(''); // 待确认的目标路由（有未保存改动时切 tab 先拦一道）
  // 侧栏徽章数字：只是展示，专门拉一次 overview，不与各 tab 内部的加载状态耦合
  const [navOv, setNavOv] = createSignal<AdminOverview>();
  void adminOverview()
    .then(setNavOv)
    .catch(() => {});

  const closeNav = () => root.querySelector('.app-frame')?.classList.remove('nav-open');
  const guardNav = (ev: MouseEvent, href: string) => {
    if (dirtyGroups().length === 0 || href === location.hash) return;
    ev.preventDefault();
    closeNav(); // 抽屉开着时先关掉，让拦截条露出来
    setPendingNav(href);
  };

  const App = () => {
    // 有未保存改动时刷新/关标签页会静默丢改动，拦一下；挂在组件内部才能随 dispose() 正常清理
    createEffect(() => {
      if (dirtyGroups().length === 0) return;
      const onBeforeUnload = (ev: BeforeUnloadEvent) => {
        ev.preventDefault();
        ev.returnValue = '';
      };
      window.addEventListener('beforeunload', onBeforeUnload);
      onCleanup(() => window.removeEventListener('beforeunload', onBeforeUnload));
    });
    return (
    <div class="app-frame">
      <div class="nav-scrim" onClick={closeNav}></div>
      <aside class="sidebar sidebar-admin">
        <div class="sidebar-head">
          {el(icon('shield', 17, 'var(--ember)'))}
          <div style="display:flex;flex-direction:column;gap:1px">
            <div style="font-size:13.5px;font-weight:700;letter-spacing:0.04em">管理后台</div>
            <div class="mono" style="font-size:9.5px;color:var(--text-2)">{location.host}</div>
          </div>
        </div>
        <div class="sidebar-body" style="gap:2px" id="admin-nav">
          <For each={NAV}>
            {(n) => (
              <a
                class="hit nav-row"
                classList={{ on: n.id === tab }}
                href={`#/admin/${n.id}`}
                aria-current={n.id === tab ? 'page' : undefined}
                onClick={(ev) => guardNav(ev, `#/admin/${n.id}`)}
              >
                {el(icon(n.icon, 16, n.id === tab ? 'var(--ember)' : 'var(--text-2)', 1.6))}
                <span style="flex-grow:1">{n.label}</span>
                <Show when={navOv() && (n.id === 'users' || n.id === 'rooms')}>
                  <span class="badge-n mono">{n.id === 'users' ? navOv()!.users : navOv()!.channels}</span>
                </Show>
              </a>
            )}
          </For>
        </div>
        <div style="padding:10px;border-top:1px solid var(--line-soft)">
          <a class="hit back-row" href="#/lobby" onClick={(ev) => guardNav(ev, '#/lobby')}>
            {el(icon('back', 16, 'var(--text-2)', 1.6))}
            <span style="flex-grow:1">返回 Hearth</span>
          </a>
        </div>
      </aside>
      <div class="content">
        <header class="topbar topbar-lg">
          {el(menuButtonHtml())}
          <h1 style="font-size:16px">{meta.label}</h1>
          <span class="sub" style="color:var(--text-2)">{meta.sub}</span>
          <div class="spacer"></div>
          <div style="display:flex;align-items:center;gap:8px;padding:5px 10px 5px 5px;border-radius:8px;background:var(--bg-3);border:1px solid var(--line)">
            {el(avatarHtml(me.username, 'avatar avatar-sm'))}
            <span style="font-size:12px;color:var(--text-1)">{me.username} · 管理员</span>
          </div>
        </header>
        <div class="page-body" id="admin-body">
          <Show when={pendingNav() && dirtyGroups().length > 0}>
            <div class="notice-bad">
              <span style="flex-grow:1">有 {dirtyGroups().length} 个配置组还没保存，离开这一页会丢弃这些改动。</span>
              <button
                class="hit btn btn-sm btn-danger"
                onClick={() => {
                  const href = pendingNav();
                  setPendingNav('');
                  setDirtyGroups([]);
                  location.hash = href;
                }}
              >
                放弃改动
              </button>
              <button class="hit btn btn-sm" onClick={() => setPendingNav('')}>
                留下
              </button>
            </div>
          </Show>
          {tab === 'status' ? (
            <StatusTab />
          ) : tab === 'config' ? (
            <ConfigTab onDirty={setDirtyGroups} />
          ) : tab === 'users' ? (
            <UsersTab />
          ) : tab === 'audit' ? (
            <AuditTab />
          ) : (
            <RoomsTab />
          )}
        </div>
      </div>
    </div>
    );
  };

  // 挂在自建容器里而不是 root：dispose() 会清空容器内容，而路由切换时新视图已经写进 root 了，
  // 直接挂 root 会让后到的 dispose 把新视图一起清掉。
  root.innerHTML = '';
  const host = document.createElement('div');
  host.className = 'view-host';
  root.appendChild(host);
  const dispose = render(App, host);
  const unwireMenu = wireMenuButton(root);

  const myHash = location.hash;
  const onHashChange = () => {
    if (location.hash !== myHash) {
      window.removeEventListener('hashchange', onHashChange);
      dispose();
      host.remove();
      unwireMenu();
    }
  };
  window.addEventListener('hashchange', onHashChange);
}
