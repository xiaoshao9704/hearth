// 设置浮层骨架（Solid）：三个维度 —— 个人（跟账号/本机走）/ 频道（房主与频道管理员的成员与准入）/ 服务器（管理后台，只跳转）。
// 浮层挂在 body 上，房间内打开不断开通话。个人 pane 的内容仍是命令式渲染（settings-panes.ts），
// 这里只管导航、上下文与生命周期；频道分区直接复用 manage.tsx 的组件。
import { createEffect, createResource, createSignal, onCleanup, For, Show } from 'solid-js';
import { render } from 'solid-js/web';
import { canInvite, getUser, listChannels } from '../api';
import { el, icon } from '../ui';
import { ChannelManage } from './manage';
import { PERSONAL_PANES, renderPane } from './settings-panes';
import type { PersonalPane } from './settings-panes';

export type Pane = PersonalPane | 'channel';

export interface SettingsContext {
  backLabel?: string;
  channel?: string; // 当前所在频道：本人是房主或频道管理员时出现「频道」分区
}

let dispose: (() => void) | null = null;

export function openSettings(pane: Pane = 'av', ctx: SettingsContext = {}) {
  closeSettings();
  const host = document.createElement('div');
  host.className = 'settings-overlay';
  document.body.appendChild(host);
  const d = render(() => <SettingsOverlay pane={pane} ctx={ctx} />, host);
  dispose = () => {
    d();
    host.remove();
  };
}

export function closeSettings() {
  dispose?.();
  dispose = null;
}

function SettingsOverlay(p: { pane: Pane; ctx: SettingsContext }) {
  const onKeydown = (ev: KeyboardEvent) => {
    if (ev.key === 'Escape') closeSettings();
  };
  document.addEventListener('keydown', onKeydown);
  onCleanup(() => document.removeEventListener('keydown', onKeydown));
  const [pane, setPane] = createSignal<Pane>(p.pane === 'channel' && !p.ctx.channel ? 'av' : p.pane);
  // 「频道」分区判定取服务端 channels.my_role（owner/moderator 可见），浮层自己查：各入口只需报当前频道
  const [managedChannel] = createResource(async () => {
    if (!p.ctx.channel) return null;
    const chs = await listChannels().catch(() => []);
    const r = chs.find((c) => c.name === p.ctx.channel)?.my_role;
    return r === 'owner' || r === 'moderator' ? p.ctx.channel : null;
  });
  const isAdmin = getUser()?.is_admin === true;
  // 「邀请」pane 只对 power 及以上出现；直接以它打开但权限不够时回落个人首页
  const panes = () => PERSONAL_PANES.filter((x) => x.id !== 'invites' || canInvite(getUser()));
  createEffect(() => {
    // 直接以频道分区打开、查出来却没有管理角色：回落个人首页
    if (pane() === 'channel' && managedChannel.state === 'ready' && !managedChannel()) setPane('av');
    if (pane() === 'invites' && !canInvite(getUser())) setPane('av');
  });
  const meta = () =>
    pane() === 'channel'
      ? { label: '频道管理', sub: `成员、黑名单与准入 · ${p.ctx.channel}` }
      : PERSONAL_PANES.find((x) => x.id === pane())!;

  const NavRow = (r: { id: Pane; label: string; icon: string }) => (
    <button class="hit nav-row" classList={{ on: pane() === r.id }} onClick={() => setPane(r.id)}>
      {el(icon(r.icon, 16, 'currentColor', 1.6))}
      <span class="label" style="flex-grow:1;text-align:left">{r.label}</span>
    </button>
  );

  return (
    <>
      <div class="settings-nav">
        <div class="nav-head">设置</div>
        <div class="nav-body">
          <div class="nav-group">个人</div>
          <For each={panes()}>{(r) => <NavRow id={r.id} label={r.label} icon={r.icon} />}</For>
          <Show when={managedChannel()}>
            <div class="nav-group">频道 · {p.ctx.channel}</div>
            <NavRow id="channel" label="频道管理" icon="shield" />
          </Show>
          <Show when={isAdmin}>
            <div class="nav-group">服务器</div>
            <button
              class="hit nav-row"
              onClick={() => {
                closeSettings();
                location.hash = '#/admin';
              }}
            >
              {el(icon('gauge', 16, 'currentColor', 1.6))}
              <span class="label" style="flex-grow:1;text-align:left">管理后台</span>
              <span class="badge-n">→</span>
            </button>
          </Show>
        </div>
        <div class="nav-foot">
          <button class="hit back-row" style="width:100%" onClick={closeSettings}>
            {el(icon('back', 15, 'var(--text-1)'))}
            <span class="back-label" style="flex-grow:1;text-align:left">{p.ctx.backLabel ?? '返回'}</span>
          </button>
        </div>
      </div>
      <div class="settings-main">
        <header class="topbar">
          <h1>{meta().label}</h1>
          <span class="sub">{meta().sub}</span>
          <Show when={pane() === 'av' || pane() === 'screen' || pane() === 'appearance'}>
            <span class="tag tag-sage">改动立即保存并生效</span>
          </Show>
          <div class="spacer"></div>
          <button class="hit btn btn-icon" onClick={closeSettings}>
            {el(icon('close', 16, 'var(--text-1)', 1.8))}
          </button>
        </header>
        <Show when={pane() === 'channel'} fallback={<PersonalHost pane={pane() as PersonalPane} go={setPane} />}>
          <div class="settings-body" style="padding:0;gap:0">
            <ChannelManage channel={p.ctx.channel!} />
          </div>
        </Show>
      </div>
    </>
  );
}

// 个人 pane 的命令式内容挂载点：切 pane 先跑上一个的清理再重画
function PersonalHost(p: { pane: PersonalPane; go: (pane: PersonalPane) => void }) {
  let body!: HTMLDivElement;
  createEffect(() => {
    const pane = p.pane;
    body.innerHTML = '';
    const cleanup = renderPane(body, pane, { close: closeSettings, go: p.go });
    onCleanup(() => cleanup?.());
  });
  return <div class="settings-body" ref={body} />;
}
