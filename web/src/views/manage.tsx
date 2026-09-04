// 频道管理（房主视角）：在房成员 / 黑名单 / 白名单 / 邀请制。
// ChannelManage 嵌在设置浮层的「频道」分区；#/manage/<频道名> 保留为直达路由，页面只是给它套个头。
import { createMemo, createSignal, For, Show } from 'solid-js';
import { render } from 'solid-js/web';
import {
  addMember,
  banUser,
  getUser,
  kickUser,
  listBans,
  listChannels,
  listMembers,
  listParticipants,
  removeMember,
  setInviteOnly,
  unbanUser,
} from '../api';
import type { RoomParticipant, UserRef } from '../api';
import { avatarHtml, confirmDialog, el, icon, menuButtonHtml, timeAgo, toast, wireMenuButton } from '../ui';
import type { ConfirmOpts } from '../ui';

type Tab = 'members' | 'bans' | 'allow';

export function ChannelManage(p: { channel: string }) {
  const me = getUser();
  const [tab, setTab] = createSignal<Tab>('members');
  const [inviteOnly, setInvite] = createSignal(false);
  // 三个列表初值 undefined = 加载中，空数组才是真的空态
  const [participants, setParticipants] = createSignal<RoomParticipant[]>();
  const [pErr, setPErr] = createSignal(''); // 在房成员单独拉取，失败不吞掉，面板顶部给重试
  const [bans, setBans] = createSignal<UserRef[]>();
  const [allow, setAllow] = createSignal<UserRef[]>();
  const [denied, setDenied] = createSignal<string | null>(null); // 频道不存在 / 非房主：服务端会拒，前端只提示
  const [busy, setBusy] = createSignal(''); // 正在执行的操作 key，空串=空闲
  let allowInput!: HTMLInputElement;

  async function loadParticipants() {
    try {
      setParticipants(await listParticipants(p.channel));
      setPErr('');
    } catch (err) {
      setPErr((err as Error).message || '成员列表加载失败');
    }
  }

  async function load() {
    void loadParticipants();
    try {
      const [chs, bs, ms] = await Promise.all([listChannels(), listBans(p.channel), listMembers(p.channel)]);
      const ch = chs.find((c) => c.name === p.channel);
      if (!ch) return setDenied('频道不存在');
      if (!ch.is_owner) return setDenied('只有房主能进频道管理');
      setInvite(ch.invite_only);
      setBans(bs);
      setAllow(ms);
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  }
  void load();

  // 参与者按 user_id 聚合（同名不同人的歧义不存在；用户名只用于展示）
  const users = createMemo(() => {
    const m = new Map<number, RoomParticipant[]>();
    for (const x of participants() ?? []) {
      if (!m.has(x.uid)) m.set(x.uid, []);
      m.get(x.uid)!.push(x);
    }
    return [...m.entries()];
  });

  // 频道维度的操作都是落库即生效：每次都给回执，再重拉一遍；confirmOpts 给了就先过一道确认
  const act = (key: string, fn: () => Promise<unknown>, okMsg: string, confirmOpts?: ConfirmOpts) => async () => {
    if (busy()) return;
    if (confirmOpts && !(await confirmDialog(confirmOpts))) return;
    setBusy(key);
    try {
      await fn();
      toast(okMsg, 'ok');
      await load();
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

  const toggleInvite = async () => {
    if (busy()) return;
    if (inviteOnly()) {
      const ok = await confirmDialog({
        title: '关闭邀请制？',
        body: `关闭后任何账号都能进「${p.channel}」。`,
        danger: true,
        confirmText: '关闭',
      });
      if (!ok) return;
    }
    setBusy('invite-toggle');
    try {
      const r = await setInviteOnly(p.channel, !inviteOnly());
      setInvite(r.invite_only);
      toast(r.invite_only ? '已开启邀请制，只有白名单里的人能进' : '已关闭邀请制，所有账号都能进', 'ok');
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

  const InviteCard = () => (
    <div class="card" style="display:flex;align-items:center;gap:16px">
      <div style="flex-grow:1">
        <div style="font-size:13.5px;font-weight:600">邀请制（白名单）</div>
        <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);margin-top:4px;text-wrap:pretty">
          {inviteOnly()
            ? `已开启：只有白名单里的人能进「${p.channel}」，其他人看得到、进不去。`
            : `打开后这个频道只有白名单里的人能进，其他人看得到、进不去。适合把「${p.channel}」留给固定几个人。`}
        </div>
      </div>
      <button
        class="hit switch"
        classList={{ on: inviteOnly() }}
        role="switch"
        aria-checked={inviteOnly()}
        disabled={busy() !== ''}
        style="width:40px;height:23px"
        onClick={() => void toggleInvite()}
      >
        <div class="knob"></div>
      </button>
    </div>
  );

  const tabs: { id: Tab; label: string; n: () => number | undefined }[] = [
    { id: 'members', label: '在房成员', n: () => (participants() ? users().length : undefined) },
    { id: 'bans', label: '黑名单', n: () => bans()?.length },
    { id: 'allow', label: '白名单', n: () => allow()?.length },
  ];

  return (
    <Show when={!denied()} fallback={<div class="error-text" style="padding:22px 26px">{denied()}</div>}>
      <div class="mtabs" role="tablist">
        <For each={tabs}>
          {(t) => (
            <button class="hit mtab" role="tab" aria-selected={tab() === t.id} classList={{ on: tab() === t.id }} onClick={() => setTab(t.id)}>
              {t.label} <Show when={t.n() !== undefined}><span class="n">{t.n()}</span></Show>
            </button>
          )}
        </For>
      </div>
      <div class="page-body">
        <Show when={tab() === 'members'}>
          <div style="display:flex;flex-direction:column;gap:8px">
            <div style="display:flex;align-items:baseline;gap:10px;flex-wrap:wrap">
              <div style="font-size:13.5px;font-weight:600">在房 {participants() ? users().length : '…'} 人</div>
              <div style="font-size:11.5px;color:var(--text-2)">踢出只断当前连接，人还能再进；要挡住得拉黑</div>
            </div>
            <Show when={pErr()}>
              <div class="state-block error">
                <span>{pErr()}</span>
                <button class="hit btn btn-sm" onClick={() => void loadParticipants()}>
                  重试
                </button>
              </div>
            </Show>
            <div class="list-box" style="background:var(--bg-2)">
              <Show when={participants() === undefined && !pErr()}>
                <div class="table-empty">加载中…</div>
              </Show>
              <Show when={participants()}>
                <Show when={users().length > 0} fallback={<div class="table-empty">现在没人在房里。</div>}>
                  <For each={users()}>
                    {([uid, plist]) => {
                      const uname = plist[0].username || plist[0].name;
                      const isMe = uid === (me?.id ?? 0);
                      // 推流设备按内核透传的 kind=ingest 判断（不解析 identity 后缀），标签随行展示
                      const ing = plist.find((x) => x.kind === 'ingest');
                      const meta = [
                        ing ? `OBS 推流中${ing.tag ? `（${ing.tag}）` : ''} · 踢出会同时断掉推流` : '',
                        `进房 ${timeAgo(new Date(Math.min(...plist.map((x) => x.joined_at * 1000))).toISOString()).replace('前', '')}`,
                        plist.length > 1 ? `${plist.length} 台设备` : '',
                      ]
                        .filter(Boolean)
                        .join(' · ');
                      return (
                        <div class="list-row">
                          {el(avatarHtml(uname, 'avatar'))}
                          <div style="flex-grow:1;min-width:0">
                            <div style="display:flex;align-items:center;gap:8px">
                              <span class="cell-ellipsis" style="font-size:13.5px;font-weight:500">{uname}</span>
                              <Show when={isMe}>
                                <span class="tag tag-ember">房主</span>
                              </Show>
                            </div>
                            <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:2px">
                              {isMe ? 'created_by · 不可移出' : meta}
                            </div>
                          </div>
                          <Show when={!isMe}>
                            <div class="table-actions">
                              <button
                                class="hit btn btn-sm"
                                classList={{ loading: busy() === `kick-${uid}` }}
                                disabled={busy() !== ''}
                                onClick={act(`kick-${uid}`, () => kickUser(p.channel, uid), `已把 ${uname} 移出房间，对方可以重新进来。`)}
                              >
                                踢出房间
                              </button>
                              <button
                                class="hit btn btn-sm btn-danger"
                                classList={{ loading: busy() === `ban-${uid}` }}
                                disabled={busy() !== ''}
                                onClick={act(
                                  `ban-${uid}`,
                                  () => banUser(p.channel, uid),
                                  `已拉黑 ${uname}，之后进不了「${p.channel}」。`,
                                  { title: `拉黑 ${uname}？`, body: `之后进不了「${p.channel}」，可以在黑名单里解封。`, danger: true, confirmText: '拉黑' },
                                )}
                              >
                                拉黑
                              </button>
                            </div>
                          </Show>
                        </div>
                      );
                    }}
                  </For>
                </Show>
              </Show>
            </div>
          </div>
          <InviteCard />
        </Show>

        <Show when={tab() === 'bans'}>
          <div style="display:flex;flex-direction:column;gap:8px">
            <div style="display:flex;align-items:baseline;gap:10px;flex-wrap:wrap">
              <div style="font-size:13.5px;font-weight:600">黑名单 {bans() ? bans()!.length : '…'} 人</div>
              <div style="font-size:11.5px;color:var(--text-2)">被拉黑的人进不了这个频道</div>
            </div>
            <div class="list-box" style="background:var(--bg-2)">
              <Show when={bans() === undefined}>
                <div class="table-empty">加载中…</div>
              </Show>
              <Show when={bans()}>
                <Show when={bans()!.length > 0} fallback={<div class="table-empty">黑名单是空的。</div>}>
                  <For each={bans()}>
                    {(b) => (
                      <div class="list-row">
                        <div
                          class="avatar"
                          style="width:28px;height:28px;font-size:10.5px;background:var(--bg-4);color:var(--text-2)"
                        >
                          {b.username.slice(0, 1)}
                        </div>
                        <div class="cell-ellipsis" style="flex-grow:1;font-size:13px">{b.username}</div>
                        <button
                          class="hit btn btn-sm"
                          classList={{ loading: busy() === `unban-${b.id}` }}
                          disabled={busy() !== ''}
                          onClick={act(`unban-${b.id}`, () => unbanUser(p.channel, b.id), `已解封 ${b.username}。`)}
                        >
                          解封
                        </button>
                      </div>
                    )}
                  </For>
                </Show>
              </Show>
            </div>
          </div>
        </Show>

        <Show when={tab() === 'allow'}>
          <InviteCard />
          <div style={{ opacity: inviteOnly() ? '' : '0.6' }}>
            <div style="display:flex;align-items:baseline;gap:10px;padding:0 2px">
              <div style="font-size:13.5px;font-weight:600">可进入的人 {(allow()?.length ?? 0) + 1}</div>
              <div style="font-size:11.5px;color:var(--text-2)">房主始终在列</div>
            </div>
            <div class="list-box" style="margin-top:8px;background:var(--bg-2)">
              <div class="list-row" style="padding:11px 16px">
                {el(avatarHtml(me?.username ?? '?', 'avatar'))}
                <div style="flex-grow:1;display:flex;align-items:center;gap:8px">
                  <span class="cell-ellipsis" style="font-size:13px">{me?.username ?? ''}</span>
                  <span class="tag tag-ember">房主</span>
                </div>
              </div>
              <Show when={allow() === undefined}>
                <div class="table-empty">加载中…</div>
              </Show>
              <For each={allow()}>
                {(m) => (
                  <div class="list-row" style="padding:11px 16px">
                    {el(avatarHtml(m.username, 'avatar'))}
                    <div class="cell-ellipsis" style="flex-grow:1;font-size:13px">{m.username}</div>
                    <button
                      class="hit btn btn-sm"
                      classList={{ loading: busy() === `remove-${m.id}` }}
                      disabled={busy() !== ''}
                      onClick={act(
                        `remove-${m.id}`,
                        () => removeMember(p.channel, m.id),
                        `已把 ${m.username} 移出白名单。`,
                        { title: `移出 ${m.username}？`, body: '移出后这个账号不在白名单里了，邀请制开着的话会进不来。', danger: true, confirmText: '移出' },
                      )}
                    >
                      移出
                    </button>
                  </div>
                )}
              </For>
            </div>
            <form
              style="display:flex;gap:10px;margin-top:10px"
              onSubmit={(ev) => {
                ev.preventDefault();
                const name = allowInput.value.trim();
                if (!name || busy()) return;
                void act('add-member', async () => {
                  await addMember(p.channel, name);
                  allowInput.value = '';
                }, `已把 ${name} 加入白名单。`)();
              }}
            >
              <div class="field" style="flex-grow:1;height:38px;background:var(--bg-2)">
                <input ref={allowInput} placeholder="用户名" autocomplete="off" />
              </div>
              <button type="submit" class="hit btn btn-primary" classList={{ loading: busy() === 'add-member' }} disabled={busy() !== ''}>
                {el(icon('plus', 14, 'var(--on-ember)', 2))} 加入白名单
              </button>
            </form>
          </div>
          <Show when={!inviteOnly()}>
            <div class="hint-card">
              {el(icon('info', 15, 'var(--text-2)'))}
              <span>邀请制没开，白名单只是一份草稿——现在所有账号都能进「{p.channel}」。</span>
            </div>
          </Show>
        </Show>
      </div>
    </Show>
  );
}

// #/manage/<频道名> 直达页：套个头，内容与浮层分区同一个组件
export function renderManage(root: HTMLElement, channel: string) {
  // 挂在自己的宿主节点上：hashchange 时 route() 已把下一个视图画进 root，
  // dispose 会清空所挂容器——直接挂 root 会把新视图一起擦掉
  const host = document.createElement('div');
  host.className = 'view-host';
  root.innerHTML = '';
  root.appendChild(host);
  const closeNav = () => root.querySelector('.app-frame')?.classList.remove('nav-open');
  const dispose = render(
    () => (
      <div class="app-frame">
        <div class="nav-scrim" onClick={closeNav}></div>
        <aside class="sidebar sidebar-admin">
          <div class="sidebar-head">
            {el(icon('shield', 17, 'var(--ember)'))}
            <div style="font-size:13.5px;font-weight:700;letter-spacing:0.04em">频道管理</div>
          </div>
          <div class="sidebar-body" style="gap:2px">
            <a class="hit back-row" href="#/lobby">
              {el(icon('back', 16, 'var(--text-2)', 1.6))}
              <span style="flex-grow:1">返回大厅</span>
            </a>
            <a class="hit back-row" href={`#/room/${encodeURIComponent(channel)}`}>
              {el(icon('volume', 16, 'var(--text-2)', 1.6))}
              <span style="flex-grow:1">返回房间</span>
            </a>
          </div>
        </aside>
        <div class="content">
          <header class="topbar topbar-lg">
            {el(menuButtonHtml())}
            {el(icon('shield', 18, 'var(--ember)'))}
            <h1 style="font-size:16px">频道管理</h1>
            <span class="sub" style="color:var(--text-2)">· {channel}</span>
            <div class="spacer"></div>
            <a class="hit btn btn-sm" href={`#/room/${encodeURIComponent(channel)}`}>
              {el(icon('back', 14, 'var(--text-1)'))} 返回房间
            </a>
          </header>
          <ChannelManage channel={channel} />
        </div>
      </div>
    ),
    host,
  );
  const unwireMenu = wireMenuButton(root);
  window.addEventListener(
    'hashchange',
    () => {
      dispose();
      unwireMenu();
    },
    { once: true },
  );
}
