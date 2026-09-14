// 服务器地址页：只在桌面壳里出现。同一个安装包要能连任意一台 hearth，
// 首屏先问「连哪台」，填完存进本机存档后整页重载，后面一切照旧走网页原有流程。
import { clearSession, getServerURL, setServerURL } from '../api';
import { wireThemeButton } from '../theme';
import { esc, flameLogo } from '../ui';

export function renderServerPick(root: HTMLElement) {
  const current = getServerURL() ?? '';
  document.title = '连接服务器 · Hearth';

  root.innerHTML = `
    <div class="auth-page" style="position:relative">
      <button class="hit theme-fab" id="theme-fab"></button>
      <div class="auth-card">
        <div class="auth-brand">
          ${flameLogo(38, 42)}
          <div class="word">HEARTH</div>
          <div class="host mono">连接到服务器</div>
        </div>
        <form class="auth-form" id="sv-form">
          <div style="display:flex;flex-direction:column;gap:7px">
            <label class="field-label" for="sv-url">服务器地址</label>
            <div class="field" id="sv-field">
              <input id="sv-url" placeholder="https://hearth.example.com" autocapitalize="off" autocomplete="url"
                     spellcheck="false" enterkeyhint="go" value="${esc(current)}" />
            </div>
          </div>
          <p class="error-text" id="sv-error" style="margin:0;min-height:1em"></p>
          <button type="submit" class="hit btn btn-primary btn-lg" id="sv-btn">连接</button>
        </form>
      </div>
    </div>
  `;

  wireThemeButton(root.querySelector<HTMLButtonElement>('#theme-fab')!);
  const form = root.querySelector<HTMLFormElement>('#sv-form')!;
  const input = root.querySelector<HTMLInputElement>('#sv-url')!;
  const errEl = root.querySelector<HTMLParagraphElement>('#sv-error')!;

  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const raw = input.value.trim();
    // 没写协议就按 https 补：填 hearth.example.com 是最常见的写法
    const withScheme = /^https?:\/\//i.test(raw) ? raw : `https://${raw}`;
    let url: URL;
    try {
      url = new URL(withScheme);
    } catch {
      errEl.textContent = '地址填得不对，形如 https://hearth.example.com';
      return;
    }
    if (!url.hostname) {
      errEl.textContent = '地址填得不对，形如 https://hearth.example.com';
      return;
    }
    // 换服务器等于换了整套账号：旧会话留着只会在新服务器上撞 401
    if (getServerURL() && getServerURL() !== url.origin) clearSession();
    setServerURL(url.origin);
    location.hash = '#/lobby';
    location.reload(); // SERVER_URL 在模块加载时定一次，必须整页重来
  });

  input.focus();
}
