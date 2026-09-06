// 管理后台「审计」tab：管制动作的流水，按频道/操作者/动作筛选，游标往更早翻页。
// 只读视图——审计是事实记录，界面上不提供删除（清理只由 audit_retention_days 定时做）。
import { createSignal, For, Show } from 'solid-js';
import { adminAudit, listChannels } from '../api';
import type { AuditEntry, Channel } from '../api';
import { el, icon, timeAgo, toast } from '../ui';

const PAGE = 50;

// 动作名 → [中文名, 色调类]；未知动作名原样显示（服务端加了新动作、前端还没跟上时不至于空白）
const ACTION_META: Record<string, [string, string]> = {
  mute: ['禁言', 'tag-ember'],
  unmute: ['解禁', 'tag-sage'],
  kick: ['踢出', 'tag-ember'],
  ban: ['封禁', 'tag-red'],
  unban: ['解封', 'tag-sage'],
  channel_role: ['频道角色', ''],
  message_delete: ['删消息', 'tag-ember'],
  channel_clear: ['清空聊天', 'tag-red'],
};

function actionLabel(action: string): string {
  return ACTION_META[action]?.[0] ?? action;
}

export function AuditTab() {
  const [entries, setEntries] = createSignal<AuditEntry[]>();
  const [actions, setActions] = createSignal<string[]>([]);
  const [channels, setChannels] = createSignal<Channel[]>([]);
  const [err, setErr] = createSignal('');
  const [next, setNext] = createSignal(0); // 继续往更早翻的游标，0 = 到底了
  const [busy, setBusy] = createSignal(false);
  // 筛选：频道 id / 操作者 user_id / 动作名（都是「空 = 不筛」）
  const [channel, setChannel] = createSignal(0);
  const [actor, setActor] = createSignal('');
  const [action, setAction] = createSignal('');

  const query = () => ({
    channel: channel() || undefined,
    actor: Number(actor().trim()) || undefined,
    action: action() || undefined,
    limit: PAGE,
  });

  // reload 换筛选条件时从头取；loadMore 用游标续取，两者共用一次请求
  const load = async (cursor: number) => {
    if (busy()) return;
    setBusy(true);
    try {
      const r = await adminAudit({ ...query(), after: cursor || undefined });
      setActions(r.actions);
      setEntries(cursor ? [...(entries() ?? []), ...r.entries] : r.entries);
      setNext(r.next);
    } catch (e) {
      if (cursor) toast((e as Error).message, 'bad');
      else setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  void load(0);
  void listChannels()
    .then(setChannels)
    .catch(() => {}); // 频道下拉拉不到就只剩「全部频道」，不影响列表

  const reload = () => {
    setEntries(undefined);
    setErr('');
    void load(0);
  };

  return (
    <Show when={entries()} fallback={<Placeholder err={err()} />}>
      <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
        <select
          class="hit audit-select"
          value={String(channel())}
          onChange={(ev) => {
            setChannel(Number(ev.currentTarget.value));
            reload();
          }}
        >
          <option value="0">全部频道</option>
          <For each={channels()}>{(c) => <option value={String(c.id)}>{c.name}</option>}</For>
        </select>
        <select
          class="hit audit-select"
          value={action()}
          onChange={(ev) => {
            setAction(ev.currentTarget.value);
            reload();
          }}
        >
          <option value="">全部动作</option>
          <For each={actions()}>{(a) => <option value={a}>{actionLabel(a)}</option>}</For>
        </select>
        <form
          style="flex-shrink:0"
          onSubmit={(ev) => {
            ev.preventDefault();
            reload();
          }}
        >
          <div class="field" style="width:200px;height:38px">
            {el(icon('user', 15, 'var(--text-2)'))}
            <input
              placeholder="操作者 user_id"
              inputmode="numeric"
              value={actor()}
              onInput={(ev) => setActor(ev.currentTarget.value)}
              onBlur={reload}
            />
          </div>
        </form>
        <div class="spacer"></div>
        <button class="hit btn" disabled={busy()} onClick={reload}>
          {el(icon('reset', 15, 'var(--text-1)', 1.8))} 刷新
        </button>
      </div>

      <div class="table-box audit-table" style={{ '--col-1': '150px', '--col-2': '110px', '--col-3': '150px', '--col-4': '130px' }}>
        <div class="table-head">
          <div>时间</div>
          <div>动作</div>
          <div>操作者</div>
          <div>目标</div>
          <div style="flex-grow:1">频道 / 说明</div>
        </div>
        <Show when={entries()!.length > 0} fallback={<div class="table-empty">这段条件下没有审计记录。</div>}>
          <For each={entries()}>
            {(e) => (
              <div class="table-row">
                <div class="mono" data-label="时间" style="font-size:11.5px;color:var(--text-2)" title={e.at}>
                  {timeAgo(e.at)}
                </div>
                <div data-label="动作">
                  <span class={`chip ${ACTION_META[e.action]?.[1] ?? ''}`}>{actionLabel(e.action)}</span>
                </div>
                <div class="cell-ellipsis" data-label="操作者" style="font-size:12.5px;color:var(--text-1)">
                  {e.actor_name || `usr_${e.actor_uid}`}
                </div>
                <div class="cell-ellipsis" data-label="目标" style="font-size:12.5px;color:var(--text-1)">
                  {e.target_uid ? e.target_name || `usr_${e.target_uid}` : '—'}
                </div>
                <div data-label="频道 / 说明" style="flex-grow:1;min-width:0;font-size:12px;color:var(--text-2)">
                  <span class="cell-ellipsis">
                    {[e.channel_name || (e.channel_id ? `chan_${e.channel_id}` : ''), e.detail].filter(Boolean).join(' · ') || '—'}
                  </span>
                </div>
              </div>
            )}
          </For>
        </Show>
      </div>

      <Show when={next() > 0}>
        <button class="hit btn" style="align-self:center" disabled={busy()} onClick={() => void load(next())}>
          {busy() ? '加载中…' : '加载更早的记录'}
        </button>
      </Show>
    </Show>
  );
}

// 与 admin.tsx 的同名占位一致（那个是模块私有的，这里各留一份，避免为它改动 admin.tsx）
function Placeholder(props: { err: string }) {
  return (
    <Show when={props.err} fallback={<div class="muted">加载中…</div>}>
      <div class="error-text">{props.err}</div>
    </Show>
  );
}
