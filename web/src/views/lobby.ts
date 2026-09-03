// 大厅：频道卡片、创建频道、设备提示。
import { createChannel, getUser, listChannels } from '../api';
import { menuButtonHtml, renderShell, wireMenuButton } from '../shell';
import { esc, icon, toast } from '../ui';
import { openSettings } from './settings';

function greeting(): string {
  const h = new Date().getHours();
  if (h < 5) return '夜深了';
  if (h < 11) return '早上好';
  if (h < 14) return '中午好';
  if (h < 18) return '下午好';
  return '晚上好';
}

export async function renderLobby(root: HTMLElement) {
  const user = getUser();
  const shell = renderShell(root, {});
  shell.setConn(false, '进入频道后自动协商');

  shell.content.innerHTML = `
    <header class="topbar">
      ${menuButtonHtml()}
      <h1 id="greet">${greeting()}，${esc(user?.username ?? '')}</h1>
      <div class="spacer"></div>
      ${user?.is_admin ? `<a class="hit btn btn-sm" href="#/admin">${icon('shield', 14, 'var(--text-1)', 1.6)} 管理后台</a>` : ''}
      <div class="status-chip mono"><span style="display:flex;align-items:center;gap:5px"><span class="ok-dot"></span>服务器在线</span></div>
    </header>
    <div class="lobby-body">
      <div class="lobby-title">
        <div class="big">还没进频道</div>
        <div class="sub">挑一个进去，或者等人来找你</div>
      </div>
      <div class="channel-cards" id="cards"><div class="muted">加载中…</div></div>
      <form class="card" id="create-form" style="display:flex;align-items:center;gap:10px;padding:12px 14px">
        <span style="flex-shrink:0">${icon('plus', 16, 'var(--text-2)', 1.8)}</span>
        <input id="new-name" placeholder="新频道名（字母数字 - _）" autocomplete="off"
          style="flex-grow:1;min-width:0;background:transparent;border:0;outline:0;font:inherit;font-size:13.5px;color:var(--text-0)" />
        <button type="submit" class="hit btn btn-primary btn-sm">创建频道</button>
      </form>
      <div class="spacer"></div>
      <div class="lobby-foot">
        <span style="flex-shrink:0">${icon('mic', 20, 'var(--text-3)', 1.5)}</span>
        <span style="flex-grow:1">还没进任何频道，所以麦克风和投屏都没开。进去之后底部会出现控制栏。</span>
        <button class="hit btn btn-sm" id="tune-av" style="flex-shrink:0">${icon('mic', 14, 'var(--text-1)', 1.7)} 先调设备</button>
      </div>
    </div>
  `;
  wireMenuButton(root);

  const greetEl = root.querySelector<HTMLHeadingElement>('#greet')!;
  const onUser = () => {
    greetEl.textContent = `${greeting()}，${getUser()?.username ?? ''}`;
  };
  window.addEventListener('hearth:user', onUser);

  root.querySelector('#tune-av')!.addEventListener('click', () => openSettings('av'));

  const cardsEl = root.querySelector<HTMLDivElement>('#cards')!;

  function statusOf(online: number, inviteOnly: boolean): string {
    if (online <= 0) return inviteOnly ? '邀请制 · 现在没人' : '空着，进去就是你的';
    if (online === 1) return '1 个人在里面';
    return `${online} 个人在聊`;
  }

  async function paint() {
    let channels;
    try {
      channels = await listChannels();
    } catch (err) {
      cardsEl.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
      return;
    }
    if (channels.length === 0) {
      cardsEl.innerHTML = '<div class="muted">还没有频道，先在下面创建一个。</div>';
      return;
    }
    cardsEl.innerHTML = channels
      .map((c) => {
        const busy = c.online > 0;
        return `
        <div class="hit channel-card" data-join="${esc(c.name)}">
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
        </div>`;
      })
      .join('');
    cardsEl.querySelectorAll<HTMLElement>('[data-join]').forEach((el) => {
      el.addEventListener('click', () => {
        location.hash = `#/room/${encodeURIComponent(el.dataset.join!)}`;
      });
    });
  }
  await paint();

  root.querySelector('#create-form')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const input = root.querySelector<HTMLInputElement>('#new-name')!;
    const name = input.value.trim();
    if (!name) return;
    try {
      const ch = await createChannel(name);
      location.hash = `#/room/${encodeURIComponent(ch.name)}`;
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  });

  // 大厅停留时每 15 秒刷新在线人数
  const timer = setInterval(() => {
    if (!root.isConnected || !isLobbyHash()) {
      clearInterval(timer);
      shell.destroy();
      window.removeEventListener('hearth:user', onUser);
      return;
    }
    void paint();
    void shell.refreshChannels();
  }, 15000);
}

function isLobbyHash(): boolean {
  const h = location.hash;
  return h === '' || h === '#/' || h === '#/lobby' || h === '#/channels';
}
