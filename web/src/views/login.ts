// 登录 / 注册页。
import { login, register } from '../api';

export function renderLogin(root: HTMLElement) {
  root.innerHTML = `
    <div class="center-page">
      <div class="card">
        <h1>Hearth</h1>
        <p class="muted">围炉夜话 · 低延迟语音聊天室</p>
        <form id="auth-form">
          <input id="username" placeholder="用户名" autocomplete="username" required />
          <input id="password" type="password" placeholder="密码（至少 6 位）" autocomplete="current-password" required />
          <div class="row">
            <button type="submit" data-mode="login" class="primary">登录</button>
            <button type="submit" data-mode="register">注册</button>
          </div>
          <p id="error" class="error"></p>
        </form>
      </div>
    </div>
  `;

  const form = root.querySelector<HTMLFormElement>('#auth-form')!;
  const errEl = root.querySelector<HTMLParagraphElement>('#error')!;
  let mode: 'login' | 'register' = 'login';

  form.querySelectorAll('button').forEach((btn) => {
    btn.addEventListener('click', () => {
      mode = btn.dataset.mode as 'login' | 'register';
    });
  });

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    errEl.textContent = '';
    const username = root.querySelector<HTMLInputElement>('#username')!.value.trim();
    const password = root.querySelector<HTMLInputElement>('#password')!.value;
    try {
      if (mode === 'login') {
        await login(username, password);
      } else {
        await register(username, password);
      }
      location.hash = '#/channels';
    } catch (err) {
      errEl.textContent = (err as Error).message;
    }
  });
}
