// 频道管理（房主与频道管理员视角）：在房成员 / 黑名单 / 白名单 / 访客邀请 / 邀请制；归属类操作（管理员、转让、邀请制开关）仅房主可见。
// ChannelManage 嵌在设置浮层的「频道」分区；#/manage/<频道名> 保留为直达路由，页面只是给它套个头。
import { createMemo, createSignal, For, Show } from 'solid-js';
import { render } from 'solid-js/web';
import {
  addMember,
  addModerator,
  banUser,
  createChannelInvite,
  deleteChannelInvite,
  getUser,
  kickUser,
  listBans,
  listChannelInvites,
  listChannels,
  listMembers,
  listModerators,
  listParticipants,
  removeMember,
  removeModerator,
  setInviteOnly,
  transferChannel,
  unbanUser,
} from '../api';
import type { ChannelRole, Invite, RoomParticipant, UserRef } from '../api';
import { avatarHtml, confirmDialog, copyText, el, esc, icon, menuButtonHtml, timeAgo, toast, wireMenuButton } from '../ui';
import type { ConfirmOpts } from '../ui';
import { inviteState } from './settings-panes';

type Tab = 'members' | 'bans' | 'allow' | 'guests' | 'mods' | 'transfer';

export function ChannelManage(p: { channel: string }) {
  const me = getUser();
  const [tab, setTab] = createSignal<Tab>('members');
  const [inviteOnly, setInvite] = createSignal(false);
  // 三个列表初值 undefined = 加载中，空数组才是真的空态
  const [participants, setParticipants] = createSignal<RoomParticipant[]>();
  const [pErr, setPErr] = createSignal(''); // 在房成员单独拉取，失败不吞掉，面板顶部给重试
  const [bans, setBans] = createSignal<UserRef[]>();
  const [allow, setAllow] = createSignal<UserRef[]>();
  const [mods, setMods] = createSignal<UserRef[]>();
  const [ginvites, setGinvites] = createSignal<Invite[]>();
  const [gbase, setGbase] = createSignal('');
  const [myRole, setMyRole] = createSignal<ChannelRole>(''); // 服务端下发的我的频道角色；owner 才有归属类操作
  const [denied, setDenied] = createSignal<string | null>(null); // 频道不存在 / 没有管理角色：服务端会拒，前端只提示
  const [busy, setBusy] = createSignal(''); // 正在执行的操作 key，空串=空闲
  // 访客邀请表单（两挡有效期 + 次数）
  const [gTtl, setGTtl] = createSignal('24h');
  const [gGuestTtl, setGGuestTtl] = createSignal('24h');
  const [gUses, setGUses] = createSignal('1');
  const [gFresh, setGFresh] = createSignal('');
  let allowInput!: HTMLInputElement;
  let transferSel!: HTMLSelectElement;

  const isOwner = () => myRole() === 'owner';

  async function loadParticipants() {
    try {
      setParticipants(await listParticipants(p.channel));
      setPErr('');
    } catch (err) {
      setPErr((err as Error).message || '成员列表加载失败');
    }
  }

  // moderators 接口是房主限定，管理员视角不拉
  async function loadMods() {
    try {
      setMods(await listModerators(p.channel));
    } catch {
      setMods([]);
    }
  }

  // 访客邀请 moderator+ 都能管
  async function loadGinvites() {
    try {
      const r = await listChannelInvites(p.channel);
      setGinvites(r.invites);
      setGbase(r.base);
    } catch {
      setGinvites([]);
    }
  }

  async function load() {
    void loadParticipants();
    try {
      const [chs, bs, ms] = await Promise.all([listChannels(), listBans(p.channel), listMembers(p.channel)]);
      const ch = chs.find((c) => c.name === p.channel);
      if (!ch) return setDenied('频道不存在');
      if (ch.my_role !== 'owner' && ch.my_role !== 'moderator') return setDenied('只有房主和频道管理员能进频道管理');
      setMyRole(ch.my_role);
      setInvite(ch.invite_only);
      setBans(bs);
      setAllow(ms);
      void loadGinvites();
      if (ch.my_role === 'owner') void loadMods();
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

  // 房主 uid：白名单行里 role=owner 的那位；自己就是房主时白名单还没回来先用自己兜
  const ownerUid = () => (allow() ?? []).find((m) => m.role === 'owner')?.id ?? (isOwner() ? (me?.id ?? 0) : 0);

  // 「授予管理员 / 转让」候选人：在房成员与白名单合并去重（按 uid），排除自己与房主；
  // 授予管理员再排除已是管理员的（转让不排除——管理员是合法的接让人）。
  // 访客等不合法目标由服务端拒绝（错误文案直接 toast），前端拿不到别人的系统角色、不做推导
  const mergedCandidates = (excludeMods: boolean): UserRef[] => {
    const m = new Map<number, string>();
    for (const x of participants() ?? []) {
      if (x.kind === 'ingest') continue; // 推流设备不是人
      if (!m.has(x.uid)) m.set(x.uid, x.username || x.name);
    }
    for (const x of allow() ?? []) {
      if (!m.has(x.id)) m.set(x.id, x.username);
    }
    m.delete(me?.id ?? 0);
    m.delete(ownerUid());
    if (excludeMods) for (const mod of mods() ?? []) m.delete(mod.id);
    return [...m.entries()].map(([id, username]) => ({ id, username }));
  };
  const modCandidates = createMemo(() => mergedCandidates(true));
  const transferCandidates = createMemo(() => mergedCandidates(false));

  // 转让确认：要手输一遍频道名才点得动确认（ui.ts 的 confirmDialog 没有输入框，这里复刻同一套 dialog 样式）
  function confirmTransfer(target: UserRef): Promise<boolean> {
    const prev = document.activeElement as HTMLElement | null;
    const dlg = document.createElement('dialog');
    dlg.className = 'dialog';
    dlg.innerHTML = `<div class="dialog-box">
  <div class="dialog-title">把「${esc(p.channel)}」转让给 ${esc(target.username)}？</div>
  <div class="dialog-body">转让后你是频道管理员，对方成为频道主。输入频道名 <b>${esc(p.channel)}</b> 确认。</div>
  <div class="field" style="margin:4px 0 14px"><input data-act="name" placeholder="${esc(p.channel)}" autocomplete="off" /></div>
  <div class="dialog-actions">
    <button type="button" class="btn" data-act="cancel">取消</button>
    <button type="button" class="btn btn-danger-solid" data-act="ok" disabled>转让</button>
  </div>
</div>`;
    const input = dlg.querySelector<HTMLInputElement>('[data-act="name"]')!;
    const okBtn = dlg.querySelector<HTMLButtonElement>('[data-act="ok"]')!;
    input.addEventListener('input', () => {
      okBtn.disabled = input.value.trim() !== p.channel;
    });
    return new Promise<boolean>((resolve) => {
      let settled = false;
      const settle = (ok: boolean) => {
        if (settled) return;
        settled = true;
        dlg.close();
        dlg.remove();
        if (prev?.isConnected) prev.focus?.();
        resolve(ok);
      };
      dlg.addEventListener('cancel', (e) => {
        e.preventDefault();
        settle(false);
      });
      dlg.addEventListener('click', (e) => {
        if (e.target === dlg) settle(false);
      });
      dlg.querySelector('[data-act="cancel"]')!.addEventListener('click', () => settle(false));
      okBtn.addEventListener('click', () => {
        if (!okBtn.disabled) settle(true);
      });
      document.body.appendChild(dlg);
      dlg.showModal();
      input.focus();
    });
  }

  const doTransfer = async (target: UserRef) => {
    if (busy()) return;
    if (!(await confirmTransfer(target))) return;
    setBusy(`transfer-${target.id}`);
    try {
      const r = await transferChannel(p.channel, target.id);
      toast(`已把「${p.channel}」转让给 ${r.owner}，你现在是频道管理员。`, 'ok');
      setTab('members');
      await load(); // 转让后自己是 moderator：归属类分区随 my_role 刷新消失
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

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

  const makeGuestInvite = async () => {
    if (busy()) return;
    setBusy('ginvite-make');
    try {
      const r = await createChannelInvite(p.channel, gTtl(), gGuestTtl(), Number(gUses()));
      setGFresh(r.url);
      toast('访客邀请链接已生成', 'ok');
      await loadGinvites();
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

  // 访客邀请的撤销/删除是同一个按钮（失效的直接删）
  const revokeGuestInvite = async (iv: Invite, dead: boolean) => {
    if (busy()) return;
    if (!dead) {
      const ok = await confirmDialog({ title: '撤销这条访客邀请？', body: '撤销后链接立即失效；已进房的访客不受影响，到期自动消失。', danger: true, confirmText: '撤销' });
      if (!ok) return;
    }
    setBusy(`ginvite-${iv.id}`);
    try {
      await deleteChannelInvite(p.channel, iv.id);
      toast(dead ? '邀请已删除' : '邀请已撤销', 'ok');
      await loadGinvites();
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

  const GSeg = (gp: { val: () => string; set: (v: string) => void }) => (
    <div class="seg-group" style="background:var(--bg-2)">
      <For
        each={[
          ['1h', '1 小时'],
          ['24h', '24 小时'],
          ['7d', '7 天'],
        ]}
      >
        {([v, label]) => (
          <button type="button" class="hit seg" classList={{ on: gp.val() === v }} onClick={() => gp.set(v)}>
            {label}
          </button>
        )}
      </For>
    </div>
  );

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

  const tabs = (): { id: Tab; label: string; n: () => number | undefined }[] => {
    const base = [
      { id: 'members' as Tab, label: '在房成员', n: () => (participants() ? users().length : undefined) },
      { id: 'bans' as Tab, label: '黑名单', n: () => bans()?.length },
      { id: 'allow' as Tab, label: '白名单', n: () => allow()?.length },
      { id: 'guests' as Tab, label: '访客邀请', n: () => ginvites()?.length },
    ];
    // 归属类分区仅房主可见：频道管理员只管现场管制与名单
    if (isOwner()) {
      base.push(
        { id: 'mods', label: '管理员', n: () => mods()?.length },
        { id: 'transfer', label: '转让频道', n: () => undefined },
      );
    }
    return base;
  };

  return (
    <Show when={!denied()} fallback={<div class="error-text" style="padding:22px 26px">{denied()}</div>}>
      <div class="mtabs" role="tablist">
        <For each={tabs()}>
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
                      const isChOwner = uid === ownerUid();
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
                              <Show when={isChOwner}>
                                <span class="tag tag-ember">房主</span>
                              </Show>
                            </div>
                            <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:2px">
                              {isChOwner ? 'owner · 不可移出' : meta}
                            </div>
                          </div>
                          <Show when={!isChOwner}>
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
          <Show when={isOwner()}>
            <InviteCard />
          </Show>
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
          <Show when={isOwner()}>
            <InviteCard />
          </Show>
          <div style={{ opacity: inviteOnly() ? '' : '0.6' }}>
            <div style="display:flex;align-items:baseline;gap:10px;padding:0 2px">
              <div style="font-size:13.5px;font-weight:600">可进入的人 {allow()?.length ?? '…'}</div>
              <div style="font-size:11.5px;color:var(--text-2)">房主与频道管理员始终在列</div>
            </div>
            <div class="list-box" style="margin-top:8px;background:var(--bg-2)">
              <Show when={allow() === undefined}>
                <div class="table-empty">加载中…</div>
              </Show>
              <For each={allow()}>
                {(m) => (
                  <div class="list-row" style="padding:11px 16px">
                    {el(avatarHtml(m.username, 'avatar'))}
                    <div style="flex-grow:1;display:flex;align-items:center;gap:8px;min-width:0">
                      <span class="cell-ellipsis" style="font-size:13px">{m.username}</span>
                      <Show when={m.role === 'owner'}>
                        <span class="tag tag-ember">房主</span>
                      </Show>
                      <Show when={m.role === 'moderator'}>
                        <span class="tag">管理员</span>
                      </Show>
                    </div>
                    <Show when={m.role !== 'owner' && m.role !== 'moderator'}>
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
                    </Show>
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

        <Show when={tab() === 'guests'}>
          <div style="display:flex;flex-direction:column;gap:8px">
            <div style="display:flex;align-items:baseline;gap:10px;flex-wrap:wrap">
              <div style="font-size:13.5px;font-weight:600">访客邀请</div>
              <div style="font-size:11.5px;color:var(--text-2)">不注册、只进「{p.channel}」的临时身份，到期自动消失</div>
            </div>
            <div class="card" style="padding:16px 18px">
              <form
                style="display:flex;gap:18px;align-items:flex-end;flex-wrap:wrap"
                onSubmit={(ev) => {
                  ev.preventDefault();
                  void makeGuestInvite();
                }}
              >
                <div>
                  <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">链接有效期</div>
                  <GSeg val={gTtl} set={setGTtl} />
                </div>
                <div>
                  <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">访客身份寿命</div>
                  <GSeg val={gGuestTtl} set={setGGuestTtl} />
                </div>
                <div>
                  <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">可用次数</div>
                  <div class="seg-group" style="background:var(--bg-2)">
                    <For
                      each={[
                        ['1', '1 次'],
                        ['5', '5 次'],
                        ['0', '不限'],
                      ]}
                    >
                      {([v, label]) => (
                        <button type="button" class="hit seg" classList={{ on: gUses() === v }} onClick={() => setGUses(v)}>
                          {label}
                        </button>
                      )}
                    </For>
                  </div>
                </div>
                <button type="submit" class="hit btn btn-primary" classList={{ loading: busy() === 'ginvite-make' }} disabled={busy() !== ''}>
                  生成链接
                </button>
              </form>
              <Show when={gFresh()}>
                <div style="display:flex;align-items:center;gap:10px;height:42px;margin-top:14px;padding:0 6px 0 14px;border-radius:9px;background:var(--sage-tint);border:1px solid var(--sage-line)">
                  <span class="mono" style="font-size:12.5px;flex-grow:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">
                    {gFresh()}
                  </span>
                  <button
                    class="hit btn btn-sm"
                    onClick={async () => {
                      if (await copyText(gFresh())) toast('已复制', 'ok', 1400);
                    }}
                  >
                    {el(icon('copy', 13))} 复制
                  </button>
                </div>
              </Show>
            </div>
            <div class="list-box" style="background:var(--bg-2)">
              <Show when={ginvites() === undefined}>
                <div class="table-empty">加载中…</div>
              </Show>
              <Show when={ginvites()}>
                <Show when={ginvites()!.length > 0} fallback={<div class="table-empty">还没有访客邀请。</div>}>
                  <For each={ginvites()}>
                    {(iv) => {
                      const st = inviteState(iv);
                      return (
                        <div class="list-row" classList={{ dim: st.dead }}>
                          <div style="flex-grow:1;min-width:0">
                            <div style="display:flex;align-items:center;gap:8px">
                              <span class="mono" style="font-size:12.5px;color:var(--text-0)">{iv.code}</span>
                              <span class={`chip ${st.cls}`} style={st.cls ? '' : 'background:var(--bg-4);color:var(--text-2)'}>
                                {st.label}
                              </span>
                            </div>
                            <div style="font-size:11px;color:var(--text-2);margin-top:3px">
                              {`${iv.used} / ${iv.max_uses === 0 ? '∞' : iv.max_uses} 次 · 访客寿命 ${iv.guest_ttl_sec >= 86400 ? `${Math.round(iv.guest_ttl_sec / 86400)} 天` : `${Math.round(iv.guest_ttl_sec / 3600)} 小时`}`}
                            </div>
                          </div>
                          <Show when={!st.dead}>
                            <button
                              class="hit btn btn-sm"
                              disabled={busy() !== ''}
                              onClick={async () => {
                                if (await copyText(`${gbase()}/#/join/${iv.code}`)) toast('已复制', 'ok', 1400);
                              }}
                            >
                              复制链接
                            </button>
                          </Show>
                          <button
                            class="hit btn btn-sm"
                            classList={{ loading: busy() === `ginvite-${iv.id}` }}
                            disabled={busy() !== ''}
                            onClick={() => void revokeGuestInvite(iv, st.dead)}
                          >
                            {st.dead ? '删除' : '撤销'}
                          </button>
                        </div>
                      );
                    }}
                  </For>
                </Show>
              </Show>
            </div>
          </div>
        </Show>

        <Show when={tab() === 'mods'}>
          <div style="display:flex;flex-direction:column;gap:8px">
            <div style="display:flex;align-items:baseline;gap:10px;flex-wrap:wrap">
              <div style="font-size:13.5px;font-weight:600">频道管理员 {mods() ? mods()!.length : '…'} 人</div>
              <div style="font-size:11.5px;color:var(--text-2)">管理员能踢人、拉黑、管白名单；邀请制开关与转让仍是房主专属</div>
            </div>
            <div class="list-box" style="background:var(--bg-2)">
              <Show when={mods() === undefined}>
                <div class="table-empty">加载中…</div>
              </Show>
              <Show when={mods()}>
                <Show when={mods()!.length > 0} fallback={<div class="table-empty">还没有频道管理员。</div>}>
                  <For each={mods()}>
                    {(m) => (
                      <div class="list-row">
                        {el(avatarHtml(m.username, 'avatar'))}
                        <div class="cell-ellipsis" style="flex-grow:1;font-size:13px">{m.username}</div>
                        <button
                          class="hit btn btn-sm"
                          classList={{ loading: busy() === `demod-${m.id}` }}
                          disabled={busy() !== ''}
                          onClick={act(
                            `demod-${m.id}`,
                            () => removeModerator(p.channel, m.id),
                            `已收回 ${m.username} 的管理员。`,
                            { title: `收回 ${m.username} 的管理员？`, body: '收回后对方只是普通成员，白名单里的位置保留。', danger: true, confirmText: '收回' },
                          )}
                        >
                          收回
                        </button>
                      </div>
                    )}
                  </For>
                </Show>
              </Show>
            </div>
            <div style="display:flex;align-items:baseline;gap:10px;padding:12px 2px 0">
              <div style="font-size:13.5px;font-weight:600">授予管理员</div>
              <div style="font-size:11.5px;color:var(--text-2)">候选人来自在房成员与白名单</div>
            </div>
            <div class="list-box" style="background:var(--bg-2)">
              <Show when={modCandidates().length > 0} fallback={<div class="table-empty">没有可授予的人（在房成员与白名单里的人都已是管理员）。</div>}>
                <For each={modCandidates()}>
                  {(c) => (
                    <div class="list-row">
                      {el(avatarHtml(c.username, 'avatar'))}
                      <div class="cell-ellipsis" style="flex-grow:1;font-size:13px">{c.username}</div>
                      <button
                        class="hit btn btn-sm"
                        classList={{ loading: busy() === `mod-${c.id}` }}
                        disabled={busy() !== ''}
                        onClick={act(`mod-${c.id}`, () => addModerator(p.channel, c.id), `已把 ${c.username} 设为频道管理员。`)}
                      >
                        授予
                      </button>
                    </div>
                  )}
                </For>
              </Show>
            </div>
          </div>
        </Show>

        <Show when={tab() === 'transfer'}>
          <div style="display:flex;flex-direction:column;gap:8px">
            <div class="card" style="display:flex;flex-direction:column;gap:10px">
              <div style="font-size:13.5px;font-weight:600">转让频道</div>
              <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);text-wrap:pretty">
                把「{p.channel}」的房主身份交给别人：对方成为频道主，你自动降为频道管理员。频道本身、聊天记录与名单都不动。
              </div>
              <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
                <select
                  class="hit"
                  style="height:36px;padding:0 10px;border-radius:8px;border:1px solid var(--line);background:var(--bg-2);color:var(--text-1);font-size:12.5px;flex-grow:1;min-width:180px"
                  ref={transferSel}
                >
                  <For each={transferCandidates()}>{(c) => <option value={c.id}>{c.username}</option>}</For>
                </select>
                <button
                  class="hit btn btn-danger-solid"
                  disabled={busy() !== '' || transferCandidates().length === 0}
                  onClick={() => {
                    const c = transferCandidates().find((x) => x.id === Number(transferSel.value));
                    if (c) void doTransfer(c);
                  }}
                >
                  转让频道
                </button>
              </div>
              <Show when={transferCandidates().length === 0}>
                <div style="font-size:11.5px;color:var(--text-3)">没有可接手的人：让对方先进一次房间，或先把对方加进白名单。</div>
              </Show>
            </div>
          </div>
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
