// 房间页（核心）：LiveKit 音视频 + 高码率投屏 + 成员/聊天双面板。
// 布局与交互按 artifacts 原型：顶栏（九宫格/聚焦）、底部控制栏、右侧成员或聊天面板；
// 设置改为全屏浮层（不断开通话），偏好改动经 prefsBus 热应用。
import {
  ConnectionState,
  Participant,
  RemoteParticipant,
  RemoteTrack,
  Room,
  RoomEvent,
  Track,
} from 'livekit-client';
import type { AudioCaptureOptions, ScreenShareCaptureOptions, TrackPublishOptions } from 'livekit-client';
import { LIVEKIT_URL_FALLBACK, fetchLiveKitToken, getUser, listChannels } from '../api';
import { connectChat } from '../chat';
import type { ChatMessage } from '../chat';
import { RnnoisePipeline } from '../audio';
import { RES_DIMS, loadPrefs, prefsBus, savePrefs } from '../prefs';
import { menuButtonHtml, renderShell, wireMenuButton } from '../shell';
import { avatarHtml, esc, fmtClock, icon, micIcon, slashIcon, toast } from '../ui';
import { openSettings } from './settings';

// iOS Safari 的私有全屏 API（iPhone 仅 video 元素可用）
type IOSVideo = HTMLVideoElement & { webkitEnterFullscreen?: () => void };
type SinkMedia = HTMLMediaElement & { setSinkId?: (id: string) => Promise<void> };

const displayName = (p: Participant) => p.name || p.identity;
const usernameOf = (p: Participant) => (p.name ? p.name : p.identity.split('-')[0]);

export async function renderRoom(root: HTMLElement, channel: string) {
  const prefs = loadPrefs();
  const canScreenShare = typeof navigator.mediaDevices?.getDisplayMedia === 'function';
  const obsIdentity = `${getUser()?.username ?? ''}-obs`;
  let isOwner = false;

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

  // ---- LiveKit ----
  const room = new Room();
  let roomDisconnected = false;
  let leaving = false;
  const tiles = new Map<string, HTMLDivElement>(); // `${identity}:${source}` -> 视频卡片
  const audioTiles = new Map<string, HTMLDivElement>(); // identity -> 无视频参与者的音频块
  const audioEls = new Map<string, Set<SinkMedia>>();
  const volumes = new Map<string, number>(); // identity -> 本机静音 0/1
  let deafened = false;
  const speakingSet = new Set<string>();

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
      if (key.endsWith(`:${Track.Source.ScreenShare}`)) return key;
    }
    if (pinnedKey && merged.has(pinnedKey)) return pinnedKey;
    if (lastSpeaker) {
      for (const cand of [`${lastSpeaker}:${Track.Source.Camera}`, `${lastSpeaker}:audio-tile`]) {
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

  function labelHtml(p: Participant, source: Track.Source, isLocal: boolean): string {
    const name = isLocal && source === Track.Source.Camera ? '你' : displayName(p);
    const suffix = source === Track.Source.ScreenShare ? '的投屏' : '';
    return `${avatarHtml(usernameOf(p), 'avatar avatar-sm')}<span>${esc(name)}${suffix}</span>`;
  }

  function addTile(p: Participant, track: Track, isLocal: boolean) {
    if (track.kind === Track.Kind.Audio) {
      if (isLocal) return; // 本地麦克风不回放
      const el = track.attach() as SinkMedia;
      let set = audioEls.get(p.identity);
      if (!set) {
        set = new Set();
        audioEls.set(p.identity, set);
      }
      set.add(el);
      audioBin.appendChild(el);
      applyAudioPrefs();
      return;
    }
    const key = `${p.identity}:${track.source}`;
    if (tiles.has(key)) return;
    const tile = document.createElement('div');
    tile.className = 'tile' + (track.source === Track.Source.ScreenShare ? ' tile-screen' : '');
    const video = track.attach() as IOSVideo;
    video.autoplay = true;
    if (video instanceof HTMLVideoElement) video.playsInline = true;
    if (isLocal) video.muted = true;
    if (isLocal && track.source === Track.Source.Camera && loadPrefs().mirror) video.style.transform = 'scaleX(-1)';

    const label = document.createElement('div');
    label.className = 'tile-label';
    label.innerHTML = labelHtml(p, track.source, isLocal);

    const badges = document.createElement('div');
    badges.className = 'tile-badges';
    if (track.source === Track.Source.ScreenShare) {
      const p2 = loadPrefs();
      const spec = isLocal ? `${p2.res} · ${p2.fps}fps · ${p2.bitrate.toFixed(1)}M` : '';
      badges.innerHTML = `<div class="live-badge">LIVE</div>${spec ? `<div class="spec-badge mono">${spec}</div>` : ''}`;
    }
    if (p.identity.endsWith('-obs')) {
      badges.innerHTML += '<div class="spec-badge">OBS · WHIP</div>';
    }

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
    if (typeof video.requestFullscreen !== 'function' && typeof video.webkitEnterFullscreen !== 'function') {
      fsBtn.hidden = true;
    }
    fsBtn.addEventListener('click', () => {
      if (typeof video.requestFullscreen === 'function') void video.requestFullscreen();
      else video.webkitEnterFullscreen?.();
    });

    tile.append(video, badges, label, pinIcon, fsBtn);
    gridEl.insertBefore(tile, railEl);
    tiles.set(key, tile);
    syncAudioTiles();
    applyLayout();
    refreshMeta();
  }

  function removeTile(p: Participant, track: Track) {
    if (track.kind === Track.Kind.Audio) {
      track.detach().forEach((el) => {
        el.remove();
        audioEls.get(p.identity)?.delete(el as SinkMedia);
      });
      return;
    }
    const key = `${p.identity}:${track.source}`;
    const tile = tiles.get(key);
    if (tile) {
      track.detach().forEach((el) => el.remove());
      tile.remove();
      tiles.delete(key);
      if (pinnedKey === key) pinnedKey = null;
      syncAudioTiles();
      applyLayout();
      refreshMeta();
    }
  }

  // 无视频参与者渲染音频块（头像大圆 + 名字 + 麦克风状态）
  function syncAudioTiles() {
    const ps = [room.localParticipant, ...room.remoteParticipants.values()];
    const want = new Set<string>();
    for (const p of ps) {
      const hasVideo = [...tiles.keys()].some((k) => k.startsWith(`${p.identity}:`));
      if (!hasVideo) want.add(p.identity);
    }
    // 移除多余
    for (const [identity, el] of audioTiles) {
      if (!want.has(identity)) {
        el.remove();
        audioTiles.delete(identity);
        if (pinnedKey === `${identity}:audio-tile`) pinnedKey = null;
      }
    }
    // 补充缺少 + 刷新状态
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
      const micPub = p.getTrackPublication(Track.Source.Microphone);
      const micOn = !!micPub && !micPub.isMuted;
      const isLocal = p.identity === room.localParticipant.identity;
      el.classList.toggle('speaking', speaking);
      el.classList.toggle('muted', !micOn && !isLocal);
      el.innerHTML = `
        ${avatarHtml(usernameOf(p), 'avatar avatar-xl' + (speaking ? ' speaking' : ''))}
        <div class="a-name">
          <span>${esc(isLocal ? '你' : displayName(p))}</span>
          ${micIcon(13, !micOn, !micOn ? 'var(--red)' : speaking ? 'var(--ember)' : 'var(--text-2)')}
        </div>
        ${p.identity.endsWith('-obs') ? '<div style="position:absolute;top:12px;right:12px" class="tag tag-ember mono">OBS 推流</div>' : ''}`;
    }
    statusEl.textContent = '';
    applyLayout(); // 聚焦布局下新建的块要归位（焦点/缩略轨），否则会被 CSS 隐藏
  }

  // ---- 成员面板 ----
  const membersBody = shell.content.querySelector<HTMLDivElement>('#members-body')!;
  const membersCount = shell.content.querySelector<HTMLSpanElement>('#members-count')!;

  function refreshMembers() {
    const ps = [room.localParticipant, ...room.remoteParticipants.values()];
    // 按用户名聚合（同账号多设备）
    const byUser = new Map<string, Participant[]>();
    for (const p of ps) {
      const u = usernameOf(p);
      if (!byUser.has(u)) byUser.set(u, []);
      byUser.get(u)!.push(p);
    }
    membersCount.textContent = String(byUser.size);
    const ownerName = ownerUsername;
    membersBody.innerHTML = `
      <div>
        <div class="side-section-title">在房 — ${byUser.size}</div>
        ${[...byUser.entries()]
          .map(([uname, plist]) => {
            const isMe = uname === (getUser()?.username ?? '');
            const speaking = plist.some((p) => speakingSet.has(p.identity));
            const sharing = plist.some((p) => !!p.getTrackPublication(Track.Source.ScreenShare));
            const obs = plist.some((p) => p.identity.endsWith('-obs'));
            const muted = plist.every((p) => {
              const pub = p.getTrackPublication(Track.Source.Microphone);
              return !pub || pub.isMuted;
            });
            const localMuted = plist.some((p) => volumeFor(p.identity) === 0);
            const statusBits = [
              sharing ? '投屏中' : '',
              obs ? 'OBS 推流' : '',
              speaking ? '说话中' : muted ? '已静音' : '',
              plist.length > 1 ? `${plist.length} 台设备` : '',
              localMuted && !isMe ? '已本地静音' : '',
            ].filter(Boolean);
            return `
          <div class="member-row ${uname === ownerName ? 'owner-row' : ''}">
            ${avatarHtml(uname, 'avatar' + (speaking ? ' speaking' : ''))}
            <div style="flex-grow:1;min-width:0">
              <div class="m-name">${esc(uname)}${isMe ? '<span class="muted">（我）</span>' : ''}
                ${uname === ownerName ? '<span class="tag tag-ember">房主</span>' : ''}
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
        const targets = [...room.remoteParticipants.values()].filter((p) => usernameOf(p) === uname);
        const nowMuted = targets.some((p) => volumeFor(p.identity) === 0);
        targets.forEach((p) => volumes.set(p.identity, nowMuted ? 1 : 0));
        applyAudioPrefs();
        refreshMembers();
      });
    });
  }

  function refreshMeta() {
    const n = new Set(
      [room.localParticipant, ...room.remoteParticipants.values()].map((p) => usernameOf(p)),
    ).size;
    const screens = [...tiles.keys()].filter((k) => k.endsWith(`:${Track.Source.ScreenShare}`)).length;
    roomMetaEl.textContent = `${n} 人在房${screens ? ` · ${screens} 路投屏` : ''}`;
  }

  // 房主探测
  let ownerUsername = '';
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

  room
    .on(RoomEvent.TrackSubscribed, (track: RemoteTrack, _pub, participant: RemoteParticipant) =>
      addTile(participant, track, false),
    )
    .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack, _pub, participant: RemoteParticipant) =>
      removeTile(participant, track),
    )
    .on(RoomEvent.TrackMuted, (pub, participant) => {
      if (pub.track && pub.kind === Track.Kind.Video) removeTile(participant, pub.track);
      if (pub.source === Track.Source.Microphone) {
        syncAudioTiles();
        refreshMembers();
      }
    })
    .on(RoomEvent.TrackUnmuted, (pub, participant) => {
      if (pub.track && pub.kind === Track.Kind.Video) {
        addTile(participant, pub.track, participant.identity === room.localParticipant.identity);
      }
      if (pub.source === Track.Source.Microphone) {
        lastSpeaker = participant.identity;
        syncAudioTiles();
        refreshMembers();
        applyLayout();
      }
    })
    .on(RoomEvent.ActiveSpeakersChanged, (speakers) => {
      speakingSet.clear();
      speakers.forEach((s) => speakingSet.add(s.identity));
      if (speakers[0]) lastSpeaker = speakers[0].identity;
      syncAudioTiles();
      refreshMembers();
      applyLayout();
    })
    .on(RoomEvent.LocalTrackPublished, (pub) => {
      if (pub.track) addTile(room.localParticipant, pub.track, true);
      refreshButtons();
      refreshMembers();
    })
    .on(RoomEvent.LocalTrackUnpublished, (pub) => {
      if (pub.track) removeTile(room.localParticipant, pub.track);
      refreshButtons();
      refreshMembers();
    })
    .on(RoomEvent.ParticipantConnected, () => {
      syncAudioTiles();
      refreshMembers();
      refreshMeta();
    })
    .on(RoomEvent.ParticipantDisconnected, () => {
      syncAudioTiles();
      refreshMembers();
      refreshMeta();
    })
    .on(RoomEvent.Disconnected, () => {
      roomDisconnected = true;
      statusEl.textContent = '连接已断开';
      shell.setConn(false, '连接已断开');
    });

  // ---- 麦克风采集与发布 ----
  const rnnoisePipe = new RnnoisePipeline();
  let rnnoiseBroken = false;
  document.addEventListener('pointerdown', () => void rnnoisePipe.resume(), false);

  function micCaptureOptions(): AudioCaptureOptions {
    const p = loadPrefs();
    const music = p.musicMode;
    return {
      deviceId: p.micDeviceId ? { ideal: p.micDeviceId } : undefined,
      echoCancellation: music ? false : p.echoCancellation,
      noiseSuppression: music ? false : p.denoise === 'browser',
      autoGainControl: music ? false : p.autoGainControl,
    };
  }

  function micPublishOptions(): TrackPublishOptions {
    return { audioPreset: { maxBitrate: loadPrefs().voiceBitrate } };
  }

  async function enableMic() {
    const p = loadPrefs();
    if (p.denoise === 'rnnoise' && !p.musicMode && !rnnoiseBroken) {
      try {
        const raw = await navigator.mediaDevices.getUserMedia({ audio: micCaptureOptions() });
        const processed = await rnnoisePipe.start(raw);
        await room.localParticipant.publishTrack(processed, {
          ...micPublishOptions(),
          source: Track.Source.Microphone,
        });
        return;
      } catch (err) {
        console.warn('RNNoise 不可用，回退浏览器内置处理:', err);
        rnnoiseBroken = true;
        await rnnoisePipe.stop();
      }
    }
    await room.localParticipant.setMicrophoneEnabled(true, micCaptureOptions(), micPublishOptions());
  }

  async function disableMic() {
    await room.localParticipant.setMicrophoneEnabled(false);
    await rnnoisePipe.stop();
  }

  // ---- 控制按钮 ----
  const btnMic = shell.content.querySelector<HTMLButtonElement>('#btn-mic')!;
  const btnDeaf = shell.content.querySelector<HTMLButtonElement>('#btn-deaf')!;
  const btnCamera = shell.content.querySelector<HTMLButtonElement>('#btn-camera')!;
  const btnScreen = shell.content.querySelector<HTMLButtonElement>('#btn-screen')!;

  let micOn = prefs.mic;
  let cameraOn = prefs.camera;
  let screenOn = false;

  function refreshButtons() {
    btnMic.classList.toggle('on', micOn);
    btnMic.innerHTML = `${micIcon(17, !micOn, 'currentColor')}<span class="pill-label">${micOn ? '麦克风' : '已静音'}</span>`;
    btnDeaf.classList.toggle('on', false);
    btnDeaf.classList.toggle('danger', deafened);
    btnDeaf.innerHTML = slashIcon('speaker', 17, deafened, deafened ? 'var(--red)' : 'currentColor');
    btnCamera.classList.toggle('on', cameraOn);
    btnCamera.innerHTML = slashIcon('camera', 17, !cameraOn, 'currentColor');
    btnScreen.classList.toggle('on', screenOn);
    btnScreen.innerHTML = `${icon('screen', 17, 'currentColor')}<span class="pill-label">${screenOn ? '投屏中' : '投屏'}</span>`;
  }
  refreshButtons();

  btnMic.addEventListener('click', async () => {
    micOn = !micOn;
    refreshButtons();
    try {
      if (micOn) await enableMic();
      else await disableMic();
      const p = loadPrefs();
      p.mic = micOn;
      savePrefs(p);
    } catch {
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
    cameraOn = !cameraOn;
    refreshButtons();
    try {
      const p = loadPrefs();
      await room.localParticipant.setCameraEnabled(
        cameraOn,
        cameraOn && p.camDeviceId ? { deviceId: { ideal: p.camDeviceId } } : undefined,
      );
      p.camera = cameraOn;
      savePrefs(p);
    } catch {
      cameraOn = !cameraOn;
      refreshButtons();
    }
  });

  btnScreen.addEventListener('click', async () => {
    screenOn = !screenOn;
    refreshButtons();
    try {
      const p = loadPrefs();
      const d = RES_DIMS[p.res];
      const capture: ScreenShareCaptureOptions = {
        resolution: { width: d.width, height: d.height, frameRate: p.fps },
        contentHint: 'detail',
      };
      const publish: TrackPublishOptions = {
        videoCodec: 'h264',
        screenShareEncoding: {
          maxBitrate: Math.round(p.bitrate * 1e6),
          maxFramerate: p.fps,
        },
        // 单层：浏览器投屏只有软编，simulcast 双层会把 CPU 拖垮
        screenShareSimulcastLayers: [],
      };
      await room.localParticipant.setScreenShareEnabled(
        screenOn,
        screenOn ? capture : undefined,
        screenOn ? publish : undefined,
      );
    } catch {
      screenOn = !screenOn;
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

  // 布局切换
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
    onClose: () => {
      if (leaving) return;
      if (roomDisconnected) {
        statusEl.textContent = '你已被移出该频道';
        toast('你已被移出该频道', 'bad');
        setTimeout(() => {
          location.hash = '#/lobby';
        }, 1500);
      } else {
        statusEl.textContent = '聊天连接已断开';
      }
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
      const key = `${room.localParticipant.identity}:${Track.Source.Camera}`;
      const video = tiles.get(key)?.querySelector('video');
      if (video) video.style.transform = loadPrefs().mirror ? 'scaleX(-1)' : '';
    }
    if (what === 'mic' && loadPrefs().mic !== micOn) {
      btnMic.click();
      return;
    }
    if ((what === 'mic-device' || what === 'audio-chain') && micOn) {
      // 开麦中换设备/处理链：重启采集
      try {
        await disableMic();
        await enableMic();
      } catch {
        micOn = false;
        refreshButtons();
      }
    }
    if (what === 'cam-device' && cameraOn) {
      const p = loadPrefs();
      if (p.camDeviceId) void room.switchActiveDevice('videoinput', p.camDeviceId).catch(() => {});
    }
  };
  prefsBus.addEventListener('prefs', onPrefs);

  // ---- 连接与清理 ----
  applyPanel();
  try {
    const cred = await fetchLiveKitToken(channel);
    statusEl.textContent = '正在加入房间…';
    await room.connect(cred.url || LIVEKIT_URL_FALLBACK, cred.token);
    statusEl.textContent = room.state === ConnectionState.Connected ? '' : '连接中…';
    shell.setConn(true, `LiveKit · ${new URL(cred.url || LIVEKIT_URL_FALLBACK).host}`);
    syncAudioTiles();
    refreshMembers();
    refreshMeta();
    if (prefs.mic) {
      try {
        await enableMic();
        micOn = true;
      } catch {
        micOn = false;
      }
    }
    if (prefs.camera) {
      try {
        const p = loadPrefs();
        await room.localParticipant.setCameraEnabled(
          true,
          p.camDeviceId ? { deviceId: { ideal: p.camDeviceId } } : undefined,
        );
        cameraOn = true;
      } catch {
        cameraOn = false;
      }
    }
    refreshButtons();
  } catch (err) {
    statusEl.textContent = `连接失败：${(err as Error).message}`;
    shell.setConn(false, '连接失败');
  }

  const myHash = location.hash;
  const onHashChange = () => {
    if (location.hash !== myHash) {
      window.removeEventListener('hashchange', onHashChange);
      prefsBus.removeEventListener('prefs', onPrefs);
      leaving = true;
      chat.close();
      void rnnoisePipe.stop();
      room.disconnect();
      shell.destroy();
    }
  };
  window.addEventListener('hashchange', onHashChange);
  shell.content.querySelector('#btn-leave')!.addEventListener('click', () => {
    location.hash = '#/lobby';
  });
}
