// 管理后台「用户」tab：账号、角色与启停。
import { createSignal, For, Show } from 'solid-js';
import { adminDeleteUser, adminListUsers, adminSetUserDisabled, adminSetUserRole, getUser } from '../../api';
import type { AdminUser } from '../../api';
import { avatarHtml, confirmDialog, el, icon, timeAgo, toast } from '../../ui';
import { openSettings } from '../settings';
import { Placeholder } from './shared';

const ROLE_LABELS: Record<string, string> = {
  super: '超级管理员',
  admin: '管理员',
  power: '高级用户',
  user: '成员',
  guest: '访客',
};

// 访客行的过期时间：未来 = 还剩多久，已过 = 已过期（清理协程每 10 分钟扫一次，可能短暂滞留）
function fmtExpiry(iso: string | null): string {
  if (!iso) return '';
  const ms = new Date(iso).getTime() - Date.now();
  if (ms <= 0) return '已过期';
  const h = Math.ceil(ms / 3600_000);
  return h > 48 ? `${Math.ceil(h / 24)} 天后过期` : `${h} 小时后过期`;
}

export function UsersTab() {
  const [users, setUsers] = createSignal<AdminUser[]>();
  const [err, setErr] = createSignal('');
  const [query, setQuery] = createSignal('');
  const [busy, setBusy] = createSignal(''); // 正在操作的用户 key（"toggle-<id>" / "del-<id>" / "role-<id>"）
  const meId = getUser()?.id;

  const load = async () => {
    try {
      setUsers(await adminListUsers());
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  void load();

  const shown = () => {
    const q = query().trim().toLowerCase();
    const all = users() ?? [];
    return q ? all.filter((u) => u.username.toLowerCase().includes(q)) : all;
  };

  const setRole = async (u: AdminUser, role: string) => {
    if (busy() || role === u.role) return;
    setBusy(`role-${u.id}`);
    try {
      const r = await adminSetUserRole(u.id, role);
      toast(
        r.owned_channels > 0
          ? `已把 ${u.username} 调整为「${ROLE_LABELS[r.role] ?? r.role}」；其名下仍有 ${r.owned_channels} 个频道`
          : `已把 ${u.username} 调整为「${ROLE_LABELS[r.role] ?? r.role}」`,
        'ok',
      );
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

  const toggle = async (u: AdminUser) => {
    if (busy()) return;
    if (!u.disabled) {
      const ok = await confirmDialog({ title: `停用账号 ${u.username}？`, body: '停用后该账号无法登录，可以随时重新启用。', danger: true, confirmText: '停用' });
      if (!ok) return;
    }
    setBusy(`toggle-${u.id}`);
    try {
      await adminSetUserDisabled(u.id, !u.disabled);
      toast(u.disabled ? '已启用该账号' : '已禁用该账号', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

  const remove = async (u: AdminUser) => {
    if (busy()) return;
    const ok = await confirmDialog({
      title: `删除用户 ${u.username}？`,
      body: '其会话、设备档案和白名单记录一并清除，名下的频道会转到你的名下。不可恢复。',
      danger: true,
      confirmText: '删除',
    });
    if (!ok) return;
    setBusy(`del-${u.id}`);
    try {
      const r = await adminDeleteUser(u.id);
      toast(r.adopted_channels > 0 ? `用户已删除，其 ${r.adopted_channels} 个频道已转到你的名下` : '用户已删除', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setBusy('');
    }
  };

  return (
    <Show when={users()} fallback={<Placeholder err={err()} />}>
      <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        <form style="flex-shrink:0" onSubmit={(ev) => ev.preventDefault()}>
          <div class="field" style="width:280px;height:38px">
            {el(icon('search', 15, 'var(--text-2)'))}
            <input id="u-query" placeholder="搜用户名" value={query()} onInput={(ev) => setQuery(ev.currentTarget.value)} />
          </div>
        </form>
        <div class="spacer"></div>
        <button class="hit btn btn-primary" onClick={() => openSettings('invites')}>
          {el(icon('plus', 15, 'var(--on-ember)', 1.9))} 邀请新用户
        </button>
      </div>
      <div class="table-box" style={{ '--col-1': '200px', '--col-2': '150px', '--col-3': '80px', '--col-5': '170px' }}>
        <div class="table-head">
          <div>用户</div>
          <div>角色</div>
          <div>设备</div>
          <div style="flex-grow:1">最后活跃</div>
          <div style="text-align:right">操作</div>
        </div>
        <Show when={shown().length > 0} fallback={<div class="table-empty">没有匹配的用户。</div>}>
          <For each={shown()}>
            {(u) => {
              const self = u.id === meId;
              // 可授角色候选由服务端随行下发（can_set_roles），前端不推导阶梯；null/空 = 不可操作
              const settable = () => (u.can_set_roles ?? []).filter((r) => r !== u.role);
              return (
                <div class="table-row" classList={{ dim: u.disabled }}>
                  <div data-label="用户" style="display:flex;align-items:center;gap:10px;min-width:0">
                    {el(avatarHtml(u.username))}
                    <div style="min-width:0">
                      <div class="cell-ellipsis" style="font-size:13px;font-weight:500">{u.username}</div>
                      <div class="mono" style="font-size:10px;color:var(--text-2);margin-top:2px">
                        usr_{u.id}
                        {u.role === 'guest' && u.invite ? ` · 来自邀请 ${u.invite}` : ''}
                      </div>
                    </div>
                  </div>
                  <div data-label="角色">
                    <Show
                      when={settable().length > 0}
                      fallback={
                        <span
                          class="chip"
                          classList={{ 'tag-ember': u.is_admin }}
                          style={u.is_admin ? '' : 'background:var(--bg-4);color:var(--text-1)'}
                        >
                          {ROLE_LABELS[u.role] ?? u.role}
                        </span>
                      }
                    >
                      <select
                        class="hit"
                        style="height:28px;padding:0 6px;border-radius:7px;border:1px solid var(--line);background:var(--bg-2);color:var(--text-1);font-size:12px"
                        disabled={busy() !== ''}
                        value={u.role}
                        onChange={(ev) => void setRole(u, ev.currentTarget.value)}
                      >
                        <option value={u.role}>{ROLE_LABELS[u.role] ?? u.role}</option>
                        <For each={settable()}>{(r) => <option value={r}>{ROLE_LABELS[r] ?? r}</option>}</For>
                      </select>
                    </Show>
                    <Show when={u.role === 'guest' && u.expires_at}>
                      <div class="mono" style="font-size:10px;color:var(--text-2);margin-top:3px">{fmtExpiry(u.expires_at)}</div>
                    </Show>
                  </div>
                  <div class="mono" data-label="设备" style="font-size:12px;color:var(--text-1)">
                    {u.devices}
                  </div>
                  <div data-label="最后活跃" style="flex-grow:1;font-size:12px;color:var(--text-2)">{timeAgo(u.last_seen)}</div>
                  <div class="table-actions">
                    <button
                      class="hit btn btn-sm"
                      classList={{ loading: busy() === `toggle-${u.id}` }}
                      disabled={self || busy() !== ''}
                      onClick={() => void toggle(u)}
                    >
                      {u.disabled ? '启用' : '禁用'}
                    </button>
                    <button
                      class="hit btn btn-sm"
                      classList={{ 'btn-danger': !self, loading: busy() === `del-${u.id}` }}
                      style="width:28px;padding:0"
                      disabled={self || busy() !== ''}
                      onClick={() => void remove(u)}
                    >
                      {el(icon('trash', 13, self ? 'var(--text-3)' : 'var(--red)'))}
                    </button>
                  </div>
                </div>
              );
            }}
          </For>
        </Show>
      </div>
    </Show>
  );
}
