// 桌面端授权页：只在系统浏览器里打开（壳把 #/device-auth?ch=<challenge> 交给默认浏览器）。
// 用户在这里用密码或通行密钥正常登录——浏览器的 origin 是服务器真实域名，WebAuthn 可用，
// 壳里那个 tauri://localhost 不可用的问题就绕开了。
//
// 必须点「允许」才签码：这一页是授权确认，不是跳板。任何自动批准都等于让打开链接
// 这个动作本身变成登录，那样就没有确认可言了。
import { deviceApprove, getUser } from '../api';
import { wireThemeButton } from '../theme';
import { esc, flameLogo, icon } from '../ui';

// challenge 是 base64url 无填充的 sha256（32 字节 → 43 字符）
const CHALLENGE_RE = /^[A-Za-z0-9_-]{43}$/;

export function renderDeviceAuth(root: HTMLElement, challenge: string) {
  document.title = '授权桌面端 · Hearth';
  const user = getUser();

  root.innerHTML = `
    <div class="auth-page" style="position:relative">
      <button class="hit theme-fab" id="theme-fab"></button>
      <div class="auth-card">
        <div class="auth-brand">
          ${flameLogo(38, 42)}
          <div class="word">HEARTH</div>
          <div class="host mono">${esc(location.host || 'localhost')}</div>
        </div>
        <div class="auth-form" id="da-body"></div>
      </div>
    </div>
  `;

  wireThemeButton(root.querySelector<HTMLButtonElement>('#theme-fab')!);
  const body = root.querySelector<HTMLDivElement>('#da-body')!;

  // 收尾态：授权流程已经结束（允许 / 拒绝 / 参数不对），页面只剩一句说明
  const finish = (title: string, detail: string) => {
    body.innerHTML = `
      <div class="auth-note card" style="display:flex;gap:12px">
        <span style="flex-shrink:0;margin-top:1px">${icon('info', 17, 'var(--text-2)', 1.6)}</span>
        <div style="display:flex;flex-direction:column;gap:5px">
          <div style="font-size:12.5px;font-weight:600">${esc(title)}</div>
          <div style="font-size:12px;line-height:1.6;color:var(--text-2);text-wrap:pretty">${esc(detail)}</div>
        </div>
      </div>`;
  };

  if (!CHALLENGE_RE.test(challenge)) {
    finish('授权链接不完整', '请回到桌面端重新点一次「在浏览器中登录」。');
    return;
  }

  body.innerHTML = `
    <p style="margin:0;font-size:13.5px;line-height:1.7;text-wrap:pretty">
      桌面端请求登录你的账号 <strong>${esc(user?.username ?? '')}</strong>。
      允许后这台设备上的 Hearth 桌面端将以该账号登录。
    </p>
    <p class="muted" style="margin:0;font-size:12.5px;line-height:1.6;text-wrap:pretty">
      如果刚才不是你在桌面端点的「在浏览器中登录」，请点拒绝。
    </p>
    <p class="error-text" id="da-error" style="margin:0;min-height:1em"></p>
    <button type="button" class="hit btn btn-primary btn-lg" id="da-allow">允许</button>
    <button type="button" class="hit btn btn-lg" id="da-deny">拒绝</button>
  `;

  const errEl = body.querySelector<HTMLParagraphElement>('#da-error')!;
  const allowBtn = body.querySelector<HTMLButtonElement>('#da-allow')!;
  const denyBtn = body.querySelector<HTMLButtonElement>('#da-deny')!;

  denyBtn.addEventListener('click', () => finish('已拒绝', '桌面端不会拿到这个账号的登录凭证，可以关闭此页。'));

  allowBtn.addEventListener('click', async () => {
    allowBtn.disabled = true;
    denyBtn.disabled = true;
    errEl.textContent = '';
    allowBtn.textContent = '正在签发…';
    try {
      const code = await deviceApprove(challenge);
      // 导航到自定义 scheme：系统把它交给桌面端。本页不会因此被顶掉，留一句说明即可。
      location.href = `hearth://auth?code=${encodeURIComponent(code)}`;
      finish('已跳回桌面端', '可以关闭此页。如果桌面端没有反应，回到桌面端重新点一次「在浏览器中登录」。');
    } catch (err) {
      errEl.textContent = (err as Error).message;
      allowBtn.disabled = false;
      denyBtn.disabled = false;
      allowBtn.textContent = '允许';
    }
  });
}
