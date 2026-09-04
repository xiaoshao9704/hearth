// 管理后台（服务器级，仅管理员）：服务状态 / 服务参数 / 用户 / 房间。
// Solid 渲染：每个配置组与实例表单各持一份局部输入状态，保存一处或展开表单不再重建整页，
// 别处未提交的输入不会被清掉；配置组按「当前输入 vs 已保存值」自己算脏，脏时才允许保存并拦截切 tab。
import { createEffect, createMemo, createSignal, For, onCleanup, Show } from 'solid-js';
import { render } from 'solid-js/web';
import {
  adminCreateProvider,
  adminDeleteChannel,
  adminDeleteProvider,
  adminDeleteUser,
  adminGetConfig,
  adminGetPolicy,
  adminListProviders,
  adminListUsers,
  adminNetcheck,
  adminOverview,
  adminSetConfig,
  adminSetPolicy,
  adminSetUserDisabled,
  adminSetUserRole,
  adminUpdateProvider,
  adminVersion,
  downloadCACert,
  getUser,
  listChannels,
} from '../api';
import type { AdminOverview, AdminUser, Channel, ConfigItem, NetcheckResult, ProviderField, ProviderInstance, ProviderType, VersionInfo } from '../api';
import { avatarHtml, confirmDialog, el, fmtClock, icon, menuButtonHtml, timeAgo, toast, wireMenuButton } from '../ui';
import { openSettings } from './settings';

type Tab = 'status' | 'config' | 'network' | 'users' | 'rooms';

const NAV: { id: Tab; label: string; icon: string; sub: string }[] = [
  { id: 'status', label: '服务状态', icon: 'pulse', sub: '常驻进程与宿主资源' },
  { id: 'config', label: '服务参数', icon: 'gear', sub: '组件地址与注册策略' },
  { id: 'network', label: '网络', icon: 'globe', sub: '端口映射、证书与外部可达性' },
  { id: 'users', label: '用户', icon: 'users', sub: '账号、角色与启停' },
  { id: 'rooms', label: '房间', icon: 'volume', sub: '频道、房主与可见性' },
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
          ) : tab === 'network' ? (
            <NetworkTab />
          ) : tab === 'users' ? (
            <UsersTab />
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

// ---- 服务状态 ----

function StatusTab() {
  const [ov, setOv] = createSignal<AdminOverview>();
  const [ver, setVer] = createSignal<VersionInfo>();
  const [err, setErr] = createSignal('');
  const [updatedAt, setUpdatedAt] = createSignal(0);
  const [now, setNow] = createSignal(Date.now());

  const refresh = () => {
    adminOverview()
      .then((o) => {
        setOv(o);
        setErr('');
        setUpdatedAt(Date.now());
      })
      .catch((e) => setErr((e as Error).message));
  };
  refresh();
  // 版本检查服务端缓存 1 小时，这里跟着概览拉一次即可，不轮询
  adminVersion()
    .then(setVer)
    .catch(() => {});

  // 后台标签页不轮询；每秒 tick 一下只为了让「更新于 N 秒前」走字
  const poll = setInterval(() => {
    if (document.visibilityState === 'visible') refresh();
  }, 10_000);
  const tick = setInterval(() => setNow(Date.now()), 1000);
  onCleanup(() => {
    clearInterval(poll);
    clearInterval(tick);
  });
  const agoText = () => (updatedAt() ? `更新于 ${Math.max(0, Math.round((now() - updatedAt()) / 1000))} 秒前` : '');

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
      // 温度不是负载/占用率，不画进度条，只显示数值
      { label: '温度', value: r.temp_c !== null ? `${r.temp_c.toFixed(0)} °C` : '不可用', pct: null },
    ];
  };

  const services = () => {
    const o = ov()!;
    return [
      {
        name: 'hearth-server',
        meta: `Go 单体 · ${o.go_version} · 已运行 ${fmtUptime(o.uptime_seconds)}${o.version ? ` · ${o.version}` : ''}`,
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
      <Show when={ver()?.outdated}>
        <div class="card" style="padding:11px 16px;margin-bottom:16px;display:flex;gap:10px;align-items:baseline;font-size:12.5px">
          <span style="color:var(--ember);font-weight:600">新版本可用：{ver()!.latest}</span>
          <span style="color:var(--text-2);flex-grow:1">当前 {ver()!.version}</span>
          <Show when={ver()!.url}>
            <a class="hit" style="color:var(--text-1)" href={ver()!.url} target="_blank" rel="noreferrer">
              去下载
            </a>
          </Show>
        </div>
      </Show>
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
          <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600;display:flex;align-items:baseline;gap:8px">
            <span>常驻组件</span>
            <span class="mono" style="font-size:10px;color:var(--text-3);margin-left:auto">{agoText()}</span>
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
          <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600;display:flex;align-items:baseline;gap:8px">
            <span>宿主资源</span>
            <span class="mono" style="font-size:10px;color:var(--text-3);margin-left:auto">{agoText()}</span>
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

// ---- 网络（自检：端口映射 / 公网地址 / 域名 / 证书 / 外部可达）----

const TLS_MODE_LABELS: Record<string, string> = {
  off: '关闭（纯 HTTP）',
  acme: '自动证书（ACME）',
  selfsigned: '自签名（本地 CA）',
};
const TLS_ACTIVE_LABELS: Record<string, string> = {
  off: '未使用',
  acme: 'ACME 公开证书',
  selfsigned: '自签名兜底证书',
};
const VERDICT_META: Record<string, [string, string]> = {
  reachable: ['外部可达', 'var(--sage)'],
  unknown: ['本机无法确认', 'var(--text-2)'],
  failed: ['不可达', 'var(--red)'],
};
const DOMAIN_MATCH_LABELS: Record<string, string> = {
  ok: '与公网地址一致',
  mismatch: '与公网地址不一致',
  unconfigured: '未配置',
  error: '异常',
};

function NetworkTab() {
  const [nc, setNc] = createSignal<NetcheckResult>();
  const [err, setErr] = createSignal('');
  const [busy, setBusy] = createSignal(false);
  const [caBusy, setCaBusy] = createSignal(false);

  const refresh = async () => {
    setBusy(true);
    try {
      setNc(await adminNetcheck());
      setErr('');
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  void refresh();

  const kv = (k: string, v: string, mono = true) => (
    <div style="display:flex;gap:12px;padding:9px 14px;border-bottom:1px solid var(--line-soft);align-items:baseline">
      <span style="font-size:12px;color:var(--text-2);flex-shrink:0;width:76px">{k}</span>
      <span class={mono ? 'mono' : ''} style="font-size:12px;line-height:1.6;color:var(--text-1);flex-grow:1;word-break:break-all">
        {v}
      </span>
    </div>
  );

  const downloadCA = async () => {
    setCaBusy(true);
    try {
      await downloadCACert();
      toast('CA 证书已下载', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setCaBusy(false);
    }
  };

  return (
    <Show when={nc()} fallback={<Placeholder err={err()} />}>
      {(n) => {
        const verdict = () => VERDICT_META[n().external.verdict] ?? VERDICT_META.unknown;
        return (
          <div style="display:flex;flex-direction:column;gap:16px">
            <div class="card" style="padding:0">
              <div style="display:flex;align-items:center;gap:9px;padding:13px 18px;border-bottom:1px solid var(--line-soft)">
                <span style="font-size:13px;font-weight:600;flex-grow:1">外部可达性</span>
                <button class="hit btn btn-sm" classList={{ loading: busy() }} disabled={busy()} onClick={() => void refresh()}>
                  重新自检
                </button>
              </div>
              <div style="padding:14px 18px;display:flex;align-items:baseline;gap:10px">
                <span style={`font-size:14px;font-weight:600;color:${verdict()[1]}`}>{verdict()[0]}</span>
                <Show when={n().external.url}>
                  <span class="mono" style="font-size:11px;color:var(--text-3)">{n().external.url}</span>
                </Show>
              </div>
              <div style="padding:0 18px 14px;font-size:12px;line-height:1.7;color:var(--text-2)">{n().external.detail}</div>
            </div>

            <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:16px;align-items:start">
              <div class="card" style="padding:0">
                <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600">端口映射</div>
                <div style="padding:4px 4px">
                  {kv('状态', n().portmap.Detail || n().portmap.Diagnosis, false)}
                  {kv('方式', n().portmap.Method || '未发现网关')}
                  <Show when={(n().portmap.Mappings ?? []).length > 0}>
                    {kv(
                      '映射',
                      (n().portmap.Mappings ?? [])
                        .map((mp) => `${mp.Proto} ${mp.Internal} → ${mp.ExternalIP || '?'}:${mp.External}`)
                        .join('，'),
                    )}
                  </Show>
                  <Show when={(n().portmap.Hops ?? []).length > 1}>
                    {kv('级联', (n().portmap.Hops ?? []).map((h) => h.Gateway).join(' → '))}
                  </Show>
                  <Show when={n().portmap.V6Detail}>
                    {kv('IPv6', n().portmap.V6Detail, false)}
                  </Show>
                </div>
              </div>

              <div class="card" style="padding:0">
                <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600">公网地址与域名</div>
                <div style="padding:4px 4px">
                  {kv('探测结果', (n().externals ?? []).length > 0 ? (n().externals ?? []).join('，') : '还没有探测到（STUN 不可达且未建映射）')}
                  <Show when={n().probed_at && !n().probed_at.startsWith('0001')}>
                    {kv('探测时间', fmtClock(n().probed_at))}
                  </Show>
                  {kv('站点域名', n().domain.configured || '未配置')}
                  <Show when={n().domain.configured !== ''}>
                    {kv('域名解析', (n().domain.resolved ?? []).join('，') || '无记录')}
                    {kv('比对', DOMAIN_MATCH_LABELS[n().domain.match] ?? n().domain.match, false)}
                    <Show when={n().domain.detail}>{kv('说明', n().domain.detail!, false)}</Show>
                  </Show>
                  <Show when={n().domain.configured === ''}>
                    {kv('IP 证书', n().ip_certificate.available ? `${n().ip_certificate.subject}（可用）` : n().ip_certificate.reason, false)}
                  </Show>
                  <Show when={n().ddns.provider !== 'off' && n().ddns.provider !== ''}>
                    {kv('DDNS', `${KERNEL_LABELS[n().ddns.provider] ?? n().ddns.provider} → ${n().ddns.host || '（未填主机名）'}`, false)}
                    <Show when={n().ddns.v4 || n().ddns.v6}>
                      {kv('已推送', [n().ddns.v4 ? `A=${n().ddns.v4}` : '', n().ddns.v6 ? `AAAA=${n().ddns.v6}` : ''].filter(Boolean).join('，'))}
                    </Show>
                    <Show when={n().ddns.updated_at && !n().ddns.updated_at.startsWith('0001')}>
                      {kv('更新时间', fmtClock(n().ddns.updated_at))}
                    </Show>
                    <Show when={n().ddns.last_error}>{kv('最近错误', n().ddns.last_error, false)}</Show>
                  </Show>
                </div>
              </div>

              <div class="card" style="padding:0">
                <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600;display:flex;align-items:center;gap:9px">
                  <span style="flex-grow:1">HTTPS 证书</span>
                  <Show when={n().tls.ca_available}>
                    <button class="hit btn btn-sm" classList={{ loading: caBusy() }} disabled={caBusy()} onClick={() => void downloadCA()}>
                      下载 CA 证书
                    </button>
                  </Show>
                </div>
                <div style="padding:4px 4px">
                  {kv('模式', TLS_MODE_LABELS[n().tls.mode] ?? n().tls.mode, false)}
                  <Show when={n().tls.mode !== 'off'}>
                    {kv('当前证书', TLS_ACTIVE_LABELS[n().tls.active] ?? n().tls.active, false)}
                    {kv('HTTPS 监听', `${n().tls.https_addr}（${n().tls.listening ? '在跑' : '未监听'}）`)}
                  </Show>
                  <Show when={n().tls.subject}>{kv('ACME 标识', n().tls.subject)}</Show>
                  <Show when={n().tls.profile}>{kv('ACME Profile', n().tls.profile)}</Show>
                  <Show when={(n().tls.sans ?? []).length > 0}>{kv('证书覆盖', (n().tls.sans ?? []).join('，'))}</Show>
                  <Show when={n().tls.not_after && !n().tls.not_after.startsWith('0001')}>
                    {kv('到期', fmtClock(n().tls.not_after))}
                  </Show>
                  <Show when={n().tls.next_retry && !n().tls.next_retry.startsWith('0001')}>
                    {kv('下次重试', fmtClock(n().tls.next_retry))}
                  </Show>
                  <Show when={n().tls.last_error}>{kv('最近错误', n().tls.last_error, false)}</Show>
                </div>
                <Show when={n().tls.active === 'selfsigned'}>
                  <div style="padding:12px 18px;border-top:1px solid var(--line-soft);font-size:11.5px;line-height:1.7;color:var(--text-2)">
                    不装 CA 也能用——每台设备首次访问点一次「继续访问」即可。装上 CA 后浏览器完全信任本机证书：
                    <br />
                    macOS：双击下载的 .crt 进钥匙串，双击该证书设「始终信任」；
                    <br />
                    Windows：双击 .crt → 安装证书 → 放入「受信任的根证书颁发机构」；
                    <br />
                    iOS：把文件发到手机 → 设置里安装描述文件 → 关于本机 → 证书信任设置里打开。
                  </div>
                </Show>
              </div>
            </div>
          </div>
        );
      }}
    </Show>
  );
}

// ---- 服务参数 ----

const CAP_LABELS: Record<string, string> = { voice: '语音', stage: '舞台', ingest: '推流' };
// 内建类型不在可注册类型列表里，展示名这里补
const TYPE_LABELS: Record<string, string> = {
  ember: 'Ember（内置语音）',
  bellows: 'Bellows（内置推流网关）',
  'livekit-embedded': 'LiveKit（进程内）',
};
// 枚举值的人话标签（值本身仍以英文存库）；选择器可选项是实例 alias，未知名直接显示原值
const KERNEL_LABELS: Record<string, string> = {
  livekit: 'LiveKit',
  ember: 'Ember（内置语音）',
  bellows: 'Bellows（内置推流网关）',
  lkembed: 'LiveKit（进程内）',
  none: '关闭',
  auto: '自动',
  off: '关闭',
  on: '开启',
  acme: '自动证书（ACME）',
  selfsigned: '自签名（本地 CA）',
  duckdns: 'DuckDNS',
  cloudflare: 'Cloudflare',
  dnspod: 'DNSPod（腾讯云）',
  aliyun: '阿里云',
};
const GROUP_META: Record<string, [string, string]> = {
  core: ['内核选择', '语音 / 舞台（投屏）/ 推流入口分别选服务实例'],
  livekit: ['LiveKit', '信令与令牌签发'],
  voice: ['Ember 内置语音', '本进程直出音频，UDP 单端口'],
  stage: ['进程内 LiveKit（舞台）', '舞台选 lkembed 时才启动，信令只监听回环'],
  ingress: ['推流入口（OBS）', 'WHIP 上游与公开地址'],
  network: ['网络', '向默认网关申请端口映射，仅 host 网络或裸机可用'],
  site: ['站点', '公开域名：邀请链接与证书签发都按它'],
  tls: ['HTTPS 证书', '保存即热生效；外部 80/443 由端口映射指到本机'],
  ddns: ['DDNS', '公网 IP 变化时自动更新域名解析；地址没变不会打提供方 API'],
  system: ['服务', '服务级杂项'],
};
const POLICIES = [
  { id: 'closed', label: '关闭注册', desc: '只能用 CLI 在服务器上开通' },
  { id: 'invite', label: '邀请制', desc: '管理员发链接，有效期内可自助注册' },
  { id: 'open', label: '开放注册', desc: '任何人都能注册——公网上不建议' },
];
// 与服务端 aliasRe（server/internal/api/providers.go）保持一致
const ALIAS_RE = /^[a-z0-9][a-z0-9-]{0,31}$/;

function ConfigTab(props: { onDirty: (groups: string[]) => void }) {
  const [ov, setOv] = createSignal<AdminOverview>();
  const [items, setItems] = createSignal<ConfigItem[]>([]);
  const [instances, setInstances] = createSignal<ProviderInstance[]>([]);
  const [types, setTypes] = createSignal<ProviderType[]>([]);
  const [policy, setPolicy] = createSignal('');
  const [defaultRole, setDefaultRole] = createSignal(''); // 注册产出默认档（user/power）
  const [policyBusy, setPolicyBusy] = createSignal(''); // 正在切换的策略/默认档标识
  const [err, setErr] = createSignal('');
  // 输入草稿：只存被改过的键，未改的键取 ConfigItem.value（= 已保存值），两者比较即脏标记
  const [draft, setDraft] = createSignal<Record<string, string>>({});

  const [pMode, setPMode] = createSignal<'create' | 'edit'>('create');
  const [pAlias, setPAlias] = createSignal('');
  const [pType, setPType] = createSignal('');
  const [pValues, setPValues] = createSignal<Record<string, string>>({});
  const [saving, setSaving] = createSignal(''); // 正在保存的配置组名，空串=空闲
  const [provBusy, setProvBusy] = createSignal(false); // 实例表单提交中
  const [delBusy, setDelBusy] = createSignal(''); // 正在删除的实例 alias
  let provCard!: HTMLDivElement;

  void (async () => {
    try {
      const [o, its, provs, pol] = await Promise.all([adminOverview(), adminGetConfig(), adminListProviders(), adminGetPolicy()]);
      setItems(its);
      setInstances(provs.instances);
      setTypes(provs.types);
      setPType(provs.types[0]?.type ?? '');
      setPolicy(pol.policy);
      setDefaultRole(pol.default_role);
      setOv(o);
    } catch (e) {
      setErr((e as Error).message);
    }
  })();

  // 策略与默认档是同一份设置，任一变都两项一起提交
  const savePolicy = async (nextPolicy: string, nextRole: string, busyKey: string) => {
    if (policyBusy()) return;
    setPolicyBusy(busyKey);
    try {
      const r = await adminSetPolicy(nextPolicy, nextRole);
      setPolicy(r.policy);
      setDefaultRole(r.default_role);
      toast('注册策略已更新', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setPolicyBusy('');
    }
  };

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

  const resetProviderForm = () => {
    setPMode('create');
    setPAlias('');
    setPType(types()[0]?.type ?? '');
    setPValues({});
  };

  const editing = () => (pMode() === 'edit' ? instances().find((i) => i.alias === pAlias()) : undefined);

  // 实例表单是否有未提交的改动：创建态看是否已填过什么，编辑态跟已保存的 params 逐字段比
  const providerDirty = createMemo(() => {
    const inst = editing();
    if (inst) {
      return fieldsOf(inst.type).some((f) => {
        const v = pValues()[f.name] ?? '';
        return f.secret ? v.trim() !== '' : v !== (inst.params[f.name] ?? '');
      });
    }
    return pAlias().trim() !== '' || Object.values(pValues()).some((v) => v.trim() !== '');
  });
  createEffect(() => props.onDirty(providerDirty() ? [...dirty(), '__provider'] : dirty()));

  const saveGroup = async (g: string) => {
    if (saving()) return;
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
    setSaving(g);
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
    } finally {
      setSaving('');
    }
  };

  // 非 Secret 字段必填（livekit 的 livekit_url 例外，服务端 checkProviderParams 同一豁免）；
  // Secret 字段创建时必填，编辑时留空 = 保持不变
  const fieldMissing = (f: ProviderField) => {
    const v = (pValues()[f.name] ?? '').trim();
    if (f.secret) return pMode() === 'create' && v === '';
    if (pType() === 'livekit' && f.name === 'livekit_url') return false;
    return v === '';
  };
  const aliasValid = () => ALIAS_RE.test(pAlias().trim());
  const canSubmit = createMemo(() => {
    if (pMode() === 'create' && !aliasValid()) return false;
    return fieldsOf(pType()).every((f) => !fieldMissing(f));
  });

  const submitProvider = async () => {
    if (!canSubmit() || provBusy()) return;
    // 全量替换语义：该类型全部字段都提交
    const params: Record<string, string> = {};
    for (const f of fieldsOf(pType())) params[f.name] = (pValues()[f.name] ?? '').trim();
    setProvBusy(true);
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
    } finally {
      setProvBusy(false);
    }
  };

  const deleteProvider = async (alias: string) => {
    if (delBusy()) return;
    const ok = await confirmDialog({
      title: `删除实例 ${alias}？`,
      body: '选择器仍引用它时服务端会拒绝；删除不可恢复。',
      danger: true,
      confirmText: '删除',
    });
    if (!ok) return;
    setDelBusy(alias);
    try {
      await adminDeleteProvider(alias);
      if (pMode() === 'edit' && pAlias() === alias) resetProviderForm();
      await reloadProviders();
      toast('实例已删除', 'ok');
    } catch (e) {
      toast((e as Error).message, 'bad');
    } finally {
      setDelBusy('');
    }
  };

  // 切换实例类型时已填参数作废：已经填了点什么就先确认一次
  const switchType = async (t: string) => {
    if (pType() === t) return;
    const hasValues = Object.values(pValues()).some((v) => v.trim() !== '');
    if (hasValues) {
      const ok = await confirmDialog({
        title: '切换实例类型？',
        body: '已填写的参数会被清空（alias 保留）。',
        danger: true,
        confirmText: '切换并清空',
      });
      if (!ok) return;
    }
    setPType(t);
    setPValues({});
  };

  const startEdit = (inst: ProviderInstance) => {
    // params 里 Secret 已被服务端掩码为空串，提交时空串 = 保留旧值
    setPMode('edit');
    setPAlias(inst.alias);
    setPType(inst.type);
    setPValues({ ...inst.params });
    provCard?.scrollIntoView({ block: 'nearest' });
  };

  const fieldInput = (f: ProviderField) => {
    const placeholder = () =>
      f.secret && pMode() === 'edit' && editing()?.params_set[f.name] ? '已设置（留空保持不变）' : f.hint;
    const bad = () => fieldMissing(f);
    return (
      <div>
        <div style="display:flex;align-items:baseline;gap:8px;margin-bottom:6px">
          <span style="font-size:11px;color:var(--text-2)">{f.label}</span>
          <span class="mono" style="font-size:10px;color:var(--text-3)">{f.name}</span>
        </div>
        <div class="field" classList={{ bad: bad() }} style="height:38px;background:var(--bg-2)">
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
    <div class="card" id="prov-card" ref={provCard} style="padding:18px 20px">
      <div style="display:flex;align-items:baseline;gap:9px;margin-bottom:4px">
        <div style="font-size:13px;font-weight:600">服务实例</div>
        <div style="font-size:11px;color:var(--text-2)">语音 / 舞台 / 推流接入的内核实例；内建与环境变量锁定的只读</div>
      </div>
      <div
        class="table-box"
        style={{ 'margin-top': '11px', '--col-1': '170px', '--col-2': '170px', '--col-4': '90px', '--col-5': '150px' }}
      >
        <div class="table-head">
          <div>alias</div>
          <div>类型</div>
          <div style="flex-grow:1">能力</div>
          <div>来源</div>
          <div style="text-align:right">操作</div>
        </div>
        <Show when={instances().length > 0} fallback={<div class="table-empty">没有实例。</div>}>
          <For each={instances()}>
            {(inst) => (
              <div class="table-row">
                <div class="mono cell-ellipsis" data-label="alias" style="font-size:12.5px;font-weight:600;color:var(--text-0)">
                  {inst.alias}
                </div>
                <div data-label="类型" style="font-size:12px;color:var(--text-1)">{typeLabel(inst.type)}</div>
                <div data-label="能力" style="flex-grow:1;font-size:12px;color:var(--text-1)">
                  {inst.caps.map((c) => CAP_LABELS[c] ?? c).join(' / ') || '—'}
                </div>
                <div data-label="来源">
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
                <div class="table-actions">
                  <Show when={!inst.builtin && !inst.locked}>
                    <button class="hit btn btn-sm" disabled={delBusy() !== ''} onClick={() => startEdit(inst)}>
                      编辑
                    </button>
                    <button
                      class="hit btn btn-sm btn-danger"
                      classList={{ loading: delBusy() === inst.alias }}
                      disabled={delBusy() !== ''}
                      onClick={() => void deleteProvider(inst.alias)}
                    >
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
          <form
            style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px"
            onSubmit={(ev) => {
              ev.preventDefault();
              void submitProvider();
            }}
          >
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
                      <button type="button" class="hit seg" classList={{ on: pType() === t.type }} onClick={() => void switchType(t.type)}>
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
                <div class="field" classList={{ bad: pAlias().trim() !== '' && !aliasValid() }} style="height:38px;background:var(--bg-2)">
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
              <button type="submit" class="hit btn btn-sm btn-primary" classList={{ loading: provBusy() }} disabled={!canSubmit() || provBusy()}>
                注册实例
              </button>
            </div>
          </form>
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
            <button
              class="hit btn btn-sm btn-primary"
              classList={{ loading: provBusy() }}
              disabled={!canSubmit() || provBusy()}
              onClick={() => void submitProvider()}
            >
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
              classList={{ loading: saving() === gp.group }}
              disabled={!dirtyOf(gp.group) || saving() !== ''}
              onClick={() => void saveGroup(gp.group)}
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
          <div style="display:flex;flex-direction:column;gap:8px" role="radiogroup">
            <For each={POLICIES}>
              {(p) => {
                const on = () => policy() === p.id;
                return (
                  <button
                    class="hit"
                    role="radio"
                    aria-checked={on()}
                    disabled={policyBusy() !== ''}
                    style={`display:flex;align-items:flex-start;gap:11px;padding:11px 12px;border-radius:9px;border:1px solid ${on() ? 'var(--ember-line)' : 'var(--line)'};background:${on() ? 'var(--ember-tint)' : 'var(--bg-2)'};text-align:left;width:100%`}
                    onClick={() => {
                      if (!on()) void savePolicy(p.id, defaultRole(), p.id);
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
          <div style="display:flex;align-items:center;gap:10px;margin-top:13px">
            <span style="font-size:11.5px;color:var(--text-2);flex-shrink:0">注册默认档</span>
            <div class="seg-group" style="background:var(--bg-2);flex-grow:1">
              <For
                each={[
                  ['user', '普通用户'],
                  ['power', '高级用户'],
                ]}
              >
                {([v, label]) => (
                  <button
                    class="hit seg"
                    classList={{ on: defaultRole() === v }}
                    disabled={policyBusy() !== ''}
                    title={v === 'power' ? '新账号落地即可创建频道、发邀请' : '新账号不能创建频道，需要时由管理员提升'}
                    onClick={() => {
                      if (defaultRole() !== v) void savePolicy(policy(), v, `role-${v}`);
                    }}
                  >
                    {label}
                  </button>
                )}
              </For>
            </div>
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

function UsersTab() {
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
