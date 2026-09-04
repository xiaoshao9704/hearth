// 凭邀请链接注册：#/join/<code>。倒计时真跑，归零切过期态。
import { ApiError, getToken, getUser, inviteInfo, register, siteInfo } from '../api';
import { wireThemeButton } from '../theme';
import { avatarHtml, esc, flameLogo, icon, pwBarsHtml, pwScore } from '../ui';

const USER_RE = /^[a-zA-Z0-9_-]{2,32}$/;

export async function renderJoin(root: HTMLElement, code: string, alive: () => boolean) {
  // 已登录时打开邀请链接：先问清楚，别悄悄顶掉当前会话
  if (getToken()) {
    paintAlreadyLoggedIn(root, code, alive);
    return;
  }
  await renderInviteFlow(root, code, alive);
}

function paintAlreadyLoggedIn(root: HTMLElement, code: string, alive: () => boolean) {
  const user = getUser();
  root.innerHTML = `
    <div class="auth-page">
      <div class="auth-card" style="align-items:center;text-align:center;gap:14px">
        ${flameLogo(34, 38)}
        <div style="font-size:13.5px;line-height:1.6">你已登录为 <span style="font-weight:600">${esc(user?.username ?? '')}</span>。<br/>用这条邀请另建账号，还是直接进大厅？</div>
        <div style="display:flex;gap:10px">
          <button type="button" class="hit btn" id="go-lobby">直接进大厅</button>
          <button type="button" class="hit btn btn-primary" id="go-register">另建账号</button>
        </div>
      </div>
    </div>`;
  root.querySelector('#go-lobby')!.addEventListener('click', () => {
    location.hash = '#/lobby';
  });
  root.querySelector('#go-register')!.addEventListener('click', () => {
    void renderInviteFlow(root, code, alive);
  });
}

async function renderInviteFlow(root: HTMLElement, code: string, alive: () => boolean) {
  root.innerHTML = `<div class="auth-page"><div class="muted">正在核对邀请…</div></div>`;

  let inviter = '';
  let expiresAt = 0;
  let known = true; // 邀请码是否存在（404 = 不存在）
  let connError = false; // 网络/服务器错误，与「不存在」分开提示
  let siteName = 'Hearth'; // 站点名（/api/site），拉不到就用默认名
  try {
    const [info, site] = await Promise.all([inviteInfo(code), siteInfo().catch(() => null)]);
    inviter = info.inviter;
    expiresAt = new Date(info.expires_at).getTime();
    if (!info.alive) expiresAt = 0; // 名额用完/撤销也按失效展示
    if (site?.name) siteName = site.name;
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) {
      known = false;
    } else {
      connError = true;
    }
  }
  if (!alive()) return;

  if (connError) {
    root.innerHTML = `
      <div class="auth-page">
        <div class="state-block error" style="max-width:340px;margin:auto">
          ${icon('warn', 22, 'var(--red)', 1.6)}
          <div>暂时连不上服务器</div>
          <button type="button" class="hit btn btn-sm" id="retry-btn">重试</button>
        </div>
      </div>`;
    root.querySelector('#retry-btn')!.addEventListener('click', () => void renderInviteFlow(root, code, alive));
    return;
  }

  let reveal = false;
  let busy = false;
  let done = false;
  let redirectTimer = 0;

  root.innerHTML = `
    <div class="auth-page" style="position:relative">
      <button class="hit theme-fab" id="theme-fab"></button>
      <div class="auth-card" style="width:420px">
        <div class="auth-brand">
          ${flameLogo(34, 38)}
          <div class="word" style="font-size:21px;letter-spacing:0.2em;padding-left:0.2em">HEARTH</div>
        </div>

        <div class="card" style="margin-top:26px;display:flex;align-items:center;gap:13px">
          ${avatarHtml(inviter || '?', 'avatar-lg avatar')}
          <div style="flex-grow:1;min-width:0">
            <div style="font-size:13.5px;line-height:1.5"><span style="font-weight:600">${esc(inviter || '有人')}</span> 邀请你加入 ${esc(siteName)}</div>
            <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:3px">${esc(location.host)}</div>
          </div>
          <div id="ttl-chip"></div>
        </div>

        <div id="join-body"></div>

        <div class="auth-note hint-card" style="border-color:var(--line-soft)">
          <span style="flex-shrink:0;margin-top:1px">${icon('shield', 16, 'var(--text-2)', 1.6)}</span>
          <span>这个账号只在这台服务器上有效，不联通任何第三方。密码存的是哈希，管理员也看不到。</span>
        </div>
      </div>
    </div>
  `;

  const themeFab = root.querySelector<HTMLButtonElement>('#theme-fab')!;
  wireThemeButton(themeFab);

  const ttlChip = root.querySelector<HTMLDivElement>('#ttl-chip')!;
  const body = root.querySelector<HTMLDivElement>('#join-body')!;

  const inviteAlive = () => known && expiresAt > Date.now();

  function paintTTL() {
    if (!inviteAlive()) {
      ttlChip.innerHTML = `<div style="display:flex;align-items:center;gap:6px;padding:6px 10px;border-radius:8px;background:var(--red-tint);border:1px solid var(--red-line)">${icon('clock', 13, 'var(--red-text)', 1.8)}<span class="mono" style="font-size:11.5px;color:var(--red-text)">已过期</span></div>`;
      return;
    }
    const left = Math.max(0, Math.floor((expiresAt - Date.now()) / 1000));
    const urgent = left < 3600;
    const p = (n: number) => String(n).padStart(2, '0');
    const text = `${p(Math.floor(left / 3600))}:${p(Math.floor((left % 3600) / 60))}:${p(left % 60)} 后过期`;
    const ink = urgent ? 'var(--red-text)' : 'var(--sage)';
    const bg = urgent ? 'var(--red-tint)' : 'var(--sage-tint)';
    const line = urgent ? 'var(--red-line)' : 'var(--sage-line)';
    ttlChip.innerHTML = `<div style="display:flex;align-items:center;gap:6px;padding:6px 10px;border-radius:8px;background:${bg};border:1px solid ${line}">${icon('clock', 13, ink, 1.8)}<span class="mono" style="font-size:11.5px;color:${ink}">${text}</span></div>`;
  }

  function paintExpired() {
    body.innerHTML = `
      <div style="margin-top:24px;padding:22px 20px;border-radius:12px;background:var(--red-tint);border:1px solid var(--red-line);display:flex;flex-direction:column;align-items:center;gap:10px">
        ${icon('warn', 26, 'var(--red)', 1.6)}
        <div style="font-size:14px;font-weight:600;color:var(--red-text)">邀请链接${known ? '已过期' : '不存在'}</div>
        <div style="font-size:12.5px;line-height:1.6;color:var(--text-1);text-align:center;text-wrap:pretty">找${esc(inviter || '对方')}再要一条，重新生成只要点一下。</div>
      </div>
      <div style="text-align:center;margin-top:14px;font-size:12px"><a href="#/login">已有账号？去登录</a></div>`;
  }

  function paintForm() {
    body.innerHTML = `
      <form style="margin-top:20px;display:flex;flex-direction:column;gap:13px" id="join-form">
        <div style="display:flex;flex-direction:column;gap:7px">
          <div style="display:flex;align-items:baseline;gap:8px">
            <label class="field-label" for="jn-user" style="flex-grow:1">用户名</label>
            <div id="hint-user" style="font-size:11px"></div>
          </div>
          <div class="field" style="height:46px"><input id="jn-user" placeholder="2–32 位，字母数字 - _" autocapitalize="off" autocomplete="username" enterkeyhint="next" /></div>
        </div>
        <div style="display:flex;flex-direction:column;gap:7px">
          <div style="display:flex;align-items:baseline;gap:8px">
            <label class="field-label" for="jn-pass" style="flex-grow:1">密码</label>
            <div id="hint-pass" style="font-size:11px"></div>
          </div>
          <div class="field" style="height:46px">
            <input id="jn-pass" type="password" placeholder="至少 8 位" autocomplete="new-password" enterkeyhint="next" />
            <button type="button" class="hit" id="jn-reveal" style="width:28px;height:28px;border-radius:7px;display:flex;align-items:center;justify-content:center">${icon('eyeOff', 18, 'var(--text-2)', 1.6)}</button>
          </div>
          <div class="pw-bars" id="pw-bars">${pwBarsHtml(0)}</div>
        </div>
        <div style="display:flex;flex-direction:column;gap:7px">
          <div style="display:flex;align-items:baseline;gap:8px">
            <label class="field-label" for="jn-conf" style="flex-grow:1">确认密码</label>
            <div id="hint-conf" style="font-size:11px"></div>
          </div>
          <div class="field" style="height:46px"><input id="jn-conf" type="password" placeholder="再输一次" autocomplete="new-password" enterkeyhint="go" /></div>
        </div>
        <p class="error-text" id="jn-error" style="margin:0;min-height:0"></p>
        <button type="submit" class="hit btn btn-primary btn-lg disabled" id="jn-btn" style="margin-top:4px" disabled>创建账号并进入</button>
        <div class="notice-ok hidden" id="jn-done">
          ${icon('check', 16, 'var(--sage)', 1.9)}<span style="flex-grow:1">账号建好了，正在带你进大厅…</span>
          <button type="button" class="hit btn btn-sm" id="jn-go-now">立即进入</button>
        </div>
        <div style="text-align:center;font-size:12px"><a href="#/login">已有账号？去登录</a></div>
      </form>`;

    const userInput = body.querySelector<HTMLInputElement>('#jn-user')!;
    const passInput = body.querySelector<HTMLInputElement>('#jn-pass')!;
    const confInput = body.querySelector<HTMLInputElement>('#jn-conf')!;
    const revealBtn = body.querySelector<HTMLButtonElement>('#jn-reveal')!;
    const btn = body.querySelector<HTMLButtonElement>('#jn-btn')!;
    const errEl = body.querySelector<HTMLParagraphElement>('#jn-error')!;

    const userOk = () => USER_RE.test(userInput.value);
    const passOk = () => passInput.value.length >= 8;
    const confOk = () => confInput.value.length > 0 && confInput.value === passInput.value;
    const valid = () => userOk() && passOk() && confOk();

    const hint = (id: string, text: string, tone: 'good' | 'bad' | '') => {
      const el = body.querySelector<HTMLDivElement>(`#${id}`)!;
      el.textContent = text;
      el.style.color = tone === 'bad' ? 'var(--red-text)' : tone === 'good' ? 'var(--sage)' : 'var(--text-2)';
    };

    function sync() {
      hint('hint-user', userInput.value ? (userOk() ? '可用' : '2–32 位字母数字 - _') : '', userOk() ? 'good' : 'bad');
      hint('hint-pass', passInput.value ? (passOk() ? '长度够了' : `还差 ${8 - passInput.value.length} 位`) : '', passOk() ? 'good' : 'bad');
      hint('hint-conf', confInput.value ? (confOk() ? '一致' : '两次不一样') : '', confOk() ? 'good' : 'bad');
      body.querySelector('#pw-bars')!.innerHTML = pwBarsHtml(pwScore(passInput.value));
      const disabled = !valid() || busy || done;
      btn.classList.toggle('disabled', disabled);
      btn.disabled = disabled;
    }
    [userInput, passInput, confInput].forEach((el) => el.addEventListener('input', sync));

    revealBtn.addEventListener('click', () => {
      reveal = !reveal;
      passInput.type = confInput.type = reveal ? 'text' : 'password';
      revealBtn.innerHTML = icon(reveal ? 'eye' : 'eyeOff', 18, reveal ? 'var(--ember)' : 'var(--text-2)', 1.6);
    });

    body.querySelector('#join-form')!.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      if (!valid() || busy || done) return;
      busy = true;
      errEl.textContent = '';
      btn.textContent = '正在创建…';
      sync();
      try {
        await register(userInput.value, passInput.value, code);
        if (!alive()) return;
        done = true;
        btn.textContent = '已创建';
        body.querySelector('#jn-done')!.classList.remove('hidden');
        body.querySelector('#jn-go-now')!.addEventListener('click', () => {
          clearTimeout(redirectTimer);
          location.hash = '#/lobby';
        });
        redirectTimer = window.setTimeout(() => {
          location.hash = '#/lobby';
        }, 900);
      } catch (err) {
        errEl.textContent = (err as Error).message;
        busy = false;
        btn.textContent = '创建账号并进入';
        sync();
      }
    });
  }

  paintTTL();
  if (inviteAlive()) {
    paintForm();
  } else {
    paintExpired();
  }

  const timer = setInterval(() => {
    if (!root.isConnected || !alive()) {
      clearInterval(timer);
      clearTimeout(redirectTimer);
      return;
    }
    paintTTL();
    if (!inviteAlive() && body.querySelector('#join-form') && !done && !busy) paintExpired();
  }, 1000);
}
