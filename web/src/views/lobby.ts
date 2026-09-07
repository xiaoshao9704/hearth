// 大厅：频道卡片、创建频道、设备提示。
import { canInvite, createChannel, fetchMe, getUser, guestTimeLeft, isGuest, listChannels } from '../api';
import type { Channel } from '../api';
import { isSupported, passkeyErrorText, registerPasskey } from '../passkey';
import { renderShell } from '../shell';
import { esc, icon, menuButtonHtml, toast, wireMenuButton } from '../ui';
import { openSettings } from './settings';

const NAME_RE = /^[A-Za-z0-9_-]{1,64}$/;
const POLL_MS = 15000;

// 通行密钥推荐：登录页留下的一次性标记 + 本设备的「稍后 / 不再提示」记录
const VIA_KEY = 'hearth_login_via';
const NUDGE_KEY = 'hearth_passkey_nudge';
const NUDGE_SNOOZE_MS = 7 * 24 * 3600_000;

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
  const banned = !!c.banned;
  const busy = c.online > 0;
  const roleTag =
    c.my_role === 'owner' ? '<span class="tag">我的频道</span>' : c.my_role === 'moderator' ? '<span class="tag">我管理的</span>' : '';
  const hiddenTag = c.hidden ? '<span class="tag tag-red">隐藏中</span>' : '';
  const canManage = c.my_role === 'owner' || c.my_role === 'moderator';
  // 被封禁：卡片不可进入（用 div 而非 a，不给出可点的 href），状态与按钮都改成封禁提示
  const tag = banned ? 'div' : 'a';
  const hrefAttr = banned ? '' : ` href="#/room/${encodeURIComponent(c.name)}"`;
  const statusHtml = banned
    ? '<span style="color:var(--red-text)">已被封禁</span>'
    : statusOf(c.online, c.invite_only);
  return `
    <div class="channel-card-wrap">
      <${tag} class="hit channel-card${banned ? ' disabled' : ''}"${hrefAttr}${banned ? ' aria-disabled="true"' : ''}>
        <div class="head">
          <div class="icon-wrap">${icon('volume', 18, busy ? 'var(--ember)' : 'var(--text-1)', 1.7)}</div>
          <div style="flex-grow:1;min-width:0">
            <div style="display:flex;align-items:center;gap:8px">
              <span class="name">${esc(c.name)}</span>
              ${c.invite_only ? '<span class="tag tag-ember">邀请制</span>' : ''}
              ${hiddenTag}
              ${roleTag}
            </div>
            <div class="status ${busy ? 'busy' : ''}">${statusHtml}</div>
          </div>
        </div>
        <div class="foot">
          ${busy && !banned ? `<span class="mono" style="font-size:11px;color:var(--ember)">${c.online} 人在线</span>` : ''}
          <div class="spacer"></div>
          <div class="join-btn">${icon('back', 15, 'var(--on-ember)', 1.8)}<span>${banned ? '无法进入' : '加入'}</span></div>
        </div>
      </${tag}>
      ${canManage ? `<button type="button" class="hit btn btn-icon btn-sm card-gear" data-gear="${esc(c.name)}" title="频道管理" aria-label="频道管理">${icon('gear', 14, 'var(--text-1)', 1.7)}</button>` : ''}
    </div>`;
}

// nudgeAllowed 本设备是否还愿意看这张推荐卡：never = 永久关掉，时间戳 = 7 天内不再提。
function nudgeAllowed(): boolean {
  let mark: string | null = null;
  try {
    mark = localStorage.getItem(NUDGE_KEY);
  } catch {
    return false; // 存不住偏好就别弹——否则每次登录都弹，反而更烦
  }
  if (mark === 'never') return false;
  const at = Number(mark);
  return !mark || !Number.isFinite(at) || Date.now() - at >= NUDGE_SNOOZE_MS;
}

function setNudge(v: string) {
  try {
    localStorage.setItem(NUDGE_KEY, v);
  } catch {
    /* 隐私模式下存不住，下次再问一遍 */
  }
}

// maybeNudgePasskey 密码登录后的一次性推荐卡（非模态，挂在大厅顶部）。
// 四个条件都满足才画：这次是密码登录（标记读后即删，保证只在登录后那一次）、
// 账号还没有通行密钥、不是访客、这台浏览器支持且没被关掉过。
async function maybeNudgePasskey(host: HTMLElement, alive: () => boolean) {
  const via = sessionStorage.getItem(VIA_KEY);
  sessionStorage.removeItem(VIA_KEY);
  if (via !== 'password' || !isSupported() || !nudgeAllowed()) return;
  let me;
  try {
    me = await fetchMe(); // passkey_count 只有 /api/me 带
  } catch {
    return;
  }
  if (!alive() || isGuest(me) || (me.passkey_count ?? 0) > 0) return;

  host.innerHTML = `
    <div class="card passkey-nudge">
      <span class="nudge-icon">${icon('key', 17, 'var(--ember)', 1.7)}</span>
      <div style="flex-grow:1;min-width:0">
        <div style="font-size:13px;font-weight:600">下次一键登录</div>
        <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);margin-top:3px;text-wrap:pretty">
          给这个账号加一枚通行密钥：以后用指纹/面容/设备密码就能进，不用再输密码。密码仍然保留。
        </div>
      </div>
      <div class="nudge-acts">
        <button type="button" class="hit btn btn-primary btn-sm" id="nudge-add">立即添加</button>
        <button type="button" class="hit btn btn-sm" id="nudge-later">稍后</button>
        <button type="button" class="hit btn btn-sm" id="nudge-never">不再提示</button>
      </div>
    </div>`;

  const addBtn = host.querySelector<HTMLButtonElement>('#nudge-add')!;
  addBtn.addEventListener('click', async () => {
    if (addBtn.classList.contains('loading')) return;
    addBtn.classList.add('loading');
    try {
      await registerPasskey();
      toast('通行密钥已添加，下次登录一键就进。', 'ok');
      host.innerHTML = '';
    } catch (err) {
      const msg = passkeyErrorText(err);
      if (msg) toast(msg, 'bad');
      addBtn.classList.remove('loading');
    }
  });
  host.querySelector('#nudge-later')!.addEventListener('click', () => {
    setNudge(String(Date.now()));
    host.innerHTML = '';
  });
  host.querySelector('#nudge-never')!.addEventListener('click', () => {
    setNudge('never');
    host.innerHTML = '';
  });
}

export async function renderLobby(root: HTMLElement, alive: () => boolean) {
  const user = getUser();
  // 创建频道需 power 及以上（服务端 POST /api/channels 也会拒，这里只做显隐）
  const canCreate = canInvite(user);
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
      <div id="passkey-nudge"></div>
      ${
        isGuest(user)
          ? `<button type="button" class="hit card" id="guest-bar" style="display:flex;align-items:center;gap:11px;padding:12px 16px;border-color:var(--ember-line);text-align:left;width:100%">
        <span style="flex-shrink:0">${icon('user', 16, 'var(--ember)', 1.7)}</span>
        <span style="flex-grow:1;font-size:12.5px;line-height:1.6;color:var(--text-1);text-wrap:pretty">你正以访客身份使用，${esc(guestTimeLeft(user))}。<span style="color:var(--ember)">注册以保留身份</span>——user_id 不变，聊天记录和频道里的位置都留下。</span>
      </button>`
          : ''
      }
      <div class="lobby-title">
        <div class="big">还没进频道</div>
        <div class="sub">挑一个进去，或者等人来找你</div>
      </div>
      <div class="channel-cards" id="cards"><div class="muted">加载中…</div></div>
      ${
        canCreate
          ? `<form class="card" id="create-form" style="display:flex;align-items:center;gap:10px;padding:12px 14px">
        <span style="flex-shrink:0">${icon('plus', 16, 'var(--text-2)', 1.8)}</span>
        <input id="new-name" placeholder="新频道名（字母数字 - _）" autocomplete="off" maxlength="64"
          style="flex-grow:1;min-width:0;background:transparent;border:0;outline:0;font:inherit;font-size:13.5px;color:var(--text-0)" />
        <button type="submit" class="hit btn btn-primary btn-sm" id="create-btn">创建频道</button>
      </form>`
          : ''
      }
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
    const u = getUser();
    greetEl.textContent = `${greeting()}，${u?.username ?? ''}`;
    // 转正后提示条要立刻消失（浮层里改完就派了 hearth:user）
    const bar = root.querySelector<HTMLElement>('#guest-bar');
    if (bar && !isGuest(u)) bar.remove();
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

  void maybeNudgePasskey(root.querySelector<HTMLElement>('#passkey-nudge')!, alive);

  root.querySelector('#tune-av')!.addEventListener('click', () => openSettings('av'));
  root.querySelector('#guest-bar')?.addEventListener('click', () => openSettings('account'));

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
      cardsEl.innerHTML = canCreate
        ? `
        <div class="state-block">
          <span>还没有频道，先创建一个吧</span>
          <button type="button" class="hit btn btn-sm" id="cards-empty-create">创建一个</button>
        </div>`
        : `<div class="state-block"><span>还没有频道</span></div>`;
      cardsEl.querySelector('#cards-empty-create')?.addEventListener('click', () => {
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
  // 非 power 用户没有创建表单，整段跳过
  const input = root.querySelector<HTMLInputElement>('#new-name');
  const createBtn = root.querySelector<HTMLButtonElement>('#create-btn');
  if (canCreate && input && createBtn) {
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
  }

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
