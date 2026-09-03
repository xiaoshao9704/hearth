// 管理后台（服务器级，仅管理员）：服务状态 / 服务参数 / 用户 / 房间 / 邀请。
// Solid 渲染：每个配置组与实例表单各持一份局部输入状态，保存一处或展开表单不再重建整页，
// 别处未提交的输入不会被清掉；配置组按「当前输入 vs 已保存值」自己算脏，脏时才允许保存并拦截切 tab。
import { createEffect, createMemo, createSignal, For, Show } from 'solid-js';
import { render } from 'solid-js/web';
import {
  adminCreateInvite,
  adminCreateProvider,
  adminDeleteChannel,
  adminDeleteInvite,
  adminDeleteProvider,
  adminDeleteUser,
  adminGetConfig,
  adminListInvites,
  adminListProviders,
  adminListUsers,
  adminOverview,
  adminSetConfig,
  adminSetPolicy,
  adminSetUserDisabled,
  adminUpdateProvider,
  getUser,
  listChannels,
} from '../api';
import type { AdminOverview, AdminUser, Channel, ConfigItem, Invite, ProviderField, ProviderInstance, ProviderType } from '../api';
import { avatarHtml, copyText, el, icon, timeAgo, toast } from '../ui';
import { menuButtonHtml, wireMenuButton } from '../shell';

type Tab = 'status' | 'config' | 'users' | 'rooms' | 'invites';

const NAV: { id: Tab; label: string; icon: string; sub: string }[] = [
  { id: 'status', label: '服务状态', icon: 'pulse', sub: '常驻进程与宿主资源' },
  { id: 'config', label: '服务参数', icon: 'gear', sub: '组件地址与注册策略' },
  { id: 'users', label: '用户', icon: 'users', sub: '账号、设备与启停' },
  { id: 'rooms', label: '房间', icon: 'volume', sub: '频道、房主与可见性' },
  { id: 'invites', label: '邀请', icon: 'mail', sub: '生成有时效的注册链接' },
];

// 各 tab 共用的「加载中 / 出错」占位
function Placeholder(props: { err: string }) {
  return (
    <Show when={props.err} fallback={<div class="muted">加载中…</div>}>
      <div class="error-text">{props.err}</div>
    </Show>
  );
}

function fmtUptime(s: number): string {
  if (s < 3600) return `${Math.floor(s / 60)} 分钟`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时`;
  return `${Math.floor(s / 86400)} 天`;
}

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

  const closeNav = () => root.querySelector('.app-frame')?.classList.remove('nav-open');
  const guardNav = (ev: MouseEvent, href: string) => {
    if (dirtyGroups().length === 0 || href === location.hash) return;
    ev.preventDefault();
    setPendingNav(href);
  };

  const App = () => (
    <div class="app-frame">
      <div class="nav-scrim" onClick={closeNav}></div>
      <aside class="sidebar" style="width:210px;background-image:none">
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
                onClick={(ev) => guardNav(ev, `#/admin/${n.id}`)}
              >
                {el(icon(n.icon, 16, n.id === tab ? 'var(--ember)' : 'var(--text-2)', 1.6))}
                <span style="flex-grow:1">{n.label}</span>
                <span class="badge-n mono" data-badge={n.id}></span>
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
        <header class="topbar" style="height:62px;padding:0 24px">
          {el(menuButtonHtml())}
          <h1 style="font-size:16px">{meta.label}</h1>
          <span class="sub" style="color:var(--text-2)">{meta.sub}</span>
          <div class="spacer"></div>
          <div style="display:flex;align-items:center;gap:8px;padding:5px 10px 5px 5px;border-radius:8px;background:var(--bg-3);border:1px solid var(--line)">
            {el(avatarHtml(me.username, 'avatar avatar-sm'))}
            <span style="font-size:12px;color:var(--text-1)">{me.username} · 管理员</span>
          </div>
        </header>
        <div
          style="flex-grow:1;padding:22px 24px;overflow-y:auto;display:flex;flex-direction:column;gap:18px"
          id="admin-body"
        >
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
          ) : tab === 'rooms' ? (
            <RoomsTab />
          ) : (
            <InvitesTab />
          )}
        </div>
      </div>
    </div>
  );

  // 挂在自建容器里而不是 root：dispose() 会清空容器内容，而路由切换时新视图已经写进 root 了，
  // 直接挂 root 会让后到的 dispose 把新视图一起清掉。
  root.innerHTML = '';
  const host = document.createElement('div');
  host.style.height = '100%';
  root.appendChild(host);
  const dispose = render(App, host);
  wireMenuButton(root);

  const myHash = location.hash;
  const onHashChange = () => {
    if (location.hash !== myHash) {
      window.removeEventListener('hashchange', onHashChange);
      dispose();
      host.remove();
    }
  };
  window.addEventListener('hashchange', onHashChange);
}

// ---- 服务状态 ----

function StatusTab() {
  const [ov, setOv] = createSignal<AdminOverview>();
  const [err, setErr] = createSignal('');
  void adminOverview()
    .then((o) => setOv(o))
    .catch((e) => setErr((e as Error).message));

  const resources = () => {
    const r = ov()!.resources;
    const loadPct = r.load !== null ? Math.min(100, Math.round((r.load / Math.max(1, r.cpus)) * 100)) : null;
    const memPct = r.mem_used_mb !== null && r.mem_total_mb ? Math.round((r.mem_used_mb / r.mem_total_mb) * 100) : null;
    return [
      { label: `负载（${r.cpus} 核）`, value: r.load !== null ? r.load.toFixed(2) : '不可用', pct: loadPct },
      {
        label: '内存',
        value:
          r.mem_used_mb !== null
            ? `${((r.mem_used_mb ?? 0) / 1024).toFixed(1)} / ${((r.mem_total_mb ?? 0) / 1024).toFixed(1)} GB`
            : '不可用',
        pct: memPct,
      },
      { label: '温度', value: r.temp_c !== null ? `${r.temp_c.toFixed(0)} °C` : '不可用', pct: r.temp_c },
    ];
  };

  const services = () => {
    const o = ov()!;
    return [
      {
        name: 'hearth-server',
        meta: `Go 单体 · ${o.go_version} · 已运行 ${fmtUptime(o.uptime_seconds)}`,
        ok: true,
        state: 'running',
      },
      {
        name: `语音内核 · ${o.services.voice?.name ?? '?'}`,
        meta: o.services.voice?.url || '进程内嵌（/providers/<alias>/voice）',
        ok: o.services.voice?.ok ?? false,
        state: o.services.voice?.ok ? 'running' : 'unreachable',
      },
      {
        name: `舞台内核 · ${o.services.stage?.name ?? '?'}`,
        meta: o.services.stage?.name === 'none' ? '未启用（投屏/摄像头不可用）' : (o.services.stage?.url ?? ''),
        ok: o.services.stage?.ok ?? false,
        state: o.services.stage?.ok ? 'running' : o.services.stage?.name === 'none' ? 'off' : 'unreachable',
      },
      {
        name: `推流入口 · ${o.services.ingest?.name ?? '?'}`,
        meta: o.services.ingest?.ok ? o.services.ingest.url : '未启用（在服务参数里补齐所选内核的配置）',
        ok: o.services.ingest?.ok ?? false,
        state: o.services.ingest?.ok ? 'running' : 'off',
      },
      { name: '数据库', meta: o.services.db?.url ?? '', ok: true, state: 'ok' },
    ];
  };

  return (
    <Show when={ov()} fallback={<Placeholder err={err()} />}>
      <div class="stat-cards">
        <div class="stat-card">
          <div class="s-label">注册用户</div>
          <div class="s-value">
            <span class="n">{ov()!.users}</span>
            <span class="u mono">个</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="s-label">在房人数</div>
          <div class="s-value">
            <span class="n">{ov()!.online}</span>
            <span class="u mono">人</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="s-label">频道</div>
          <div class="s-value">
            <span class="n">{ov()!.channels}</span>
            <span class="u mono">个</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="s-label">运行时长</div>
          <div class="s-value">
            <span class="n" style="font-size:20px">
              {fmtUptime(ov()!.uptime_seconds)}
            </span>
          </div>
        </div>
      </div>
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:16px">
        <div class="list-box">
          <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600">
            常驻组件
          </div>
          <For each={services()}>
            {(sv) => (
              <div class="svc-row">
                <div class="dot" classList={{ down: !sv.ok }}></div>
                <div style="flex-grow:1;min-width:0">
                  <div class="s-name">{sv.name}</div>
                  <div class="s-meta mono">{sv.meta}</div>
                </div>
                <span class="mono" style={`font-size:11px;color:${sv.ok ? 'var(--sage)' : 'var(--red)'}`}>
                  {sv.state}
                </span>
              </div>
            )}
          </For>
        </div>
        <div class="list-box">
          <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600">
            宿主资源
          </div>
          <div style="padding:16px 18px;display:flex;flex-direction:column;gap:15px">
            <For each={resources()}>
              {(res) => (
                <div>
                  <div style="display:flex;align-items:baseline;gap:8px">
                    <span style="font-size:12px;color:var(--text-1);flex-grow:1">{res.label}</span>
                    <span class="mono" style="font-size:11.5px">
                      {res.value}
                    </span>
                  </div>
                  <Show when={res.pct !== null}>
                    <div class="res-bar">
                      <div classList={{ hot: res.pct! > 80 }} style={`width:${Math.min(100, res.pct!)}%`}></div>
                    </div>
                  </Show>
                </div>
              )}
            </For>
            <div style="font-size:10.5px;color:var(--text-3)">资源读数来自 /proc 与 /sys，仅 Linux 宿主可用。</div>
          </div>
        </div>
      </div>
    </Show>
  );
}

// ---- 服务参数 ----

const CAP_LABELS: Record<string, string> = { voice: '语音', stage: '舞台', ingest: '推流' };
// 内建类型不在可注册类型列表里，展示名这里补
const TYPE_LABELS: Record<string, string> = { ember: 'Ember（内置语音）', bellows: 'Bellows（内置推流网关）' };
// 枚举值的人话标签（值本身仍以英文存库）；选择器可选项是实例 alias，未知名直接显示原值
const KERNEL_LABELS: Record<string, string> = {
  livekit: 'LiveKit',
  ember: 'Ember（内置语音）',
  bellows: 'Bellows（内置推流网关）',
  none: '关闭',
  auto: '自动',
  off: '关闭',
};
const GROUP_META: Record<string, [string, string]> = {
  core: ['内核选择', '语音 / 舞台（投屏）/ 推流入口分别选服务实例'],
  livekit: ['LiveKit', '信令与令牌签发'],
  voice: ['Ember 内置语音', '本进程直出音频，UDP 单端口'],
  ingress: ['推流入口（OBS）', 'WHIP 上游与公开地址'],
  network: ['网络', '向默认网关申请端口映射，仅 host 网络或裸机可用'],
};
const POLICIES = [
  { id: 'closed', label: '关闭注册', desc: '只能用 CLI 在服务器上开通' },
  { id: 'invite', label: '邀请制', desc: '管理员发链接，有效期内可自助注册' },
  { id: 'open', label: '开放注册', desc: '任何人都能注册——公网上不建议' },
];

function ConfigTab(props: { onDirty: (groups: string[]) => void }) {
  const [ov, setOv] = createSignal<AdminOverview>();
  const [items, setItems] = createSignal<ConfigItem[]>([]);
  const [instances, setInstances] = createSignal<ProviderInstance[]>([]);
  const [types, setTypes] = createSignal<ProviderType[]>([]);
  const [policy, setPolicy] = createSignal('');
  const [err, setErr] = createSignal('');
  // 输入草稿：只存被改过的键，未改的键取 ConfigItem.value（= 已保存值），两者比较即脏标记
  const [draft, setDraft] = createSignal<Record<string, string>>({});

  const [pMode, setPMode] = createSignal<'create' | 'edit'>('create');
  const [pAlias, setPAlias] = createSignal('');
  const [pType, setPType] = createSignal('');
  const [pValues, setPValues] = createSignal<Record<string, string>>({});

  void (async () => {
    try {
      const [o, its, provs] = await Promise.all([adminOverview(), adminGetConfig(), adminListProviders()]);
      setItems(its);
      setInstances(provs.instances);
      setTypes(provs.types);
      setPType(provs.types[0]?.type ?? '');
      setPolicy(o.policy);
      setOv(o);
    } catch (e) {
      setErr((e as Error).message);
    }
  })();

  const typeLabel = (t: string) => types().find((x) => x.type === t)?.label ?? TYPE_LABELS[t] ?? t;
  const fieldsOf = (t: string) => types().find((x) => x.type === t)?.fields ?? [];

  // 实例增删改后选择器可选项跟着变，配置项一并重拉（草稿不动，别处未保存的输入保留）
  const reloadProviders = async () => {
    const provs = await adminListProviders();
    setInstances(provs.instances);
    setTypes(provs.types);
    setItems(await adminGetConfig());
  };

  const groups = createMemo(() => [...new Set(items().map((it) => it.group))]);
  const groupItems = (g: string) => items().filter((it) => it.group === g);
  const valOf = (it: ConfigItem) => draft()[it.name] ?? it.value;
  const setVal = (it: ConfigItem, v: string) => setDraft((d) => ({ ...d, [it.name]: v }));
  const dirtyOf = (g: string) => groupItems(g).some((it) => !it.locked && valOf(it) !== it.value);
  const dirty = createMemo(() => groups().filter(dirtyOf));
  createEffect(() => props.onDirty(dirty()));

  const saveGroup = async (g: string) => {
    const values: Record<string, string> = {};
    for (const it of groupItems(g)) {
      if (it.locked) continue;
      const v = valOf(it);
      if (it.options?.length) {
        values[it.name] = v;
        continue;
      }
      // 密钥留空表示保持不变，不提交
      if (it.secret && it.set && v.trim() === '') continue;
      values[it.name] = v.trim();
    }
    try {
      await adminSetConfig(values);
      toast('已保存并生效', 'ok');
      const fresh = await adminGetConfig();
      setItems(fresh);
      setDraft((d) => {
        const next = { ...d };
        for (const it of fresh) if (it.group === g) delete next[it.name];
        return next;
      });
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  const resetProviderForm = () => {
    setPMode('create');
    setPAlias('');
    setPType(types()[0]?.type ?? '');
    setPValues({});
  };

  const editing = () => (pMode() === 'edit' ? instances().find((i) => i.alias === pAlias()) : undefined);

  const submitProvider = async () => {
    // 全量替换语义：该类型全部字段都提交
    const params: Record<string, string> = {};
    for (const f of fieldsOf(pType())) params[f.name] = (pValues()[f.name] ?? '').trim();
    try {
      if (pMode() === 'edit') {
        await adminUpdateProvider(pAlias(), params);
        toast('实例已保存并生效', 'ok');
      } else {
        await adminCreateProvider({ type: pType(), alias: pAlias().trim(), params });
        toast('实例已注册', 'ok');
      }
      resetProviderForm();
      await reloadProviders();
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  const deleteProvider = async (alias: string) => {
    if (!confirm(`删除实例 ${alias}？选择器仍引用它时服务端会拒绝；删除不可恢复。`)) return;
    try {
      await adminDeleteProvider(alias);
      if (pMode() === 'edit' && pAlias() === alias) resetProviderForm();
      await reloadProviders();
      toast('实例已删除', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  const fieldInput = (f: ProviderField) => {
    const placeholder = () =>
      f.secret && pMode() === 'edit' && editing()?.params_set[f.name] ? '已设置（留空保持不变）' : f.hint;
    return (
      <div>
        <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">{f.label}</div>
        <div class="field" style="height:38px;background:var(--bg-2)">
          <input
            class="mono"
            style="font-size:12px"
            type={f.secret ? 'password' : 'text'}
            value={pValues()[f.name] ?? ''}
            placeholder={placeholder()}
            autocomplete="off"
            onInput={(ev) => setPValues((v) => ({ ...v, [f.name]: ev.currentTarget.value }))}
          />
        </div>
      </div>
    );
  };

  const ProvidersCard = () => (
    <div class="card" id="prov-card" style="padding:18px 20px">
      <div style="display:flex;align-items:baseline;gap:9px;margin-bottom:4px">
        <div style="font-size:13px;font-weight:600">服务实例</div>
        <div style="font-size:11px;color:var(--text-2)">语音 / 舞台 / 推流接入的内核实例；内建与环境变量锁定的只读</div>
      </div>
      <div class="table-box" style="margin-top:11px">
        <div class="table-head">
          <div style="width:170px">alias</div>
          <div style="width:170px">类型</div>
          <div style="flex-grow:1">能力</div>
          <div style="width:90px">来源</div>
          <div style="width:150px;text-align:right">操作</div>
        </div>
        <Show when={instances().length > 0} fallback={<div class="table-empty">没有实例。</div>}>
          <For each={instances()}>
            {(inst) => (
              <div class="table-row">
                <div class="mono" style="width:170px;font-size:12.5px;font-weight:600;color:var(--text-0)">
                  {inst.alias}
                </div>
                <div style="width:170px;font-size:12px;color:var(--text-1)">{typeLabel(inst.type)}</div>
                <div style="flex-grow:1;font-size:12px;color:var(--text-1)">
                  {inst.caps.map((c) => CAP_LABELS[c] ?? c).join(' / ') || '—'}
                </div>
                <div style="width:90px">
                  {inst.builtin ? (
                    <span class="chip tag-ember">内建</span>
                  ) : inst.locked ? (
                    <span
                      class="chip"
                      style="background:var(--bg-4);color:var(--text-1)"
                      title="部署侧 .env / compose 里改，重启生效"
                    >
                      环境锁定
                    </span>
                  ) : (
                    <span class="chip tag-sage">DB</span>
                  )}
                </div>
                <div style="width:150px;display:flex;gap:7px;justify-content:flex-end">
                  <Show when={!inst.builtin && !inst.locked}>
                    <button
                      class="hit btn btn-sm"
                      onClick={() => {
                        // params 里 Secret 已被服务端掩码为空串，提交时空串 = 保留旧值
                        setPMode('edit');
                        setPAlias(inst.alias);
                        setPType(inst.type);
                        setPValues({ ...inst.params });
                      }}
                    >
                      编辑
                    </button>
                    <button class="hit btn btn-sm btn-danger" onClick={() => void deleteProvider(inst.alias)}>
                      删除
                    </button>
                  </Show>
                </div>
              </div>
            )}
          </For>
        </Show>
      </div>
      <Show
        when={pMode() === 'edit' && editing()}
        fallback={
          <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
            <div style="display:flex;align-items:baseline;gap:9px">
              <div style="font-size:12.5px;font-weight:600">注册新实例</div>
              <div style="font-size:11px;color:var(--text-3)">注册后在「内核选择」里按 alias 选用</div>
            </div>
            <div style="display:flex;gap:20px;margin-top:12px;align-items:flex-end;flex-wrap:wrap">
              <div>
                <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">类型</div>
                <div class="seg-group" style="background:var(--bg-2)">
                  <For each={types()}>
                    {(t) => (
                      <button
                        class="hit seg"
                        classList={{ on: pType() === t.type }}
                        onClick={() => {
                          setPType(t.type);
                          setPValues({}); // 类型不同字段不同，已填值作废
                        }}
                      >
                        {t.label}
                      </button>
                    )}
                  </For>
                </div>
              </div>
              <div style="flex-grow:1;min-width:220px">
                <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">
                  alias（小写字母数字与 -，会出现在 /providers/&lt;alias&gt; 连接路径里）
                </div>
                <div class="field" style="height:38px;background:var(--bg-2)">
                  <input
                    class="mono"
                    style="font-size:12px"
                    id="prov-alias"
                    value={pAlias()}
                    placeholder="如 lk-main"
                    onInput={(ev) => setPAlias(ev.currentTarget.value)}
                  />
                </div>
              </div>
            </div>
            <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:11px;margin-top:12px">
              <For each={fieldsOf(pType())}>{(f) => fieldInput(f)}</For>
            </div>
            <div style="display:flex;justify-content:flex-end;margin-top:13px">
              <button class="hit btn btn-sm btn-primary" onClick={() => void submitProvider()}>
                注册实例
              </button>
            </div>
          </div>
        }
      >
        <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
          <div style="display:flex;align-items:baseline;gap:9px">
            <div style="font-size:12.5px;font-weight:600">
              编辑实例 <span class="mono">{editing()!.alias}</span>（{typeLabel(editing()!.type)}）
            </div>
            <div style="font-size:11px;color:var(--text-3)">保存为全量替换；密钥留空 = 保持不变</div>
            <div class="spacer"></div>
            <button class="hit btn btn-sm" onClick={resetProviderForm}>
              取消
            </button>
          </div>
          <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:11px;margin-top:12px">
            <For each={fieldsOf(editing()!.type)}>{(f) => fieldInput(f)}</For>
          </div>
          <div style="display:flex;justify-content:flex-end;margin-top:13px">
            <button class="hit btn btn-sm btn-primary" onClick={() => void submitProvider()}>
              保存并生效
            </button>
          </div>
        </div>
      </Show>
    </div>
  );

  // 依赖服务配置卡片：环境变量固定的只读展示；未固定的可编辑，保存落库即时生效
  const GroupCard = (gp: { group: string }) => {
    const list = () => groupItems(gp.group);
    const title = () => GROUP_META[gp.group]?.[0] ?? gp.group;
    const sub = () => GROUP_META[gp.group]?.[1] ?? '';
    return (
      <div class="card">
        <div style="display:flex;align-items:baseline;gap:9px;margin-bottom:4px">
          <div style="font-size:13px;font-weight:600">{title()}</div>
          <div style="font-size:11px;color:var(--text-2);flex-grow:1">{sub()}</div>
          <Show when={dirtyOf(gp.group)}>
            <span class="tag tag-ember">未保存</span>
          </Show>
        </div>
        <div style="display:flex;flex-direction:column;gap:11px;margin-top:11px">
          <For each={list()}>
            {(it) => (
              <div>
                <div style="display:flex;align-items:baseline;gap:8px;margin-bottom:6px">
                  <span style="font-size:11px;color:var(--text-2)">{it.label}</span>
                  <span class="mono" style="font-size:10px;color:var(--text-3)">
                    {it.env}
                  </span>
                  <Show when={it.locked}>
                    <span class="tag" title="部署侧 .env / compose 里改，重启生效">
                      环境变量固定
                    </span>
                  </Show>
                </div>
                {it.locked ? (
                  <div
                    class="mono"
                    style="padding:9px 12px;border-radius:8px;background:var(--bg-2);border:1px solid var(--line);font-size:12px;color:var(--text-1);overflow:hidden;text-overflow:ellipsis;white-space:nowrap"
                  >
                    {it.secret ? '••••••••' : it.value || '（空）'}
                  </div>
                ) : it.options?.length ? (
                  <div class="seg-group" style="background:var(--bg-2)">
                    <For each={it.options}>
                      {(opt) => (
                        <button class="hit seg" classList={{ on: valOf(it) === opt }} onClick={() => setVal(it, opt)}>
                          {KERNEL_LABELS[opt] ?? opt}
                        </button>
                      )}
                    </For>
                  </div>
                ) : (
                  <div class="field" style="height:38px;background:var(--bg-2)">
                    <input
                      class="mono"
                      style="font-size:12px"
                      value={valOf(it)}
                      placeholder={it.secret && it.set ? '已设置（输入新值覆盖）' : it.hint}
                      autocomplete={it.secret ? 'off' : undefined}
                      onInput={(ev) => setVal(it, ev.currentTarget.value)}
                    />
                  </div>
                )}
              </div>
            )}
          </For>
        </div>
        <Show when={list().some((it) => !it.locked)}>
          <div style="display:flex;align-items:center;gap:10px;margin-top:13px">
            <span style="font-size:11px;color:var(--text-3);flex-grow:1">
              {dirtyOf(gp.group) ? '有改动还没保存，点右边生效' : '保存后立即生效，无需重启'}
            </span>
            <button
              class="hit btn btn-sm btn-primary"
              classList={{ disabled: !dirtyOf(gp.group) }}
              onClick={() => {
                if (!dirtyOf(gp.group)) return;
                void saveGroup(gp.group);
              }}
            >
              保存并生效
            </button>
          </div>
        </Show>
      </div>
    );
  };

  return (
    <Show when={ov()} fallback={<Placeholder err={err()} />}>
      <ProvidersCard />
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(340px,1fr));gap:16px">
        <For each={groups()}>{(g) => <GroupCard group={g} />}</For>
        <div class="card">
          <div style="font-size:13px;font-weight:600;margin-bottom:5px">注册策略</div>
          <div style="font-size:11.5px;color:var(--text-2);margin-bottom:13px">决定新账号怎么来，改动立即生效</div>
          <div style="display:flex;flex-direction:column;gap:8px">
            <For each={POLICIES}>
              {(p) => {
                const on = () => policy() === p.id;
                return (
                  <button
                    class="hit"
                    style={`display:flex;align-items:flex-start;gap:11px;padding:11px 12px;border-radius:9px;border:1px solid ${on() ? 'var(--ember-line)' : 'var(--line)'};background:${on() ? 'var(--ember-tint)' : 'var(--bg-2)'};text-align:left;width:100%`}
                    onClick={async () => {
                      try {
                        const r = await adminSetPolicy(p.id);
                        setPolicy(r.policy);
                        toast('注册策略已更新', 'ok');
                      } catch (e) {
                        toast((e as Error).message, 'bad');
                      }
                    }}
                  >
                    <div class="radio" classList={{ on: on() }} style="margin-top:1px">
                      <div class="dot"></div>
                    </div>
                    <div style="flex-grow:1">
                      <div
                        style={`font-size:13px;font-weight:${on() ? 600 : 500};color:${on() ? 'var(--ember)' : 'var(--text-0)'}`}
                      >
                        {p.label}
                      </div>
                      <div style="font-size:11px;color:var(--text-2);margin-top:2px">{p.desc}</div>
                    </div>
                  </button>
                );
              }}
            </For>
          </div>
          <div style="margin-top:13px;font-size:11px;line-height:1.6;color:var(--text-3)">
            数据库：<span class="mono">{ov()!.services.db?.url ?? ''}</span>
            <br />
            媒体端口与域名在部署侧（.env / compose）改，改完重启容器生效。
          </div>
        </div>
      </div>
    </Show>
  );
}

// ---- 用户 ----

function UsersTab() {
  const [users, setUsers] = createSignal<AdminUser[]>();
  const [err, setErr] = createSignal('');
  const [query, setQuery] = createSignal('');
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

  const toggle = async (u: AdminUser) => {
    try {
      await adminSetUserDisabled(u.id, !u.disabled);
      toast(u.disabled ? '已启用该账号' : '已禁用该账号', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  const remove = async (u: AdminUser) => {
    if (!confirm(`删除用户 ${u.username}？其会话、设备档案和白名单记录一并清除，不可恢复。`)) return;
    try {
      await adminDeleteUser(u.id);
      toast('用户已删除', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  return (
    <Show when={users()} fallback={<Placeholder err={err()} />}>
      <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        <div class="field" style="width:280px;height:38px">
          {el(icon('search', 15, 'var(--text-2)'))}
          <input id="u-query" placeholder="搜用户名" value={query()} onInput={(ev) => setQuery(ev.currentTarget.value)} />
        </div>
        <div class="spacer"></div>
        <a class="hit btn btn-primary" href="#/admin/invites">
          {el(icon('plus', 15, 'var(--on-ember)', 1.9))} 邀请新用户
        </a>
      </div>
      <div class="table-box">
        <div class="table-head">
          <div style="width:200px">用户</div>
          <div style="width:90px">角色</div>
          <div style="width:80px">设备</div>
          <div style="flex-grow:1">最后活跃</div>
          <div style="width:170px;text-align:right">操作</div>
        </div>
        <Show when={shown().length > 0} fallback={<div class="table-empty">没有匹配的用户。</div>}>
          <For each={shown()}>
            {(u) => {
              const self = u.id === meId;
              return (
                <div class="table-row" classList={{ dim: u.disabled }}>
                  <div style="width:200px;display:flex;align-items:center;gap:10px;min-width:0">
                    {el(avatarHtml(u.username))}
                    <div style="min-width:0">
                      <div style="font-size:13px;font-weight:500">{u.username}</div>
                      <div class="mono" style="font-size:10px;color:var(--text-2);margin-top:2px">
                        usr_{u.id}
                      </div>
                    </div>
                  </div>
                  <div style="width:90px">
                    <span
                      class="chip"
                      classList={{ 'tag-ember': u.is_admin }}
                      style={u.is_admin ? '' : 'background:var(--bg-4);color:var(--text-1)'}
                    >
                      {u.is_admin ? '管理员' : '成员'}
                    </span>
                  </div>
                  <div class="mono" style="width:80px;font-size:12px;color:var(--text-1)">
                    {u.devices}
                  </div>
                  <div style="flex-grow:1;font-size:12px;color:var(--text-2)">{timeAgo(u.last_seen)}</div>
                  <div style="width:170px;display:flex;gap:7px;justify-content:flex-end">
                    <button class="hit btn btn-sm" classList={{ disabled: self }} onClick={() => !self && void toggle(u)}>
                      {u.disabled ? '启用' : '禁用'}
                    </button>
                    <button
                      class="hit btn btn-sm"
                      classList={{ disabled: self, 'btn-danger': !self }}
                      style="width:28px;padding:0"
                      onClick={() => !self && void remove(u)}
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

// ---- 房间 ----

function RoomsTab() {
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

  const remove = async (c: Channel) => {
    if (!confirm(`删除频道「${c.name}」？聊天记录、黑白名单和推流 key 一并清除，不可恢复。`)) return;
    try {
      await adminDeleteChannel(c.id);
      toast('频道已删除', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  return (
    <Show when={channels()} fallback={<Placeholder err={err()} />}>
      <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        <div style="font-size:13px;color:var(--text-2)">
          常驻频道进程一直在，人进来就连上——不需要「开房」。建频道在大厅操作。
        </div>
      </div>
      <div class="table-box">
        <div class="table-head">
          <div style="width:190px">频道</div>
          <div style="width:90px">在线</div>
          <div style="width:140px">房主</div>
          <div style="flex-grow:1">可见性</div>
          <div style="width:100px;text-align:right">操作</div>
        </div>
        <Show when={channels()!.length > 0} fallback={<div class="table-empty">还没有频道。</div>}>
          <For each={channels()}>
            {(c) => (
              <div class="table-row">
                <div style="width:190px;display:flex;align-items:center;gap:9px;min-width:0">
                  {el(icon('volume', 15, c.online ? 'var(--ember)' : 'var(--text-2)'))}
                  <span style="font-size:13px;font-weight:500">{c.name}</span>
                </div>
                <div class="mono" style="width:90px;font-size:12px;color:var(--text-1)">
                  {c.online ? `${c.online} 人` : '—'}
                </div>
                <div style="width:140px;font-size:12px;color:var(--text-1)">{c.created_by}</div>
                <div style="flex-grow:1">
                  <span
                    class="chip"
                    classList={{ 'tag-ember': c.invite_only }}
                    style={c.invite_only ? '' : 'background:var(--bg-4);color:var(--text-1)'}
                  >
                    {c.invite_only ? '邀请制' : '公开'}
                  </span>
                </div>
                <div style="width:100px;display:flex;justify-content:flex-end">
                  <button class="hit btn btn-sm btn-danger" onClick={() => void remove(c)}>
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

// ---- 邀请 ----

function stateOf(iv: Invite): { label: string; cls: string; dead: boolean } {
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

function InvitesTab() {
  const [invites, setInvites] = createSignal<Invite[]>();
  const [base, setBase] = createSignal('');
  const [err, setErr] = createSignal('');
  const [ttl, setTtl] = createSignal('24h');
  const [uses, setUses] = createSignal('1');
  const [note, setNote] = createSignal('');
  const [fresh, setFresh] = createSignal('');

  const load = async () => {
    try {
      const r = await adminListInvites();
      setInvites(r.invites);
      setBase(r.base);
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  void load();

  const make = async () => {
    try {
      const r = await adminCreateInvite(note(), Number(uses()), ttl());
      setFresh(r.url);
      setNote('');
      toast('邀请链接已生成', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  const revoke = async (iv: Invite, dead: boolean) => {
    try {
      await adminDeleteInvite(iv.id);
      toast(dead ? '邀请已删除' : '邀请已撤销', 'ok');
      await load();
    } catch (e) {
      toast((e as Error).message, 'bad');
    }
  };

  const Seg = (p: { val: () => string; set: (v: string) => void; opts: [string, string][] }) => (
    <div class="seg-group" style="background:var(--bg-2)">
      <For each={p.opts}>
        {([v, label]) => (
          <button class="hit seg" classList={{ on: p.val() === v }} onClick={() => p.set(v)}>
            {label}
          </button>
        )}
      </For>
    </div>
  );

  return (
    <Show when={invites()} fallback={<Placeholder err={err()} />}>
      <div class="card" style="padding:18px 20px">
        <div style="font-size:13.5px;font-weight:600">生成邀请链接</div>
        <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">链接在有效期内可用，点开就能自己设账号密码</div>
        <div style="display:flex;gap:20px;margin-top:16px;align-items:flex-end;flex-wrap:wrap">
          <div>
            <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">有效期</div>
            <Seg
              val={ttl}
              set={setTtl}
              opts={[
                ['1h', '1 小时'],
                ['24h', '24 小时'],
                ['7d', '7 天'],
              ]}
            />
          </div>
          <div>
            <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">可用次数</div>
            <Seg
              val={uses}
              set={setUses}
              opts={[
                ['1', '1 次'],
                ['5', '5 次'],
                ['0', '不限'],
              ]}
            />
          </div>
          <div style="flex-grow:1;min-width:160px">
            <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">备注（给谁）</div>
            <div class="field" style="height:38px;background:var(--bg-2)">
              <input id="iv-note" value={note()} onInput={(ev) => setNote(ev.currentTarget.value)} />
            </div>
          </div>
          <button class="hit btn btn-primary" id="iv-make" onClick={() => void make()}>
            生成链接
          </button>
        </div>
        <Show when={fresh()}>
          <div style="display:flex;align-items:center;gap:10px;height:42px;margin-top:14px;padding:0 6px 0 14px;border-radius:9px;background:var(--sage-tint);border:1px solid var(--sage-line)">
            <span class="mono" style="font-size:12.5px;flex-grow:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">
              {fresh()}
            </span>
            <button
              class="hit btn btn-sm"
              id="iv-copy"
              onClick={async () => {
                if (await copyText(fresh())) toast('已复制', 'ok', 1400);
              }}
            >
              {el(icon('copy', 13))} 复制
            </button>
          </div>
        </Show>
      </div>
      <div class="table-box">
        <div class="table-head">
          <div style="width:150px">邀请码</div>
          <div style="width:150px">备注</div>
          <div style="width:110px">可用次数</div>
          <div style="flex-grow:1">状态</div>
          <div style="width:150px;text-align:right">操作</div>
        </div>
        <Show when={invites()!.length > 0} fallback={<div class="table-empty">还没有发过邀请。</div>}>
          <For each={invites()}>
            {(iv) => {
              const st = stateOf(iv);
              return (
                <div class="table-row" classList={{ dim: st.dead }}>
                  <div class="mono" style="width:150px;font-size:12px;color:var(--text-1)">
                    {iv.code}
                  </div>
                  <div style="width:150px;font-size:12.5px;color:var(--text-1)">{iv.note || '（无）'}</div>
                  <div class="mono" style="width:110px;font-size:12px;color:var(--text-1)">
                    {iv.used} / {iv.max_uses === 0 ? '∞' : iv.max_uses}
                  </div>
                  <div style="flex-grow:1">
                    <span class={`chip ${st.cls}`} style={st.cls ? '' : 'background:var(--bg-4);color:var(--text-2)'}>
                      {st.label}
                    </span>
                  </div>
                  <div style="width:150px;display:flex;gap:7px;justify-content:flex-end">
                    <Show when={!st.dead}>
                      <button
                        class="hit btn btn-sm"
                        onClick={async () => {
                          if (await copyText(`${base()}/#/join/${iv.code}`)) toast('已复制', 'ok', 1400);
                        }}
                      >
                        复制链接
                      </button>
                    </Show>
                    <button class="hit btn btn-sm" onClick={() => void revoke(iv, st.dead)}>
                      {st.dead ? '删除' : '撤销'}
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
