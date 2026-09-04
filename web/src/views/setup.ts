// 首启向导 #/setup：仅 users 表为空时可用（main.ts 路由层已挡）。
// 四步：管理员账号 → 域名与 DDNS（可跳过）→ 证书方式 → 自检与邀请链接。
// 一次性轻页面，vanilla TS 渲染（与 login/join 同类，不进 Solid）。
import {
  createInvite,
  adminNetcheck,
  adminSetConfig,
  refreshSite,
  register,
  type NetcheckResult,
} from '../api';
import { wireThemeButton } from '../theme';
import { copyText, esc, flameLogo, icon, toast } from '../ui';

const USER_RE = /^[a-zA-Z0-9_-]{2,32}$/;
const DOMAIN_RE = /^(?=.{1,253}$)([a-z0-9](-?[a-z0-9])*\.)+[a-z]{2,}$/i;

interface WizardState {
  domain: string;
  siteDomain: string;
  ddnsHost: string;
  ddnsZone: string;
  ddnsProvider: 'off' | 'duckdns' | 'cloudflare' | 'dnspod' | 'aliyun';
  tlsChoice: 'domain' | 'ip' | 'selfsigned';
}

// DDNS 提供方各自的凭证字段（键名即服务端 dyncfg 键）
const DDNS_CREDS: Record<string, { key: string; label: string; hint: string }[]> = {
  duckdns: [{ key: 'ddns_duckdns_token', label: 'Token', hint: 'duckdns.org 账户页的 token' }],
  cloudflare: [{ key: 'ddns_cf_token', label: 'API Token', hint: '需 Zone.DNS 编辑权限' }],
  dnspod: [
    { key: 'ddns_dnspod_id', label: 'ID', hint: 'DNSPod 控制台「密钥管理」创建' },
    { key: 'ddns_dnspod_token', label: 'Token', hint: '' },
  ],
  aliyun: [
    { key: 'ddns_aliyun_id', label: 'AccessKey ID', hint: '建议 RAM 子账户，只授 AliyunDNSFullAccess' },
    { key: 'ddns_aliyun_secret', label: 'AccessKey Secret', hint: '' },
  ],
};
const DDNS_LABELS: Record<string, string> = {
  duckdns: 'DuckDNS',
  cloudflare: 'Cloudflare',
  dnspod: 'DNSPod',
  aliyun: '阿里云',
};

export function renderSetup(root: HTMLElement, alive: () => boolean) {
  const state: WizardState = {
    domain: '',
    siteDomain: '',
    ddnsHost: '',
    ddnsZone: '',
    ddnsProvider: 'off',
    tlsChoice: 'selfsigned',
  };
  let tlsChosen = false;

  const paint = (inner: string) => {
    root.innerHTML = `
      <div class="auth-page" style="position:relative">
        <button class="hit theme-fab" id="theme-fab"></button>
        <div class="auth-card" style="width:460px">
          <div class="auth-brand">
            ${flameLogo(34, 38)}
            <div class="word" style="font-size:21px;letter-spacing:0.2em;padding-left:0.2em">HEARTH</div>
            <div style="font-size:12px;color:var(--text-2);margin-top:2px">初始设置</div>
          </div>
          <div id="wz-body" style="width:100%">${inner}</div>
        </div>
      </div>`;
    wireThemeButton(root.querySelector<HTMLButtonElement>('#theme-fab')!);
    return root.querySelector<HTMLDivElement>('#wz-body')!;
  };

  const stepChip = (n: number, label: string) =>
    `<div style="display:flex;align-items:center;gap:8px;margin-top:22px;margin-bottom:14px">
      <span class="mono" style="font-size:10.5px;color:var(--ember);padding:3px 8px;border-radius:6px;background:var(--ember-tint);border:1px solid var(--ember-line)">第 ${n} / 4 步</span>
      <span style="font-size:14px;font-weight:600">${label}</span>
    </div>`;

  const errLine = `<p class="error-text" id="wz-error" style="margin:10px 0 0;min-height:1em"></p>`;
  const showErr = (e: unknown) => {
    const el = body().querySelector<HTMLParagraphElement>('#wz-error');
    if (el) el.textContent = (e as Error).message;
  };
  const body = () => root.querySelector<HTMLDivElement>('#wz-body')!;

  // ---- 第 1 步：管理员账号 ----
  function stepAccount() {
    const el = paint(`
      ${stepChip(1, '创建管理员账号')}
      <form id="wz-form" style="display:flex;flex-direction:column;gap:13px">
        <div style="display:flex;flex-direction:column;gap:7px">
          <label class="field-label" for="wz-user">用户名</label>
          <div class="field" style="height:44px"><input id="wz-user" placeholder="2–32 位，字母数字 - _" autocapitalize="off" autocomplete="username" /></div>
        </div>
        <div style="display:flex;flex-direction:column;gap:7px">
          <label class="field-label" for="wz-pass">密码</label>
          <div class="field" style="height:44px"><input id="wz-pass" type="password" placeholder="至少 6 位" autocomplete="new-password" /></div>
        </div>
        ${errLine}
        <button type="submit" class="hit btn btn-primary btn-lg" id="wz-next">下一步</button>
      </form>
      <div class="auth-note hint-card" style="border-color:var(--line-soft);margin-top:14px">
        <span style="flex-shrink:0;margin-top:1px">${icon('shield', 16, 'var(--text-2)', 1.6)}</span>
        <span>这是这台服务器的第一个账号，自动成为管理员。建好之后注册就恢复为邀请制。</span>
      </div>`);
    const form = el.querySelector<HTMLFormElement>('#wz-form')!;
    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      const username = el.querySelector<HTMLInputElement>('#wz-user')!.value.trim();
      const password = el.querySelector<HTMLInputElement>('#wz-pass')!.value;
      if (!USER_RE.test(username) || password.length < 6) {
        showErr(new Error('用户名需 2-32 位字母数字、-、_，密码至少 6 位'));
        return;
      }
      try {
        await register(username, password);
        await refreshSite(); // needs_setup 已翻转为 false，作废路由缓存
        if (!alive()) return;
        stepDomain();
      } catch (e) {
        showErr(e);
      }
    });
  }

  // ---- 第 2 步：域名与 DDNS（都可跳过）----
  function stepDomain() {
    const el = paint(`
      ${stepChip(2, '公开域名（可跳过）')}
      <div style="display:flex;flex-direction:column;gap:7px">
        <label class="field-label" for="wz-domain">站点域名</label>
        <div class="field" style="height:44px"><input id="wz-domain" class="mono" placeholder="如 voice.example.com" autocapitalize="off" spellcheck="false" /></div>
        <div style="font-size:11.5px;line-height:1.6;color:var(--text-2)">把域名解析到这台机器的公网地址后填在这里；选了下面的 DDNS 且这里留空，会自动用 DDNS 主机名。没有域名就跳过，走自签名证书，只能局域网 + 手动信任。</div>
      </div>
      <div style="display:flex;flex-direction:column;gap:7px;margin-top:15px">
        <label class="field-label">DDNS 自动更新（公网 IP 变了自动改解析）</label>
        <div class="seg-group" style="background:var(--bg-2)" id="wz-ddns">
          ${['off', 'duckdns', 'cloudflare', 'dnspod', 'aliyun']
            .map((p) => `<button type="button" class="hit seg" data-id="${p}">${p === 'off' ? '不用' : DDNS_LABELS[p]}</button>`)
            .join('')}
        </div>
        <div style="font-size:11.5px;line-height:1.6;color:var(--text-2)">DuckDNS 最适合没有域名的人：注册即得免费子域名，只要一个 token。其余三家按自己域名所在的 DNS 服务商选。</div>
      </div>
      <div id="wz-ddns-fields" style="display:none;flex-direction:column;gap:11px;margin-top:13px">
        <div style="display:flex;flex-direction:column;gap:7px">
          <label class="field-label" for="wz-ddns-host">DDNS 主机名</label>
          <div class="field" style="height:44px"><input id="wz-ddns-host" class="mono" placeholder="如 voice.duckdns.org" autocapitalize="off" spellcheck="false" /></div>
        </div>
        <div id="wz-ddns-zone-box" style="display:none;flex-direction:column;gap:7px">
          <label class="field-label" for="wz-ddns-zone">DNS Zone（建议填写）</label>
          <div class="field" style="height:44px"><input id="wz-ddns-zone" class="mono" placeholder="如 example.com" autocapitalize="off" spellcheck="false" /></div>
          <div style="font-size:11.5px;line-height:1.6;color:var(--text-2)">由 DNS 服务商管理的区域；留空时才按主机名逐级猜测。</div>
        </div>
        <div id="wz-ddns-creds" style="display:flex;flex-direction:column;gap:11px"></div>
      </div>
      ${errLine}
      <div style="display:flex;gap:10px;margin-top:14px">
        <button type="button" class="hit btn" id="wz-skip" style="flex:1">跳过</button>
        <button type="button" class="hit btn btn-primary" id="wz-next" style="flex:1">下一步</button>
      </div>`);

    const fieldsBox = el.querySelector<HTMLDivElement>('#wz-ddns-fields')!;
    const zoneBox = el.querySelector<HTMLDivElement>('#wz-ddns-zone-box')!;
    const credsBox = el.querySelector<HTMLDivElement>('#wz-ddns-creds')!;
    let provider: WizardState['ddnsProvider'] = state.ddnsProvider;
    const syncDdns = () => {
      el.querySelectorAll<HTMLButtonElement>('#wz-ddns .seg').forEach((b) => b.classList.toggle('on', b.dataset.id === provider));
      fieldsBox.style.display = provider === 'off' ? 'none' : 'flex';
      zoneBox.style.display = provider === 'off' || provider === 'duckdns' ? 'none' : 'flex';
      credsBox.innerHTML = (DDNS_CREDS[provider] ?? [])
        .map(
          (c) => `
        <div style="display:flex;flex-direction:column;gap:7px">
          <label class="field-label" for="wz-cred-${c.key}">${DDNS_LABELS[provider]} ${c.label}</label>
          <div class="field" style="height:44px"><input id="wz-cred-${c.key}" data-key="${c.key}" type="password" placeholder="${esc(c.hint)}" autocomplete="off" /></div>
        </div>`,
        )
        .join('');
    };
    el.querySelectorAll<HTMLButtonElement>('#wz-ddns .seg').forEach((b) => {
      b.addEventListener('click', () => {
        provider = b.dataset.id as WizardState['ddnsProvider'];
        syncDdns();
      });
    });
    syncDdns();

    const input = el.querySelector<HTMLInputElement>('#wz-domain')!;
    input.value = state.siteDomain;
    el.querySelector<HTMLInputElement>('#wz-ddns-host')!.value = state.ddnsHost;
    el.querySelector<HTMLInputElement>('#wz-ddns-zone')!.value = state.ddnsZone;
    const save = async () => {
      const domain = input.value.trim().toLowerCase();
      if (domain && !DOMAIN_RE.test(domain)) {
        showErr(new Error('域名格式不对，如 voice.example.com'));
        return false;
      }
      const values: Record<string, string> = {};
      state.siteDomain = domain;
      state.domain = domain;
      if (domain) values.site_domain = domain;
      state.ddnsProvider = provider;
      if (provider !== 'off') {
        const host = el.querySelector<HTMLInputElement>('#wz-ddns-host')!.value.trim().toLowerCase();
        if (!DOMAIN_RE.test(host)) {
          showErr(new Error('DDNS 主机名格式不对，如 voice.duckdns.org'));
          return false;
        }
        values.ddns_provider = provider;
        values.ddns_host = host;
        state.ddnsHost = host;
        const zone = provider === 'duckdns' ? '' : el.querySelector<HTMLInputElement>('#wz-ddns-zone')!.value.trim().toLowerCase();
        if (zone && !DOMAIN_RE.test(zone)) {
          showErr(new Error('DNS Zone 格式不对，如 example.com'));
          return false;
        }
        if (zone && host !== zone && !host.endsWith(`.${zone}`)) {
          showErr(new Error('DDNS 主机名必须属于填写的 DNS Zone'));
          return false;
        }
        values.ddns_zone = zone;
        state.ddnsZone = zone;
        for (const c of DDNS_CREDS[provider] ?? []) {
          const v = credsBox.querySelector<HTMLInputElement>(`#wz-cred-${c.key}`)!.value.trim();
          if (!v) {
            showErr(new Error(`请填 ${DDNS_LABELS[provider]} 的 ${c.label}`));
            return false;
          }
          values[c.key] = v;
        }
        // 域名留空时服务端会把 ddns_host 回填进 site_domain，前端同步这个结论
        if (!domain) state.domain = host;
      } else {
        state.ddnsHost = '';
        state.ddnsZone = '';
        state.domain = domain;
        values.ddns_provider = 'off';
        values.ddns_host = '';
        values.ddns_zone = '';
      }
      if (Object.keys(values).length > 0) {
        try {
          await adminSetConfig(values);
        } catch (e) {
          showErr(e);
          return false;
        }
      }
      if (!tlsChosen) state.tlsChoice = state.domain ? 'domain' : 'selfsigned';
      return true;
    };
    el.querySelector('#wz-next')!.addEventListener('click', async () => {
      if (await save()) stepTLS();
    });
    el.querySelector('#wz-skip')!.addEventListener('click', async () => {
      input.value = '';
      provider = 'off';
      syncDdns();
      state.domain = '';
      state.siteDomain = '';
      state.ddnsHost = '';
      state.ddnsZone = '';
      state.ddnsProvider = 'off';
      state.tlsChoice = 'selfsigned';
      stepTLS();
    });
  }

  // ---- 第 3 步：证书方式 ----
  async function stepTLS() {
    paint(`${stepChip(3, '证书方式')}<div class="muted">正在读取公网可达性…</div>`);
    let ipCertificate: NetcheckResult['ip_certificate'] = {
      available: false,
      reason: '网络自检失败，暂时无法选择 IP 证书',
    };
    try {
      ipCertificate = (await adminNetcheck()).ip_certificate;
    } catch {
      // 自检失败不阻塞自签名路径。
    }
    if (!alive()) return;
    const hasDomain = state.domain !== '';
    if (!tlsChosen) state.tlsChoice = hasDomain ? 'domain' : ipCertificate.available ? 'ip' : 'selfsigned';
    const options = [
      {
        id: 'domain',
        label: '域名 + ACME 证书',
        desc: hasDomain
          ? `按 ${state.domain} 自动签发与续期；配置了 Cloudflare 或阿里云时优先用 DNS-01。`
          : '需要第 2 步填了域名才能用。',
        disabled: !hasDomain,
      },
      {
        id: 'ip',
        label: '仅公网 IP + 短期证书',
        desc: hasDomain
          ? '已配置公开域名，将优先使用域名证书；清空域名后才能选 IP 证书。'
          : ipCertificate.available
            ? `按 ${ipCertificate.subject} 自动签发 shortlived 证书；${ipCertificate.reason}`
            : ipCertificate.reason,
        disabled: hasDomain || !ipCertificate.available,
      },
      {
        id: 'selfsigned',
        label: '自签名证书（本地 CA）',
        desc: '立即可用，不依赖公网验证。设备需信任管理后台可下载的本地 CA 才能去掉证书警告。',
        disabled: false,
      },
    ];
    const el = paint(`
      ${stepChip(3, '证书方式')}
      <div style="display:flex;flex-direction:column;gap:8px" role="radiogroup">
        ${options
          .map(
            (o) => `
          <button type="button" class="hit wz-tls-opt" data-id="${o.id}" ${o.disabled ? 'disabled' : ''}
            style="display:flex;align-items:flex-start;gap:11px;padding:11px 12px;border-radius:9px;border:1px solid var(--line);background:var(--bg-2);text-align:left;width:100%;${o.disabled ? 'opacity:0.5' : ''}">
            <div class="radio" style="margin-top:1px"><div class="dot"></div></div>
            <div style="flex-grow:1">
              <div style="font-size:13px;font-weight:500;color:var(--text-0)">${esc(o.label)}</div>
              <div style="font-size:11px;color:var(--text-2);margin-top:2px;line-height:1.6">${esc(o.desc)}</div>
            </div>
          </button>`,
          )
          .join('')}
      </div>
      ${errLine}
      <div style="display:flex;gap:10px;margin-top:14px">
        <button type="button" class="hit btn" id="wz-back">上一步</button>
        <button type="button" class="hit btn btn-primary" id="wz-next" style="flex:1">保存并继续</button>
      </div>`);

    let picked: WizardState['tlsChoice'] = state.tlsChoice;
    const syncRadios = () => {
      el.querySelectorAll<HTMLButtonElement>('.wz-tls-opt').forEach((b) => {
        const on = b.dataset.id === picked;
        b.classList.toggle('on', on);
        b.style.borderColor = on ? 'var(--ember-line)' : 'var(--line)';
        b.style.background = on ? 'var(--ember-tint)' : 'var(--bg-2)';
        b.querySelector('.radio')!.classList.toggle('on', on);
      });
    };
    el.querySelectorAll<HTMLButtonElement>('.wz-tls-opt').forEach((b) => {
      b.addEventListener('click', () => {
        picked = b.dataset.id as WizardState['tlsChoice'];
        state.tlsChoice = picked;
        tlsChosen = true;
        syncRadios();
      });
    });
    syncRadios();

    el.querySelector('#wz-back')!.addEventListener('click', stepDomain);
    el.querySelector('#wz-next')!.addEventListener('click', async () => {
      state.tlsChoice = picked;
      try {
        // 热生效：保存后服务端立刻起/换 HTTPS listener，不用重启
        await adminSetConfig({ tls_mode: picked === 'selfsigned' ? 'selfsigned' : 'acme' });
      } catch (e) {
        showErr(e);
        return;
      }
      stepCheck();
    });
  }

  // ---- 第 4 步：自检 + 邀请链接 ----
  function stepCheck() {
    const el = paint(`
      ${stepChip(4, '自检与邀请')}
      <div id="wz-check" class="muted">正在自检…</div>
      <div id="wz-invite" style="margin-top:14px"></div>
      <div style="display:flex;gap:10px;margin-top:16px">
        <button type="button" class="hit btn" id="wz-recheck">重新自检</button>
        <button type="button" class="hit btn btn-primary" id="wz-done" style="flex:1">完成，进入 Hearth</button>
      </div>`);
    el.querySelector('#wz-done')!.addEventListener('click', () => {
      location.hash = '#/lobby';
    });
    el.querySelector('#wz-recheck')!.addEventListener('click', () => void runCheck());

    const VERDICT: Record<string, [string, string]> = {
      reachable: ['外部可达', 'var(--sage)'],
      unknown: ['本机无法确认', 'var(--text-2)'],
      failed: ['不可达', 'var(--red)'],
    };

    async function runCheck() {
      const box = el.querySelector<HTMLDivElement>('#wz-check')!;
      box.innerHTML = `<div class="muted">正在自检…</div>`;
      let nc: NetcheckResult;
      try {
        nc = await adminNetcheck();
      } catch (e) {
        box.innerHTML = `<div class="error-text">${esc((e as Error).message)}</div>`;
        return;
      }
      if (!alive()) return;
      const [vLabel, vColor] = VERDICT[nc.external.verdict] ?? VERDICT.unknown;
      const activeCertificate =
        nc.tls.active === 'acme' ? 'ACME 公开证书' : nc.tls.active === 'selfsigned' ? '自签名兜底证书' : '未使用';
      const rows: [string, string][] = [
        ['配置模式', nc.tls.mode === 'off' ? '关闭（纯 HTTP）' : nc.tls.mode === 'acme' ? '自动证书（ACME）' : '自签名（本地 CA）'],
        ['当前证书', activeCertificate],
        ['端口映射', nc.portmap.Detail || nc.portmap.Diagnosis],
        ['域名解析', nc.domain.configured === '' ? '未配置' : nc.domain.match === 'ok' ? '一致' : (nc.domain.detail ?? nc.domain.match)],
        ['外部可达', nc.external.detail],
      ];
      box.innerHTML = `
        <div class="card" style="padding:0">
          ${rows
            .map(
              ([k, v]) => `
            <div style="display:flex;gap:12px;padding:10px 14px;border-bottom:1px solid var(--line-soft);align-items:baseline">
              <span style="font-size:12px;color:var(--text-2);flex-shrink:0;width:64px">${k}</span>
              <span style="font-size:12px;line-height:1.6;color:var(--text-1);flex-grow:1">${esc(v)}</span>
            </div>`,
            )
            .join('')}
          <div style="display:flex;gap:12px;padding:10px 14px;align-items:center">
            <span style="font-size:12px;color:var(--text-2);flex-shrink:0;width:64px">结论</span>
            <span style="font-size:12.5px;font-weight:600;color:${vColor}">${vLabel}</span>
          </div>
        </div>
        ${
          nc.tls.active === 'selfsigned'
            ? `<div style="margin-top:10px;font-size:11.5px;line-height:1.7;color:var(--text-2)">${
                nc.tls.mode === 'acme'
                  ? `ACME 正在签发或重试，期间 HTTPS 已由自签名证书兜底。${nc.tls.last_error ? `最近错误：${esc(nc.tls.last_error)}` : ''}`
                  : '自签名模式已启用。'
              } 想去掉警告：管理后台 → 网络 → 下载 CA 证书并在设备上信任。</div>`
            : ''
        }`;
    }

    async function makeInvite() {
      const box = el.querySelector<HTMLDivElement>('#wz-invite')!;
      try {
        const { url } = await createInvite('初始邀请', 0, '7d');
        if (!alive()) return;
        box.innerHTML = `
          <div class="card" style="padding:13px 14px">
            <div style="font-size:12.5px;font-weight:600;margin-bottom:7px">把这条链接发给要一起开黑的人</div>
            <div style="display:flex;gap:8px;align-items:center">
              <div class="mono" style="flex-grow:1;min-width:0;padding:8px 10px;border-radius:7px;background:var(--bg-0);border:1px solid var(--line-soft);font-size:11px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(url)}</div>
              <button type="button" class="hit btn btn-sm" id="wz-copy">复制</button>
            </div>
            <div style="font-size:11px;color:var(--text-3);margin-top:7px">7 天有效、不限人数。之后随时能在管理后台 → 邀请 里再发。</div>
          </div>`;
        box.querySelector('#wz-copy')!.addEventListener('click', () => {
          copyText(url);
          toast('已复制邀请链接', 'ok');
        });
      } catch {
        // 邀请链接建失败不挡向导收尾，大厅里还能再发
      }
    }

    void runCheck();
    void makeInvite();
  }

  stepAccount();
}
