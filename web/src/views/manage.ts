// 频道内管理（房主视角）：在房成员 / 黑名单 / 白名单。#/manage/<频道名>
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
import { avatarHtml, esc, icon, timeAgo, toast } from '../ui';

type Tab = 'members' | 'bans' | 'allow';

export async function renderManage(root: HTMLElement, channel: string) {
  const me = getUser();
  let tab: Tab = 'members';
  let inviteOnly = false;
  let participants: { identity: string; name: string; joined_at: number }[] = [];
  let bans: string[] = [];
  let allow: string[] = [];

  root.innerHTML = `
    <div style="height:100%;display:flex;flex-direction:column;background:var(--bg-1)">
      <header class="topbar" style="height:62px;padding:0 26px;background:var(--bg-1)">
        ${icon('shield', 18, 'var(--ember)')}
        <h1 style="font-size:16px">频道管理</h1>
        <span class="sub" style="color:var(--text-2)">· ${esc(channel)}</span>
        <div class="spacer"></div>
        <a class="hit btn btn-sm" href="#/room/${encodeURIComponent(channel)}">${icon('back', 14, 'var(--text-1)')} 返回房间</a>
      </header>
      <div class="mtabs" id="mtabs"></div>
      <div style="flex-grow:1;padding:22px 26px;display:flex;flex-direction:column;gap:20px;overflow-y:auto" id="mbody"></div>
      <div style="height:44px;flex-shrink:0;padding:0 26px;display:flex;align-items:center;border-top:1px solid var(--line-soft)">
        <span class="mono" style="font-size:10.5px;color:var(--text-3)">房主判定取 channels.created_by · 踢人走 LiveKit RemoveParticipant</span>
      </div>
    </div>
  `;

  const tabsEl = root.querySelector<HTMLDivElement>('#mtabs')!;
  const bodyEl = root.querySelector<HTMLDivElement>('#mbody')!;

  async function load() {
    try {
      const [chs, ps, bs, ms] = await Promise.all([
        listChannels(),
        listParticipants(channel).catch(() => []),
        listBans(channel),
        listMembers(channel),
      ]);
      const ch = chs.find((c) => c.name === channel);
      if (!ch) {
        toast('频道不存在', 'bad');
        location.hash = '#/lobby';
        return;
      }
      if (!ch.is_owner) {
        toast('只有房主能进频道管理', 'bad');
        location.hash = `#/room/${encodeURIComponent(channel)}`;
        return;
      }
      inviteOnly = ch.invite_only;
      participants = ps;
      bans = bs;
      allow = ms;
      paint();
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  }

  // 参与者按用户名聚合
  function byUser(): Map<string, typeof participants> {
    const m = new Map<string, typeof participants>();
    for (const p of participants) {
      const u = p.name || p.identity.split('-')[0];
      if (!m.has(u)) m.set(u, []);
      m.get(u)!.push(p);
    }
    return m;
  }

  function inviteCardHtml(): string {
    return `
      <div class="card" style="display:flex;align-items:center;gap:16px">
        <div style="flex-grow:1">
          <div style="font-size:13.5px;font-weight:600">邀请制（白名单）</div>
          <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);margin-top:4px;text-wrap:pretty">${
            inviteOnly
              ? `已开启：只有白名单里的人能进「${esc(channel)}」，其他人看得到、进不去。`
              : `打开后这个频道只有白名单里的人能进，其他人看得到、进不去。适合把「${esc(channel)}」留给固定几个人。`
          }</div>
        </div>
        <button class="hit switch ${inviteOnly ? 'on' : ''}" id="invite-toggle" style="width:40px;height:23px"><div class="knob"></div></button>
      </div>`;
  }

  function paint() {
    const users = byUser();
    const tabs: { id: Tab; label: string; n: string }[] = [
      { id: 'members', label: '在房成员', n: String(users.size) },
      { id: 'bans', label: '黑名单', n: String(bans.length) },
      { id: 'allow', label: '白名单', n: String(allow.length) },
    ];
    tabsEl.innerHTML = tabs
      .map(
        (t) =>
          `<button class="hit mtab ${tab === t.id ? 'on' : ''}" data-tab="${t.id}">${t.label} <span class="n">${t.n}</span></button>`,
      )
      .join('');
    tabsEl.querySelectorAll<HTMLButtonElement>('[data-tab]').forEach((btn) => {
      btn.addEventListener('click', () => {
        tab = btn.dataset.tab as Tab;
        paint();
      });
    });

    if (tab === 'members') {
      bodyEl.innerHTML = `
        <div style="display:flex;flex-direction:column;gap:8px">
          <div style="display:flex;align-items:baseline;gap:10px;flex-wrap:wrap">
            <div style="font-size:13.5px;font-weight:600">在房 ${users.size} 人</div>
            <div style="font-size:11.5px;color:var(--text-2)">踢出只断当前连接，人还能再进；要挡住得拉黑</div>
          </div>
          <div class="list-box" style="background:var(--bg-2)">
            ${
              users.size === 0
                ? '<div class="table-empty">现在没人在房里。</div>'
                : [...users.entries()]
                    .map(([uname, plist]) => {
                      const isMe = uname === (me?.username ?? '');
                      const obs = plist.some((p) => p.identity.endsWith('-obs'));
                      const meta = [
                        obs ? 'OBS 推流中 · 踢出会同时断掉推流' : '',
                        `进房 ${timeAgo(new Date(Math.min(...plist.map((p) => p.joined_at * 1000))).toISOString()).replace('前', '')}`,
                        plist.length > 1 ? `${plist.length} 台设备` : '',
                      ]
                        .filter(Boolean)
                        .join(' · ');
                      return `
              <div class="list-row">
                ${avatarHtml(uname, 'avatar')}
                <div style="flex-grow:1;min-width:0">
                  <div style="display:flex;align-items:center;gap:8px">
                    <span style="font-size:13.5px;font-weight:500">${esc(uname)}</span>
                    ${isMe ? '<span class="tag tag-ember">房主</span>' : ''}
                  </div>
                  <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:2px">${esc(isMe ? 'created_by · 不可移出' : meta)}</div>
                </div>
                ${
                  isMe
                    ? ''
                    : `<div style="display:flex;gap:8px">
                        <button class="hit btn btn-sm" data-kick="${esc(uname)}">踢出房间</button>
                        <button class="hit btn btn-sm btn-danger" data-ban="${esc(uname)}">拉黑</button>
                      </div>`
                }
              </div>`;
                    })
                    .join('')
            }
          </div>
        </div>
        ${inviteCardHtml()}`;
    } else if (tab === 'bans') {
      bodyEl.innerHTML = `
        <div style="display:flex;flex-direction:column;gap:8px">
          <div style="display:flex;align-items:baseline;gap:10px;flex-wrap:wrap">
            <div style="font-size:13.5px;font-weight:600">黑名单 ${bans.length} 人</div>
            <div style="font-size:11.5px;color:var(--text-2)">被拉黑的人进不了这个频道</div>
          </div>
          <div class="list-box" style="background:var(--bg-2)">
            ${
              bans.length === 0
                ? '<div class="table-empty">黑名单是空的。</div>'
                : bans
                    .map(
                      (name) => `
              <div class="list-row">
                <div class="avatar" style="width:28px;height:28px;font-size:10.5px;background:var(--bg-4);color:var(--text-2)">${esc(name.slice(0, 1))}</div>
                <div style="flex-grow:1;font-size:13px">${esc(name)}</div>
                <button class="hit btn btn-sm" data-unban="${esc(name)}">解封</button>
              </div>`,
                    )
                    .join('')
            }
          </div>
        </div>`;
    } else {
      bodyEl.innerHTML = `
        ${inviteCardHtml()}
        <div style="${inviteOnly ? '' : 'opacity:0.6'}">
          <div style="display:flex;align-items:baseline;gap:10px;padding:0 2px">
            <div style="font-size:13.5px;font-weight:600">可进入的人 ${allow.length + 1}</div>
            <div style="font-size:11.5px;color:var(--text-2)">房主始终在列</div>
          </div>
          <div class="list-box" style="margin-top:8px;background:var(--bg-2)">
            <div class="list-row" style="padding:11px 16px">
              ${avatarHtml(me?.username ?? '?', 'avatar')}
              <div style="flex-grow:1;display:flex;align-items:center;gap:8px">
                <span style="font-size:13px">${esc(me?.username ?? '')}</span>
                <span class="tag tag-ember">房主</span>
              </div>
            </div>
            ${allow
              .map(
                (name) => `
            <div class="list-row" style="padding:11px 16px">
              ${avatarHtml(name, 'avatar')}
              <div style="flex-grow:1;font-size:13px">${esc(name)}</div>
              <button class="hit btn btn-sm" data-remove="${esc(name)}">移出</button>
            </div>`,
              )
              .join('')}
          </div>
          <form id="allow-add" style="display:flex;gap:10px;margin-top:10px">
            <div class="field" style="flex-grow:1;height:38px;background:var(--bg-2)"><input id="allow-name" placeholder="用户名" autocomplete="off" /></div>
            <button type="submit" class="hit btn btn-primary">${icon('plus', 14, 'var(--on-ember)', 2)} 加入白名单</button>
          </form>
        </div>
        ${
          inviteOnly
            ? ''
            : `<div class="hint-card">${icon('info', 15, 'var(--text-2)')}<span>邀请制没开，白名单只是一份草稿——现在所有账号都能进「${esc(channel)}」。</span></div>`
        }`;
    }

    // 事件
    bodyEl.querySelector('#invite-toggle')?.addEventListener('click', async () => {
      try {
        const r = await setInviteOnly(channel, !inviteOnly);
        inviteOnly = r.invite_only;
        paint();
      } catch (err) {
        toast((err as Error).message, 'bad');
      }
    });
    bodyEl.querySelectorAll<HTMLButtonElement>('[data-kick]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        try {
          await kickUser(channel, btn.dataset.kick!);
          toast(`已把 ${btn.dataset.kick} 移出房间，对方可以重新进来。`, 'ok');
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
    bodyEl.querySelectorAll<HTMLButtonElement>('[data-ban]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        try {
          await banUser(channel, btn.dataset.ban!);
          toast(`已拉黑 ${btn.dataset.ban}，之后进不了「${channel}」。`, 'ok');
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
    bodyEl.querySelectorAll<HTMLButtonElement>('[data-unban]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        try {
          await unbanUser(channel, btn.dataset.unban!);
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
    bodyEl.querySelectorAll<HTMLButtonElement>('[data-remove]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        try {
          await removeMember(channel, btn.dataset.remove!);
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
    bodyEl.querySelector('#allow-add')?.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const input = bodyEl.querySelector<HTMLInputElement>('#allow-name')!;
      const name = input.value.trim();
      if (!name) return;
      try {
        await addMember(channel, name);
        input.value = '';
        await load();
      } catch (err) {
        toast((err as Error).message, 'bad');
      }
    });
  }

  await load();
}
