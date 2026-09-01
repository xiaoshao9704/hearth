// 房间页：布局、面板、聊天与断线重连都在这里；音视频经 AVEngine 接口驱动，
// 不感知具体内核（LiveKit / 将来的 pion-voice 由凭证里的 engine 字段选择）。
import { LIVEKIT_URL_FALLBACK, clearSession, fetchLiveKitToken, getUser, listChannels } from '../api';
import { connectChat } from '../chat';
import type { ChatMessage } from '../chat';
import { createEngine } from '../engine';
import type { AVEngine, EPart, EngineCallbacks, TrackSource } from '../engine/types';
import { loadPrefs, prefsBus, savePrefs } from '../prefs';
import { menuButtonHtml, renderShell, wireMenuButton } from '../shell';
import { avatarHtml, esc, fmtClock, icon, micIcon, slashIcon, toast } from '../ui';
import { openSettings } from './settings';

// iOS Safari 的私有全屏 API（iPhone 仅 video 元素可用）
type IOSVideo = HTMLVideoElement & { webkitEnterFullscreen?: () => void };
type SinkMedia = HTMLMediaElement & { setSinkId?: (id: string) => Promise<void> };

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
  let engine: AVEngine | null = null;
  let engineName = '';
  let leaving = false;
  let bounced = false;
  const tiles = new Map<string, HTMLDivElement>(); // `${identity}:${source}` -> 视频卡片
  const audioTiles = new Map<string, HTMLDivElement>(); // identity -> 无视频参与者的音频块
  const audioEls = new Map<string, Set<SinkMedia>>();
  const volumes = new Map<string, number>(); // identity -> 本机静音 0/1
  let deafened = false;
  let speakingSet = new Set<string>();
  let micOn = prefs.mic;
  let cameraOn = prefs.camera;
  let screenOn = false;
  const myUsername = getUser()?.username ?? '';
  const obsIdentity = `${myUsername}-obs`;

  const parts = () => engine?.participants() ?? [];

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

  // 无视频参与者渲染音频块（头像大圆 + 名字 + 麦克风状态）
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
      el.classList.toggle('muted', !p.micOn && !p.isLocal);
      el.innerHTML = `
        ${avatarHtml(p.username, 'avatar avatar-xl' + (speaking ? ' speaking' : ''))}
        <div class="a-name">
          <span>${esc(p.isLocal ? '你' : p.display)}</span>
          ${micIcon(13, !p.micOn, !p.micOn ? 'var(--red)' : speaking ? 'var(--ember)' : 'var(--text-2)')}
        </div>
        ${p.obs ? '<div style="position:absolute;top:12px;right:12px" class="tag tag-ember mono">OBS 推流</div>' : ''}`;
    }
    if (engine?.connected()) statusEl.textContent = '';
    applyLayout();
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
            const muted = plist.every((p) => !p.micOn);
            const localMuted = plist.some((p) => volumeFor(p.identity) === 0);
            const statusBits = [
              sharing ? '投屏中' : '',
              obs ? 'OBS 推流' : '',
              speaking ? '说话中' : muted ? '已静音' : '',
              plist.length > 1 ? `${plist.length} 台设备` : '',
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
                : `<button class="hit m-btn ${localMuted ? 'muted-on' : ''}" data-lmute="${esc(uname)}" title="${localMuted ? '恢复' : '本地静音'}">${slashIcon('volume', 14, localMuted, localMuted ? 'var(--red)' : 'var(--text-2)')}</button>`
            }
          </div>`;
          })
          .join('')}
      </div>`;
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

  // ---- 断线自动重连 ----
  let connMeta = '';
  let rejoinTimer = 0;
  let rejoinAttempts = 0;
  let rejoinInFlight = false;

  function bounce(msg: string) {
    if (bounced) return;
    bounced = true;
    statusEl.textContent = msg;
    toast(msg, 'bad');
    setTimeout(() => {
      location.hash = '#/lobby';
    }, 1500);
  }

  function clearAllTiles() {
    speakingSet.clear();
    tiles.forEach((tile) => tile.remove());
    tiles.clear();
    audioTiles.forEach((tile) => tile.remove());
    audioTiles.clear();
    audioEls.forEach((set) => set.forEach((el) => el.remove()));
    audioEls.clear();
    pinnedKey = null;
    refreshMembers();
    refreshMeta();
  }

  function scheduleRejoin(delay?: number) {
    if (leaving || bounced) return;
    clearTimeout(rejoinTimer);
    const wait = delay ?? Math.min(30000, 2000 * 2 ** Math.min(rejoinAttempts, 4)) * (0.7 + Math.random() * 0.6);
    statusEl.textContent = rejoinAttempts === 0 ? '连接断开，正在重连…' : `连接断开，正在重连…（第 ${rejoinAttempts + 1} 次）`;
    shell.setConn(false, '正在重连…');
    rejoinTimer = window.setTimeout(() => void connectRoom(false), wait);
  }

  // 进房（首连与重连共用）：拿凭证 → 按 engine 名建/复用引擎 → 连接 → 恢复发布状态
  async function connectRoom(first: boolean) {
    if (leaving || bounced || rejoinInFlight || engine?.connected()) return;
    rejoinInFlight = true;
    if (!first) rejoinAttempts++;
    try {
      const cred = await fetchLiveKitToken(channel);
      const name = cred.engine || 'livekit';
      if (!engine || name !== engineName) {
        engine?.dispose();
        engine = await createEngine(name, callbacks);
        engineName = name;
      }
      const url = cred.url || LIVEKIT_URL_FALLBACK;
      await engine.connect(url, cred.token);
      connMeta = `${engineName} · ${new URL(url).host}`;
      rejoinAttempts = 0;
      statusEl.textContent = '';
      shell.setConn(true, connMeta);
      if (!first) toast('已重新连接', 'ok', 1800);
      // 恢复发布状态：麦克风/摄像头按开关重开；投屏需要用户手势，不自动恢复
      if (micOn) {
        try {
          await engine.setMic(true);
        } catch (err) {
          micOn = false;
          if (first) toast(captureErrorMsg('麦克风', err), 'bad');
        }
      }
      if (cameraOn) {
        try {
          await engine.setCamera(true);
        } catch {
          cameraOn = false;
        }
      }
      if (screenOn) {
        screenOn = false;
        toast('重连后投屏需要重新发起', '', 4000);
      }
      refreshButtons();
      syncAudioTiles();
      refreshMembers();
      refreshMeta();
    } catch (err) {
      const msg = (err as Error).message ?? '';
      statusEl.textContent = first ? `连接失败：${msg}` : statusEl.textContent;
      if (msg.includes('封禁') || msg.includes('邀请制') || msg.includes('不存在')) {
        bounce(msg);
      } else if (msg.includes('登录已失效') || msg.includes('登录凭证')) {
        bounced = true;
        clearSession(); // 带着失效 token 去登录页会被路由守卫弹回，先清掉
        location.hash = '#/login';
      } else {
        scheduleRejoin();
      }
    } finally {
      rejoinInFlight = false;
    }
  }

  // 回前台 / 网络恢复：跳过退避立即重连（iOS 切后台回来最常见）
  const retryNow = () => {
    if (leaving || bounced) return;
    if (engine && !engine.connected()) {
      clearTimeout(rejoinTimer);
      rejoinAttempts = 0;
      void connectRoom(false);
    }
  };
  const onVisible = () => {
    if (document.visibilityState === 'visible') retryNow();
  };
  document.addEventListener('visibilitychange', onVisible);
  window.addEventListener('online', retryNow);

  // ---- 引擎回调 ----
  const callbacks: EngineCallbacks = {
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
      speakingSet = new Set(identities);
      if (identities[0]) lastSpeaker = identities[0];
      syncAudioTiles();
      refreshMembers();
    },
    onReconnecting: () => {
      statusEl.textContent = '连接不稳定，正在恢复…';
      shell.setConn(false, '正在恢复连接…');
    },
    onReconnected: () => {
      statusEl.textContent = '';
      shell.setConn(true, connMeta);
      syncAudioTiles();
      refreshMembers();
      refreshMeta();
    },
    onEnded: (reason) => {
      if (leaving) return;
      clearAllTiles();
      if (reason === 'kicked') return bounce('你已被移出该频道');
      if (reason === 'room-deleted') return bounce('频道已删除');
      if (reason === 'duplicate') return bounce('该设备在其他页面进入了房间');
      scheduleRejoin(0);
    },
    onLocalTrackEnded: (kind) => {
      if (kind === 'mic' && micOn) {
        micOn = false;
        void engine?.setMic(false).catch(() => {});
        toast('麦克风设备断开了（iPhone 连续互通断开会这样），已自动闭麦', 'bad');
      }
      if (kind === 'camera' && cameraOn) {
        cameraOn = false;
        void engine?.setCamera(false).catch(() => {});
        toast('摄像头设备断开了，已自动关闭', 'bad');
      }
      refreshButtons();
      syncAudioTiles();
      refreshMembers();
    },
  };

  // 采集失败的人话提示（Mac mini 无内置麦，iPhone 连续互通断开就会 NotFound）
  function captureErrorMsg(kind: '麦克风' | '摄像头', err: unknown): string {
    const name = (err as DOMException)?.name ?? '';
    if (name === 'NotFoundError' || name === 'DevicesNotFoundError')
      return `没有可用的${kind}设备——iPhone 连续互通断开、或没接外设时会这样，接上后再点一次`;
    if (name === 'NotAllowedError' || name === 'PermissionDeniedError')
      return `${kind}权限被拒绝，点地址栏右侧的图标允许后重试`;
    if (name === 'NotReadableError') return `${kind}被其他应用占用`;
    return `${kind}启动失败：${(err as Error)?.message ?? name}`;
  }

  // 设备失而复得（iPhone 靠近重连/插上耳机）时提醒一声
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
    btnMic.innerHTML = `${micIcon(17, !micOn, 'currentColor')}<span class="pill-label">${micOn ? '麦克风' : '已静音'}</span>`;
    btnDeaf.classList.toggle('danger', deafened);
    btnDeaf.innerHTML = slashIcon('speaker', 17, deafened, deafened ? 'var(--red)' : 'currentColor');
    btnCamera.classList.toggle('on', cameraOn);
    btnCamera.innerHTML = slashIcon('camera', 17, !cameraOn, 'currentColor');
    btnScreen.classList.toggle('on', screenOn);
    btnScreen.innerHTML = `${icon('screen', 17, 'currentColor')}<span class="pill-label">${screenOn ? '投屏中' : '投屏'}</span>`;
  }
  refreshButtons();

  btnMic.addEventListener('click', async () => {
    if (!engine) return;
    micOn = !micOn;
    refreshButtons();
    try {
      await engine.setMic(micOn);
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
    if (!engine) return;
    cameraOn = !cameraOn;
    refreshButtons();
    try {
      await engine.setCamera(cameraOn);
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
    if (!engine) return;
    screenOn = !screenOn;
    refreshButtons();
    try {
      await engine.setScreen(screenOn);
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
    openSettings('account', { backLabel: `返回 ${channel}` });
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
      const video = tiles.get(`${engine?.localIdentity() ?? ''}:camera`)?.querySelector('video');
      if (video) video.style.transform = loadPrefs().mirror ? 'scaleX(-1)' : '';
    }
    if (what === 'mic' && loadPrefs().mic !== micOn) {
      btnMic.click();
      return;
    }
    if ((what === 'mic-device' || what === 'audio-chain') && micOn && engine) {
      try {
        await engine.restartMic();
      } catch {
        micOn = false;
        refreshButtons();
      }
    }
    if (what === 'cam-device' && cameraOn && engine) {
      const p = loadPrefs();
      if (p.camDeviceId) void engine.switchCamera(p.camDeviceId).catch(() => {});
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
        btn.addEventListener('click', () => {
          location.hash = `#/manage/${encodeURIComponent(channel)}`;
        });
      }
      refreshMembers();
    })
    .catch(() => {});

  // ---- 首次连接与清理 ----
  applyPanel();
  await connectRoom(true);

  const myHash = location.hash;
  const onHashChange = () => {
    if (location.hash !== myHash) {
      window.removeEventListener('hashchange', onHashChange);
      prefsBus.removeEventListener('prefs', onPrefs);
      navigator.mediaDevices?.removeEventListener?.('devicechange', onDeviceChange);
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('online', retryNow);
      leaving = true;
      clearTimeout(rejoinTimer);
      chat.close();
      engine?.dispose();
      shell.destroy();
    }
  };
  window.addEventListener('hashchange', onHashChange);
  shell.content.querySelector('#btn-leave')!.addEventListener('click', () => {
    location.hash = '#/lobby';
  });
}
