// ---- 邀请（power+；admin+ 可指定产出档、可见全部邀请）----
import {
  createInvite,
  deleteInvite,
  getUser,
  listInvites,
} from '../../api';
import type { Invite } from '../../api';
import { confirmDialog, copyText, esc, icon, toast } from '../../ui';

// 邀请链接的存活态（注册邀请与频道访客邀请同一套判定，manage.tsx 也用）
export function inviteState(iv: Invite): { label: string; cls: string; dead: boolean } {
  const now = Date.now();
  const exp = new Date(iv.expires_at).getTime();
  if (iv.revoked) return { label: '已撤销', cls: 'tag-red', dead: true };
  if (exp < now) return { label: '已过期', cls: 'tag-red', dead: true };
  if (iv.max_uses > 0 && iv.used >= iv.max_uses) return { label: '已用完', cls: '', dead: true };
  const leftH = Math.ceil((exp - now) / 3600_000);
  return {
    label: `有效 · 剩 ${leftH > 48 ? `${Math.ceil(leftH / 24)} 天` : `${leftH} 小时`}`,
    cls: 'tag-sage',
    dead: false,
  };
}

export function renderInvites(body: HTMLElement) {
  const admin = getUser()?.is_admin === true; // 产出档选择与全量列表只对管理员开放（is_admin 为 role≥admin 的服务端派生）
  let invites: Invite[] | null = null;
  let base = '';
  let ttl = '24h';
  let uses = '1';
  let note = '';
  let role = ''; // 产出档：空 = 跟随注册默认档
  let allowGuest = '0'; // 允许对方「先以访客进入」再注册转正
  let fresh = '';
  let making = false;
  let revokeBusy = 0; // 正在撤销/删除的邀请 id，0=空闲

  body.innerHTML = '<div class="muted">加载邀请…</div>';

  const seg = (cur: string, opts: [string, string][], attr: string) =>
    `<div class="seg-group" style="background:var(--bg-2)">${opts
      .map(([v, label]) => `<button type="button" class="hit seg ${cur === v ? 'on' : ''}" data-${attr}="${v}">${label}</button>`)
      .join('')}</div>`;

  function paint() {
    if (invites === null) return; // 首屏等加载
    body.innerHTML = `
      <div class="pane-col pane-wide">
        <div class="card" style="padding:18px 20px">
          <div style="font-size:13.5px;font-weight:600">生成邀请链接</div>
          <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">链接在有效期内可用，点开就能自己设账号密码；允许「先以访客进入」的话，对方也可以先不注册进来看看</div>
          <form style="display:flex;gap:20px;margin-top:16px;align-items:flex-end;flex-wrap:wrap" id="iv-form">
            <div>
              <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">有效期</div>
              ${seg(ttl, [['1h', '1 小时'], ['24h', '24 小时'], ['7d', '7 天']], 'ttl')}
            </div>
            <div>
              <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">可用次数</div>
              ${seg(uses, [['1', '1 次'], ['5', '5 次'], ['0', '不限']], 'uses')}
            </div>
            ${
              admin
                ? `<div>
              <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">产出档</div>
              ${seg(role, [['', '跟随默认档'], ['user', '普通用户'], ['power', '高级用户']], 'role')}
            </div>`
                : ''
            }
            <div>
              <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">先以访客进入</div>
              ${seg(allowGuest, [['0', '不允许'], ['1', '允许']], 'allowguest')}
            </div>
            <div style="flex-grow:1;min-width:160px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">备注（给谁）</div>
              <div class="field" style="height:38px;background:var(--bg-2)">
                <input id="iv-note" value="${esc(note)}" />
              </div>
            </div>
            <button type="submit" class="hit btn btn-primary ${making ? 'loading' : ''}" id="iv-make" ${making ? 'disabled' : ''}>生成链接</button>
          </form>
          ${
            fresh
              ? `<div style="display:flex;align-items:center;gap:10px;height:42px;margin-top:14px;padding:0 6px 0 14px;border-radius:9px;background:var(--sage-tint);border:1px solid var(--sage-line)">
            <span class="mono" style="font-size:12.5px;flex-grow:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(fresh)}</span>
            <button class="hit btn btn-sm" data-copy="${esc(fresh)}">${icon('copy', 13)} 复制</button>
          </div>`
              : ''
          }
        </div>
        <div class="list-box">
          <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600">
            ${admin ? '全部邀请' : '我发出的邀请'}
          </div>
          ${
            invites.length === 0
              ? '<div class="table-empty">还没有发过邀请。</div>'
              : invites
                  .map((iv) => {
                    const st = inviteState(iv);
                    const meta = [
                      iv.note || '（无备注）',
                      `${iv.used} / ${iv.max_uses === 0 ? '∞' : iv.max_uses} 次`,
                      iv.allow_guest ? '可先以访客进入' : '',
                      admin ? `by ${iv.created_by}` : '',
                    ]
                      .filter(Boolean)
                      .join(' · ');
                    return `
            <div class="list-row" style="${st.dead ? 'opacity:0.55' : ''}">
              <div style="flex-grow:1;min-width:0">
                <div style="display:flex;align-items:center;gap:8px">
                  <span class="mono" style="font-size:12.5px;color:var(--text-0)">${esc(iv.code)}</span>
                  <span class="chip ${st.cls}" style="${st.cls ? '' : 'background:var(--bg-4);color:var(--text-2)'}">${st.label}</span>
                </div>
                <div style="font-size:11px;color:var(--text-2);margin-top:3px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(meta)}</div>
              </div>
              ${st.dead ? '' : `<button class="hit btn btn-sm" data-copy="${esc(`${base}/#/join/${iv.code}`)}" style="flex-shrink:0">复制链接</button>`}
              <button class="hit btn btn-sm ${revokeBusy === iv.id ? 'loading' : ''}" data-revoke="${iv.id}" data-dead="${st.dead ? 1 : ''}" ${revokeBusy !== 0 ? 'disabled' : ''} style="flex-shrink:0">${st.dead ? '删除' : '撤销'}</button>
            </div>`;
                  })
                  .join('')
          }
        </div>
      </div>`;

    body.querySelectorAll<HTMLButtonElement>('[data-ttl]').forEach((b) =>
      b.addEventListener('click', () => {
        ttl = b.dataset.ttl!;
        paint();
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-uses]').forEach((b) =>
      b.addEventListener('click', () => {
        uses = b.dataset.uses!;
        paint();
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-role]').forEach((b) =>
      b.addEventListener('click', () => {
        role = b.dataset.role!;
        paint();
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-allowguest]').forEach((b) =>
      b.addEventListener('click', () => {
        allowGuest = b.dataset.allowguest!;
        paint();
      }),
    );
    body.querySelector<HTMLInputElement>('#iv-note')!.addEventListener('input', (ev) => {
      note = (ev.target as HTMLInputElement).value;
    });
    body.querySelectorAll<HTMLButtonElement>('[data-copy]').forEach((b) =>
      b.addEventListener('click', async () => {
        if (await copyText(b.dataset.copy!)) toast('已复制', 'ok', 1400);
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-revoke]').forEach((b) =>
      b.addEventListener('click', async () => {
        if (revokeBusy !== 0) return;
        const id = Number(b.dataset.revoke);
        const dead = b.dataset.dead === '1';
        if (!dead) {
          const ok = await confirmDialog({ title: '撤销这条邀请？', body: '撤销后这条链接立即失效，未使用的次数作废。', danger: true, confirmText: '撤销' });
          if (!ok) return;
        }
        revokeBusy = id;
        paint();
        try {
          await deleteInvite(id);
          toast(dead ? '邀请已删除' : '邀请已撤销', 'ok');
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        } finally {
          revokeBusy = 0;
          paint();
        }
      }),
    );
    body.querySelector('#iv-form')!.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      if (making) return;
      making = true;
      paint();
      try {
        const r = await createInvite(note, Number(uses), ttl, role, allowGuest === '1');
        fresh = r.url;
        note = '';
        toast('邀请链接已生成', 'ok');
        await load();
      } catch (err) {
        toast((err as Error).message, 'bad');
      } finally {
        making = false;
        paint();
      }
    });
  }

  async function load() {
    try {
      const r = await listInvites();
      invites = r.invites;
      base = r.base;
    } catch (err) {
      body.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
      throw err;
    }
  }

  void load()
    .then(paint)
    .catch(() => {});
}
