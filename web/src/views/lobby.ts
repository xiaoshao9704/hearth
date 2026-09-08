// 大厅：频道卡片、创建频道、设备提示。
import { canInvite, createChannel, fetchMe, getUser, guestTimeLeft, isGuest, listChannels } from '../api';
import type { Channel } from '../api';
import { isSupported, passkeyErrorDetail, registerPasskey } from '../passkey';
import { installMode, isStandalone, onInstallAvailable, promptInstall } from '../install';
import type { InstallMode } from '../install';
import { closeAccountMenu, openAccountMenu } from '../account-menu';
import { renderShell } from '../shell';
import { closeChannelMenu, openChannelMenu } from './room/channel-menu';
import { avatarHtml, esc, icon, menuButtonHtml, slashIcon, toast, wireMenuButton } from '../ui';
import { openSettings } from './settings';

const NAME_RE = /^[A-Za-z0-9_-]{1,64}$/;
const POLL_MS = 15000;

// 通行密钥推荐：登录页留下的一次性标记（register = 刚注册，这次跳过）+ 本会话已弹过 + 本设备的「稍后 / 不再提示」记录
const VIA_KEY = 'hearth_login_via';
const NUDGED_KEY = 'hearth_passkey_nudged';
const NUDGE_KEY = 'hearth_passkey_nudge';
const NUDGE_SNOOZE_MS = 7 * 24 * 3600_000;

// 安装到桌面推荐：本会话已弹过 + 本设备的「稍后 / 不再提示」记录，键名与通行密钥卡分开
const INSTALL_NUDGED_KEY = 'hearth_install_nudged';
const INSTALL_NUDGE_KEY = 'hearth_install_nudge';

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
  const mutedTag = c.muted ? `<span class="tag" title="已静音">${slashIcon('bell', 11, true, 'currentColor')}静音</span>` : '';
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
              ${mutedTag}
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
      <button type="button" class="hit btn btn-icon btn-sm card-menu" data-menu="${esc(c.name)}"
        aria-haspopup="menu" title="频道菜单" aria-label="频道菜单">${icon('more', 14, 'var(--text-1)', 1.7)}</button>
    </div>`;
}

// nudgeAllowed 本设备是否还愿意看这张推荐卡：never = 永久关掉，时间戳 = 7 天内不再提。
// key 区分是哪张卡（通行密钥 / 安装），两张卡各自的「稍后 / 不再提示」互不影响。
function nudgeAllowed(key: string): boolean {
  let mark: string | null = null;
  try {
    mark = localStorage.getItem(key);
  } catch {
    return false; // 存不住偏好就别弹——否则每次登录都弹，反而更烦
  }
  if (mark === 'never') return false;
  const at = Number(mark);
  return !mark || !Number.isFinite(at) || Date.now() - at >= NUDGE_SNOOZE_MS;
}

function setNudge(key: string, v: string) {
  try {
    localStorage.setItem(key, v);
  } catch {
    /* 隐私模式下存不住，下次再问一遍 */
  }
}

// maybeNudgePasskey 进大厅时的推荐卡（非模态，挂在大厅顶部）。返回是否实际画出了卡片
// （安装推荐卡靠这个决定这次要不要一起出现——两张卡同时满足条件时只出通行密钥这张）。
// 不绑定「刚刚密码登录」：升级前就已登录、会话一直存着的人从没触发过登录事件，也该被提醒一次。
// 每个浏览器会话最多弹一次；刚注册完的人正在建账号，那一次跳过；账号已有通行密钥、访客、
// 浏览器不支持、本设备关掉过（稍后 7 天 / 不再提示）都不画。
async function maybeNudgePasskey(host: HTMLElement, alive: () => boolean): Promise<boolean> {
  const via = sessionStorage.getItem(VIA_KEY);
  sessionStorage.removeItem(VIA_KEY);
  if (via === 'register') return false;
  let shown = false;
  try {
    shown = sessionStorage.getItem(NUDGED_KEY) === '1';
  } catch {
    return false;
  }
  if (shown || !isSupported() || !nudgeAllowed(NUDGE_KEY)) return false;
  let me;
  try {
    me = await fetchMe(); // passkey_count 只有 /api/me 带
  } catch {
    return false;
  }
  if (!alive() || isGuest(me) || (me.passkey_count ?? 0) > 0) return false;
  try {
    sessionStorage.setItem(NUDGED_KEY, '1');
  } catch {
    /* 存不住就可能下次再弹一次，可接受 */
  }

  host.innerHTML = `
    <div class="card nudge-card">
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
      const msg = passkeyErrorDetail(err); // 明确点了「立即添加」：连没完成也说清原因
      if (msg) toast(msg, 'bad');
      addBtn.classList.remove('loading');
    }
  });
  host.querySelector('#nudge-later')!.addEventListener('click', () => {
    setNudge(NUDGE_KEY, String(Date.now()));
    host.innerHTML = '';
  });
  host.querySelector('#nudge-never')!.addEventListener('click', () => {
    setNudge(NUDGE_KEY, 'never');
    host.innerHTML = '';
  });
  return true;
}

// installMode → 推荐卡文案；prompt 模式带一个能直接触发系统安装对话框的主按钮，
// 其余模式（iOS / macOS Safari / 其他浏览器）没有编程接口，只能给操作说明。
const INSTALL_COPY: Record<'prompt' | 'ios' | 'mac-safari' | 'manual', { title: string; body: string; primary?: string }> = {
  prompt: { title: '安装到桌面', body: '像应用一样独立打开，带图标和角标，通知更可靠。', primary: '安装' },
  ios: { title: '添加到主屏幕', body: 'Safari 里点底部「分享」→「添加到主屏幕」。装好后从主屏幕打开，才能收到通知。' },
  'mac-safari': { title: '安装到程序坞', body: 'Safari 菜单「文件」→「添加到程序坞」。' },
  manual: { title: '安装到桌面', body: '在浏览器菜单里找「安装」或「添加到主屏幕」。' },
};

// maybeNudgeInstall 进大厅时的安装推荐卡，挂在通行密钥卡下面。条件：非独立窗口运行、
// 当前平台有安装路径（installMode() !== 'none'）、本设备没关掉过、本会话没弹过。
async function maybeNudgeInstall(host: HTMLElement, alive: () => boolean): Promise<boolean> {
  let shown = false;
  try {
    shown = sessionStorage.getItem(INSTALL_NUDGED_KEY) === '1';
  } catch {
    return false;
  }
  if (shown || isStandalone() || !nudgeAllowed(INSTALL_NUDGE_KEY)) return false;
  const mode = installMode();
  if (mode === 'none') return false;
  if (!alive()) return false;
  try {
    sessionStorage.setItem(INSTALL_NUDGED_KEY, '1');
  } catch {
    /* 存不住就可能下次再弹一次，可接受 */
  }

  // 桌面 Chrome / Edge 的 beforeinstallprompt 常在大厅画完之后才到：先按当前模式画，
  // 事件到了且卡片还在就换成带「安装」按钮的形态
  const paint = (m: Exclude<InstallMode, 'none'>) => {
    const copy = INSTALL_COPY[m];
    host.innerHTML = `
      <div class="card nudge-card">
        <span class="nudge-icon">${icon('install', 17, 'var(--ember)', 1.7)}</span>
        <div style="flex-grow:1;min-width:0">
          <div style="font-size:13px;font-weight:600">${esc(copy.title)}</div>
          <div style="font-size:11.5px;line-height:1.6;color:var(--text-2);margin-top:3px;text-wrap:pretty">
            ${esc(copy.body)}
          </div>
        </div>
        <div class="nudge-acts">
          ${copy.primary ? `<button type="button" class="hit btn btn-primary btn-sm" id="install-nudge-do">${esc(copy.primary)}</button>` : ''}
          <button type="button" class="hit btn btn-sm" id="install-nudge-later">${copy.primary ? '稍后' : '知道了'}</button>
          <button type="button" class="hit btn btn-sm" id="install-nudge-never">不再提示</button>
        </div>
      </div>`;

    host.querySelector('#install-nudge-do')?.addEventListener('click', async () => {
      const outcome = await promptInstall();
      if (outcome === 'accepted') toast('已安装到桌面。', 'ok');
      else if (outcome === 'dismissed') setNudge(INSTALL_NUDGE_KEY, String(Date.now()));
      host.innerHTML = ''; // unavailable（事件已用掉）也没有别的动作可做，一并收起
    });
    host.querySelector('#install-nudge-later')!.addEventListener('click', () => {
      setNudge(INSTALL_NUDGE_KEY, String(Date.now()));
      host.innerHTML = '';
    });
    host.querySelector('#install-nudge-never')!.addEventListener('click', () => {
      setNudge(INSTALL_NUDGE_KEY, 'never');
      host.innerHTML = '';
    });
  };
  paint(mode);
  if (mode !== 'prompt') {
    onInstallAvailable(() => {
      if (alive() && host.innerHTML !== '') paint('prompt');
    });
  }
  return true;
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
      <!-- 账户入口：手机上侧栏是抽屉，顶栏这个小头像才是能看见的入口 -->
      <button type="button" class="hit acct-entry" id="acct-entry" aria-haspopup="menu" title="账户" aria-label="账户">${avatarHtml(user?.username ?? '?', 'avatar avatar-sm')}</button>
    </header>
    <div class="lobby-body">
      <div id="passkey-nudge"></div>
      <div id="install-nudge"></div>
      ${
        isGuest(user)
          ? user?.can_claim
            ? `<button type="button" class="hit card" id="guest-bar" style="display:flex;align-items:center;gap:11px;padding:12px 16px;border-color:var(--ember-line);text-align:left;width:100%">
        <span style="flex-shrink:0">${icon('user', 16, 'var(--ember)', 1.7)}</span>
        <span style="flex-grow:1;font-size:12.5px;line-height:1.6;color:var(--text-1);text-wrap:pretty">你正以访客身份使用，${esc(guestTimeLeft(user))}。<span style="color:var(--ember)">注册以保留身份</span>——user_id 不变，聊天记录和频道里的位置都留下。</span>
      </button>`
            : // 站点没开访客转正：只报剩余时间，不给引导也不点开账号页（那里也没有表单）
              `<div class="card" id="guest-bar" style="display:flex;align-items:center;gap:11px;padding:12px 16px;border-color:var(--ember-line)">
        <span style="flex-shrink:0">${icon('user', 16, 'var(--ember)', 1.7)}</span>
        <span style="flex-grow:1;font-size:12.5px;line-height:1.6;color:var(--text-1);text-wrap:pretty">你正以访客身份使用，${esc(guestTimeLeft(user))}。</span>
      </div>`
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

  // 菜单里改完静音会派这个事件：卡片立刻重画，不等 15 秒轮询（状态仍只从服务端来）
  const onChannelsChanged = () => void paint();
  window.addEventListener('hearth:channels', onChannelsChanged);

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
    window.removeEventListener('hearth:channels', onChannelsChanged);
    closeChannelMenu(); // 两个浮层都挂在 body 上，切页不会带走它们
    closeAccountMenu();
    shell.destroy();
    unwireMenu();
  };
  window.addEventListener('hashchange', onLeave, { once: true });

  // 通行密钥优先：这次弹了就不再看安装卡，安装卡留到下次会话（各自的「稍后/不再提示」互不影响）
  void (async () => {
    const shownPasskey = await maybeNudgePasskey(root.querySelector<HTMLElement>('#passkey-nudge')!, alive);
    if (!shownPasskey) await maybeNudgeInstall(root.querySelector<HTMLElement>('#install-nudge')!, alive);
  })();

  const acctEntry = root.querySelector<HTMLButtonElement>('#acct-entry')!;
  acctEntry.addEventListener('click', () => openAccountMenu(acctEntry));

  root.querySelector('#tune-av')!.addEventListener('click', () => openSettings('av'));
  if (user?.can_claim) root.querySelector('#guest-bar')?.addEventListener('click', () => openSettings('account'));

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
    // 卡片菜单：与房间顶栏同一份（大厅没有房间上下文，不给推流地址与离开两项）
    cardsEl.querySelectorAll<HTMLButtonElement>('[data-menu]').forEach((btn) => {
      btn.addEventListener('click', (ev) => {
        ev.stopPropagation();
        ev.preventDefault();
        const c = channels.find((x) => x.name === btn.dataset.menu);
        if (!c) return;
        openChannelMenu(btn, c.name, { muted: c.muted, myRole: c.my_role });
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
