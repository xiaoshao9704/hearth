// 频道列表页：展示常驻频道，可创建新频道。
import { createChannel, getUser, listChannels, logout } from '../api';

export async function renderChannels(root: HTMLElement) {
  const user = getUser();
  root.innerHTML = `
    <div class="page">
      <header class="topbar">
        <h1>Hearth</h1>
        <div class="row">
          <span class="muted">${user?.username ?? ''}</span>
          <button id="logout">退出登录</button>
        </div>
      </header>
      <section class="card">
        <h2>频道</h2>
        <form id="create-form" class="row">
          <input id="channel-name" placeholder="新频道名（字母数字 - _）" required />
          <button type="submit" class="primary">创建</button>
        </form>
        <p id="error" class="error"></p>
        <ul id="channel-list" class="channel-list"><li class="muted">加载中…</li></ul>
      </section>
    </div>
  `;

  root.querySelector('#logout')!.addEventListener('click', async () => {
    await logout();
    location.hash = '#/login';
  });

  const errEl = root.querySelector<HTMLParagraphElement>('#error')!;
  root.querySelector('#create-form')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    errEl.textContent = '';
    const input = root.querySelector<HTMLInputElement>('#channel-name')!;
    try {
      const ch = await createChannel(input.value.trim());
      location.hash = `#/room/${encodeURIComponent(ch.name)}`;
    } catch (err) {
      errEl.textContent = (err as Error).message;
    }
  });

  const listEl = root.querySelector<HTMLUListElement>('#channel-list')!;
  try {
    const channels = await listChannels();
    listEl.innerHTML = channels.length
      ? channels
          .map(
            (c) => `
          <li>
            <a href="#/room/${encodeURIComponent(c.name)}"># ${c.name}</a>
            <span class="muted">${c.invite_only ? '🔒 ' : ''}${c.created_by} 创建</span>
          </li>`,
          )
          .join('')
      : '<li class="muted">还没有频道，创建一个吧</li>';
  } catch (err) {
    listEl.innerHTML = `<li class="error">${(err as Error).message}</li>`;
  }
}
