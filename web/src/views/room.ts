// 房间页：布局、面板、聊天与断线重连；音视频经 AVEngine 接口驱动。
// 双线模型：语音线（voice，权威名册/说话状态）+ 舞台线（stage：投屏/摄像头/OBS 及其伴音）。
// 两线同一内核时（combined）一条连接承担两种角色；舞台线缺席或断开只禁用投屏/摄像头，语音不受影响。
import { clearSession, fetchJoinCredentials, getUser, kickUser, listChannels } from '../api';
import type { EngineCred } from '../api';
import { connectChat } from '../chat';
import type { ChatMessage } from '../chat';
import { createEngine } from '../engine';
import type { AVEngine, EPart, EngineCallbacks, TrackSource } from '../engine/types';
import { encoderIsHw, loadPrefs, prefsBus, savePrefs } from '../prefs';
import { menuButtonHtml, renderShell, wireMenuButton } from '../shell';
import { avatarHtml, esc, fmtClock, icon, micIcon, slashIcon, toast } from '../ui';
import { openSettings } from './settings';

// iOS Safari 的私有全屏 API（iPhone 仅 video 元素可用）
type IOSVideo = HTMLVideoElement & { webkitEnterFullscreen?: () => void };
type SinkMedia = HTMLMediaElement & { setSinkId?: (id: string) => Promise<void> };

type Role = 'voice' | 'stage';

interface Line {
  role: Role;
  engine: AVEngine | null;
  engineName: string;
  attempts: number;
  timer: number;
  inflight: boolean;
}

export async function renderRoom(root: HTMLElement, channel: string) {
  const prefs = loadPrefs();
  const canScreenShare = typeof navigator.mediaDevices?.getDisplayMedia === 'function';
  let isOwner = false;
  let ownerUsername = '';

  const shell = renderShell(root, { activeChannel: channel });
  shell.setConn(false, '正在协商…');

  shell.content.innerHTML = `
    <div class="room-frame">
      <header class="topbar">
        ${menuButtonHtml()}
        ${icon('volume', 17, 'var(--ember)', 1.6)}
        <h1>${esc(channel)}</h1>
        <div class="vline"></div>
        <span class="sub" id="room-meta">连接中</span>
        <div class="spacer"></div>
        <button class="hit btn btn-icon hidden" id="btn-manage" title="频道管理">${icon('shield', 15, 'var(--text-1)', 1.6)}</button>
        <div class="seg-group" style="padding:3px;background:var(--bg-3)" id="layout-seg">
          <button class="hit seg" data-layout="grid" style="display:flex;align-items:center;gap:6px;padding:5px 10px;font-size:12px">${icon('grid', 14, 'currentColor', 1.7)}<span class="pill-label">九宫格</span></button>
          <button class="hit seg" data-layout="spotlight" style="display:flex;align-items:center;gap:6px;padding:5px 10px;font-size:12px">${icon('focus', 14, 'currentColor', 1.7)}<span class="pill-label">聚焦</span></button>
        </div>
      </header>
      <div style="flex-grow:1;display:flex;min-height:0">
        <div style="flex-grow:1;display:flex;flex-direction:column;min-width:0;min-height:0">
          <div class="stage-area">
            <div class="stage-status" id="status">正在连接…</div>
            <div id="video-grid" class="video-grid ${prefs.layout === 'spotlight' ? 'spotlight' : ''}"><div class="rail" id="rail"></div></div>
            <div id="audio-bin" style="display:none"></div>
          </div>
          <div class="control-bar">
            <div class="group">
              <button class="hit ctl-pill" id="btn-mic"></button>
              <button class="hit ctl-square" id="btn-deaf" title="全体静音（只影响自己）"></button>
              <button class="hit ctl-square" id="btn-camera" title="摄像头"></button>
              <button class="hit ctl-pill ${canScreenShare ? '' : 'hidden'}" id="btn-screen"></button>
              <button class="hit ctl-square" id="btn-quality" title="投屏画质">${icon('sliders', 16, 'var(--text-1)', 1.6)}</button>
            </div>
            <div class="spacer"></div>
            <div class="group">
              <button class="hit ctl-square" id="btn-panel-members" title="成员">${icon('users', 17, 'currentColor', 1.6)}</button>
              <button class="hit ctl-square" id="btn-panel-chat" title="聊天">${icon('chat', 17, 'currentColor')}<span class="unread-badge hidden" id="chat-badge">0</span></button>
              <button class="hit ctl-square" id="btn-settings" title="设置">${icon('gear', 17, 'var(--text-1)')}</button>
              <button class="hit ctl-pill danger" id="btn-leave">${icon('leave', 17, 'var(--red)')}<span class="pill-label">离开房间</span></button>
            </div>
          </div>
        </div>
        <aside class="side-panel hidden" id="members-panel">
          <div class="panel-head"><span style="flex-grow:1">成员</span><span class="mono" style="font-size:11px;color:var(--text-2)" id="members-count"></span>
            <button class="hit mini-btn" id="members-close" style="width:28px;height:28px;border-radius:7px;display:flex;align-items:center;justify-content:center">${icon('close', 15, 'var(--text-2)', 1.8)}</button>
          </div>
          <div class="panel-body" id="members-body"></div>
        </aside>
        <aside class="side-panel chat-panel hidden" id="chat-panel">
          <div class="panel-head">
            <span class="mono" style="font-size:17px;color:var(--text-2);line-height:1">#</span>
            <span style="flex-grow:1">${esc(channel)}</span>
            <button class="hit mini-btn" id="chat-close" style="width:28px;height:28px;border-radius:7px;display:flex;align-items:center;justify-content:center">${icon('close', 15, 'var(--text-2)', 1.8)}</button>
          </div>
          <div class="chat-log" id="chat-log"></div>
          <div class="chat-input-wrap">
            <form class="chat-input-box" id="chat-form">
              <input id="chat-input" placeholder="发消息到 #${esc(channel)}" autocomplete="off" />
              <button type="submit" class="hit send-btn" id="chat-send">${icon('back', 15, 'currentColor', 1.8)}</button>
            </form>
            <div class="chat-hint mono">仅保留最近 50 条历史</div>
          </div>
        </aside>
      </div>
    </div>
  `;
  wireMenuButton(root);

  const statusEl = shell.content.querySelector<HTMLDivElement>('#status')!;
  const gridEl = shell.content.querySelector<HTMLDivElement>('#video-grid')!;
  const railEl = shell.content.querySelector<HTMLDivElement>('#rail')!;
  const audioBin = shell.content.querySelector<HTMLDivElement>('#audio-bin')!;
  const roomMetaEl = shell.content.querySelector<HTMLSpanElement>('#room-meta')!;

  // ---- 状态 ----
  const voiceLine: Line = { role: 'voice', engine: null, engineName: '', attempts: 0, timer: 0, inflight: false };
  let stageLine: Line | null = null; // null = 无舞台线；combined 时与 voiceLine 同引擎
  let combined = false;
  let stageUp = false;
  let leaving = false;
  let bounced = false;
  const tiles = new Map<string, HTMLDivElement>();
  const audioTiles = new Map<string, HTMLDivElement>();
  const audioEls = new Map<string, Set<SinkMedia>>();
  const volumes = new Map<string, number>();
  let deafened = false;
  const speakingByRole: Record<Role, Set<string>> = { voice: new Set(), stage: new Set() };
  let speakingSet = new Set<string>();
  let micOn = prefs.mic;
  let cameraOn = prefs.camera;
  let screenOn = false;
  const myUsername = getUser()?.username ?? '';
  const obsIdentity = `${myUsername}-obs`;

  const stageEngine = () => (combined ? voiceLine.engine : stageLine?.engine) ?? null;

  // 双线参与者合并：语音线是名册权威（micOn），舞台线补充 sharing/OBS
  function parts(): EPart[] {
    const base = voiceLine.engine?.participants() ?? [];
    if (combined || !stageLine?.engine) return base;
    const map = new Map<string, EPart>();
    base.forEach((p) => map.set(p.identity, { ...p }));
    for (const s of stageLine.engine.participants()) {
      const ex = map.get(s.identity);
      if (ex) {
        ex.sharing = ex.sharing || s.sharing;
        ex.obs = ex.obs || s.obs;
      } else {
        map.set(s.identity, { ...s }); // OBS 推流等仅舞台线的参与者
      }
    }
    return [...map.values()];
  }

  const volumeFor = (identity: string) => volumes.get(identity) ?? (identity === obsIdentity ? 0 : 1);

  function applyAudioPrefs() {
    const p = loadPrefs();
    const master = deafened ? 0 : p.volume / 100;
    audioEls.forEach((set, identity) => {
      const v = master * volumeFor(identity);
      set.forEach((el) => {
        el.volume = v;
        if (p.speakerId && typeof el.setSinkId === 'function') {
          el.setSinkId(p.speakerId).catch(() => {});
        }
      });
    });
  }

  // ---- 聚焦布局：投屏 > pin > 发言人 > 第一块 ----
  let pinnedKey: string | null = null;
  let lastSpeaker: string | null = null;

  function allTiles(): Map<string, HTMLDivElement> {
    const merged = new Map<string, HTMLDivElement>(tiles);
    audioTiles.forEach((el, identity) => merged.set(`${identity}:audio-tile`, el));
    return merged;
  }

  function pickFocusKey(): string | null {
    const merged = allTiles();
    if (merged.size === 0) return null;
    for (const key of merged.keys()) {
      if (key.endsWith(':screen')) return key;
    }
    if (pinnedKey && merged.has(pinnedKey)) return pinnedKey;
    if (lastSpeaker) {
      for (const cand of [`${lastSpeaker}:camera`, `${lastSpeaker}:audio-tile`]) {
        if (merged.has(cand)) return cand;
      }
    }
    return merged.keys().next().value ?? null;
  }

  function applyLayout() {
    const spotlight = loadPrefs().layout === 'spotlight';
    gridEl.dataset.tiles = String(allTiles().size);
    gridEl.classList.toggle('spotlight', spotlight);
    shell.content.querySelectorAll<HTMLButtonElement>('[data-layout]').forEach((b) => {
      b.classList.toggle('on', (b.dataset.layout === 'spotlight') === spotlight);
    });
    const merged = allTiles();
    if (!spotlight) {
      merged.forEach((tile) => {
        tile.classList.remove('featured');
        if (tile.parentElement !== gridEl) gridEl.insertBefore(tile, railEl);
      });
      return;
    }
    const focusKey = pickFocusKey();
    merged.forEach((tile, key) => {
      const isFocus = key === focusKey;
      tile.classList.toggle('featured', isFocus);
      const target = isFocus ? gridEl : railEl;
      if (tile.parentElement !== target) {
        if (isFocus) gridEl.insertBefore(tile, railEl);
        else railEl.appendChild(tile);
      }
    });
  }

  function togglePin(key: string) {
    pinnedKey = pinnedKey === key ? null : key;
    allTiles().forEach((tile, k) => tile.classList.toggle('pinned', k === pinnedKey));
    applyLayout();
  }

  // ---- 视频卡片 ----

  function addVideoTile(part: EPart, source: TrackSource, video: HTMLVideoElement) {
    const key = `${part.identity}:${source}`;
    if (tiles.has(key)) return;
    const tile = document.createElement('div');
    tile.className = 'tile' + (source === 'screen' ? ' tile-screen' : '');
    if (part.isLocal && source === 'camera' && loadPrefs().mirror) video.style.transform = 'scaleX(-1)';

    const label = document.createElement('div');
    label.className = 'tile-label';
    const name = part.isLocal && source === 'camera' ? '你' : part.display;
    label.innerHTML = `${avatarHtml(part.username, 'avatar avatar-sm')}<span>${esc(name)}${source === 'screen' ? '的投屏' : ''}</span>`;

    const badges = document.createElement('div');
    badges.className = 'tile-badges';
    if (source === 'screen') {
      const p = loadPrefs();
      const spec = part.isLocal
        ? `${p.res} · ${p.fps}fps · ${p.bitrate.toFixed(1)}M · ${p.screenCodec === 'h264' ? 'H.264 单层' : p.screenCodec.toUpperCase() + ' SVC'}`
        : '';
      badges.innerHTML = `<div class="live-badge">LIVE</div>${spec ? `<div class="spec-badge mono">${spec}</div>` : ''}`;
      if (part.isLocal) {
        // 追加实际生效的编码器（getStats 真值）：硬编/软编，编码器降级时跟着变
        const specEl = badges.querySelector<HTMLElement>('.spec-badge');
        const refreshEnc = async () => {
          if (!tiles.has(key)) {
            clearInterval(encTimer);
            return;
          }
          const info = await stageEngine()?.screenEncoderInfo();
          if (!info || !specEl) return;
          const hw = encoderIsHw(info);
          const tag = hw === true ? '硬编' : hw === false ? '软编' : info.impl;
          specEl.textContent = `${spec} · ${tag}`;
        };
        const encTimer = window.setInterval(() => void refreshEnc(), 10000);
        setTimeout(() => void refreshEnc(), 3000);
      }
    }
    if (part.obs) badges.innerHTML += '<div class="spec-badge">OBS · WHIP</div>';

    const pinIcon = document.createElement('span');
    pinIcon.className = 'tile-pin';
    pinIcon.textContent = '📌';

    tile.addEventListener('click', (ev) => {
      if ((ev.target as HTMLElement).closest('.tile-fs')) return;
      togglePin(key);
    });

    const fsBtn = document.createElement('button');
    fsBtn.className = 'tile-fs hit';
    fsBtn.textContent = '⛶';
    fsBtn.title = '全屏';
    const iv = video as IOSVideo;
    if (typeof iv.requestFullscreen !== 'function' && typeof iv.webkitEnterFullscreen !== 'function') {
      fsBtn.hidden = true;
    }
    fsBtn.addEventListener('click', () => {
      if (typeof iv.requestFullscreen === 'function') void iv.requestFullscreen();
      else iv.webkitEnterFullscreen?.();
    });

    tile.append(video, badges, label, pinIcon, fsBtn);
    gridEl.insertBefore(tile, railEl);
    tiles.set(key, tile);
    syncAudioTiles();
    refreshMeta();
  }

  function removeVideoTile(identity: string, source: TrackSource, els: HTMLMediaElement[]) {
    els.forEach((el) => el.remove());
    const key = `${identity}:${source}`;
    const tile = tiles.get(key);
    if (tile) {
      tile.remove();
      tiles.delete(key);
      if (pinnedKey === key) pinnedKey = null;
      syncAudioTiles();
      refreshMeta();
    }
  }

  // 无视频参与者渲染音频块
  function syncAudioTiles() {
    const ps = parts();
    const want = new Set<string>();
    for (const p of ps) {
      const hasVideo = tiles.has(`${p.identity}:camera`) || tiles.has(`${p.identity}:screen`);
      if (!hasVideo) want.add(p.identity);
    }
    for (const [identity, el] of audioTiles) {
      if (!want.has(identity)) {
        el.remove();
        audioTiles.delete(identity);
        if (pinnedKey === `${identity}:audio-tile`) pinnedKey = null;
      }
    }
    for (const p of ps) {
      if (!want.has(p.identity)) continue;
      let el = audioTiles.get(p.identity);
      if (!el) {
        el = document.createElement('div');
        el.className = 'tile tile-audio';
        el.addEventListener('click', () => togglePin(`${p.identity}:audio-tile`));
        gridEl.insertBefore(el, railEl);
        audioTiles.set(p.identity, el);
      }
      const speaking = speakingSet.has(p.identity);
      el.classList.toggle('speaking', speaking);
      el.classList.toggle('muted', !p.micOn && !p.isLocal && !p.obs);
      el.innerHTML = `
        ${avatarHtml(p.username, 'avatar avatar-xl' + (speaking ? ' speaking' : ''))}
        <div class="a-name">
          <span>${esc(p.isLocal ? '你' : p.display)}</span>
          ${micIcon(13, !p.micOn && !p.obs, !p.micOn && !p.obs ? 'var(--red)' : speaking ? 'var(--ember)' : 'var(--text-2)')}
        </div>
        ${p.obs ? '<div style="position:absolute;top:12px;right:12px" class="tag tag-ember mono">OBS 推流</div>' : ''}`;
    }
    if (voiceLine.engine?.connected()) statusEl.textContent = '';
    applyLayout();
  }

  // ---- 用户操作菜单（聊天卡片与成员行共用）----
  function showUserMenu(x: number, y: number, username: string) {
    document.querySelector('.user-menu')?.remove();
    if (username === myUsername) return;
    const targets = parts().filter((p) => !p.isLocal && p.username === username);
    const muted = targets.some((p) => volumeFor(p.identity) === 0);
    const menu = document.createElement('div');
    menu.className = 'user-menu';
    menu.innerHTML = `
      <div class="um-title">${esc(username)}</div>
      ${targets.length ? `<button class="hit um-item" data-act="mute">${slashIcon('volume', 14, !muted, 'currentColor')}<span>${muted ? '恢复声音' : '屏蔽声音'}</span></button>` : ''}
      ${isOwner ? `<button class="hit um-item danger" data-act="kick">${icon('leave', 14, 'var(--red)')}<span>踢出房间</span></button>` : ''}`;
    if (!menu.querySelector('.um-item')) return;
    document.body.appendChild(menu);
    const mw = menu.offsetWidth;
    const mh = menu.offsetHeight;
    menu.style.left = `${Math.min(x, window.innerWidth - mw - 8)}px`;
    menu.style.top = `${Math.min(y, window.innerHeight - mh - 8)}px`;
    const close = () => {
      menu.remove();
      document.removeEventListener('pointerdown', onDoc, true);
    };
    const onDoc = (ev: Event) => {
      if (!menu.contains(ev.target as Node)) close();
    };
    setTimeout(() => document.addEventListener('pointerdown', onDoc, true));
    menu.querySelector('[data-act="mute"]')?.addEventListener('click', () => {
      targets.forEach((p) => volumes.set(p.identity, muted ? 1 : 0));
      applyAudioPrefs();
      refreshMembers();
      close();
    });
    menu.querySelector('[data-act="kick"]')?.addEventListener('click', async () => {
      close();
      try {
        await kickUser(channel, username);
        toast(`已把 ${username} 移出房间`, 'ok');
      } catch (err) {
        toast((err as Error).message, 'bad');
      }
    });
  }

  // ---- 成员面板 ----
  const membersBody = shell.content.querySelector<HTMLDivElement>('#members-body')!;
  const membersCount = shell.content.querySelector<HTMLSpanElement>('#members-count')!;

  function refreshMembers() {
    const byUser = new Map<string, EPart[]>();
    for (const p of parts()) {
      if (!byUser.has(p.username)) byUser.set(p.username, []);
      byUser.get(p.username)!.push(p);
    }
    membersCount.textContent = String(byUser.size);
    membersBody.innerHTML = `
      <div>
        <div class="side-section-title">在房 — ${byUser.size}</div>
        ${[...byUser.entries()]
          .map(([uname, plist]) => {
            const isMe = uname === myUsername;
            const speaking = plist.some((p) => speakingSet.has(p.identity));
            const sharing = plist.some((p) => p.sharing);
            const obs = plist.some((p) => p.obs);
            const muted = plist.every((p) => !p.micOn && !p.obs);
            const localMuted = plist.some((p) => volumeFor(p.identity) === 0);
            const statusBits = [
              sharing ? '投屏中' : '',
              obs ? 'OBS 推流' : '',
              speaking ? '说话中' : muted ? '已静音' : '',
              plist.filter((p) => !p.obs).length > 1 ? `${plist.filter((p) => !p.obs).length} 台设备` : '',
              localMuted && !isMe ? '已本地静音' : '',
            ].filter(Boolean);
            return `
          <div class="member-row ${uname === ownerUsername ? 'owner-row' : ''}">
            ${avatarHtml(uname, 'avatar' + (speaking ? ' speaking' : ''))}
            <div style="flex-grow:1;min-width:0">
              <div class="m-name">${esc(uname)}${isMe ? '<span class="muted">（我）</span>' : ''}
                ${uname === ownerUsername ? '<span class="tag tag-ember">房主</span>' : ''}
              </div>
              <div class="m-status ${speaking || sharing ? 'hot' : ''}">${statusBits.join(' · ')}</div>
            </div>
            ${
              isMe
                ? ''
                : `<button class="hit m-btn ${localMuted ? 'muted-on' : ''}" data-lmute="${esc(uname)}" title="${localMuted ? '恢复' : '本地静音'}">${slashIcon('volume', 14, localMuted, localMuted ? 'var(--red)' : 'var(--text-2)')}</button>${
                    isOwner ? `<button class="hit m-btn" data-mkick="${esc(uname)}" title="踢出房间">${icon('leave', 14, 'var(--red)')}</button>` : ''
                  }`
            }
          </div>`;
          })
          .join('')}
      </div>`;
    membersBody.querySelectorAll<HTMLButtonElement>('[data-mkick]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        try {
          await kickUser(channel, btn.dataset.mkick!);
          toast(`已把 ${btn.dataset.mkick} 移出房间`, 'ok');
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      });
    });
    membersBody.querySelectorAll<HTMLButtonElement>('[data-lmute]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const uname = btn.dataset.lmute!;
        const targets = parts().filter((p) => !p.isLocal && p.username === uname);
        const nowMuted = targets.some((p) => volumeFor(p.identity) === 0);
        targets.forEach((p) => volumes.set(p.identity, nowMuted ? 1 : 0));
        applyAudioPrefs();
        refreshMembers();
      });
    });
  }

  function refreshMeta() {
    const n = new Set(parts().map((p) => p.username)).size;
    const screens = [...tiles.keys()].filter((k) => k.endsWith(':screen')).length;
    roomMetaEl.textContent = `${n} 人在房${screens ? ` · ${screens} 路投屏` : ''}`;
  }

  function connBoxMeta(): string {
    let m = `voice: ${voiceLine.engineName || '—'}`;
    if (stageLine && !combined) m += ` · stage: ${stageUp ? stageLine.engineName : '重连中'}`;
    return m;
  }

  // ---- 断线自动重连（按线独立）----

  function bounce(msg: string) {
    if (bounced) return;
    bounced = true;
    statusEl.textContent = msg;
    toast(msg, 'bad');
    setTimeout(() => {
      location.hash = '#/lobby';
    }, 1500);
  }

  function lineFor(role: Role): Line | null {
    return role === 'voice' ? voiceLine : combined ? voiceLine : stageLine;
  }

  function scheduleRejoin(role: Role, delay?: number) {
    if (leaving || bounced) return;
    const line = lineFor(role);
    if (!line) return;
    clearTimeout(line.timer);
    const wait = delay ?? Math.min(30000, 2000 * 2 ** Math.min(line.attempts, 4)) * (0.7 + Math.random() * 0.6);
    if (role === 'voice' || combined) {
      statusEl.textContent = line.attempts === 0 ? '连接断开，正在重连…' : `连接断开，正在重连…（第 ${line.attempts + 1} 次）`;
      shell.setConn(false, '正在重连…');
    }
    line.timer = window.setTimeout(() => void connectLines(false, role), wait);
  }

  // 进房 / 按线重连：拿一次凭证，把指定线（或全部）接上
  async function connectLines(first: boolean, only?: Role) {
    if (leaving || bounced) return;
    let creds;
    try {
      creds = await fetchJoinCredentials(channel);
    } catch (err) {
      handleCredsError(err, first);
      return;
    }
    combined = creds.combined;
    if (!only || only === 'voice') {
      await connectLine(voiceLine, creds.voice, first, 'voice');
    }
    if (combined) {
      stageLine = voiceLine;
      stageUp = !!voiceLine.engine?.connected();
    } else if (creds.stage) {
      if (!stageLine || stageLine === voiceLine) {
        stageLine = { role: 'stage', engine: null, engineName: '', attempts: 0, timer: 0, inflight: false };
      }
      if (!only || only === 'stage') {
        await connectLine(stageLine, creds.stage, first, 'stage');
      }
    } else {
      stageLine = null;
      stageUp = false;
    }
    updateStageButtons();
    shell.setConn(!!voiceLine.engine?.connected(), connBoxMeta());
  }

  function handleCredsError(err: unknown, first: boolean) {
    const msg = (err as Error).message ?? '';
    if (first) statusEl.textContent = `连接失败：${msg}`;
    if (msg.includes('封禁') || msg.includes('邀请制') || msg.includes('不存在')) {
      bounce(msg);
    } else if (msg.includes('登录已失效') || msg.includes('登录凭证')) {
      bounced = true;
      clearSession(); // 带失效 token 去登录页会被路由守卫弹回，先清掉
      location.hash = '#/login';
    } else {
      scheduleRejoin('voice');
    }
  }

  async function connectLine(line: Line, cred: EngineCred, first: boolean, role: Role) {
    if (line.inflight || line.engine?.connected()) return;
    line.inflight = true;
    if (!first) line.attempts++;
    try {
      if (!line.engine || cred.engine !== line.engineName) {
        line.engine?.dispose();
        line.engine = await createEngine(cred.engine, makeCallbacks(role));
        line.engineName = cred.engine;
      }
      await line.engine.connect(cred.url, cred.token);
      line.attempts = 0;
      if (role === 'voice') {
        statusEl.textContent = '';
        if (!first) toast('语音已重新连接', 'ok', 1800);
        if (micOn) {
          try {
            await line.engine.setMic(true);
          } catch (err) {
            micOn = false;
            if (first) toast(captureErrorMsg('麦克风', err), 'bad');
          }
        }
      }
      if (role === 'stage' || combined) {
        stageUp = true;
        if (cameraOn) {
          try {
            await (role === 'stage' ? line.engine : voiceLine.engine)!.setCamera(true);
          } catch {
            cameraOn = false;
          }
        }
        if (screenOn) {
          screenOn = false;
          toast('重连后投屏需要重新发起', '', 4000);
        }
      }
      refreshButtons();
      syncAudioTiles();
      refreshMembers();
      refreshMeta();
    } catch (err) {
      if (role === 'voice') {
        handleCredsError(err, first);
      } else {
        stageUp = false;
        if (first) toast(`舞台线连接失败（投屏/摄像头暂不可用）：${(err as Error).message}`, 'bad', 4000);
        scheduleRejoin('stage');
      }
    } finally {
      line.inflight = false;
      updateStageButtons();
    }
  }

  // 回前台 / 网络恢复：跳过退避立即重连
  const retryNow = () => {
    if (leaving || bounced) return;
    (['voice', 'stage'] as Role[]).forEach((role) => {
      const line = lineFor(role);
      if (role === 'stage' && (combined || !line)) return;
      if (line && line.engine && !line.engine.connected()) {
        clearTimeout(line.timer);
        line.attempts = 0;
        void connectLines(false, role);
      }
    });
  };
  const onVisible = () => {
    if (document.visibilityState === 'visible') retryNow();
  };
  document.addEventListener('visibilitychange', onVisible);
  window.addEventListener('online', retryNow);

  // ---- 引擎回调（按线）----
  function makeCallbacks(role: Role): EngineCallbacks {
    return {
      onVideoTrack: (part, source, el) => addVideoTile(part, source, el),
      onVideoTrackRemoved: (identity, source, els) => removeVideoTile(identity, source, els),
      onAudioTrack: (identity, el) => {
        let set = audioEls.get(identity);
        if (!set) {
          set = new Set();
          audioEls.set(identity, set);
        }
        set.add(el as SinkMedia);
        audioBin.appendChild(el);
        applyAudioPrefs();
      },
      onAudioTrackRemoved: (identity, els) => {
        els.forEach((el) => {
          el.remove();
          audioEls.get(identity)?.delete(el as SinkMedia);
        });
      },
      onRoster: () => {
        syncAudioTiles();
        refreshMembers();
        refreshMeta();
      },
      onSpeakers: (identities) => {
        speakingByRole[role] = new Set(identities);
        speakingSet = new Set([...speakingByRole.voice, ...speakingByRole.stage]);
        if (identities[0]) lastSpeaker = identities[0];
        tiles.forEach((tile, key) => {
          const id = key.slice(0, key.lastIndexOf(':'));
          tile.classList.toggle('speaking', speakingSet.has(id));
        });
        syncAudioTiles();
        refreshMembers();
      },
      onReconnecting: () => {
        if (role === 'voice' || combined) {
          statusEl.textContent = '连接不稳定，正在恢复…';
          shell.setConn(false, '正在恢复连接…');
        }
      },
      onReconnected: () => {
        if (role === 'voice' || combined) {
          statusEl.textContent = '';
          shell.setConn(true, connBoxMeta());
        }
        syncAudioTiles();
        refreshMembers();
        refreshMeta();
      },
      onEnded: (reason) => {
        if (leaving) return;
        if (reason === 'kicked') return bounce('你已被移出该频道');
        if (reason === 'room-deleted') return bounce('频道已删除');
        if (reason === 'duplicate') return bounce('该设备在其他页面进入了房间');
        // lost
        if (role === 'voice' || combined) {
          scheduleRejoin('voice', 0);
        } else {
          stageUp = false;
          screenOn = false;
          refreshButtons();
          updateStageButtons();
          shell.setConn(!!voiceLine.engine?.connected(), connBoxMeta());
          scheduleRejoin('stage', 0);
        }
      },
      onLocalTrackEnded: (kind) => {
        if (kind === 'mic' && micOn) {
          micOn = false;
          void voiceLine.engine?.setMic(false).catch(() => {});
          toast('麦克风设备断开了（iPhone 连续互通断开会这样），已自动闭麦', 'bad');
        }
        if (kind === 'camera' && cameraOn) {
          cameraOn = false;
          void stageEngine()?.setCamera(false).catch(() => {});
          toast('摄像头设备断开了，已自动关闭', 'bad');
        }
        refreshButtons();
        syncAudioTiles();
        refreshMembers();
      },
    };
  }

  function captureErrorMsg(kind: '麦克风' | '摄像头', err: unknown): string {
    const name = (err as DOMException)?.name ?? '';
    if (name === 'NotFoundError' || name === 'DevicesNotFoundError')
      return `没有可用的${kind}设备——iPhone 连续互通断开、或没接外设时会这样，接上后再点一次`;
    if (name === 'NotAllowedError' || name === 'PermissionDeniedError')
      return `${kind}权限被拒绝，点地址栏右侧的图标允许后重试`;
    if (name === 'NotReadableError') return `${kind}被其他应用占用`;
    return `${kind}启动失败：${(err as Error)?.message ?? name}`;
  }

  // 本地麦克风电平表：让说话的人确认自己有声音
  let vuCtx: AudioContext | null = null;
  let vuAnalyser: AnalyserNode | null = null;
  let vuSrc: MediaStreamAudioSourceNode | null = null;
  let vuTrack: MediaStreamTrack | null = null;
  const vuTimer = window.setInterval(() => {
    const track = micOn ? (voiceLine.engine?.localMicTrack() ?? null) : null;
    if (track !== vuTrack) {
      vuSrc?.disconnect();
      vuSrc = null;
      vuTrack = track;
      if (track) {
        if (!vuCtx) {
          vuCtx = new AudioContext();
          vuAnalyser = vuCtx.createAnalyser();
          vuAnalyser.fftSize = 512;
        }
        if (vuCtx.state === 'suspended') void vuCtx.resume();
        vuSrc = vuCtx.createMediaStreamSource(new MediaStream([track]));
        vuSrc.connect(vuAnalyser!);
      }
    }
    const bar = btnMic.querySelector<HTMLElement>('.mic-vu i');
    if (!bar) return;
    if (!vuTrack || !vuAnalyser) {
      bar.style.width = '0%';
      return;
    }
    const buf = new Uint8Array(vuAnalyser.fftSize);
    vuAnalyser.getByteTimeDomainData(buf);
    let sum = 0;
    for (let i = 0; i < buf.length; i++) {
      const d = (buf[i] - 128) / 128;
      sum += d * d;
    }
    const rms = Math.sqrt(sum / buf.length);
    bar.style.width = `${Math.min(100, Math.round(rms * 320))}%`;
  }, 120);

  // 设备失而复得时提醒一声
  let hadAudioInput = true;
  const onDeviceChange = async () => {
    try {
      const n = (await navigator.mediaDevices.enumerateDevices()).filter((d) => d.kind === 'audioinput').length;
      if (n > 0 && !hadAudioInput) toast('检测到麦克风设备，可以重新开麦了', 'ok');
      hadAudioInput = n > 0;
    } catch {
      // 枚举失败忽略
    }
  };
  navigator.mediaDevices?.addEventListener?.('devicechange', onDeviceChange);
  void onDeviceChange();

  // ---- 控制按钮 ----
  const btnMic = shell.content.querySelector<HTMLButtonElement>('#btn-mic')!;
  const btnDeaf = shell.content.querySelector<HTMLButtonElement>('#btn-deaf')!;
  const btnCamera = shell.content.querySelector<HTMLButtonElement>('#btn-camera')!;
  const btnScreen = shell.content.querySelector<HTMLButtonElement>('#btn-screen')!;

  function refreshButtons() {
    btnMic.classList.toggle('on', micOn);
    btnMic.innerHTML = `${micIcon(17, !micOn, 'currentColor')}<span class="pill-label">${micOn ? '麦克风' : '已静音'}</span>${micOn ? '<span class="mic-vu"><i></i></span>' : ''}`;
    btnDeaf.classList.toggle('danger', deafened);
    btnDeaf.innerHTML = slashIcon('speaker', 17, deafened, deafened ? 'var(--red)' : 'currentColor');
    btnCamera.classList.toggle('on', cameraOn);
    btnCamera.innerHTML = slashIcon('camera', 17, !cameraOn, 'currentColor');
    btnScreen.classList.toggle('on', screenOn);
    btnScreen.innerHTML = `${icon('screen', 17, 'currentColor')}<span class="pill-label">${screenOn ? '投屏中' : '投屏'}</span>`;
  }
  refreshButtons();

  function updateStageButtons() {
    const ok = !!stageEngine() && stageUp;
    btnCamera.classList.toggle('disabled', !ok);
    btnScreen.classList.toggle('disabled', !ok);
    const hint = stageLine ? '舞台线重连中' : '本服未启用舞台线（投屏/摄像头）';
    btnCamera.title = ok ? '摄像头' : hint;
    btnScreen.title = ok ? '投屏' : hint;
  }

  btnMic.addEventListener('click', async () => {
    if (!voiceLine.engine) return;
    micOn = !micOn;
    refreshButtons();
    try {
      await voiceLine.engine.setMic(micOn);
      const p = loadPrefs();
      p.mic = micOn;
      savePrefs(p);
    } catch (err) {
      if (micOn) toast(captureErrorMsg('麦克风', err), 'bad');
      micOn = !micOn;
      refreshButtons();
    }
    syncAudioTiles();
    refreshMembers();
  });

  btnDeaf.addEventListener('click', () => {
    deafened = !deafened;
    applyAudioPrefs();
    refreshButtons();
  });

  btnCamera.addEventListener('click', async () => {
    const eng = stageEngine();
    if (!eng || !stageUp) return;
    cameraOn = !cameraOn;
    refreshButtons();
    try {
      await eng.setCamera(cameraOn);
      const p = loadPrefs();
      p.camera = cameraOn;
      savePrefs(p);
    } catch (err) {
      if (cameraOn) toast(captureErrorMsg('摄像头', err), 'bad');
      cameraOn = !cameraOn;
      refreshButtons();
    }
  });

  btnScreen.addEventListener('click', async () => {
    const eng = stageEngine();
    if (!eng || !stageUp) return;
    screenOn = !screenOn;
    refreshButtons();
    try {
      await eng.setScreen(screenOn);
    } catch {
      screenOn = !screenOn; // 用户取消选择等情况回退
      refreshButtons();
    }
    refreshMeta();
  });

  shell.content.querySelector('#btn-quality')!.addEventListener('click', () => {
    openSettings('screen', { backLabel: `返回 ${channel}` });
  });
  shell.content.querySelector('#btn-settings')!.addEventListener('click', () => {
    openSettings('devices', { backLabel: `返回 ${channel}` });
  });

  shell.content.querySelectorAll<HTMLButtonElement>('[data-layout]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const p = loadPrefs();
      p.layout = btn.dataset.layout as 'grid' | 'spotlight';
      savePrefs(p);
      applyLayout();
    });
  });
  applyLayout();

  // ---- 右侧面板：成员 / 聊天 ----
  const membersPanel = shell.content.querySelector<HTMLDivElement>('#members-panel')!;
  const chatPanel = shell.content.querySelector<HTMLDivElement>('#chat-panel')!;
  const btnPanelMembers = shell.content.querySelector<HTMLButtonElement>('#btn-panel-members')!;
  const btnPanelChat = shell.content.querySelector<HTMLButtonElement>('#btn-panel-chat')!;
  const badgeEl = shell.content.querySelector<HTMLSpanElement>('#chat-badge')!;
  let panel: 'members' | 'chat' | '' = window.matchMedia('(min-width: 1200px)').matches ? 'members' : '';
  let unread = 0;

  function applyPanel() {
    membersPanel.classList.toggle('hidden', panel !== 'members');
    chatPanel.classList.toggle('hidden', panel !== 'chat');
    btnPanelMembers.classList.toggle('on', panel === 'members');
    btnPanelChat.classList.toggle('on', panel === 'chat');
    if (panel === 'chat') {
      unread = 0;
      badgeEl.classList.add('hidden');
      chatLog.scrollTop = chatLog.scrollHeight;
    }
  }
  btnPanelMembers.addEventListener('click', () => {
    panel = panel === 'members' ? '' : 'members';
    applyPanel();
  });
  btnPanelChat.addEventListener('click', () => {
    panel = panel === 'chat' ? '' : 'chat';
    applyPanel();
  });
  shell.content.querySelector('#members-close')!.addEventListener('click', () => {
    panel = '';
    applyPanel();
  });
  shell.content.querySelector('#chat-close')!.addEventListener('click', () => {
    panel = '';
    applyPanel();
  });

  // ---- 聊天 ----
  const chatLog = shell.content.querySelector<HTMLDivElement>('#chat-log')!;
  const chatInput = shell.content.querySelector<HTMLInputElement>('#chat-input')!;
  const chatSend = shell.content.querySelector<HTMLButtonElement>('#chat-send')!;

  chatInput.addEventListener('input', () => {
    chatSend.classList.toggle('ready', chatInput.value.trim().length > 0);
  });

  const appendMsg = (m: ChatMessage, isHistory = false) => {
    const div = document.createElement('div');
    div.className = 'chat-msg';
    div.innerHTML = `
      ${avatarHtml(m.username, 'avatar')}
      <div class="body">
        <div class="meta"><span class="who">${esc(m.username)}</span><span class="at">${fmtClock(m.created_at)}</span></div>
        <div class="text">${esc(m.content)}</div>
      </div>`;
    if (m.username !== myUsername) {
      div.addEventListener('contextmenu', (ev) => {
        ev.preventDefault();
        showUserMenu(ev.clientX, ev.clientY, m.username);
      });
      div.querySelector('.avatar')?.addEventListener('click', (ev) => {
        showUserMenu((ev as MouseEvent).clientX, (ev as MouseEvent).clientY, m.username);
      });
    }
    chatLog.appendChild(div);
    chatLog.scrollTop = chatLog.scrollHeight;
    if (!isHistory && panel !== 'chat') {
      unread += 1;
      badgeEl.textContent = unread > 99 ? '99+' : String(unread);
      badgeEl.classList.remove('hidden');
    }
  };

  const chat = connectChat(channel, {
    onHistory: (messages) => {
      chatLog.innerHTML = '<div class="chat-day">最近</div>';
      messages.forEach((m) => appendMsg(m, true));
      unread = 0;
      badgeEl.classList.add('hidden');
    },
    onMessage: appendMsg,
    onKicked: (code) => {
      if (leaving) return;
      bounce(code === 1001 ? '频道已删除' : '你已被移出该频道');
    },
    onState: (state) => {
      chatInput.placeholder = state === 'open' ? `发消息到 #${channel}` : '聊天重连中…';
    },
  });

  shell.content.querySelector('#chat-form')!.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const content = chatInput.value.trim();
    if (content) {
      chat.send(content);
      chatInput.value = '';
      chatSend.classList.remove('ready');
    }
  });

  // ---- 设置页偏好热应用 ----
  const onPrefs = async (ev: Event) => {
    const what = (ev as CustomEvent).detail as string;
    if (what === 'volume' || what === 'speaker') applyAudioPrefs();
    if (what === 'mirror') {
      const id = stageEngine()?.localIdentity() ?? '';
      const video = tiles.get(`${id}:camera`)?.querySelector('video');
      if (video) video.style.transform = loadPrefs().mirror ? 'scaleX(-1)' : '';
    }
    if (what === 'mic' && loadPrefs().mic !== micOn) {
      btnMic.click();
      return;
    }
    if ((what === 'mic-device' || what === 'audio-chain') && micOn && voiceLine.engine) {
      try {
        await voiceLine.engine.restartMic();
      } catch {
        micOn = false;
        refreshButtons();
      }
    }
    if (what === 'cam-device' && cameraOn) {
      const p = loadPrefs();
      if (p.camDeviceId) void stageEngine()?.switchCamera(p.camDeviceId).catch(() => {});
    }
  };
  prefsBus.addEventListener('prefs', onPrefs);

  // ---- 房主探测 ----
  void listChannels()
    .then((chs) => {
      const ch = chs.find((c) => c.name === channel);
      isOwner = ch?.is_owner === true;
      ownerUsername = ch?.created_by ?? '';
      if (isOwner) {
        const btn = shell.content.querySelector<HTMLButtonElement>('#btn-manage')!;
        btn.classList.remove('hidden');
        btn.title = '频道管理（新标签打开，不离开房间）';
        btn.addEventListener('click', () => {
          window.open(`#/manage/${encodeURIComponent(channel)}`, '_blank');
        });
      }
      refreshMembers();
    })
    .catch(() => {});

  // ---- 首次连接与清理 ----
  applyPanel();
  updateStageButtons();
  await connectLines(true);

  const myHash = location.hash;
  const onHashChange = () => {
    if (location.hash !== myHash) {
      window.removeEventListener('hashchange', onHashChange);
      prefsBus.removeEventListener('prefs', onPrefs);
      navigator.mediaDevices?.removeEventListener?.('devicechange', onDeviceChange);
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('online', retryNow);
      leaving = true;
      clearInterval(vuTimer);
      void vuCtx?.close();
      clearTimeout(voiceLine.timer);
      if (stageLine && stageLine !== voiceLine) clearTimeout(stageLine.timer);
      chat.close();
      voiceLine.engine?.dispose();
      if (stageLine && stageLine !== voiceLine) stageLine.engine?.dispose();
      shell.destroy();
    }
  };
  window.addEventListener('hashchange', onHashChange);
  shell.content.querySelector('#btn-leave')!.addEventListener('click', () => {
    location.hash = '#/lobby';
  });
}
