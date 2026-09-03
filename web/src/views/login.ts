// 登录页：邀请制服务器，不开放公开注册（凭邀请链接走 #/join/<code>）。
import { login } from '../api';
import { cycleTheme, getTheme, THEME_ICONS } from '../theme';
import { esc, flameLogo, icon } from '../ui';

export function renderLogin(root: HTMLElement) {
  let reveal = false;
  let busy = false;

  root.innerHTML = `
    <div class="auth-page" style="position:relative">
      <button class="hit theme-fab" id="theme-fab">${icon(THEME_ICONS[getTheme()], 16, 'var(--text-1)', 1.6)}</button>
      <div class="auth-card">
        <div class="auth-brand">
          ${flameLogo(38, 42)}
          <div class="word">HEARTH</div>
          <div class="host mono">${esc(location.host || 'localhost')}</div>
        </div>
        <form class="auth-form" id="login-form">
          <div style="display:flex;flex-direction:column;gap:7px">
            <div class="field-label">用户名</div>
            <div class="field"><input id="lg-user" placeholder="你的账号" autocomplete="username" /></div>
          </div>
          <div style="display:flex;flex-direction:column;gap:7px">
            <div class="field-label">密码</div>
            <div class="field">
              <input id="lg-pass" type="password" placeholder="••••••••" autocomplete="current-password" />
              <button type="button" class="hit mini-btn" id="lg-reveal" style="width:28px;height:28px;border-radius:7px;display:flex;align-items:center;justify-content:center">${icon('eyeOff', 17, 'var(--text-2)', 1.6)}</button>
            </div>
          </div>
          <p class="error-text" id="lg-error" style="margin:0;min-height:1em"></p>
          <button type="submit" class="hit btn btn-primary btn-lg disabled" id="lg-btn">进入 Hearth</button>
        </form>
        <div class="auth-note card" style="display:flex;gap:12px">
          <span style="flex-shrink:0;margin-top:1px">${icon('mail', 17, 'var(--text-2)', 1.6)}</span>
          <div style="display:flex;flex-direction:column;gap:5px">
            <div style="font-size:12.5px;font-weight:600">不开放公开注册</div>
            <div style="font-size:12px;line-height:1.6;color:var(--text-2);text-wrap:pretty">要一条邀请链接才能建号，管理员在后台点一下就能发；链接过期就作废。也可以让管理员直接在服务器上开：</div>
            <div class="mono" style="margin-top:3px;padding:7px 10px;border-radius:6px;background:var(--bg-0);border:1px solid var(--line-soft);font-size:11px;color:var(--sage)">hearth adduser &lt;name&gt; &lt;password&gt;</div>
          </div>
        </div>
      </div>
    </div>
  `;

  const form = root.querySelector<HTMLFormElement>('#login-form')!;
  const userInput = root.querySelector<HTMLInputElement>('#lg-user')!;
  const passInput = root.querySelector<HTMLInputElement>('#lg-pass')!;
  const revealBtn = root.querySelector<HTMLButtonElement>('#lg-reveal')!;
  const errEl = root.querySelector<HTMLParagraphElement>('#lg-error')!;
  const btn = root.querySelector<HTMLButtonElement>('#lg-btn')!;
  const themeFab = root.querySelector<HTMLButtonElement>('#theme-fab')!;

  themeFab.addEventListener('click', () => {
    const next = cycleTheme();
    themeFab.innerHTML = icon(THEME_ICONS[next], 16, 'var(--text-1)', 1.6);
  });

  revealBtn.addEventListener('click', () => {
    reveal = !reveal;
    passInput.type = reveal ? 'text' : 'password';
    revealBtn.innerHTML = icon(reveal ? 'eye' : 'eyeOff', 17, reveal ? 'var(--ember)' : 'var(--text-2)', 1.6);
  });

  const ready = () => userInput.value.trim().length > 0 && passInput.value.length > 0;
  const syncBtn = () => btn.classList.toggle('disabled', !ready() || busy);
  userInput.addEventListener('input', syncBtn);
  passInput.addEventListener('input', syncBtn);

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!ready() || busy) return;
    busy = true;
    errEl.textContent = '';
    btn.textContent = '正在连接…';
    syncBtn();
    try {
      await login(userInput.value.trim(), passInput.value);
      location.hash = '#/lobby';
    } catch (err) {
      errEl.textContent = (err as Error).message;
      busy = false;
      btn.textContent = '进入 Hearth';
      syncBtn();
    }
  });
  userInput.focus();
}
