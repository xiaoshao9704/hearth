// 服务器地址页：只在桌面壳里出现。同一个安装包要能连任意一台 hearth，
// 首屏先问「连哪台」，填完存进本机存档后整页重载，后面一切照旧走网页原有流程。
//
// 连之前先让壳探一次（check_server）：证书系统信任就直接进；自签则要用另一渠道拿到的
// 根指纹配对一次（pair_server），配对只给这一台服务器加一条信任锚，不装系统 CA。
import { clearServerURL, clearSession, getServerURL, setServerURL } from '../api';
import { checkServer, forgetServer, inShell, pairServer } from '../bridge';
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
          <div style="display:none;flex-direction:column;gap:7px" id="sv-pair">
            <label class="field-label" for="sv-fp">根证书 SHA-256 指纹</label>
            <div class="field">
              <input id="sv-fp" placeholder="AA:BB:CC:… 或 64 位十六进制" autocapitalize="off" autocomplete="off"
                     spellcheck="false" enterkeyhint="go" />
            </div>
            <p class="muted" style="margin:0;font-size:12.5px;line-height:1.6">
              这台服务器用的是自签证书。请向管理员另行索取根证书指纹（服务器后台的「TLS」里能看到），
              照着填一遍。<strong>不要</strong>从这个页面自己下载到的证书上抄——那样等于没校验。
            </p>
          </div>
          <p class="error-text" id="sv-error" style="margin:0;min-height:1em"></p>
          <button type="submit" class="hit btn btn-primary btn-lg" id="sv-btn">连接</button>
          ${current ? '<button type="button" class="hit btn btn-sm" id="sv-forget">忘记此服务器</button>' : ''}
        </form>
      </div>
    </div>
  `;

  wireThemeButton(root.querySelector<HTMLButtonElement>('#theme-fab')!);
  const form = root.querySelector<HTMLFormElement>('#sv-form')!;
  const input = root.querySelector<HTMLInputElement>('#sv-url')!;
  const pairBox = root.querySelector<HTMLDivElement>('#sv-pair')!;
  const fpInput = root.querySelector<HTMLInputElement>('#sv-fp')!;
  const errEl = root.querySelector<HTMLParagraphElement>('#sv-error')!;
  const btn = root.querySelector<HTMLButtonElement>('#sv-btn')!;

  // 等待配对的那台服务器：指纹框只对它有效，地址一改就收起来重新探
  let pending = '';

  function commit(origin: string) {
    // 换服务器等于换了整套账号：旧会话留着只会在新服务器上撞 401
    if (getServerURL() && getServerURL() !== origin) clearSession();
    setServerURL(origin);
    location.hash = '#/lobby';
    location.reload(); // SERVER_URL 在模块加载时定一次，必须整页重来
  }

  function reset() {
    pending = '';
    pairBox.style.display = 'none';
    fpInput.value = '';
  }

  input.addEventListener('input', () => {
    if (pending) reset();
  });

  root.querySelector<HTMLButtonElement>('#sv-forget')?.addEventListener('click', () => {
    void (async () => {
      try {
        if (inShell()) await forgetServer(current);
      } catch {
        // 壳里没有这条记录也无妨：网页侧的存档照清
      }
      clearSession();
      clearServerURL();
      location.hash = '#/server';
      location.reload();
    })();
  });

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
    const origin = url.origin;
    // 浏览器里没有壳，也就没有信任配置：保持原行为
    if (!inShell()) {
      commit(origin);
      return;
    }

    errEl.textContent = '';
    btn.classList.add('loading');
    void (async () => {
      try {
        if (pending === origin && fpInput.value.trim()) {
          await pairServer(origin, fpInput.value.trim());
          commit(origin);
          return;
        }
        const res = await checkServer(origin);
        if (res.ok) {
          commit(origin);
          return;
        }
        if (res.reason === 'untrusted') {
          pending = origin;
          pairBox.style.display = 'flex';
          errEl.textContent = fpInput.value.trim()
            ? res.detail
            : '这台服务器的证书需要先配对：填入根证书指纹后再点连接';
          fpInput.focus();
          return;
        }
        reset();
        errEl.textContent = res.detail || '连接失败';
      } catch (err) {
        errEl.textContent = err instanceof Error ? err.message : String(err);
      } finally {
        btn.classList.remove('loading');
      }
    })();
  });

  input.focus();
}
