// 管理后台（服务器级，仅管理员）：服务状态 / 服务参数 / 用户 / 房间 / 邀请。
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
import { avatarHtml, copyText, esc, icon, timeAgo, toast } from '../ui';
import { menuButtonHtml, wireMenuButton } from '../shell';

type Tab = 'status' | 'config' | 'users' | 'rooms' | 'invites';

const NAV: { id: Tab; label: string; icon: string; sub: string }[] = [
  { id: 'status', label: '服务状态', icon: 'pulse', sub: '常驻进程与宿主资源' },
  { id: 'config', label: '服务参数', icon: 'gear', sub: '组件地址与注册策略' },
  { id: 'users', label: '用户', icon: 'users', sub: '账号、设备与启停' },
  { id: 'rooms', label: '房间', icon: 'volume', sub: '频道、房主与可见性' },
  { id: 'invites', label: '邀请', icon: 'mail', sub: '生成有时效的注册链接' },
];

export async function renderAdmin(root: HTMLElement, tab: Tab) {
  const me = getUser();
  if (!me?.is_admin) {
    location.hash = '#/lobby';
    return;
  }
  if (!NAV.some((n) => n.id === tab)) tab = 'status';
  const meta = NAV.find((n) => n.id === tab)!;

  root.innerHTML = `
    <div class="app-frame">
      <div class="nav-scrim"></div>
      <aside class="sidebar" style="width:210px;background-image:none">
        <div class="sidebar-head">
          ${icon('shield', 17, 'var(--ember)')}
          <div style="display:flex;flex-direction:column;gap:1px">
            <div style="font-size:13.5px;font-weight:700;letter-spacing:0.04em">管理后台</div>
            <div class="mono" style="font-size:9.5px;color:var(--text-2)">${esc(location.host)}</div>
          </div>
        </div>
        <div class="sidebar-body" style="gap:2px" id="admin-nav">
          ${NAV.map(
            (n) => `
            <a class="hit nav-row ${n.id === tab ? 'on' : ''}" href="#/admin/${n.id}">
              ${icon(n.icon, 16, n.id === tab ? 'var(--ember)' : 'var(--text-2)', 1.6)}
              <span style="flex-grow:1">${n.label}</span>
              <span class="badge-n mono" data-badge="${n.id}"></span>
            </a>`,
          ).join('')}
        </div>
        <div style="padding:10px;border-top:1px solid var(--line-soft)">
          <a class="hit back-row" href="#/lobby">${icon('back', 16, 'var(--text-2)', 1.6)}<span style="flex-grow:1">返回 Hearth</span></a>
        </div>
      </aside>
      <div class="content">
        <header class="topbar" style="height:62px;padding:0 24px">
          ${menuButtonHtml()}
          <h1 style="font-size:16px">${meta.label}</h1>
          <span class="sub" style="color:var(--text-2)">${meta.sub}</span>
          <div class="spacer"></div>
          <div style="display:flex;align-items:center;gap:8px;padding:5px 10px 5px 5px;border-radius:8px;background:var(--bg-3);border:1px solid var(--line)">
            ${avatarHtml(me.username, 'avatar avatar-sm')}
            <span style="font-size:12px;color:var(--text-1)">${esc(me.username)} · 管理员</span>
          </div>
        </header>
        <div style="flex-grow:1;padding:22px 24px;overflow-y:auto;display:flex;flex-direction:column;gap:18px" id="admin-body"><div class="muted">加载中…</div></div>
      </div>
    </div>
  `;

  const body = root.querySelector<HTMLDivElement>('#admin-body')!;
  wireMenuButton(root);
  root.querySelector('.nav-scrim')!.addEventListener('click', () => root.querySelector('.app-frame')?.classList.remove('nav-open'));
  switch (tab) {
    case 'status':
      await paintStatus(body);
      break;
    case 'config':
      await paintConfig(body);
      break;
    case 'users':
      await paintUsers(body);
      break;
    case 'rooms':
      await paintRooms(body);
      break;
    case 'invites':
      await paintInvites(body);
      break;
  }
}

function fmtUptime(s: number): string {
  if (s < 3600) return `${Math.floor(s / 60)} 分钟`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时`;
  return `${Math.floor(s / 86400)} 天`;
}

// ---- 服务状态 ----

async function paintStatus(body: HTMLElement) {
  let ov: AdminOverview;
  try {
    ov = await adminOverview();
  } catch (err) {
    body.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
    return;
  }
  const r = ov.resources;
  const loadPct = r.load !== null ? Math.min(100, Math.round((r.load / Math.max(1, r.cpus)) * 100)) : null;
  const memPct =
    r.mem_used_mb !== null && r.mem_total_mb ? Math.round((r.mem_used_mb / r.mem_total_mb) * 100) : null;
  const resources: { label: string; value: string; pct: number | null }[] = [
    { label: `负载（${r.cpus} 核）`, value: r.load !== null ? r.load.toFixed(2) : '不可用', pct: loadPct },
    {
      label: '内存',
      value: r.mem_used_mb !== null ? `${((r.mem_used_mb ?? 0) / 1024).toFixed(1)} / ${((r.mem_total_mb ?? 0) / 1024).toFixed(1)} GB` : '不可用',
      pct: memPct,
    },
    { label: '温度', value: r.temp_c !== null ? `${r.temp_c.toFixed(0)} °C` : '不可用', pct: r.temp_c },
  ];
  const services = [
    { name: 'hearth-server', meta: `Go 单体 · ${esc(ov.go_version)} · 已运行 ${fmtUptime(ov.uptime_seconds)}`, ok: true, state: 'running' },
    {
      name: `语音内核 · ${esc(ov.services.voice?.name ?? '?')}`,
      meta: esc(ov.services.voice?.url ?? '') || '进程内嵌（/providers/<alias>/voice）',
      ok: ov.services.voice?.ok ?? false,
      state: ov.services.voice?.ok ? 'running' : 'unreachable',
    },
    {
      name: `舞台内核 · ${esc(ov.services.stage?.name ?? '?')}`,
      meta: ov.services.stage?.name === 'none' ? '未启用（投屏/摄像头不可用）' : esc(ov.services.stage?.url ?? ''),
      ok: ov.services.stage?.ok ?? false,
      state: ov.services.stage?.ok ? 'running' : ov.services.stage?.name === 'none' ? 'off' : 'unreachable',
    },
    {
      name: `推流入口 · ${esc(ov.services.ingest?.name ?? '?')}`,
      meta: ov.services.ingest?.ok ? esc(ov.services.ingest.url) : '未启用（在服务参数里补齐所选内核的配置）',
      ok: ov.services.ingest?.ok ?? false,
      state: ov.services.ingest?.ok ? 'running' : 'off',
    },
    { name: '数据库', meta: esc(ov.services.db?.url ?? ''), ok: true, state: 'ok' },
  ];
  body.innerHTML = `
    <div class="stat-cards">
      <div class="stat-card"><div class="s-label">注册用户</div><div class="s-value"><span class="n">${ov.users}</span><span class="u mono">个</span></div></div>
      <div class="stat-card"><div class="s-label">在房人数</div><div class="s-value"><span class="n">${ov.online}</span><span class="u mono">人</span></div></div>
      <div class="stat-card"><div class="s-label">频道</div><div class="s-value"><span class="n">${ov.channels}</span><span class="u mono">个</span></div></div>
      <div class="stat-card"><div class="s-label">运行时长</div><div class="s-value"><span class="n" style="font-size:20px">${fmtUptime(ov.uptime_seconds)}</span></div></div>
    </div>
    <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:16px">
      <div class="list-box">
        <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600">常驻组件</div>
        ${services
          .map(
            (sv) => `
          <div class="svc-row">
            <div class="dot ${sv.ok ? '' : 'down'}"></div>
            <div style="flex-grow:1;min-width:0">
              <div class="s-name">${sv.name}</div>
              <div class="s-meta mono">${sv.meta}</div>
            </div>
            <span class="mono" style="font-size:11px;color:${sv.ok ? 'var(--sage)' : 'var(--red)'}">${sv.state}</span>
          </div>`,
          )
          .join('')}
      </div>
      <div class="list-box">
        <div style="padding:13px 18px;border-bottom:1px solid var(--line-soft);font-size:13px;font-weight:600">宿主资源</div>
        <div style="padding:16px 18px;display:flex;flex-direction:column;gap:15px">
          ${resources
            .map(
              (res) => `
            <div>
              <div style="display:flex;align-items:baseline;gap:8px">
                <span style="font-size:12px;color:var(--text-1);flex-grow:1">${res.label}</span>
                <span class="mono" style="font-size:11.5px">${res.value}</span>
              </div>
              ${res.pct !== null ? `<div class="res-bar"><div class="${res.pct > 80 ? 'hot' : ''}" style="width:${Math.min(100, res.pct)}%"></div></div>` : ''}
            </div>`,
            )
            .join('')}
          <div style="font-size:10.5px;color:var(--text-3)">资源读数来自 /proc 与 /sys，仅 Linux 宿主可用。</div>
        </div>
      </div>
    </div>`;
}

// ---- 服务参数 ----

async function paintConfig(body: HTMLElement) {
  let ov: AdminOverview;
  let items: ConfigItem[] = [];
  let provInstances: ProviderInstance[] = [];
  let provTypes: ProviderType[] = [];
  try {
    let provs: { instances: ProviderInstance[]; types: ProviderType[] };
    [ov, items, provs] = await Promise.all([adminOverview(), adminGetConfig(), adminListProviders()]);
    provInstances = provs.instances;
    provTypes = provs.types;
  } catch (err) {
    body.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
    return;
  }
  let policy = ov.policy;

  // 实例表单状态：repaint 会重建 DOM，输入值先收进这里再重绘
  let pForm: { mode: 'create' | 'edit'; alias: string; type: string; values: Record<string, string> } = {
    mode: 'create',
    alias: '',
    type: provTypes[0]?.type ?? '',
    values: {},
  };

  const CAP_LABELS: Record<string, string> = { voice: '语音', stage: '舞台', ingest: '推流' };
  // 内建类型不在可注册类型列表里，展示名这里补
  const TYPE_LABELS: Record<string, string> = { ember: 'Ember（内置语音）', bellows: 'Bellows（内置推流网关）' };
  const typeLabel = (t: string) => provTypes.find((x) => x.type === t)?.label ?? TYPE_LABELS[t] ?? t;
  const fieldsOf = (t: string) => provTypes.find((x) => x.type === t)?.fields ?? [];

  // 实例增删改后选择器可选项跟着变，配置项一并重拉
  const reloadProviders = async () => {
    const provs = await adminListProviders();
    provInstances = provs.instances;
    provTypes = provs.types;
    items = await adminGetConfig();
  };

  const collectProviderForm = () => {
    const card = body.querySelector('#prov-card');
    card?.querySelectorAll<HTMLInputElement>('input[data-pf]').forEach((inp) => {
      pForm.values[inp.dataset.pf!] = inp.value;
    });
    const aliasInp = card?.querySelector<HTMLInputElement>('#prov-alias');
    if (aliasInp) pForm.alias = aliasInp.value;
  };

  const resetProviderForm = () => {
    pForm = { mode: 'create', alias: '', type: provTypes[0]?.type ?? '', values: {} };
  };

  const providersCard = () => {
    const editing = pForm.mode === 'edit' ? provInstances.find((i) => i.alias === pForm.alias) : undefined;
    const fields = fieldsOf(pForm.mode === 'edit' ? (editing?.type ?? '') : pForm.type);
    const fieldInput = (f: ProviderField) => {
      const placeholder =
        f.secret && pForm.mode === 'edit' && editing?.params_set[f.name] ? '已设置（留空保持不变）' : f.hint;
      return `
        <div>
          <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">${esc(f.label)}</div>
          <div class="field" style="height:38px;background:var(--bg-2)"><input class="mono" style="font-size:12px" data-pf="${esc(f.name)}" type="${f.secret ? 'password' : 'text'}" value="${esc(pForm.values[f.name] ?? '')}" placeholder="${esc(placeholder)}" autocomplete="off" /></div>
        </div>`;
    };
    const rows = provInstances
      .map((inst) => {
        const readonly = inst.builtin || inst.locked;
        const src = inst.builtin
          ? '<span class="chip tag-ember">内建</span>'
          : inst.locked
            ? '<span class="chip" style="background:var(--bg-4);color:var(--text-1)" title="部署侧 .env / compose 里改，重启生效">环境锁定</span>'
            : '<span class="chip tag-sage">DB</span>';
        return `
        <div class="table-row">
          <div class="mono" style="width:170px;font-size:12.5px;font-weight:600;color:var(--text-0)">${esc(inst.alias)}</div>
          <div style="width:170px;font-size:12px;color:var(--text-1)">${esc(typeLabel(inst.type))}</div>
          <div style="flex-grow:1;font-size:12px;color:var(--text-1)">${inst.caps.map((c) => CAP_LABELS[c] ?? esc(c)).join(' / ') || '—'}</div>
          <div style="width:90px">${src}</div>
          <div style="width:150px;display:flex;gap:7px;justify-content:flex-end">
            ${
              readonly
                ? ''
                : `<button class="hit btn btn-sm" data-p-edit="${esc(inst.alias)}">编辑</button>
                   <button class="hit btn btn-sm btn-danger" data-p-del="${esc(inst.alias)}">删除</button>`
            }
          </div>
        </div>`;
      })
      .join('');
    const formHtml =
      pForm.mode === 'edit' && editing
        ? `
        <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
          <div style="display:flex;align-items:baseline;gap:9px">
            <div style="font-size:12.5px;font-weight:600">编辑实例 <span class="mono">${esc(editing.alias)}</span>（${esc(typeLabel(editing.type))}）</div>
            <div style="font-size:11px;color:var(--text-3)">保存为全量替换；密钥留空 = 保持不变</div>
            <div class="spacer"></div>
            <button class="hit btn btn-sm" data-p-cancel>取消</button>
          </div>
          <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:11px;margin-top:12px">
            ${fields.map(fieldInput).join('')}
          </div>
          <div style="display:flex;justify-content:flex-end;margin-top:13px">
            <button class="hit btn btn-sm btn-primary" data-p-submit>保存并生效</button>
          </div>
        </div>`
        : `
        <div style="margin-top:16px;border-top:1px solid var(--line-soft);padding-top:15px">
          <div style="display:flex;align-items:baseline;gap:9px">
            <div style="font-size:12.5px;font-weight:600">注册新实例</div>
            <div style="font-size:11px;color:var(--text-3)">注册后在「内核选择」里按 alias 选用</div>
          </div>
          <div style="display:flex;gap:20px;margin-top:12px;align-items:flex-end;flex-wrap:wrap">
            <div>
              <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">类型</div>
              <div class="seg-group" style="background:var(--bg-2)">${provTypes
                .map((t) => `<button class="hit seg ${pForm.type === t.type ? 'on' : ''}" data-p-type="${esc(t.type)}">${esc(t.label)}</button>`)
                .join('')}</div>
            </div>
            <div style="flex-grow:1;min-width:220px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">alias（小写字母数字与 -，会出现在 /providers/&lt;alias&gt; 连接路径里）</div>
              <div class="field" style="height:38px;background:var(--bg-2)"><input class="mono" style="font-size:12px" id="prov-alias" value="${esc(pForm.alias)}" placeholder="如 lk-main" /></div>
            </div>
          </div>
          <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:11px;margin-top:12px">
            ${fields.map(fieldInput).join('')}
          </div>
          <div style="display:flex;justify-content:flex-end;margin-top:13px">
            <button class="hit btn btn-sm btn-primary" data-p-submit>注册实例</button>
          </div>
        </div>`;
    return `
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
          ${rows || '<div class="table-empty">没有实例。</div>'}
        </div>
        ${formHtml}
      </div>`;
  };

  // 依赖服务配置卡片：环境变量固定的只读展示；未固定的可编辑，保存落库即时生效
  // 枚举值的人话标签（值本身仍以英文存库）；选择器可选项是实例 alias，未知名直接显示原值
  const KERNEL_LABELS: Record<string, string> = {
    livekit: 'LiveKit',
    ember: 'Ember（内置语音）',
    bellows: 'Bellows（内置推流网关）',
    none: '关闭',
  };

  const groupCard = (group: string, title: string, sub: string) => {
    const list = items.filter((it) => it.group === group);
    const anyEditable = list.some((it) => !it.locked);
    return `
      <div class="card" data-group="${group}">
        <div style="display:flex;align-items:baseline;gap:9px;margin-bottom:4px">
          <div style="font-size:13px;font-weight:600">${title}</div>
          <div style="font-size:11px;color:var(--text-2)">${sub}</div>
        </div>
        <div style="display:flex;flex-direction:column;gap:11px;margin-top:11px">
          ${list
            .map((it) => {
              const placeholder = it.secret && it.set ? '已设置（输入新值覆盖）' : it.hint;
              return `
            <div>
              <div style="display:flex;align-items:baseline;gap:8px;margin-bottom:6px">
                <span style="font-size:11px;color:var(--text-2)">${esc(it.label)}</span>
                <span class="mono" style="font-size:10px;color:var(--text-3)">${esc(it.env)}</span>
                ${it.locked ? '<span class="tag" title="部署侧 .env / compose 里改，重启生效">环境变量固定</span>' : ''}
              </div>
              ${
                it.locked
                  ? `<div class="mono" style="padding:9px 12px;border-radius:8px;background:var(--bg-2);border:1px solid var(--line);font-size:12px;color:var(--text-1);overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(it.secret ? '••••••••' : it.value || '（空）')}</div>`
                  : it.options?.length
                    ? `<div class="seg-group" style="background:var(--bg-2)" data-cfg-enum="${it.name}">${it.options
                        .map(
                          (opt) =>
                            `<button class="hit seg ${it.value === opt ? 'on' : ''}" data-val="${esc(opt)}">${esc(KERNEL_LABELS[opt] ?? opt)}</button>`,
                        )
                        .join('')}</div>`
                    : `<div class="field" style="height:38px;background:var(--bg-2)"><input class="mono" style="font-size:12px" data-cfg="${it.name}" value="${esc(it.value)}" placeholder="${esc(placeholder)}" ${it.secret ? 'autocomplete="off"' : ''} /></div>`
              }
            </div>`;
            })
            .join('')}
        </div>
        ${
          anyEditable
            ? `<div style="display:flex;align-items:center;gap:10px;margin-top:13px">
                 <span style="font-size:11px;color:var(--text-3);flex-grow:1">保存后立即生效，无需重启</span>
                 <button class="hit btn btn-sm btn-primary" data-save="${group}">保存并生效</button>
               </div>`
            : ''
        }
      </div>`;
  };

  const paint = () => {
    const policies = [
      { id: 'closed', label: '关闭注册', desc: '只能用 CLI 在服务器上开通' },
      { id: 'invite', label: '邀请制', desc: '管理员发链接，有效期内可自助注册' },
      { id: 'open', label: '开放注册', desc: '任何人都能注册——公网上不建议' },
    ];
    body.innerHTML = `
      ${providersCard()}
      <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(340px,1fr));gap:16px">
        ${[...new Set(items.map((it) => it.group))]
          .map((g) => {
            const meta: Record<string, [string, string]> = {
              core: ['内核选择', '语音 / 舞台（投屏）/ 推流入口分别选服务实例'],
              livekit: ['LiveKit', '信令与令牌签发'],
              voice: ['Ember 内置语音', '本进程直出音频，UDP 单端口'],
              ingress: ['推流入口（OBS）', 'WHIP 上游与公开地址'],
            };
            const [title, sub] = meta[g] ?? [g, ''];
            return groupCard(g, title, sub);
          })
          .join('')}
        <div class="card">
          <div style="font-size:13px;font-weight:600;margin-bottom:5px">注册策略</div>
          <div style="font-size:11.5px;color:var(--text-2);margin-bottom:13px">决定新账号怎么来，改动立即生效</div>
          <div style="display:flex;flex-direction:column;gap:8px">
            ${policies
              .map((p) => {
                const on = policy === p.id;
                return `
              <button class="hit" data-policy="${p.id}" style="display:flex;align-items:flex-start;gap:11px;padding:11px 12px;border-radius:9px;border:1px solid ${on ? 'var(--ember-line)' : 'var(--line)'};background:${on ? 'var(--ember-tint)' : 'var(--bg-2)'};text-align:left;width:100%">
                <div class="radio ${on ? 'on' : ''}" style="margin-top:1px"><div class="dot"></div></div>
                <div style="flex-grow:1">
                  <div style="font-size:13px;font-weight:${on ? 600 : 500};color:${on ? 'var(--ember)' : 'var(--text-0)'}">${p.label}</div>
                  <div style="font-size:11px;color:var(--text-2);margin-top:2px">${p.desc}</div>
                </div>
              </button>`;
              })
              .join('')}
          </div>
          <div style="margin-top:13px;font-size:11px;line-height:1.6;color:var(--text-3)">数据库：<span class="mono">${esc(ov.services.db?.url ?? '')}</span><br>媒体端口与域名在部署侧（.env / compose）改，改完重启容器生效。</div>
        </div>
      </div>`;

    body.querySelectorAll<HTMLElement>('[data-cfg-enum]').forEach((seg) => {
      seg.querySelectorAll<HTMLButtonElement>('.seg').forEach((btn) => {
        btn.addEventListener('click', () => {
          seg.querySelectorAll('.seg').forEach((b) => b.classList.toggle('on', b === btn));
        });
      });
    });
    // ---- 服务实例 ----
    body.querySelectorAll<HTMLButtonElement>('[data-p-type]').forEach((btn) =>
      btn.addEventListener('click', () => {
        collectProviderForm();
        pForm.type = btn.dataset.pType!;
        pForm.values = {}; // 类型不同字段不同，已填值作废
        paint();
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-p-edit]').forEach((btn) =>
      btn.addEventListener('click', () => {
        const inst = provInstances.find((i) => i.alias === btn.dataset.pEdit);
        if (!inst) return;
        // params 里 Secret 已被服务端掩码为空串，提交时空串 = 保留旧值
        pForm = { mode: 'edit', alias: inst.alias, type: inst.type, values: { ...inst.params } };
        paint();
      }),
    );
    body.querySelector('[data-p-cancel]')?.addEventListener('click', () => {
      resetProviderForm();
      paint();
    });
    body.querySelector('[data-p-submit]')?.addEventListener('click', async () => {
      collectProviderForm();
      const fields = fieldsOf(pForm.type);
      // 全量替换语义：该类型全部字段都提交
      const params: Record<string, string> = {};
      for (const f of fields) params[f.name] = (pForm.values[f.name] ?? '').trim();
      try {
        if (pForm.mode === 'edit') {
          await adminUpdateProvider(pForm.alias, params);
          toast('实例已保存并生效', 'ok');
        } else {
          await adminCreateProvider({ type: pForm.type, alias: pForm.alias.trim(), params });
          toast('实例已注册', 'ok');
        }
        resetProviderForm();
        await reloadProviders();
        paint();
      } catch (err) {
        toast((err as Error).message, 'bad');
      }
    });
    body.querySelectorAll<HTMLButtonElement>('[data-p-del]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        const alias = btn.dataset.pDel!;
        if (!confirm(`删除实例 ${alias}？选择器仍引用它时服务端会拒绝；删除不可恢复。`)) return;
        try {
          await adminDeleteProvider(alias);
          if (pForm.mode === 'edit' && pForm.alias === alias) resetProviderForm();
          await reloadProviders();
          paint();
          toast('实例已删除', 'ok');
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-save]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const group = btn.dataset.save!;
        const values: Record<string, string> = {};
        body
          .querySelectorAll<HTMLInputElement>(`[data-group="${group}"] input[data-cfg]`)
          .forEach((input) => {
            const item = items.find((it) => it.name === input.dataset.cfg);
            // 密钥留空表示保持不变，不提交
            if (item?.secret && item.set && input.value.trim() === '') return;
            values[input.dataset.cfg!] = input.value.trim();
          });
        body.querySelectorAll<HTMLElement>(`[data-group="${group}"] [data-cfg-enum]`).forEach((seg) => {
          const on = seg.querySelector<HTMLButtonElement>('.seg.on');
          if (on) values[seg.dataset.cfgEnum!] = on.dataset.val!;
        });
        try {
          await adminSetConfig(values);
          toast('已保存并生效', 'ok');
          items = await adminGetConfig();
          paint();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      });
    });
    body.querySelectorAll<HTMLButtonElement>('[data-policy]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        try {
          const r = await adminSetPolicy(btn.dataset.policy!);
          policy = r.policy;
          paint();
          toast('注册策略已更新', 'ok');
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      });
    });
  };
  paint();
}

// ---- 用户 ----

async function paintUsers(body: HTMLElement) {
  let users: AdminUser[] = [];
  let query = '';
  const meId = getUser()?.id;

  async function load() {
    users = await adminListUsers();
    paint();
  }

  function paint() {
    const q = query.trim().toLowerCase();
    const shown = q ? users.filter((u) => u.username.toLowerCase().includes(q)) : users;
    body.innerHTML = `
      <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        <div class="field" style="width:280px;height:38px">${icon('search', 15, 'var(--text-2)')}<input id="u-query" placeholder="搜用户名" value="${esc(query)}" /></div>
        <div class="spacer"></div>
        <a class="hit btn btn-primary" href="#/admin/invites">${icon('plus', 15, 'var(--on-ember)', 1.9)} 邀请新用户</a>
      </div>
      <div class="table-box">
        <div class="table-head">
          <div style="width:200px">用户</div>
          <div style="width:90px">角色</div>
          <div style="width:80px">设备</div>
          <div style="flex-grow:1">最后活跃</div>
          <div style="width:170px;text-align:right">操作</div>
        </div>
        ${
          shown.length === 0
            ? '<div class="table-empty">没有匹配的用户。</div>'
            : shown
                .map((u) => {
                  const self = u.id === meId;
                  return `
        <div class="table-row ${u.disabled ? 'dim' : ''}">
          <div style="width:200px;display:flex;align-items:center;gap:10px;min-width:0">
            ${avatarHtml(u.username, 'avatar')}
            <div style="min-width:0">
              <div style="font-size:13px;font-weight:500">${esc(u.username)}</div>
              <div class="mono" style="font-size:10px;color:var(--text-2);margin-top:2px">usr_${u.id}</div>
            </div>
          </div>
          <div style="width:90px"><span class="chip ${u.is_admin ? 'tag-ember' : ''}" style="${u.is_admin ? '' : 'background:var(--bg-4);color:var(--text-1)'}">${u.is_admin ? '管理员' : '成员'}</span></div>
          <div class="mono" style="width:80px;font-size:12px;color:var(--text-1)">${u.devices}</div>
          <div style="flex-grow:1;font-size:12px;color:var(--text-2)">${timeAgo(u.last_seen)}</div>
          <div style="width:170px;display:flex;gap:7px;justify-content:flex-end">
            <button class="hit btn btn-sm ${self ? 'disabled' : ''}" data-toggle="${u.id}" data-disabled="${u.disabled}">${u.disabled ? '启用' : '禁用'}</button>
            <button class="hit btn btn-sm ${self ? 'disabled' : 'btn-danger'}" data-del="${u.id}" data-name="${esc(u.username)}" style="width:28px;padding:0">${icon('trash', 13, self ? 'var(--text-3)' : 'var(--red)')}</button>
          </div>
        </div>`;
                })
                .join('')
        }
      </div>`;

    const input = body.querySelector<HTMLInputElement>('#u-query')!;
    input.addEventListener('input', () => {
      query = input.value;
      const pos = input.selectionStart;
      paint();
      const again = body.querySelector<HTMLInputElement>('#u-query')!;
      again.focus();
      again.setSelectionRange(pos, pos);
    });
    body.querySelectorAll<HTMLButtonElement>('[data-toggle]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        if (btn.classList.contains('disabled')) return;
        try {
          await adminSetUserDisabled(Number(btn.dataset.toggle), btn.dataset.disabled !== 'true');
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-del]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        if (btn.classList.contains('disabled')) return;
        if (!confirm(`删除用户 ${btn.dataset.name}？其会话、设备档案和白名单记录一并清除，不可恢复。`)) return;
        try {
          await adminDeleteUser(Number(btn.dataset.del));
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
  }
  try {
    await load();
  } catch (err) {
    body.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
  }
}

// ---- 房间 ----

async function paintRooms(body: HTMLElement) {
  let channels: Channel[] = [];
  async function load() {
    channels = await listChannels();
    paint();
  }
  function paint() {
    body.innerHTML = `
      <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap">
        <div style="font-size:13px;color:var(--text-2)">常驻频道进程一直在，人进来就连上——不需要「开房」。建频道在大厅操作。</div>
      </div>
      <div class="table-box">
        <div class="table-head">
          <div style="width:190px">频道</div>
          <div style="width:90px">在线</div>
          <div style="width:140px">房主</div>
          <div style="flex-grow:1">可见性</div>
          <div style="width:100px;text-align:right">操作</div>
        </div>
        ${
          channels.length === 0
            ? '<div class="table-empty">还没有频道。</div>'
            : channels
                .map(
                  (c) => `
        <div class="table-row">
          <div style="width:190px;display:flex;align-items:center;gap:9px;min-width:0">
            ${icon('volume', 15, c.online ? 'var(--ember)' : 'var(--text-2)')}
            <span style="font-size:13px;font-weight:500">${esc(c.name)}</span>
          </div>
          <div class="mono" style="width:90px;font-size:12px;color:var(--text-1)">${c.online ? `${c.online} 人` : '—'}</div>
          <div style="width:140px;font-size:12px;color:var(--text-1)">${esc(c.created_by)}</div>
          <div style="flex-grow:1"><span class="chip ${c.invite_only ? 'tag-ember' : ''}" style="${c.invite_only ? '' : 'background:var(--bg-4);color:var(--text-1)'}">${c.invite_only ? '邀请制' : '公开'}</span></div>
          <div style="width:100px;display:flex;justify-content:flex-end">
            <button class="hit btn btn-sm btn-danger" data-del="${c.id}" data-name="${esc(c.name)}">删除</button>
          </div>
        </div>`,
                )
                .join('')
        }
      </div>`;
    body.querySelectorAll<HTMLButtonElement>('[data-del]').forEach((btn) =>
      btn.addEventListener('click', async () => {
        if (!confirm(`删除频道「${btn.dataset.name}」？聊天记录、黑白名单和推流 key 一并清除，不可恢复。`)) return;
        try {
          await adminDeleteChannel(Number(btn.dataset.del));
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
  }
  try {
    await load();
  } catch (err) {
    body.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
  }
}

// ---- 邀请 ----

async function paintInvites(body: HTMLElement) {
  let invites: Invite[] = [];
  let base = '';
  let ttl = '24h';
  let uses = '1';
  let note = '';
  let fresh = '';

  async function load() {
    const r = await adminListInvites();
    invites = r.invites;
    base = r.base;
    paint();
  }

  const stateOf = (iv: Invite): { label: string; cls: string; dead: boolean } => {
    const now = Date.now();
    const exp = new Date(iv.expires_at).getTime();
    if (iv.revoked) return { label: '已撤销', cls: 'tag-red', dead: true };
    if (exp < now) return { label: '已过期', cls: 'tag-red', dead: true };
    if (iv.max_uses > 0 && iv.used >= iv.max_uses) return { label: '已用完', cls: '', dead: true };
    const leftH = Math.ceil((exp - now) / 3600_000);
    return { label: `有效 · 剩 ${leftH > 48 ? `${Math.ceil(leftH / 24)} 天` : `${leftH} 小时`}`, cls: 'tag-sage', dead: false };
  };

  function paint() {
    const seg = (group: string, val: string, opts: [string, string][]) =>
      `<div class="seg-group" style="background:var(--bg-2)">${opts
        .map(([v, label]) => `<button class="hit seg ${val === v ? 'on' : ''}" data-${group}="${v}">${label}</button>`)
        .join('')}</div>`;
    body.innerHTML = `
      <div class="card" style="padding:18px 20px">
        <div style="font-size:13.5px;font-weight:600">生成邀请链接</div>
        <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">链接在有效期内可用，点开就能自己设账号密码</div>
        <div style="display:flex;gap:20px;margin-top:16px;align-items:flex-end;flex-wrap:wrap">
          <div>
            <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">有效期</div>
            ${seg('ttl', ttl, [['1h', '1 小时'], ['24h', '24 小时'], ['7d', '7 天']])}
          </div>
          <div>
            <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">可用次数</div>
            ${seg('uses', uses, [['1', '1 次'], ['5', '5 次'], ['0', '不限']])}
          </div>
          <div style="flex-grow:1;min-width:160px">
            <div style="font-size:11px;color:var(--text-2);margin-bottom:7px">备注（给谁）</div>
            <div class="field" style="height:38px;background:var(--bg-2)"><input id="iv-note" value="${esc(note)}" /></div>
          </div>
          <button class="hit btn btn-primary" id="iv-make">生成链接</button>
        </div>
        ${
          fresh
            ? `<div style="display:flex;align-items:center;gap:10px;height:42px;margin-top:14px;padding:0 6px 0 14px;border-radius:9px;background:var(--sage-tint);border:1px solid var(--sage-line)">
                <span class="mono" style="font-size:12.5px;flex-grow:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(fresh)}</span>
                <button class="hit btn btn-sm" id="iv-copy">${icon('copy', 13)} 复制</button>
              </div>`
            : ''
        }
      </div>
      <div class="table-box">
        <div class="table-head">
          <div style="width:150px">邀请码</div>
          <div style="width:150px">备注</div>
          <div style="width:110px">可用次数</div>
          <div style="flex-grow:1">状态</div>
          <div style="width:150px;text-align:right">操作</div>
        </div>
        ${
          invites.length === 0
            ? '<div class="table-empty">还没有发过邀请。</div>'
            : invites
                .map((iv) => {
                  const st = stateOf(iv);
                  return `
        <div class="table-row ${st.dead ? 'dim' : ''}">
          <div class="mono" style="width:150px;font-size:12px;color:var(--text-1)">${esc(iv.code)}</div>
          <div style="width:150px;font-size:12.5px;color:var(--text-1)">${esc(iv.note || '（无）')}</div>
          <div class="mono" style="width:110px;font-size:12px;color:var(--text-1)">${iv.used} / ${iv.max_uses === 0 ? '∞' : iv.max_uses}</div>
          <div style="flex-grow:1"><span class="chip ${st.cls}" style="${st.cls ? '' : 'background:var(--bg-4);color:var(--text-2)'}">${st.label}</span></div>
          <div style="width:150px;display:flex;gap:7px;justify-content:flex-end">
            ${st.dead ? '' : `<button class="hit btn btn-sm" data-copy="${esc(iv.code)}">复制链接</button>`}
            <button class="hit btn btn-sm" data-del="${iv.id}">${st.dead ? '删除' : '撤销'}</button>
          </div>
        </div>`;
                })
                .join('')
        }
      </div>`;

    body.querySelectorAll<HTMLButtonElement>('[data-ttl]').forEach((b) =>
      b.addEventListener('click', () => {
        ttl = b.dataset.ttl!;
        note = body.querySelector<HTMLInputElement>('#iv-note')!.value;
        paint();
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-uses]').forEach((b) =>
      b.addEventListener('click', () => {
        uses = b.dataset.uses!;
        note = body.querySelector<HTMLInputElement>('#iv-note')!.value;
        paint();
      }),
    );
    body.querySelector('#iv-make')!.addEventListener('click', async () => {
      note = body.querySelector<HTMLInputElement>('#iv-note')!.value;
      try {
        const r = await adminCreateInvite(note, Number(uses), ttl);
        fresh = r.url;
        note = '';
        await load();
      } catch (err) {
        toast((err as Error).message, 'bad');
      }
    });
    body.querySelector('#iv-copy')?.addEventListener('click', async () => {
      if (await copyText(fresh)) toast('已复制', 'ok', 1400);
    });
    body.querySelectorAll<HTMLButtonElement>('[data-copy]').forEach((b) =>
      b.addEventListener('click', async () => {
        if (await copyText(`${base}/#/join/${b.dataset.copy}`)) toast('已复制', 'ok', 1400);
      }),
    );
    body.querySelectorAll<HTMLButtonElement>('[data-del]').forEach((b) =>
      b.addEventListener('click', async () => {
        try {
          await adminDeleteInvite(Number(b.dataset.del));
          await load();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      }),
    );
  }
  try {
    await load();
  } catch (err) {
    body.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
  }
}
