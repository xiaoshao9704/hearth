// 管理后台「房间」tab：频道、房主与可见性。
import { createSignal, For, Show } from 'solid-js';
import { adminDeleteChannel, listChannels } from '../../api';
import type { Channel } from '../../api';
import { confirmDialog, el, icon, toast } from '../../ui';
import { Placeholder } from './shared';

export function RoomsTab() {
  const [channels, setChannels] = createSignal<Channel[]>();
  const [err, setErr] = createSignal('');

  const load = async () => {
    try {
      setChannels(await listChannels());
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  void load();

  const [busy, setBusy] = createSignal(0); // 正在删除的频道 id，0=空闲

  const remove = async (c: Channel) => {
    if (busy()) return;
    const ok = await confirmDialog({
      title: `删除频道「${c.name}」？`,
      body: '聊天记录、黑白名单和推流 key 一并清除，不可恢复。',
      danger: true,
      confirmText: '删除',
    });
    if (!ok) return;
    setBusy(c.id);
    try {
      await adminDeleteChannel(c.id);
      toast('频道已删除', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setBusy(0);
    }
  };

  return (
    <Show when={channels()} fallback={<Placeholder err={err()} />}>
      <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        <div style="font-size:13px;color:var(--text-2)">
          常驻频道进程一直在，人进来就连上——不需要「开房」。建频道在大厅操作。
        </div>
      </div>
      <div class="table-box" style={{ '--col-1': '190px', '--col-2': '90px', '--col-3': '140px', '--col-5': '100px' }}>
        <div class="table-head">
          <div>频道</div>
          <div>在线</div>
          <div>房主</div>
          <div style="flex-grow:1">可见性</div>
          <div style="text-align:right">操作</div>
        </div>
        <Show when={channels()!.length > 0} fallback={<div class="table-empty">还没有频道。</div>}>
          <For each={channels()}>
            {(c) => (
              <div class="table-row">
                <div data-label="频道" style="display:flex;align-items:center;gap:9px;min-width:0">
                  {el(icon('volume', 15, c.online ? 'var(--ember)' : 'var(--text-2)'))}
                  <span class="cell-ellipsis" style="font-size:13px;font-weight:500">{c.name}</span>
                </div>
                <div class="mono" data-label="在线" style="font-size:12px;color:var(--text-1)">
                  {c.online ? `${c.online} 人` : '—'}
                </div>
                <div class="cell-ellipsis" data-label="房主" style="font-size:12px;color:var(--text-1)">{c.created_by}</div>
                <div data-label="可见性" style="flex-grow:1">
                  <span
                    class="chip"
                    classList={{ 'tag-ember': c.invite_only }}
                    style={c.invite_only ? '' : 'background:var(--bg-4);color:var(--text-1)'}
                  >
                    {c.invite_only ? '邀请制' : '公开'}
                  </span>
                </div>
                <div class="table-actions">
                  <button
                    class="hit btn btn-sm btn-danger"
                    classList={{ loading: busy() === c.id }}
                    disabled={busy() !== 0}
                    onClick={() => void remove(c)}
                  >
                    删除
                  </button>
                </div>
              </div>
            )}
          </For>
        </Show>
      </div>
    </Show>
  );
}
