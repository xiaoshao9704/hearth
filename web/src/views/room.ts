// 房间页（核心）：LiveKit 音视频 + 高码率投屏 + 可收起文字聊天。
// 发布控制：投屏画质三控件（分辨率/帧率/码率）、RNNoise 降噪与音频处理开关、
// 麦克风设备选择与语音码率、收听侧本地静音；全部持久化到 hearth_room_prefs。
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
import {
  LIVEKIT_URL_FALLBACK,
  addMember,
  banUser,
  fetchLiveKitToken,
  getIngress,
  getUser,
  kickUser,
  listBans,
  listChannels,
  listMembers,
  removeMember,
  resetIngress,
  setInviteOnly,
  unbanUser,
} from '../api';
import { connectChat } from '../chat';
import type { ChatMessage } from '../chat';
import { RnnoisePipeline, listAudioInputs } from '../audio';

// ---- 投屏画质：分辨率 × 帧率，码率默认按 bpp 模型推导（宽×高×帧率×0.07）----
const RES_DIMS: Record<string, { width: number; height: number }> = {
  '1080p': { width: 1920, height: 1080 },
  '720p': { width: 1280, height: 720 },
};
const FPS_OPTIONS = [60, 30, 15];
const VOICE_BITRATES = [32000, 64000, 96000, 128000]; // bps

function autoBitrate(res: string, fps: number): number {
  const d = RES_DIMS[res] ?? RES_DIMS['1080p'];
  return Math.round(((d.width * d.height * fps * 0.07) / 1e6) * 10) / 10; // Mbps，保留 1 位小数
}

// ---- 房间选项持久化 ----
interface RoomPrefs {
  mic: boolean;
  camera: boolean;
  layout: 'grid' | 'spotlight';
  res: string; // 投屏分辨率档
  fps: number; // 投屏帧率档
  bitrate: number; // 投屏码率 Mbps
  bitrateAuto: boolean; // 码率是否跟随公式（false=用户手调过）
  rnnoise: boolean; // AI 降噪
  echoCancellation: boolean;
  noiseSuppression: boolean;
  autoGainControl: boolean;
  musicMode: boolean; // 一键关闭全部处理 + 语音 128k
  micDeviceId: string; // 空 = 默认设备
  voiceBitrate: number; // 语音码率 bps
}
const PREFS_KEY = 'hearth_room_prefs';

function defaultPrefs(): RoomPrefs {
  return {
    mic: false,
    camera: false,
    layout: 'grid',
    res: '1080p',
    fps: 60,
    bitrate: autoBitrate('1080p', 60),
    bitrateAuto: true,
    rnnoise: true,
    echoCancellation: true,
    noiseSuppression: true,
    autoGainControl: true,
    musicMode: false,
    micDeviceId: '',
    voiceBitrate: 64000,
  };
}

function loadPrefs(): RoomPrefs {
  const def = defaultPrefs();
  try {
    const raw = localStorage.getItem(PREFS_KEY);
    if (!raw) return def;
    const p = JSON.parse(raw) as Partial<RoomPrefs>;
    return {
      mic: p.mic === true,
      camera: p.camera === true,
      layout: p.layout === 'spotlight' ? 'spotlight' : 'grid',
      res: RES_DIMS[p.res ?? ''] ? (p.res as string) : def.res,
      fps: FPS_OPTIONS.includes(p.fps as number) ? (p.fps as number) : def.fps,
      bitrate: typeof p.bitrate === 'number' && p.bitrate >= 1 && p.bitrate <= 15 ? p.bitrate : def.bitrate,
      bitrateAuto: p.bitrateAuto !== false,
      rnnoise: p.rnnoise !== false,
      echoCancellation: p.echoCancellation !== false,
      noiseSuppression: p.noiseSuppression !== false,
      autoGainControl: p.autoGainControl !== false,
      musicMode: p.musicMode === true,
      micDeviceId: typeof p.micDeviceId === 'string' ? p.micDeviceId : '',
      voiceBitrate: VOICE_BITRATES.includes(p.voiceBitrate as number) ? (p.voiceBitrate as number) : def.voiceBitrate,
    };
  } catch {
    return def; // 解析失败用默认值
  }
}

function savePrefs(prefs: RoomPrefs) {
  localStorage.setItem(PREFS_KEY, JSON.stringify(prefs));
}

// iOS Safari 的私有全屏 API（iPhone 仅 video 元素可用）
type IOSVideo = HTMLVideoElement & { webkitEnterFullscreen?: () => void };
type IOSHTMLElement = HTMLElement & { webkitRequestFullscreen?: () => void };

// 显示名：token 里 name=用户名，identity 是 用户名-设备标签；name 为空回退 identity
const displayName = (p: Participant) => p.name || p.identity;

export async function renderRoom(root: HTMLElement, channel: string) {
  const isDesktop = window.matchMedia('(min-width: 1024px)').matches;
  const canScreenShare = typeof navigator.mediaDevices?.getDisplayMedia === 'function';
  // 整体全屏：iPhone Safari 对普通元素不支持，不支持就隐藏按钮（不留死按钮）
  const docEl = document.documentElement as IOSHTMLElement;
  const canPageFullscreen =
    typeof docEl.requestFullscreen === 'function' ||
    typeof docEl.webkitRequestFullscreen === 'function';

  const prefs = loadPrefs();
  // 自己的 OBS 推流参与者 identity：{用户名}-obs（收听侧默认本地静音）
  const obsIdentity = `${getUser()?.username ?? ''}-obs`;
  // 房主判断（异步获取）：决定参与者行内管理按钮与频道设置入口
  let isOwner = false;

  root.innerHTML = `
    <div class="room ${isDesktop ? 'chat-open' : ''}">
      <header class="topbar">
        <div class="row topbar-left">
          <a href="#/channels" class="back">← 频道</a>
          <h1># ${channel}</h1>
        </div>
        <div class="row controls">
          <button id="btn-mic">麦克风：关</button>
          <button id="btn-camera">摄像头：关</button>
          <button id="btn-screen" ${canScreenShare ? '' : 'hidden'}>投屏：关</button>
          <button id="btn-settings">⚙ 设置</button>
          <button id="btn-obs">OBS</button>
          <button id="btn-ch-settings" hidden>频道设置</button>
          <button id="btn-layout">布局：${prefs.layout === 'spotlight' ? '聚焦' : '九宫格'}</button>
          <button id="btn-pagefs" ${canPageFullscreen ? '' : 'hidden'}>全屏</button>
          <button id="btn-chat">💬<span id="chat-badge" class="badge" hidden>0</span></button>
          <button id="btn-leave">离开</button>
        </div>
      </header>
      <div id="settings-panel" class="float-panel card" hidden>
        <h2>投屏画质（下次投屏生效）</h2>
        <div class="row">
          <label>分辨率</label>
          <select id="set-res">
            <option value="1080p">1080p</option>
            <option value="720p">720p</option>
          </select>
          <label>帧率</label>
          <select id="set-fps">
            <option value="60">60</option>
            <option value="30">30</option>
            <option value="15">15</option>
          </select>
        </div>
        <div class="row">
          <label>码率</label>
          <input type="range" id="set-bitrate" min="1" max="15" step="0.5" />
          <span id="bitrate-label" class="muted"></span>
          <button id="bitrate-auto" class="mini" hidden>回自动</button>
        </div>
        <h2>麦克风</h2>
        <div class="row">
          <label>设备</label>
          <select id="set-micdev"><option value="">默认设备</option></select>
        </div>
        <div class="row">
          <label>语音码率</label>
          <select id="set-vbr">
            <option value="32000">32k</option>
            <option value="64000">64k</option>
            <option value="96000">96k</option>
            <option value="128000">128k</option>
          </select>
        </div>
        <h2>音频处理（下次开麦生效）</h2>
        <label class="check"><input type="checkbox" id="set-rnnoise" /> AI 降噪(RNNoise)</label>
        <label class="check"><input type="checkbox" id="set-ec" /> 回声消除</label>
        <label class="check"><input type="checkbox" id="set-ns" /> 降噪</label>
        <label class="check"><input type="checkbox" id="set-agc" /> 自动增益</label>
        <label class="check"><input type="checkbox" id="set-music" /> 音乐模式（关闭全部处理 + 128k）</label>
      </div>
      <div id="obs-panel" class="float-panel card" hidden>
        <h2>OBS 推流（WHIP）</h2>
        <div class="row">
          <input id="obs-url" readonly placeholder="获取中…" />
          <button id="obs-copy">复制</button>
        </div>
        <p class="muted obs-hint">OBS → 设置 → 直播 → 服务选「WHIP」→ 粘贴地址，Bearer Token 留空。</p>
        <div class="row">
          <button id="obs-reset">重置推流地址</button>
          <span id="obs-msg" class="muted"></span>
        </div>
        <p id="obs-error" class="error"></p>
      </div>
      <div id="ch-settings-panel" class="float-panel card" hidden>
        <h2>频道设置</h2>
        <label class="check"><input type="checkbox" id="ch-invite-only" /> 邀请制（仅房主与白名单可进）</label>
        <h2>成员（白名单）</h2>
        <form id="member-add-form" class="row">
          <input id="member-name" placeholder="用户名" autocomplete="off" />
          <button type="submit">添加</button>
        </form>
        <ul id="member-list" class="manage-list"></ul>
        <h2>黑名单</h2>
        <ul id="ban-list" class="manage-list"></ul>
        <p id="ch-error" class="error"></p>
      </div>
      <div class="room-body">
        <main class="stage">
          <p id="status" class="muted">正在连接…</p>
          <p id="empty-hint" class="muted" hidden>暂无视频画面，纯语音进行中</p>
          <div id="video-grid" class="video-grid ${prefs.layout === 'spotlight' ? 'spotlight' : ''}"></div>
          <div id="audio-bin" style="display:none"></div>
        </main>
        <aside class="sidebar">
          <section class="participants">
            <h2>参与者</h2>
            <ul id="participant-list" class="participant-list"></ul>
          </section>
          <section class="chat">
            <h2>聊天</h2>
            <div id="chat-log" class="chat-log"></div>
            <form id="chat-form" class="row">
              <input id="chat-input" placeholder="发送消息…" autocomplete="off" />
              <button type="submit" class="primary">发送</button>
            </form>
          </section>
        </aside>
      </div>
    </div>
  `;

  const roomEl = root.querySelector<HTMLDivElement>('.room')!;
  const statusEl = root.querySelector<HTMLParagraphElement>('#status')!;
  const emptyHint = root.querySelector<HTMLParagraphElement>('#empty-hint')!;
  const gridEl = root.querySelector<HTMLDivElement>('#video-grid')!;
  const audioBin = root.querySelector<HTMLDivElement>('#audio-bin')!;
  const listEl = root.querySelector<HTMLUListElement>('#participant-list')!;

  // ---- LiveKit ----
  const room = new Room();
  let roomDisconnected = false; // LiveKit 已断开（被踢/被封/掉线）
  let leaving = false; // 自己主动离开房间（路由切换），用于区分"被移出"
  const tiles = new Map<string, HTMLDivElement>(); // `${identity}:${source}` -> 视频卡片
  // 收听侧：远端音频元素按参与者归类，本地静音只改本机 volume，不影响他人
  const audioEls = new Map<string, Set<HTMLMediaElement>>();
  const volumes = new Map<string, number>(); // identity -> 本机音量 0/1
  const volumeFor = (identity: string) =>
    volumes.get(identity) ?? (identity === obsIdentity ? 0 : 1); // 自己的 OBS 推流默认本地静音

  const sourceLabel = (source: Track.Source) =>
    source === Track.Source.ScreenShare ? '屏幕' : source === Track.Source.Camera ? '摄像头' : '音频';

  const refreshEmptyHint = () => {
    emptyHint.hidden = tiles.size > 0;
  };

  // ---- 聚焦布局的焦点选择：投屏 > pin > 发言人（最近活跃/最近 unmute）> 第一块 ----
  let pinnedKey: string | null = null;
  let lastSpeaker: string | null = null;

  function pickFocusKey(): string | null {
    if (tiles.size === 0) return null;
    for (const key of tiles.keys()) {
      if (key.endsWith(`:${Track.Source.ScreenShare}`)) return key; // 有人投屏自动聚焦
    }
    if (pinnedKey && tiles.has(pinnedKey)) return pinnedKey;
    if (lastSpeaker) {
      const key = `${lastSpeaker}:${Track.Source.Camera}`;
      if (tiles.has(key)) return key;
    }
    return tiles.keys().next().value ?? null;
  }

  function applyFocus() {
    if (prefs.layout !== 'spotlight') return;
    const focusKey = pickFocusKey();
    tiles.forEach((tile, key) => tile.classList.toggle('featured', key === focusKey));
  }

  function togglePin(key: string) {
    pinnedKey = pinnedKey === key ? null : key;
    tiles.forEach((tile, k) => tile.classList.toggle('pinned', k === pinnedKey));
    applyFocus();
  }

  function addTile(p: Participant, track: Track, isLocal: boolean) {
    if (track.kind === Track.Kind.Audio) {
      if (isLocal) return; // 本地麦克风不回放（避免自听回授）
      // 音频挂到隐藏容器播放，按参与者归类以支持本地静音
      const el = track.attach();
      el.volume = volumeFor(p.identity);
      let set = audioEls.get(p.identity);
      if (!set) {
        set = new Set();
        audioEls.set(p.identity, set);
      }
      set.add(el);
      audioBin.appendChild(el);
      return;
    }
    const key = `${p.identity}:${track.source}`; // 同一账号多设备 identity 不同，不会冲突
    if (tiles.has(key)) return;
    const name = displayName(p);
    const tile = document.createElement('div');
    tile.className = 'tile' + (track.source === Track.Source.ScreenShare ? ' tile-screen' : '');
    const video = track.attach() as IOSVideo;
    video.autoplay = true;
    if (video instanceof HTMLVideoElement) video.playsInline = true;
    if (isLocal) video.muted = true; // 本地画面静音避免回授

    const label = document.createElement('span');
    label.className = 'tile-label';
    label.textContent = `${name} · ${sourceLabel(track.source)}`;

    // pin 状态图标（仅 .pinned 时可见）
    const pinIcon = document.createElement('span');
    pinIcon.className = 'tile-pin';
    pinIcon.textContent = '📌';

    // 点击窗口 pin/取消 pin（全屏按钮除外）
    tile.addEventListener('click', (ev) => {
      if ((ev.target as HTMLElement).closest('.tile-fs')) return;
      togglePin(key);
    });

    // 单窗口全屏：iPhone 用 webkitEnterFullscreen 兜底，都不支持则不显示按钮
    const fsBtn = document.createElement('button');
    fsBtn.className = 'tile-fs';
    fsBtn.textContent = '⛶';
    fsBtn.title = '全屏';
    if (typeof video.requestFullscreen !== 'function' && typeof video.webkitEnterFullscreen !== 'function') {
      fsBtn.hidden = true;
    }
    fsBtn.addEventListener('click', () => {
      if (typeof video.requestFullscreen === 'function') {
        void video.requestFullscreen();
      } else {
        video.webkitEnterFullscreen?.();
      }
    });

    tile.append(video, label, pinIcon, fsBtn);
    gridEl.appendChild(tile);
    tiles.set(key, tile);
    refreshEmptyHint();
    applyFocus();
  }

  function removeTile(p: Participant, track: Track) {
    if (track.kind === Track.Kind.Audio) {
      track.detach().forEach((el) => {
        el.remove();
        audioEls.get(p.identity)?.delete(el);
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
      refreshEmptyHint();
      applyFocus();
    }
  }

  // 麦克风状态图标：无麦克风 track 或已静音视为闭麦
  const micIcon = (p: Participant) => {
    const pub = p.getTrackPublication(Track.Source.Microphone);
    return pub && !pub.isMuted ? '🎤' : '🔇';
  };

  function refreshParticipants() {
    const ps = [room.localParticipant, ...room.remoteParticipants.values()];
    listEl.innerHTML = ps
      .map((p: Participant) => {
        const name = displayName(p);
        const isLocal = p.identity === room.localParticipant.identity;
        const me = isLocal ? '（我）' : '';
        let extra = '';
        if (!isLocal) {
          const v = volumeFor(p.identity);
          const tag =
            p.identity === obsIdentity && v === 0
              ? '<span class="tag">已本地静音(自己的推流)</span>'
              : '';
          extra = `${tag}<button class="mini" data-mute="${p.identity}">${v > 0 ? '本地静音' : '恢复'}</button>`;
          // 房主管理：identity 规则 {用户名}-{设备标签}，取首段为用户名
          if (isOwner) {
            const uname = p.identity.split('-')[0];
            extra += `<button class="mini" data-kick="${uname}">踢出</button><button class="mini" data-ban="${uname}">封禁</button>`;
          }
        }
        return `<li><span class="mic">${micIcon(p)}</span> ${name}${me} ${extra}</li>`;
      })
      .join('');
  }

  // 参与者列表按钮（事件委托）：本地静音/恢复，房主的踢出/封禁
  listEl.addEventListener('click', (ev) => {
    const el = ev.target as HTMLElement;
    const muteBtn = el.closest<HTMLButtonElement>('button[data-mute]');
    if (muteBtn) {
      const id = muteBtn.dataset.mute!;
      const v = volumeFor(id) > 0 ? 0 : 1;
      volumes.set(id, v);
      audioEls.get(id)?.forEach((el) => {
        el.volume = v;
      });
      refreshParticipants();
      return;
    }
    const kickBtn = el.closest<HTMLButtonElement>('button[data-kick]');
    if (kickBtn) {
      const uname = kickBtn.dataset.kick!;
      if (confirm(`确定将 ${uname} 踢出频道？（可重新加入）`)) {
        kickUser(channel, uname).catch((err) => {
          statusEl.textContent = (err as Error).message;
        });
      }
      return;
    }
    const banBtn = el.closest<HTMLButtonElement>('button[data-ban]');
    if (banBtn) {
      const uname = banBtn.dataset.ban!;
      if (confirm(`确定封禁 ${uname}？将被立即移出且无法再进入本频道`)) {
        banUser(channel, uname).catch((err) => {
          statusEl.textContent = (err as Error).message;
        });
      }
    }
  });

  // 房主探测：拿到后显示频道设置入口并刷新参与者列表（补管理按钮）
  const btnChSettings = root.querySelector<HTMLButtonElement>('#btn-ch-settings')!;
  void listChannels()
    .then((chs) => {
      const ch = chs.find((c) => c.name === channel);
      isOwner = ch?.is_owner === true;
      if (isOwner) {
        btnChSettings.hidden = false;
        refreshParticipants();
      }
    })
    .catch(() => {});

  room
    .on(RoomEvent.TrackSubscribed, (track: RemoteTrack, _pub, participant: RemoteParticipant) =>
      addTile(participant, track, false),
    )
    .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack, _pub, participant: RemoteParticipant) =>
      removeTile(participant, track),
    )
    // 视频静音即移除画面（无视频不占位），取消静音再插回网格；
    // 音频不随 mute 摘挂（mute 只是静音，unpublish 才摘除），否则 unmute 后无法恢复播放
    .on(RoomEvent.TrackMuted, (pub, participant) => {
      if (pub.track && pub.kind === Track.Kind.Video) removeTile(participant, pub.track);
      if (pub.source === Track.Source.Microphone) refreshParticipants();
    })
    .on(RoomEvent.TrackUnmuted, (pub, participant) => {
      if (pub.track && pub.kind === Track.Kind.Video) {
        addTile(participant, pub.track, participant.identity === room.localParticipant.identity);
      }
      if (pub.source === Track.Source.Microphone) {
        lastSpeaker = participant.identity; // 最近 unmute 也算发言候选
        refreshParticipants();
        applyFocus();
      }
    })
    .on(RoomEvent.ActiveSpeakersChanged, (speakers) => {
      if (speakers[0]) {
        lastSpeaker = speakers[0].identity;
        applyFocus();
      }
    })
    .on(RoomEvent.LocalTrackPublished, (pub) => {
      if (pub.track) addTile(room.localParticipant, pub.track, true);
      refreshButtons();
    })
    .on(RoomEvent.LocalTrackUnpublished, (pub) => {
      if (pub.track) removeTile(room.localParticipant, pub.track);
      refreshButtons();
    })
    .on(RoomEvent.ParticipantConnected, refreshParticipants)
    .on(RoomEvent.ParticipantDisconnected, refreshParticipants)
    .on(RoomEvent.Disconnected, () => {
      roomDisconnected = true; // 配合聊天 WS 关闭判断"被移出频道"
      statusEl.textContent = '连接已断开';
    });

  // ---- 麦克风采集与发布：RNNoise 管线 / 浏览器内置处理 ----
  const rnnoisePipe = new RnnoisePipeline();
  let rnnoiseBroken = false; // RNNoise 加载失败后置灰开关并回退

  // AudioContext 自动播放策略：用户首次点击页面时恢复
  document.addEventListener('pointerdown', () => void rnnoisePipe.resume(), false);

  function micCaptureOptions(): AudioCaptureOptions {
    return {
      deviceId: prefs.micDeviceId ? { ideal: prefs.micDeviceId } : undefined,
      echoCancellation: prefs.echoCancellation,
      noiseSuppression: prefs.noiseSuppression,
      autoGainControl: prefs.autoGainControl,
    };
  }

  function micPublishOptions(): TrackPublishOptions {
    return { audioPreset: { maxBitrate: prefs.voiceBitrate } };
  }

  async function enableMic() {
    if (prefs.rnnoise && !rnnoiseBroken) {
      try {
        const raw = await navigator.mediaDevices.getUserMedia({ audio: micCaptureOptions() });
        const processed = await rnnoisePipe.start(raw);
        await room.localParticipant.publishTrack(processed, {
          ...micPublishOptions(),
          source: Track.Source.Microphone,
        });
        return;
      } catch (err) {
        // RNNoise 加载失败（wasm/worklet 不可用等）：置灰开关，回退普通路径
        console.warn('RNNoise 不可用，回退浏览器内置处理:', err);
        rnnoiseBroken = true;
        syncSettingsUI();
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
  const btnMic = root.querySelector<HTMLButtonElement>('#btn-mic')!;
  const btnCamera = root.querySelector<HTMLButtonElement>('#btn-camera')!;
  const btnScreen = root.querySelector<HTMLButtonElement>('#btn-screen')!;
  const btnPageFs = root.querySelector<HTMLButtonElement>('#btn-pagefs')!;
  const btnLayout = root.querySelector<HTMLButtonElement>('#btn-layout')!;

  let micOn = prefs.mic;
  let cameraOn = prefs.camera;
  let screenOn = false;

  function refreshButtons() {
    btnMic.textContent = `麦克风：${micOn ? '开' : '关'}`;
    btnMic.classList.toggle('primary', micOn);
    btnCamera.textContent = `摄像头：${cameraOn ? '开' : '关'}`;
    btnCamera.classList.toggle('primary', cameraOn);
    btnScreen.textContent = `投屏：${screenOn ? '开' : '关'}`;
    btnScreen.classList.toggle('primary', screenOn);
  }
  refreshButtons();

  btnMic.addEventListener('click', async () => {
    micOn = !micOn;
    try {
      if (micOn) {
        await enableMic();
        void refreshMicDevices(); // 授权后能拿到设备名称，刷新下拉
      } else {
        await disableMic();
      }
      prefs.mic = micOn;
      savePrefs(prefs);
    } catch {
      micOn = !micOn;
    }
    refreshButtons();
  });

  btnCamera.addEventListener('click', async () => {
    cameraOn = !cameraOn;
    try {
      await room.localParticipant.setCameraEnabled(cameraOn);
      prefs.camera = cameraOn;
      savePrefs(prefs);
    } catch {
      cameraOn = !cameraOn;
    }
    refreshButtons();
  });

  btnScreen.addEventListener('click', async () => {
    screenOn = !screenOn;
    try {
      // 开投屏时按画质档应用采集参数与发布参数（发布参数必须走第三参数，塞采集参数里会被 SDK 静默忽略）
      const d = RES_DIMS[prefs.res];
      const capture: ScreenShareCaptureOptions = {
        resolution: { width: d.width, height: d.height, frameRate: prefs.fps },
        contentHint: 'detail', // 屏幕内容以文字/细节为主
      };
      const publish: TrackPublishOptions = {
        videoCodec: 'h264',
        screenShareEncoding: {
          maxBitrate: Math.round(prefs.bitrate * 1e6),
          maxFramerate: prefs.fps,
        },
        // 单层:浏览器投屏只有软编(OpenH264),simulcast 双层会把 CPU 拖垮导致帧率腰斩;
        // 代价是弱网观众失去低清自适应层
        screenShareSimulcastLayers: [],
      };
      await room.localParticipant.setScreenShareEnabled(
        screenOn,
        screenOn ? capture : undefined,
        screenOn ? publish : undefined,
      );
    } catch {
      screenOn = !screenOn; // 用户取消选择等情况回退
    }
    refreshButtons();
  });

  // 布局切换：九宫格 / 聚焦
  btnLayout.addEventListener('click', () => {
    prefs.layout = prefs.layout === 'spotlight' ? 'grid' : 'spotlight';
    savePrefs(prefs);
    gridEl.classList.toggle('spotlight', prefs.layout === 'spotlight');
    btnLayout.textContent = `布局：${prefs.layout === 'spotlight' ? '聚焦' : '九宫格'}`;
    if (prefs.layout === 'spotlight') {
      applyFocus();
    } else {
      tiles.forEach((tile) => tile.classList.remove('featured'));
    }
  });

  // 整体页面全屏（不支持时按钮已隐藏）
  btnPageFs.addEventListener('click', () => {
    if (document.fullscreenElement) {
      void document.exitFullscreen();
    } else if (typeof docEl.requestFullscreen === 'function') {
      void docEl.requestFullscreen();
    } else {
      docEl.webkitRequestFullscreen?.();
    }
  });
  document.addEventListener('fullscreenchange', () => {
    btnPageFs.textContent = document.fullscreenElement ? '退出全屏' : '全屏';
  });

  // ---- 设置面板 ----
  const settingsPanel = root.querySelector<HTMLDivElement>('#settings-panel')!;
  const resSelect = root.querySelector<HTMLSelectElement>('#set-res')!;
  const fpsSelect = root.querySelector<HTMLSelectElement>('#set-fps')!;
  const bitrateSlider = root.querySelector<HTMLInputElement>('#set-bitrate')!;
  const bitrateLabel = root.querySelector<HTMLSpanElement>('#bitrate-label')!;
  const bitrateAutoBtn = root.querySelector<HTMLButtonElement>('#bitrate-auto')!;
  const micDevSelect = root.querySelector<HTMLSelectElement>('#set-micdev')!;
  const vbrSelect = root.querySelector<HTMLSelectElement>('#set-vbr')!;
  const rnnoiseCheck = root.querySelector<HTMLInputElement>('#set-rnnoise')!;
  const ecCheck = root.querySelector<HTMLInputElement>('#set-ec')!;
  const nsCheck = root.querySelector<HTMLInputElement>('#set-ns')!;
  const agcCheck = root.querySelector<HTMLInputElement>('#set-agc')!;
  const musicCheck = root.querySelector<HTMLInputElement>('#set-music')!;

  function syncSettingsUI() {
    resSelect.value = prefs.res;
    fpsSelect.value = String(prefs.fps);
    bitrateSlider.value = String(prefs.bitrate);
    bitrateLabel.textContent = `${prefs.bitrate.toFixed(1)} Mbps${prefs.bitrateAuto ? '（自动）' : ''}`;
    bitrateAutoBtn.hidden = prefs.bitrateAuto;
    vbrSelect.value = String(prefs.voiceBitrate);
    rnnoiseCheck.checked = prefs.rnnoise;
    rnnoiseCheck.disabled = rnnoiseBroken;
    rnnoiseCheck.title = rnnoiseBroken ? 'RNNoise 加载失败，已回退浏览器内置降噪' : '';
    ecCheck.checked = prefs.echoCancellation;
    nsCheck.checked = prefs.noiseSuppression;
    agcCheck.checked = prefs.autoGainControl;
    musicCheck.checked = prefs.musicMode;
    // 音乐模式锁定处理开关与语音码率
    ecCheck.disabled = nsCheck.disabled = agcCheck.disabled = vbrSelect.disabled = prefs.musicMode;
  }

  root.querySelector('#btn-settings')!.addEventListener('click', () => {
    settingsPanel.hidden = !settingsPanel.hidden;
  });

  resSelect.addEventListener('change', () => {
    prefs.res = resSelect.value;
    prefs.bitrate = autoBitrate(prefs.res, prefs.fps); // 改档码率回公式默认值
    prefs.bitrateAuto = true;
    savePrefs(prefs);
    syncSettingsUI();
  });
  fpsSelect.addEventListener('change', () => {
    prefs.fps = parseInt(fpsSelect.value, 10);
    prefs.bitrate = autoBitrate(prefs.res, prefs.fps);
    prefs.bitrateAuto = true;
    savePrefs(prefs);
    syncSettingsUI();
  });
  bitrateSlider.addEventListener('input', () => {
    prefs.bitrate = parseFloat(bitrateSlider.value);
    prefs.bitrateAuto = false; // 手调后偏离自动，显示"回自动"
    savePrefs(prefs);
    syncSettingsUI();
  });
  bitrateAutoBtn.addEventListener('click', () => {
    prefs.bitrate = autoBitrate(prefs.res, prefs.fps);
    prefs.bitrateAuto = true;
    savePrefs(prefs);
    syncSettingsUI();
  });

  // 麦克风设备：开麦中热切换（RNNoise 管线需重启采集），未开麦记为下次使用
  micDevSelect.addEventListener('change', async () => {
    prefs.micDeviceId = micDevSelect.value;
    savePrefs(prefs);
    if (!micOn) return;
    try {
      if (rnnoisePipe.active) {
        await disableMic();
        await enableMic();
      } else {
        await room.switchActiveDevice('audioinput', prefs.micDeviceId);
      }
    } catch {
      // 设备切换失败保持现状
    }
  });

  vbrSelect.addEventListener('change', () => {
    prefs.voiceBitrate = parseInt(vbrSelect.value, 10);
    savePrefs(prefs);
  });

  rnnoiseCheck.addEventListener('change', () => {
    prefs.rnnoise = rnnoiseCheck.checked;
    savePrefs(prefs);
  });
  ecCheck.addEventListener('change', () => {
    prefs.echoCancellation = ecCheck.checked;
    savePrefs(prefs);
  });
  nsCheck.addEventListener('change', () => {
    prefs.noiseSuppression = nsCheck.checked;
    savePrefs(prefs);
  });
  agcCheck.addEventListener('change', () => {
    prefs.autoGainControl = agcCheck.checked;
    savePrefs(prefs);
  });

  // 音乐模式：一键关闭全部处理 + 语音 128k（关闭时恢复默认三开 + 64k）
  musicCheck.addEventListener('change', () => {
    prefs.musicMode = musicCheck.checked;
    if (prefs.musicMode) {
      prefs.echoCancellation = prefs.noiseSuppression = prefs.autoGainControl = false;
      prefs.voiceBitrate = 128000;
    } else {
      prefs.echoCancellation = prefs.noiseSuppression = prefs.autoGainControl = true;
      prefs.voiceBitrate = 64000;
    }
    savePrefs(prefs);
    syncSettingsUI();
  });

  syncSettingsUI();

  // 麦克风设备列表（未授权时无 label，授权开麦后会再刷一次）
  async function refreshMicDevices() {
    try {
      const devs = await listAudioInputs();
      micDevSelect.innerHTML =
        '<option value="">默认设备</option>' +
        devs
          .map(
            (d, i) =>
              `<option value="${d.deviceId}" ${d.deviceId === prefs.micDeviceId ? 'selected' : ''}>${d.label || `麦克风 ${i + 1}`}</option>`,
          )
          .join('');
      // 持久化的设备 id 已失效（拔插）时回退默认设备显示
      if (prefs.micDeviceId && !devs.some((d) => d.deviceId === prefs.micDeviceId)) {
        micDevSelect.value = '';
      }
    } catch {
      // 枚举失败保持现状
    }
  }
  void refreshMicDevices();

  // ---- OBS 推流面板 ----
  const obsPanel = root.querySelector<HTMLDivElement>('#obs-panel')!;
  const obsUrl = root.querySelector<HTMLInputElement>('#obs-url')!;
  const obsMsg = root.querySelector<HTMLSpanElement>('#obs-msg')!;
  const obsErr = root.querySelector<HTMLParagraphElement>('#obs-error')!;

  async function loadIngress(reset: boolean) {
    obsErr.textContent = '';
    obsMsg.textContent = '获取中…';
    try {
      const info = reset ? await resetIngress(channel) : await getIngress(channel);
      obsUrl.value = info.url;
      obsMsg.textContent = reset ? '已重置，旧地址已失效' : '';
    } catch (err) {
      obsMsg.textContent = '';
      obsErr.textContent = (err as Error).message;
    }
  }

  root.querySelector('#btn-obs')!.addEventListener('click', () => {
    obsPanel.hidden = !obsPanel.hidden;
    if (!obsPanel.hidden && !obsUrl.value) void loadIngress(false);
  });

  root.querySelector('#obs-copy')!.addEventListener('click', async () => {
    if (!obsUrl.value) return;
    try {
      await navigator.clipboard.writeText(obsUrl.value);
      obsMsg.textContent = '已复制';
    } catch {
      obsUrl.select();
      document.execCommand('copy'); // 旧浏览器兜底
      obsMsg.textContent = '已复制';
    }
  });

  root.querySelector('#obs-reset')!.addEventListener('click', () => {
    if (confirm('重置后旧推流地址将立即失效，确定重置？')) void loadIngress(true);
  });

  // ---- 频道设置面板（房主）----
  const chPanel = root.querySelector<HTMLDivElement>('#ch-settings-panel')!;
  const inviteOnlyCheck = root.querySelector<HTMLInputElement>('#ch-invite-only')!;
  const memberList = root.querySelector<HTMLUListElement>('#member-list')!;
  const banList = root.querySelector<HTMLUListElement>('#ban-list')!;
  const chErr = root.querySelector<HTMLParagraphElement>('#ch-error')!;

  // 渲染成员/黑名单列表，kind 决定操作按钮语义
  function renderManageList(el: HTMLUListElement, names: string[], kind: 'member' | 'ban') {
    el.innerHTML = '';
    if (names.length === 0) {
      el.innerHTML = '<li class="muted">（空）</li>';
      return;
    }
    for (const n of names) {
      const li = document.createElement('li');
      const span = document.createElement('span');
      span.textContent = n;
      const btn = document.createElement('button');
      btn.className = 'mini';
      btn.textContent = kind === 'ban' ? '解除封禁' : '移除';
      btn.addEventListener('click', async () => {
        try {
          if (kind === 'ban') {
            await unbanUser(channel, n);
          } else {
            await removeMember(channel, n);
          }
          await loadChannelSettings();
        } catch (err) {
          chErr.textContent = (err as Error).message;
        }
      });
      li.append(span, btn);
      el.appendChild(li);
    }
  }

  async function loadChannelSettings() {
    chErr.textContent = '';
    try {
      const [members, bans, chs] = await Promise.all([
        listMembers(channel),
        listBans(channel),
        listChannels(),
      ]);
      inviteOnlyCheck.checked = chs.find((c) => c.name === channel)?.invite_only ?? false;
      renderManageList(memberList, members, 'member');
      renderManageList(banList, bans, 'ban');
    } catch (err) {
      chErr.textContent = (err as Error).message;
    }
  }

  btnChSettings.addEventListener('click', () => {
    chPanel.hidden = !chPanel.hidden;
    if (!chPanel.hidden) void loadChannelSettings();
  });

  inviteOnlyCheck.addEventListener('change', async () => {
    try {
      await setInviteOnly(channel, inviteOnlyCheck.checked);
    } catch (err) {
      inviteOnlyCheck.checked = !inviteOnlyCheck.checked; // 失败回滚
      chErr.textContent = (err as Error).message;
    }
  });

  root.querySelector('#member-add-form')!.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const input = root.querySelector<HTMLInputElement>('#member-name')!;
    const name = input.value.trim();
    if (!name) return;
    try {
      await addMember(channel, name);
      input.value = '';
      await loadChannelSettings();
    } catch (err) {
      chErr.textContent = (err as Error).message;
    }
  });

  // ---- 聊天面板收起/展开 + 未读角标 ----
  const btnChat = root.querySelector<HTMLButtonElement>('#btn-chat')!;
  const badgeEl = root.querySelector<HTMLSpanElement>('#chat-badge')!;
  let unread = 0;

  const chatIsOpen = () => roomEl.classList.contains('chat-open');
  function setChatOpen(open: boolean) {
    roomEl.classList.toggle('chat-open', open);
    roomEl.classList.toggle('chat-collapsed', !open);
    if (open) {
      unread = 0;
      badgeEl.hidden = true;
      // 展开后滚到底部
      chatLog.scrollTop = chatLog.scrollHeight;
    }
  }
  btnChat.addEventListener('click', () => setChatOpen(!chatIsOpen()));

  // ---- 聊天 ----
  const chatLog = root.querySelector<HTMLDivElement>('#chat-log')!;
  const appendMsg = (m: ChatMessage) => {
    const div = document.createElement('div');
    div.className = 'chat-msg';
    const time = (m.created_at ?? '').slice(11, 19);
    div.innerHTML = `<span class="muted">${time}</span> <b></b> <span></span>`;
    div.querySelector('b')!.textContent = m.username;
    div.querySelector('span:last-child')!.textContent = m.content;
    chatLog.appendChild(div);
    chatLog.scrollTop = chatLog.scrollHeight;
    // 面板收起时累计未读角标
    if (!chatIsOpen()) {
      unread += 1;
      badgeEl.textContent = unread > 99 ? '99+' : String(unread);
      badgeEl.hidden = false;
    }
  };

  const chat = connectChat(channel, {
    onHistory: (messages) => {
      chatLog.innerHTML = '';
      messages.forEach((m) => {
        appendMsg(m);
      });
      // 历史消息不算未读
      unread = 0;
      badgeEl.hidden = true;
    },
    onMessage: appendMsg,
    onClose: () => {
      if (leaving) return; // 自己离开不处理
      if (roomDisconnected) {
        // LiveKit 与聊天 WS 同时断开 = 被踢出/封禁
        statusEl.textContent = '你已被移出该频道';
        setTimeout(() => {
          location.hash = '#/channels';
        }, 1500);
      } else {
        statusEl.textContent = '聊天连接已断开';
      }
    },
  });

  root.querySelector('#chat-form')!.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const input = root.querySelector<HTMLInputElement>('#chat-input')!;
    const content = input.value.trim();
    if (content) {
      chat.send(content);
      input.value = '';
    }
  });

  // ---- 连接与清理 ----
  try {
    const cred = await fetchLiveKitToken(channel);
    statusEl.textContent = '正在加入房间…';
    await room.connect(cred.url || LIVEKIT_URL_FALLBACK, cred.token);
    statusEl.textContent = room.state === ConnectionState.Connected ? '' : '连接中…';
    refreshParticipants();
    refreshEmptyHint();
    // 进房麦克风/摄像头默认全关；按持久化偏好恢复（上次开过则自动开）
    if (prefs.mic) {
      try {
        await enableMic();
        micOn = true;
        void refreshMicDevices();
      } catch {
        micOn = false;
      }
    }
    if (prefs.camera) {
      try {
        await room.localParticipant.setCameraEnabled(true);
        cameraOn = true;
      } catch {
        cameraOn = false;
      }
    }
    refreshButtons();
  } catch (err) {
    statusEl.textContent = `连接失败：${(err as Error).message}`;
  }

  // 路由切走时释放资源（摄像头、麦克风、WebSocket、音频管线）
  const myHash = location.hash;
  const onHashChange = () => {
    if (location.hash !== myHash) {
      window.removeEventListener('hashchange', onHashChange);
      leaving = true;
      chat.close();
      void rnnoisePipe.stop();
      room.disconnect();
    }
  };
  window.addEventListener('hashchange', onHashChange);
  root.querySelector('#btn-leave')!.addEventListener('click', () => {
    location.hash = '#/channels';
  });
}
