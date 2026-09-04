// 登录页：注册入口按 /api/site 的 policy 显隐（closed 不出；invite 提示要邀请链接；open 出自助注册表单）。
// 站点名（site.name）用于品牌位与按钮文案，拉取失败按 closed + 默认名处理。
import { login, register, siteInfo } from '../api';
import { wireThemeButton } from '../theme';
import { esc, flameLogo, icon } from '../ui';

const LAST_USER_KEY = 'hearth_last_user';
const NEXT_KEY = 'hearth_next';

const USER_RE = /^[a-zA-Z0-9_-]{2,32}$/;

export function renderLogin(root: HTMLElement) {
  let reveal = false;
  let busy = false;
  let siteName = 'Hearth';
  let mode: 'login' | 'register' = 'login';
  const lastUser = localStorage.getItem(LAST_USER_KEY) ?? '';

  root.innerHTML = `
    <div class="auth-page" style="position:relative">
      <button class="hit theme-fab" id="theme-fab"></button>
      <div class="auth-card">
        <div class="auth-brand">
          ${flameLogo(38, 42)}
          <div class="word" id="lg-word">HEARTH</div>
          <div class="host mono">${esc(location.host || 'localhost')}</div>
        </div>
        <form class="auth-form" id="login-form">
          <div style="display:flex;flex-direction:column;gap:7px">
            <label class="field-label" for="lg-user">用户名</label>
            <div class="field" id="lg-user-field">
              <input id="lg-user" placeholder="你的账号" autocapitalize="off" autocomplete="username" enterkeyhint="next" value="${esc(lastUser)}" />
            </div>
          </div>
          <div style="display:flex;flex-direction:column;gap:7px">
            <label class="field-label" for="lg-pass">密码</label>
            <div class="field" id="lg-pass-field">
              <input id="lg-pass" type="password" placeholder="••••••••" autocomplete="current-password" enterkeyhint="go" />
              <button type="button" class="hit mini-btn" id="lg-reveal" style="width:28px;height:28px;border-radius:7px;display:flex;align-items:center;justify-content:center">${icon('eyeOff', 17, 'var(--text-2)', 1.6)}</button>
            </div>
          </div>
          <div style="display:none;flex-direction:column;gap:7px" id="lg-conf-wrap">
            <label class="field-label" for="lg-conf">确认密码</label>
            <div class="field" id="lg-conf-field">
              <input id="lg-conf" type="password" placeholder="再输一次" autocomplete="new-password" enterkeyhint="go" />
            </div>
          </div>
          <p class="error-text" id="lg-error" style="margin:0;min-height:1em"></p>
          <button type="submit" class="hit btn btn-primary btn-lg disabled" id="lg-btn" disabled>进入 Hearth</button>
          <div style="text-align:center;font-size:12px" id="lg-switch" hidden><a href="" id="lg-switch-a">已有账号？去登录</a></div>
        </form>
        <div class="auth-note card" style="display:none;gap:12px" id="lg-note"></div>
      </div>
    </div>
  `;

  const form = root.querySelector<HTMLFormElement>('#login-form')!;
  const userField = root.querySelector<HTMLDivElement>('#lg-user-field')!;
  const passField = root.querySelector<HTMLDivElement>('#lg-pass-field')!;
  const confWrap = root.querySelector<HTMLDivElement>('#lg-conf-wrap')!;
  const confInput = root.querySelector<HTMLInputElement>('#lg-conf')!;
  const userInput = root.querySelector<HTMLInputElement>('#lg-user')!;
  const passInput = root.querySelector<HTMLInputElement>('#lg-pass')!;
  const revealBtn = root.querySelector<HTMLButtonElement>('#lg-reveal')!;
  const errEl = root.querySelector<HTMLParagraphElement>('#lg-error')!;
  const btn = root.querySelector<HTMLButtonElement>('#lg-btn')!;
  const switchWrap = root.querySelector<HTMLDivElement>('#lg-switch')!;
  const noteEl = root.querySelector<HTMLDivElement>('#lg-note')!;
  const themeFab = root.querySelector<HTMLButtonElement>('#theme-fab')!;

  wireThemeButton(themeFab);

  const syncBrand = () => {
    root.querySelector('#lg-word')!.textContent = siteName;
    document.title = `登录 · ${siteName}`;
    btn.textContent = mode === 'register' ? `创建账号并进入` : `进入 ${siteName}`;
  };

  // 注册入口按策略渲染进 lg-note；closed（含拉取失败）不出
  const paintNote = (policy: string) => {
    if (policy === 'open') {
      noteEl.style.display = 'flex';
      noteEl.innerHTML = `
        <span style="flex-shrink:0;margin-top:1px">${icon('user', 17, 'var(--text-2)', 1.6)}</span>
        <div style="display:flex;flex-direction:column;gap:5px">
          <div style="font-size:12.5px;font-weight:600">开放注册中</div>
          <div style="font-size:12px;line-height:1.6;color:var(--text-2);text-wrap:pretty">没有账号？<button type="button" class="hit" id="lg-go-register" style="color:var(--ember)">自己创建一个</button>，用户名密码落地即进。</div>
        </div>`;
      noteEl.querySelector('#lg-go-register')!.addEventListener('click', () => setMode('register'));
    } else if (policy === 'invite') {
      noteEl.style.display = 'flex';
      noteEl.innerHTML = `
        <span style="flex-shrink:0;margin-top:1px">${icon('mail', 17, 'var(--text-2)', 1.6)}</span>
        <div style="display:flex;flex-direction:column;gap:5px">
          <div style="font-size:12.5px;font-weight:600">需要邀请链接才能注册</div>
          <div style="font-size:12px;line-height:1.6;color:var(--text-2);text-wrap:pretty">找有发邀请权限的人要一条链接，点开就能自己设账号密码；链接过期就作废。也可以在服务器上直接开：</div>
          <div class="mono" style="margin-top:3px;padding:7px 10px;border-radius:6px;background:var(--bg-0);border:1px solid var(--line-soft);font-size:11px;color:var(--sage)">hearth adduser &lt;name&gt; &lt;password&gt;</div>
        </div>`;
    } else {
      noteEl.style.display = 'none';
      noteEl.innerHTML = '';
    }
  };

  void siteInfo()
    .then((s) => {
      siteName = s.name || siteName;
      syncBrand();
      paintNote(s.policy);
    })
    .catch(() => paintNote('closed')); // 拉不到站点信息：按关闭注册处理，不出注册入口

  const setMode = (m: 'login' | 'register') => {
    mode = m;
    // inline display:flex 会压过 hidden 属性的 UA 样式，显隐直接改 style
    confWrap.style.display = m === 'register' ? 'flex' : 'none';
    switchWrap.hidden = m !== 'register';
    errEl.textContent = '';
    syncBrand();
    syncBtn();
  };
  switchWrap.querySelector('#lg-switch-a')!.addEventListener('click', (ev) => {
    ev.preventDefault();
    setMode('login');
  });

  revealBtn.addEventListener('click', () => {
    reveal = !reveal;
    passInput.type = confInput.type = reveal ? 'text' : 'password';
    revealBtn.innerHTML = icon(reveal ? 'eye' : 'eyeOff', 17, reveal ? 'var(--ember)' : 'var(--text-2)', 1.6);
  });

  const ready = () => {
    if (!userInput.value.trim() || !passInput.value) return false;
    if (mode === 'register') {
      return USER_RE.test(userInput.value.trim()) && passInput.value.length >= 8 && confInput.value === passInput.value;
    }
    return true;
  };
  const syncBtn = () => {
    const disabled = !ready() || busy;
    btn.classList.toggle('disabled', disabled);
    btn.disabled = disabled;
  };
  const clearBad = () => {
    userField.classList.remove('bad');
    passField.classList.remove('bad');
  };
  userInput.addEventListener('input', () => {
    clearBad();
    syncBtn();
  });
  passInput.addEventListener('input', () => {
    clearBad();
    syncBtn();
  });
  confInput.addEventListener('input', syncBtn);
  syncBtn();

  const afterAuth = () => {
    localStorage.setItem(LAST_USER_KEY, userInput.value.trim());
    const next = sessionStorage.getItem(NEXT_KEY);
    sessionStorage.removeItem(NEXT_KEY);
    location.hash = next && next.startsWith('#/') ? next : '#/lobby';
  };

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!ready() || busy) return;
    busy = true;
    errEl.textContent = '';
    btn.textContent = '正在连接…';
    syncBtn();
    try {
      if (mode === 'register') {
        await register(userInput.value.trim(), passInput.value);
      } else {
        await login(userInput.value.trim(), passInput.value);
      }
      afterAuth();
    } catch (err) {
      errEl.textContent = (err as Error).message;
      userField.classList.add('bad');
      passField.classList.add('bad');
      if (mode === 'login') passInput.value = '';
      busy = false;
      syncBrand();
      syncBtn();
      passInput.focus();
    }
  });

  if (lastUser) {
    passInput.focus();
  } else {
    userInput.focus();
  }
}
