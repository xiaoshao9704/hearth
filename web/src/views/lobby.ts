// 大厅：频道卡片、创建频道、设备提示。
import { createChannel, getUser, listChannels } from '../api';
import type { Channel } from '../api';
import { renderShell } from '../shell';
import { esc, icon, menuButtonHtml, toast, wireMenuButton } from '../ui';
import { openSettings } from './settings';

const NAME_RE = /^[A-Za-z0-9_-]{1,64}$/;
const POLL_MS = 15000;

function greeting(): string {
  const h = new Date().getHours();
  if (h < 5) return '夜深了';
  if (h < 11) return '早上好';
  if (h < 14) return '中午好';
  if (h < 18) return '下午好';
  return '晚上好';
}

// main.ts 拿它判断一个 hash 是不是「大厅形状」，决定未知 hash 要不要回落大厅
export function isLobbyHash(hash: string): boolean {
  return hash === '' || hash === '#/' || hash === '#/lobby' || hash === '#/channels';
}

function statusOf(online: number, inviteOnly: boolean): string {
  if (online <= 0) return inviteOnly ? '邀请制 · 现在没人' : '空着，进去就是你的';
  if (online === 1) return '1 个人在里面';
  return `${online} 个人在聊`;
}

function cardHtml(c: Channel): string {
  const busy = c.online > 0;
  return `
    <div class="channel-card-wrap">
      <a class="hit channel-card" href="#/room/${encodeURIComponent(c.name)}">
        <div class="head">
          <div class="icon-wrap">${icon('volume', 18, busy ? 'var(--ember)' : 'var(--text-1)', 1.7)}</div>
          <div style="flex-grow:1;min-width:0">
            <div style="display:flex;align-items:center;gap:8px">
              <span class="name">${esc(c.name)}</span>
              ${c.invite_only ? '<span class="tag tag-ember">邀请制</span>' : ''}
              ${c.is_owner ? '<span class="tag">我的频道</span>' : ''}
            </div>
            <div class="status ${busy ? 'busy' : ''}">${statusOf(c.online, c.invite_only)}</div>
          </div>
        </div>
        <div class="foot">
          ${busy ? `<span class="mono" style="font-size:11px;color:var(--ember)">${c.online} 人在线</span>` : ''}
          <div class="spacer"></div>
          <div class="join-btn">${icon('back', 15, 'var(--on-ember)', 1.8)}<span>加入</span></div>
        </div>
      </a>
      ${c.is_owner ? `<button type="button" class="hit btn btn-icon btn-sm card-gear" data-gear="${esc(c.name)}" title="频道管理" aria-label="频道管理">${icon('gear', 14, 'var(--text-1)', 1.7)}</button>` : ''}
    </div>`;
}

export async function renderLobby(root: HTMLElement, alive: () => boolean) {
  const user = getUser();
  const shell = renderShell(root, {});
  shell.setConn(false, '进入频道后自动协商');

  shell.content.innerHTML = `
    <header class="topbar">
      ${menuButtonHtml()}
      <h1 id="greet">${greeting()}，${esc(user?.username ?? '')}</h1>
      <div class="spacer"></div>
      ${user?.is_admin ? `<a class="hit btn btn-sm" href="#/admin">${icon('shield', 14, 'var(--text-1)', 1.6)} 管理后台</a>` : ''}
      <div class="status-chip mono" id="status-chip"><span style="display:flex;align-items:center;gap:5px"><span class="ok-dot" id="status-dot"></span><span id="status-text">服务器在线</span></span></div>
    </header>
    <div class="lobby-body">
      <div class="lobby-title">
        <div class="big">还没进频道</div>
        <div class="sub">挑一个进去，或者等人来找你</div>
      </div>
      <div class="channel-cards" id="cards"><div class="muted">加载中…</div></div>
      <form class="card" id="create-form" style="display:flex;align-items:center;gap:10px;padding:12px 14px">
        <span style="flex-shrink:0">${icon('plus', 16, 'var(--text-2)', 1.8)}</span>
        <input id="new-name" placeholder="新频道名（字母数字 - _）" autocomplete="off" maxlength="64"
          style="flex-grow:1;min-width:0;background:transparent;border:0;outline:0;font:inherit;font-size:13.5px;color:var(--text-0)" />
        <button type="submit" class="hit btn btn-primary btn-sm" id="create-btn">创建频道</button>
      </form>
      <div class="spacer"></div>
      <div class="lobby-foot">
        <span style="flex-shrink:0">${icon('mic', 20, 'var(--text-3)', 1.5)}</span>
        <span style="flex-grow:1">还没进任何频道，所以麦克风和投屏都没开。进去之后底部会出现控制栏。</span>
        <button class="hit btn btn-sm" id="tune-av" style="flex-shrink:0">${icon('mic', 14, 'var(--text-1)', 1.7)} 先调设备</button>
      </div>
    </div>
  `;
  const unwireMenu = wireMenuButton(root);

  const greetEl = root.querySelector<HTMLHeadingElement>('#greet')!;
  const onUser = () => {
    greetEl.textContent = `${greeting()}，${getUser()?.username ?? ''}`;
  };
  window.addEventListener('hearth:user', onUser);

  // 轮询相关的变量先占位声明：onLeave 在首次 paint() 还没回来时就可能被 hashchange 触发，
  // 那时 pollTimer/onVisible 还没赋值，占位过的 let 至少不会因 TDZ 直接抛错
  let pollTimer = 0;
  let onVisible = () => {};

  // 路由切走时一次性清理：不猜 hash 形状，hashchange 一响就收（先于任何异步收尾注册，
  // 保证首次 paint() 还没回来就切页也能兜住）
  const onLeave = () => {
    clearInterval(pollTimer);
    document.removeEventListener('visibilitychange', onVisible);
    window.removeEventListener('hearth:user', onUser);
    shell.destroy();
    unwireMenu();
  };
  window.addEventListener('hashchange', onLeave, { once: true });

  root.querySelector('#tune-av')!.addEventListener('click', () => openSettings('av'));

  const statusDot = root.querySelector<HTMLSpanElement>('#status-dot')!;
  const statusText = root.querySelector<HTMLSpanElement>('#status-text')!;
  function setServerStatus(ok: boolean) {
    statusDot.className = ok ? 'ok-dot' : 'bad-dot';
    statusText.textContent = ok ? '服务器在线' : '连不上服务器';
  }

  const cardsEl = root.querySelector<HTMLDivElement>('#cards')!;

  async function paint() {
    let channels: Channel[];
    try {
      channels = await listChannels();
    } catch (err) {
      if (!alive()) return;
      setServerStatus(false);
      cardsEl.innerHTML = `
        <div class="state-block error">
          <span>${esc((err as Error).message)}</span>
          <button type="button" class="hit btn btn-sm" id="cards-retry">重试</button>
        </div>`;
      cardsEl.querySelector('#cards-retry')!.addEventListener('click', () => void paint());
      return;
    }
    if (!alive()) return;
    setServerStatus(true);
    if (channels.length === 0) {
      cardsEl.innerHTML = `
        <div class="state-block">
          <span>还没有频道，先创建一个吧</span>
          <button type="button" class="hit btn btn-sm" id="cards-empty-create">创建一个</button>
        </div>`;
      cardsEl.querySelector('#cards-empty-create')!.addEventListener('click', () => {
        root.querySelector<HTMLInputElement>('#new-name')?.focus();
      });
      return;
    }
    cardsEl.innerHTML = channels.map(cardHtml).join('');
    cardsEl.querySelectorAll<HTMLButtonElement>('[data-gear]').forEach((btn) => {
      btn.addEventListener('click', (ev) => {
        ev.stopPropagation();
        openSettings('channel', { channel: btn.dataset.gear! });
      });
    });
  }

  // 创建频道：前端先做一次正则校验，通过再发请求；进行中禁用按钮防重复提交
  const input = root.querySelector<HTMLInputElement>('#new-name')!;
  const createBtn = root.querySelector<HTMLButtonElement>('#create-btn')!;
  input.addEventListener('input', () => input.classList.remove('input-bad'));
  let creating = false;
  root.querySelector('#create-form')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (creating) return;
    const name = input.value.trim();
    if (!NAME_RE.test(name)) {
      input.classList.add('input-bad');
      toast('频道名只能是字母、数字、- 或 _，最多 64 位', 'bad');
      return;
    }
    creating = true;
    createBtn.classList.add('loading');
    createBtn.disabled = true;
    try {
      const ch = await createChannel(name);
      if (!alive()) return;
      toast('频道已创建', 'ok');
      input.value = '';
      location.hash = `#/room/${encodeURIComponent(ch.name)}`;
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      creating = false;
      createBtn.classList.remove('loading');
      createBtn.disabled = false;
    }
  });

  await paint();
  if (!alive()) return;

  // 大厅停留时轮询在线人数；只在前台跑，回前台立即补一次
  function tick() {
    void paint();
    onUser();
  }
  pollTimer = window.setInterval(() => {
    if (document.visibilityState === 'visible') tick();
  }, POLL_MS);
  onVisible = () => {
    if (document.visibilityState === 'visible') tick();
  };
  document.addEventListener('visibilitychange', onVisible);
}
