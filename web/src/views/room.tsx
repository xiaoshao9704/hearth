// 房间页（Solid 渲染）：布局、面板、聊天与断线重连；音视频经 AVEngine 接口驱动。
// 双线模型：语音线（voice，权威名册/说话状态）+ 舞台线（stage：投屏/摄像头/OBS 及其伴音）。
// 两线同一内核时（combined）一条连接承担两种角色；舞台线缺席或断开只禁用投屏/摄像头，语音不受影响。
//
// 结构分两层：
// - 连接层（非 UI）：Line 双线模型、重连调度、引擎回调、聊天连接、离开清理——命令式，逻辑与旧版一致；
// - 视图层（Solid）：信号驱动，引擎回调只写信号，DOM 由 JSX 派生，消灭手工 refresh* 互相调用。
import { createEffect, createMemo, createSignal, on, untrack, For, Show } from 'solid-js';
import { render } from 'solid-js/web';
import { ApiError, fetchJoinCredentials, getUser, kickUser, listChannels, muteUser } from '../api';
import type { EngineCred } from '../api';
import { playCue } from '../audio';
import { connectChat } from '../chat';
import type { ChatMessage } from '../chat';
import { createEngine } from '../engine';
import type { AVEngine, EPart, EngineCallbacks, TrackSource, VideoStats } from '../engine/types';
import { encoderIsHw, loadPrefs, prefsBus, savePrefs } from '../prefs';
import { renderShell } from '../shell';
import { avatarHtml, confirmDialog, el, esc, fmtClock, icon, licon, menuButtonHtml, micIcon, slashIcon, toast, wireMenuButton } from '../ui';
import { openSettings } from './settings';

type SinkMedia = HTMLMediaElement & { setSinkId?: (id: string) => Promise<void>; sinkId?: string };

type Role = 'voice' | 'stage';

interface Line {
  role: Role;
  engine: AVEngine | null;
  engineName: string;
  attempts: number;
  timer: number;
  inflight: boolean;
}

// 视频卡片：引擎产出的命令式 video 元素 + 创建时刻的参与者快照（与旧版一致，名字不随名册热更新）
interface VideoEntry {
  kind: 'video';
  key: string; // `${identity}:${source}`
  seq: number; // 到达顺序，决定九宫格里的排位
  identity: string;
  source: TrackSource;
  video: HTMLVideoElement;
  isLocal: boolean;
  username: string;
  display: string;
  ingest: boolean; // 推流参与者（kind=ingest）：卡片角标与设备名按 tag 展示
  tag: string;
  specTip: () => string; // 徽章 tooltip：本地=配置目标，远端=说明
  encTag: () => string; // 本地投屏实际生效的编码器（getStats 真值）；其余为空
  liveStats: () => VideoStats | null; // 实测：本地=发送侧，远端=本端实际接收（SVC 选层后）
}

// 无视频参与者的音频块（内容全部从名册/说话信号派生）
interface AudioEntry {
  kind: 'audio';
  key: string; // `${identity}:audio-tile`
  seq: number;
  identity: string;
}

type TileEntry = VideoEntry | AudioEntry;

// 本地事件（进出房间提示）：不是服务端消息，只在本次会话的聊天日志里与消息按时间合流
interface RoomEvent {
  sys: true;
  id: number;
  at: number;
  text: string;
}

// 语音线连接阶段（顶栏 chip 与外壳连接框的唯一来源）；retry 带上第几次，避免再开一个信号
type VoiceState = { phase: 'connecting' | 'up' | 'retry'; attempt: number };

// ---- 每设备本地音量持久化（0~100，按 identity）----
const VOLS_KEY = 'hearth_room_volumes';

function loadVolumes(): Map<string, number> {
  try {
    const raw = localStorage.getItem(VOLS_KEY);
    if (!raw) return new Map();
    const m = new Map<string, number>();
    for (const [k, v] of Object.entries(JSON.parse(raw) as Record<string, unknown>)) {
      if (typeof v === 'number' && v >= 0 && v <= 100) m.set(k, Math.round(v));
    }
    return m;
  } catch {
    return new Map();
  }
}

function saveVolumes(m: Map<string, number>) {
  localStorage.setItem(VOLS_KEY, JSON.stringify(Object.fromEntries(m)));
}

export async function renderRoom(root: HTMLElement, channel: string) {
  const prefs = loadPrefs();
  const canScreenShare = typeof navigator.mediaDevices?.getDisplayMedia === 'function';

  const shell = renderShell(root, { activeChannel: channel });
  shell.setConn(false, '正在协商…');

  const myUid = getUser()?.id ?? 0;

  // ---- 连接层状态（非响应式）----
  const voiceLine: Line = { role: 'voice', engine: null, engineName: '', attempts: 0, timer: 0, inflight: false };
  let stageLine: Line | null = null; // null = 无舞台线；combined 时与 voiceLine 同引擎
  let combined = false;
  let stageUp = false;
  let leaving = false;
  let bounced = false;
  let seq = 0; // 卡片到达顺序计数
  const audioEls = new Map<string, Set<SinkMedia>>();

  // iOS Safari 的 volume 只读（设置被静默忽略）：探测后切到 Web Audio 增益链，
  // 每 identity 一个 GainNode，元素本体 muted；可写平台维持 elm.volume 直控
  const volProbe = document.createElement('audio');
  volProbe.volume = 0.5;
  const useGain = volProbe.volume !== 0.5;
  let audioCtx: AudioContext | null = null;
  let gainResume: (() => void) | null = null;
  const gainNodes = new Map<string, GainNode>();
  const srcNodes = new Map<SinkMedia, MediaStreamAudioSourceNode>();

  function wireGain(identity: string, elm: SinkMedia) {
    const stream = elm.srcObject as MediaStream | null;
    if (!stream) return;
    if (!audioCtx) {
      audioCtx = new AudioContext();
      // 自动播放策略：上下文要在用户手势里 resume
      gainResume = () => void audioCtx?.resume();
      document.addEventListener('pointerdown', gainResume);
      document.addEventListener('keydown', gainResume);
    }
    let g = gainNodes.get(identity);
    if (!g) {
      g = audioCtx.createGain();
      g.connect(audioCtx.destination);
      gainNodes.set(identity, g);
    }
    const src = audioCtx.createMediaStreamSource(stream);
    src.connect(g);
    srcNodes.set(elm, src);
    elm.muted = true;
    // livekit 的 attach/startAudio（webAudioMix 关闭时）会把 muted 翻回 false，
    // 元素一旦直放就和增益链双路出声：盯 volumechange 压回去
    elm.addEventListener('volumechange', reassertMute);
  }

  const reassertMute = (ev: Event) => {
    const elm = ev.currentTarget as SinkMedia;
    if (srcNodes.has(elm) && !elm.muted) elm.muted = true;
  };

  function unwireGain(elm: SinkMedia) {
    elm.removeEventListener('volumechange', reassertMute);
    srcNodes.get(elm)?.disconnect();
    srcNodes.delete(elm);
  }
  const speakingByRole: Record<Role, Set<string>> = { voice: new Set(), stage: new Set() };
  const tileTimers = new Set<number>(); // 投屏徽章的实测轮询定时器，离房时兜底清掉

  const stageEngine = () => (combined ? voiceLine.engine : stageLine?.engine) ?? null;

  // ---- 视图层信号 ----
  const [roster, setRoster] = createSignal<EPart[]>([]); // parts() 合并结果的快照
  const [speaking, setSpeaking] = createSignal<Set<string>>(new Set());
  const [videoEntries, setVideoEntries] = createSignal<VideoEntry[]>([]);
  const [audioEntries, setAudioEntries] = createSignal<AudioEntry[]>([]);
  const [micOn, setMicOn] = createSignal(prefs.mic);
  const [cameraOn, setCameraOn] = createSignal(prefs.camera);
  const [screenOn, setScreenOn] = createSignal(false);
  const [deafened, setDeafened] = createSignal(false);
  const [stageOk, setStageOk] = createSignal(false); // 舞台线可用（决定摄像头/投屏按钮禁用态）
  const [stageHint, setStageHint] = createSignal('本服未启用舞台线（投屏/摄像头）');
  const [statusText, setStatusText] = createSignal('正在连接…'); // 舞台区中央状态文案
  const [metaText, setMetaText] = createSignal('连接中'); // 顶栏 "N 人在房 · x 路投屏"
  const [layoutPref, setLayoutPref] = createSignal<'grid' | 'spotlight'>(prefs.layout);
  const [pinnedKey, setPinnedKey] = createSignal<string | null>(null);
  const [fsKey, setFsKey] = createSignal<string | null>(null); // 全屏中的卡片 key（含 iOS 模拟全屏）
  const [lastSpeaker, setLastSpeaker] = createSignal<string | null>(null);
  // 每设备本地音量（0~100，0=屏蔽），按 identity 持久化，跨频道/会话记住
  const [volumes, setVolumes] = createSignal<Map<string, number>>(loadVolumes());
  const restoreVol = new Map<string, number>(); // 屏蔽前的音量（仅本会话），恢复时回填
  const [panel, setPanel] = createSignal<'members' | 'chat' | ''>(
    window.matchMedia('(min-width: 1200px)').matches ? 'members' : '',
  );
  const [unread, setUnread] = createSignal(0);
  const [msgs, setMsgs] = createSignal<ChatMessage[]>([]);
  const [historyLoaded, setHistoryLoaded] = createSignal(false);
  const [chatReady, setChatReady] = createSignal(false); // 输入框非空 → 发送按钮点亮
  const [chatPlaceholder, setChatPlaceholder] = createSignal(`发消息到 #${channel}`);
  const [roomEvents, setRoomEvents] = createSignal<RoomEvent[]>([]);
  const [voiceState, setVoiceState] = createSignal<VoiceState>({ phase: 'connecting', attempt: 0 });
  const [audioBlocked, setAudioBlocked] = createSignal(false);
  const [myRoleSig, setMyRoleSig] = createSignal(''); // 服务端下发的我在本频道的角色（owner/moderator/member/""）
  const settingsCtx = { backLabel: `返回 ${channel}`, channel }; // 浮层按频道自查管理角色（owner/moderator），决定是否出「频道」分区
  const [ownerName, setOwnerName] = createSignal('');

  // DOM ref（引擎产的命令式元素挂载点等）
  let audioBinEl!: HTMLDivElement;
  let chatLogEl!: HTMLDivElement;
  let chatInputEl!: HTMLInputElement;
  let vuBarEl: HTMLElement | undefined; // 麦克风 VU 条（micOn 时才在 DOM 里）

  // 双线参与者合并：语音线是名册权威（micOn），舞台线补充 sharing/推流标记
  function parts(): EPart[] {
    const base = voiceLine.engine?.participants() ?? [];
    if (combined || !stageLine?.engine) return base;
    const map = new Map<string, EPart>();
    base.forEach((p) => map.set(p.identity, { ...p }));
    for (const s of stageLine.engine.participants()) {
      const ex = map.get(s.identity);
      if (ex) {
        ex.sharing = ex.sharing || s.sharing;
        ex.ingest = ex.ingest || s.ingest;
        ex.tag = ex.tag || s.tag;
      } else {
        map.set(s.identity, { ...s }); // OBS 推流等仅舞台线的参与者
      }
    }
    return [...map.values()];
  }

  // 分组/归属键：正常取 uid；拿不到元数据的参与者（滚动升级窗口里的旧连接、
  // 非 hearth 签发的连接）uid 为 0，退回 identity——否则这些互不相干的人会被
  // 并成同一个账号的多台设备，右键菜单的批量操作会误伤
  const groupKey = (p: EPart): number | string => (p.uid > 0 ? p.uid : p.identity);

  // 自己的推流设备默认静音（自己的 OBS 不出声）；推流身份看名册，不再拼 identity 后缀
  const isOwnIngest = (identity: string) => {
    const p = parts().find((pp) => pp.identity === identity);
    return !!p?.ingest && p.uid === myUid;
  };
  const volumePctFor = (identity: string) => volumes().get(identity) ?? (isOwnIngest(identity) ? 0 : 100);
  const volumeFor = (identity: string) => volumePctFor(identity) / 100;

  // 拖动滑条每个 tick 都会来：持久化做去抖，别把 localStorage 当流式写
  let volSaveTimer = 0;
  function scheduleSaveVolumes() {
    clearTimeout(volSaveTimer);
    volSaveTimer = window.setTimeout(() => saveVolumes(volumes()), 300);
  }

  function setVolumePct(identity: string, pct: number) {
    pct = Math.max(0, Math.min(100, Math.round(pct)));
    setVolumes((prev) => {
      const m = new Map(prev);
      m.set(identity, pct);
      return m;
    });
    scheduleSaveVolumes();
    applyAudioPrefs();
    if (useGain) void audioCtx?.resume(); // 拖滑条本身是手势，顺手唤醒挂起的上下文
  }

  // 拖动起点记为恢复点：拖到 0 再点恢复，回到开拖前的值而不是最后一个 tick
  const captureRestore = (identity: string) => {
    const v = volumePctFor(identity);
    if (v > 0) restoreVol.set(identity, v);
  };

  // 屏蔽 ↔ 恢复（恢复屏蔽前的音量，无记录回 100）
  const toggleVol = (identity: string) => {
    if (volumePctFor(identity) > 0) {
      captureRestore(identity);
      setVolumePct(identity, 0);
    } else {
      setVolumePct(identity, restoreVol.get(identity) ?? 100);
    }
  };

  function applyAudioPrefs() {
    const p = loadPrefs();
    const master = deafened() ? 0 : p.volume / 100;
    audioEls.forEach((set, identity) => {
      const v = master * volumeFor(identity);
      if (useGain) {
        // 增益链路径：音量全在 GainNode 上，元素保持 muted（iOS 也没有 setSinkId 可用）
        const g = gainNodes.get(identity);
        if (g) g.gain.value = v;
        set.forEach((elm) => {
          elm.muted = true;
        });
        return;
      }
      set.forEach((elm) => {
        // iOS Safari 走不到这里（useGain）；静音保留 muted 属性兜底
        elm.muted = v === 0;
        elm.volume = v;
        // 拖滑条会反复进这里：sinkId 没变就别重复调用（返回 promise，有成本）
        if (p.speakerId && typeof elm.setSinkId === 'function' && elm.sinkId !== p.speakerId) {
          elm.setSinkId(p.speakerId).catch(() => {});
        }
      });
    });
  }

  // 语音连着时清掉舞台区状态文案（旧版 syncAudioTiles 尾部的行为）
  function maybeClearStatus() {
    if (voiceLine.engine?.connected()) setStatusText('');
  }

  // 名册重取：引擎回调统一走这里写信号，成员面板/音频块/徽标全部派生
  function refreshRoster() {
    setRoster(parts());
    maybeClearStatus();
  }

  function refreshMeta() {
    const n = new Set(parts().map((p) => p.username)).size;
    const screens = videoEntries().filter((e) => e.source === 'screen').length;
    setMetaText(`${n} 人在房${screens ? ` · ${screens} 路投屏` : ''}`);
  }

  // ---- 进出房间提示（名册差分，引擎中立）----

  // 重连成功前的第一份名册只做基线：断线期间名册会整份重来，逐条比会刷屏
  let cueBaseline = true;
  let cueSeq = 0;

  function pushRoomEvent(text: string) {
    const ev: RoomEvent = { sys: true, id: cueSeq++, at: Date.now(), text };
    setRoomEvents((list) => [...list, ev].slice(-50));
    toast(text, '', 2500);
  }

  // 按 uid 聚合的在场集合：同账号多设备只算一份；ingest（OBS 推流）与真人设备分开两张表，
  // 否则推流上线会被当成「那个人又进了一次房间」
  function cueKeys(list: EPart[], ingest: boolean): Map<number | string, string> {
    const m = new Map<number | string, string>();
    for (const p of list) {
      if (p.ingest !== ingest) continue;
      if (!ingest && (p.isLocal || (p.uid > 0 && p.uid === myUid))) continue; // 自己的进出不提示
      m.set(groupKey(p), p.uid > 0 && p.uid === myUid ? '你' : p.username || p.display);
    }
    return m;
  }

  // 一次差分里多人同时进/出只响一声，避免提示音叠成噪音
  function diffRoster(prev: EPart[], cur: EPart[]) {
    let joined = false;
    let left = false;
    for (const ingest of [false, true]) {
      const before = cueKeys(prev, ingest);
      const after = cueKeys(cur, ingest);
      after.forEach((name, k) => {
        if (before.has(k)) return;
        joined = true;
        pushRoomEvent(ingest ? `${name === '你' ? '你的' : `${name} 的`} OBS 开始推流` : `${name} 进入了房间`);
      });
      before.forEach((name, k) => {
        if (after.has(k)) return;
        left = true;
        pushRoomEvent(ingest ? `${name === '你' ? '你的' : `${name} 的`} OBS 停止推流` : `${name} 离开了房间`);
      });
    }
    if (!loadPrefs().joinCue) return;
    if (joined) playCue('join');
    if (left) playCue('leave');
  }

  // ---- 视频卡片（引擎回调增删条目；渲染交给 JSX）----

  function addVideoTile(part: EPart, source: TrackSource, video: HTMLVideoElement) {
    const key = `${part.identity}:${source}`;
    if (videoEntries().some((e) => e.key === key)) return;
    if (part.isLocal && source === 'camera' && loadPrefs().mirror) video.style.transform = 'scaleX(-1)';

    let specTip: () => string = () => '';
    let encTag: () => string = () => '';
    let liveStats: () => VideoStats | null = () => null;
    if (source === 'screen') {
      const [stats, setStats] = createSignal<VideoStats | null>(null);
      const [tag, setTag] = createSignal('');
      liveStats = stats;
      encTag = tag;
      const p = loadPrefs();
      specTip = () =>
        part.isLocal
          ? `目标 ${p.res} · ${p.fps}fps · 上限 ${p.bitrate.toFixed(1)}M · ${p.screenCodec === 'h264' ? 'H.264 单层' : p.screenCodec === 'h265' ? 'HEVC 单层' : p.screenCodec.toUpperCase() + ' SVC'}`
          : '你实际接收到的规格（SVC 按你的带宽选层，与他人可能不同）';
      // 实测轮询（getStats 差分）；本地附带编码器真值（硬编/软编，降级时跟着变）
      const refresh = async () => {
        if (!videoEntries().some((e) => e.key === key)) {
          clearInterval(timer);
          tileTimers.delete(timer);
          return;
        }
        const eng = stageEngine();
        if (!eng) return;
        setStats(part.isLocal ? await eng.screenStats() : await eng.remoteVideoStats(part.identity, source));
        if (part.isLocal) {
          const info = await eng.screenEncoderInfo();
          if (info) {
            const hw = encoderIsHw(info);
            setTag(`${p.screenCodec.toUpperCase()}·${hw === true ? '硬' : hw === false ? '软' : info.impl}`);
          }
        }
      };
      const timer = window.setInterval(() => void refresh(), 2000);
      tileTimers.add(timer);
      setTimeout(() => void refresh(), 1500);
    }

    setVideoEntries((v) => [
      ...v,
      {
        kind: 'video',
        key,
        seq: seq++,
        identity: part.identity,
        source,
        video,
        isLocal: part.isLocal,
        username: part.username,
        display: part.display,
        ingest: part.ingest,
        tag: part.tag,
        specTip,
        encTag,
        liveStats,
      },
    ]);
    maybeClearStatus();
    refreshMeta();
  }

  function removeVideoTile(identity: string, source: TrackSource, els: HTMLMediaElement[]) {
    els.forEach((e) => e.remove());
    const key = `${identity}:${source}`;
    if (!videoEntries().some((e) => e.key === key)) return;
    setVideoEntries((v) => v.filter((e) => e.key !== key));
    if (pinnedKey() === key) setPinnedKey(null);
    if (fsKey() === key) setFsKey(null);
    maybeClearStatus();
    refreshMeta();
  }

  function togglePin(key: string) {
    setPinnedKey((k) => (k === key ? null : key));
  }

  // 全屏对 tile 容器请求（不是 video 元素），才能叠自定义控制条（音量滑条）；
  // iOS 私有全屏只接受 video 元素，或被浏览器拒绝时，退回 fixed 定位的模拟全屏
  function toggleFs(key: string, tileEl: HTMLElement) {
    if (fsKey() === key) return exitFs();
    if (typeof tileEl.requestFullscreen === 'function') {
      tileEl.requestFullscreen().catch(() => setFsKey(key));
    } else {
      setFsKey(key);
    }
  }
  function exitFs() {
    if (document.fullscreenElement) void document.exitFullscreen();
    setFsKey(null);
  }
  const onFsChange = () => {
    const t = document.fullscreenElement as HTMLElement | null;
    setFsKey(t?.dataset.tileKey ?? null);
  };
  // 模拟全屏没有浏览器的 Esc 退出，自己补
  const onFsKey = (ev: KeyboardEvent) => {
    if (ev.key === 'Escape' && fsKey() && !document.fullscreenElement) exitFs();
  };

  // ---- 用户操作菜单（聊天卡片、成员行与视频卡片右键共用；挂 body，保持命令式）----
  // 管理操作（禁言/踢出）= 频道 owner 或 moderator（与后端 requireModerator 一致；系统 admin 的隐含 owner 已由 my_role 下发）
  const canModerate = () => myRoleSig() === 'owner' || myRoleSig() === 'moderator';

  let longPressTimer = 0; // 触屏长按弹菜单的定时器（tile 触摸事件共用）
  let longPressFired = false; // 本次触摸已触发长按：touchend 要吞掉随之而来的合成 click

  // 命令式按钮的进行中态：await 期间置灰并禁止重复触发
  function markBusy(btn: HTMLButtonElement): () => void {
    btn.classList.add('loading');
    btn.disabled = true;
    return () => {
      btn.classList.remove('loading');
      btn.disabled = false;
    };
  }

  // identity 非空 = 设备模式（卡片/成员行入口）：菜单只控制这一台设备；
  // 空 = 用户模式（聊天头像入口）：列出该用户全部设备。禁言始终是用户级操作。
  function showUserMenu(x: number, y: number, uid: number, username: string, identity?: string) {
    document.querySelector('.user-menu')?.remove();
    const isSelf = uid === myUid;
    const deviceMode = !!identity;
    const targets = parts().filter(
      (p) =>
        !p.isLocal &&
        (uid > 0 ? p.uid === uid : p.identity === identity) &&
        (!deviceMode || p.identity === identity),
    );
    const muted = targets.some((p) => volumeFor(p.identity) === 0);
    const devName = (p: EPart) => (p.ingest ? `OBS 推流${p.tag ? ` · ${p.tag}` : ''}` : p.tag || p.identity);
    // 禁言判定只看真人设备：推流参与者（推流凭证自带发布权限）会污染 every() 推断
    const voiceTargets = targets.filter((p) => !p.ingest);
    const gagged = voiceTargets.length > 0 && voiceTargets.every((p) => !p.canPublish);
    const gagBtn = (on: boolean, label: string) =>
      `<button class="hit um-item${on ? ' danger' : ''}" data-gag="${on}">${micIcon(14, on, on ? 'var(--red)' : 'currentColor')}<span>${label}</span></button>`;
    // 音量行：图标 = 屏蔽/恢复快捷开关，滑条 0~100；多设备用户每台一行
    const volRow = (p: EPart) => {
      const v = volumePctFor(p.identity);
      return `<div class="um-item um-vol" data-vol-id="${esc(p.identity)}">
        <button class="hit um-vol-mute" title="屏蔽/恢复">${slashIcon('volume', 14, v === 0, v === 0 ? 'var(--red)' : 'currentColor')}</button>
        ${targets.length > 1 ? `<span class="um-vol-name">${esc(devName(p))}</span>` : ''}
        <input class="range" type="range" min="0" max="100" step="1" value="${v}">
        <span class="mono vol-num">${v}</span>
      </div>`;
    };
    const menu = document.createElement('div');
    menu.className = 'user-menu';
    menu.innerHTML = `
      <div class="um-title">${esc(username)}${isSelf ? '（我）' : ''}${deviceMode && targets[0] ? ` · ${esc(devName(targets[0]))}` : ''}</div>
      ${
        targets.length > 1
          ? targets.map(volRow).join('') +
            `<button class="hit um-item" data-act="mute">${slashIcon('volume', 14, !muted, 'currentColor')}<span>${muted ? '恢复全部声音' : '屏蔽全部声音'}</span></button>`
          : targets.length
            ? volRow(targets[0])
            : ''
      }
      ${
        !isSelf && canModerate()
          ? voiceTargets.length
            ? gagBtn(!gagged, gagged ? '解除禁言' : '禁言')
            : gagBtn(true, '禁言') + gagBtn(false, '解除禁言') // 不在语音房也可改（落库权威，下次进房生效）
          : ''
      }
      ${
        isSelf || canModerate()
          ? targets
              .map(
                (p) =>
                  `<button class="hit um-item" data-kick-id="${esc(p.identity)}">${icon('leave', 14, 'currentColor')}<span>踢出 ${esc(devName(p))}</span></button>`,
              )
              .join('') +
            (deviceMode
              ? ''
              : `<button class="hit um-item danger" data-act="kick">${icon('leave', 14, 'var(--red)')}<span>${isSelf ? '踢出我的全部设备' : '踢出房间'}</span></button>`)
          : ''
      }`;
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
    // 屏蔽全部/恢复全部：状态按点击当时重算（菜单开着时滑条可能改过）；
    // 恢复只动被屏蔽的设备，不覆盖其他设备手调的音量
    const muteAllBtn = menu.querySelector('[data-act="mute"]');
    const paintMuteAll = () => {
      if (!muteAllBtn) return;
      const anyMuted = targets.some((p) => volumePctFor(p.identity) === 0);
      muteAllBtn.innerHTML = `${slashIcon('volume', 14, !anyMuted, 'currentColor')}<span>${anyMuted ? '恢复全部声音' : '屏蔽全部声音'}</span>`;
    };
    muteAllBtn?.addEventListener('click', () => {
      const anyMuted = targets.some((p) => volumePctFor(p.identity) === 0);
      targets.forEach((p) => {
        const cur = volumePctFor(p.identity);
        if (anyMuted ? cur === 0 : cur > 0) {
          if (!anyMuted) restoreVol.set(p.identity, cur);
          setVolumePct(p.identity, anyMuted ? (restoreVol.get(p.identity) ?? 100) : 0);
        }
      });
      close();
    });
    menu.querySelectorAll<HTMLButtonElement>('[data-gag]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const on = btn.dataset.gag === 'true';
        // 解除禁言不是破坏性操作，不打断
        if (
          on &&
          !(await confirmDialog({
            title: `确定禁言 ${username}？`,
            body: '禁言后对方无法开麦、开摄像头或推流，直到你解除。',
            confirmText: '禁言',
            danger: true,
          }))
        ) {
          return;
        }
        const done = markBusy(btn);
        try {
          await muteUser(channel, uid, on);
          toast(on ? `已禁言 ${username}` : `已解除 ${username} 的禁言`, 'ok');
        } catch (err) {
          toast((err as Error).message, 'bad');
        } finally {
          done();
          close();
        }
      });
    });
    // 音量行：拖滑条/点图标不关菜单，就地刷新行内数值、图标与「屏蔽全部」的文案
    menu.querySelectorAll<HTMLElement>('.um-vol').forEach((row) => {
      const id = row.dataset.volId!;
      const input = row.querySelector<HTMLInputElement>('input.range')!;
      const num = row.querySelector<HTMLElement>('.vol-num')!;
      const muteBtnEl = row.querySelector<HTMLButtonElement>('.um-vol-mute')!;
      const paint = () => {
        const v = volumePctFor(id);
        input.value = String(v);
        num.textContent = String(v);
        muteBtnEl.innerHTML = slashIcon('volume', 14, v === 0, v === 0 ? 'var(--red)' : 'currentColor');
        paintMuteAll();
      };
      input.addEventListener('pointerdown', () => captureRestore(id));
      input.addEventListener('input', () => {
        setVolumePct(id, Number(input.value));
        paint();
      });
      muteBtnEl.addEventListener('click', () => {
        toggleVol(id);
        paint();
      });
    });
    menu.querySelectorAll<HTMLButtonElement>('[data-kick-id]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const id = btn.dataset.kickId!;
        const target = targets.find((p) => p.identity === id);
        const ok = await confirmDialog({
          title: `确定踢出「${target ? devName(target) : id}」？`,
          body: '这台设备会被移出房间，之后仍可重新进入。',
          confirmText: '踢出',
          danger: true,
        });
        if (!ok) return;
        const done = markBusy(btn);
        try {
          await kickUser(channel, uid, id);
          toast('已踢出该设备', 'ok');
        } catch (err) {
          toast((err as Error).message, 'bad');
        } finally {
          done();
          close();
        }
      });
    });
    const kickAllBtn = menu.querySelector<HTMLButtonElement>('[data-act="kick"]');
    kickAllBtn?.addEventListener('click', async () => {
      const ok = await confirmDialog({
        title: isSelf ? '确定踢出你的全部设备？' : `确定把 ${username} 移出房间？`,
        body: isSelf ? '本机与 OBS 推流都会断开。' : '对方的全部设备（含 OBS 推流）都会被移出，之后仍可重新进入。',
        confirmText: '踢出',
        danger: true,
      });
      if (!ok) return;
      const done = markBusy(kickAllBtn);
      try {
        await kickUser(channel, uid);
        toast(isSelf ? '已踢出你的全部设备（含本机与 OBS）' : `已把 ${username} 移出房间`, 'ok');
      } catch (err) {
        toast((err as Error).message, 'bad');
      } finally {
        done();
        close();
      }
    });
  }

  // ---- 断线自动重连（按线独立）----

  function bounce(msg: string) {
    if (bounced) return;
    bounced = true;
    setStatusText(msg);
    toast(msg, 'bad');
    setTimeout(() => {
      location.hash = '#/lobby';
    }, 1500);
  }

  function lineFor(role: Role): Line | null {
    return role === 'voice' ? voiceLine : combined ? voiceLine : stageLine;
  }

  function connBoxMeta(): string {
    // 图标 chip：窄屏放不下时按 chip 整体折行，不在值中间断
    const chip = (ic: string, label: string) =>
      `<span class="conn-chip">${icon(ic, 11, 'var(--text-2)', 1.7)}<span>${esc(label)}</span></span>`;
    let h = chip('volume', voiceLine.engineName || '—');
    if (stageLine && !combined) h += chip('screen', stageUp ? stageLine.engineName : '重连中');
    return h;
  }

  function scheduleRejoin(role: Role, delay?: number) {
    if (leaving || bounced) return;
    const line = lineFor(role);
    if (!line) return;
    clearTimeout(line.timer);
    const wait = delay ?? Math.min(30000, 2000 * 2 ** Math.min(line.attempts, 4)) * (0.7 + Math.random() * 0.6);
    if (role === 'voice' || combined) {
      setStatusText(line.attempts === 0 ? '连接断开，正在重连…' : `连接断开，正在重连…（第 ${line.attempts + 1} 次）`);
      setVoiceState({ phase: 'retry', attempt: line.attempts + 1 });
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
    if (leaving || bounced) return;
    combined = creds.combined;
    if (!only || only === 'voice') {
      await connectLine(voiceLine, creds.voice, first, 'voice');
      if (leaving) return;
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
        if (leaving) return;
      }
    } else {
      stageLine = null;
      stageUp = false;
    }
    updateStageButtons();
    shell.setConn(!!voiceLine.engine?.connected(), connBoxMeta());
  }

  // 按状态码分支：403/404 是服务端的确定拒绝（封禁、邀请制、频道没了），重试没有意义；
  // 网络失败（status=0）与 5xx 都退避重连。401 已由 api.ts 清会话跳登录
  function handleCredsError(err: unknown, first: boolean) {
    const msg = (err as Error).message ?? '';
    if (first) setStatusText(`连接失败：${msg}`);
    if (err instanceof ApiError) {
      if (err.status === 401) {
        bounced = true;
        return;
      }
      if (err.status === 403 || err.status === 404) {
        bounce(msg);
        return;
      }
    }
    scheduleRejoin('voice');
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
        // 建引擎期间用户可能已经离开房间：清理块跑过了就没人再释放这个半成品
        if (leaving) {
          line.engine.dispose();
          line.engine = null;
          return;
        }
      }
      await line.engine.connect(cred.url, cred.token);
      if (leaving) {
        line.engine.dispose();
        line.engine = null;
        return;
      }
      line.attempts = 0;
      if (role === 'voice') {
        setStatusText('');
        cueBaseline = true; // 重连成功后的第一份名册只重置基线，不当成一屋子人刚进来
        setVoiceState({ phase: 'up', attempt: 0 });
        if (!first) toast('语音已重新连接', 'ok', 1800);
        checkSelfGag(); // 持久禁言：重进房时权限初始就是 canPublish=false
        if (micOn()) {
          try {
            await line.engine.setMic(true);
          } catch (err) {
            setMicOn(false);
            if (first) toast(captureErrorMsg('麦克风', err), 'bad');
          }
        }
      }
      if (role === 'stage' || combined) {
        stageUp = true;
        if (cameraOn()) {
          try {
            await (role === 'stage' ? line.engine : voiceLine.engine)!.setCamera(true);
          } catch {
            setCameraOn(false);
          }
        }
        if (screenOn()) {
          setScreenOn(false);
          toast('重连后投屏需要重新发起', '', 4000);
        }
      }
      refreshRoster();
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
      // 引擎还没建起来（首次连接就失败）也要能重试，否则「立即重试」按钮点了没反应
      if (line && !line.engine?.connected()) {
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
  document.addEventListener('fullscreenchange', onFsChange);
  document.addEventListener('keydown', onFsKey);
  window.addEventListener('online', retryNow);

  // ---- 引擎回调（按线）：只写信号 / 增删条目，不碰 DOM ----
  function makeCallbacks(role: Role): EngineCallbacks {
    return {
      onVideoTrack: (part, source, elm) => addVideoTile(part, source, elm),
      onVideoTrackRemoved: (identity, source, els) => removeVideoTile(identity, source, els),
      onAudioTrack: (identity, elm) => {
        let set = audioEls.get(identity);
        if (!set) {
          set = new Set();
          audioEls.set(identity, set);
        }
        set.add(elm as SinkMedia);
        audioBinEl.appendChild(elm);
        if (useGain) wireGain(identity, elm as SinkMedia);
        applyAudioPrefs();
      },
      onAudioTrackRemoved: (identity, els) => {
        els.forEach((elm) => {
          elm.remove();
          unwireGain(elm as SinkMedia);
          audioEls.get(identity)?.delete(elm as SinkMedia);
        });
        // 该 identity 没有音轨了：拆掉增益节点，重进时重建
        if (!audioEls.get(identity)?.size) {
          gainNodes.get(identity)?.disconnect();
          gainNodes.delete(identity);
        }
      },
      onRoster: () => {
        refreshRoster();
        refreshMeta();
        if (role === 'voice') checkSelfGag(); // 禁言/解禁经名册刷新到达（权限变化 → canPublish）
      },
      onSpeakers: (identities) => {
        speakingByRole[role] = new Set(identities);
        setSpeaking(new Set([...speakingByRole.voice, ...speakingByRole.stage]));
        if (identities[0]) setLastSpeaker(identities[0]);
        refreshRoster();
      },
      onReconnecting: () => {
        if (role === 'voice' || combined) {
          setStatusText('连接不稳定，正在恢复…');
          setVoiceState((s) => ({ phase: 'retry', attempt: s.attempt }));
        }
      },
      onReconnected: () => {
        if (role === 'voice' || combined) {
          setStatusText('');
          cueBaseline = true; // 引擎自愈也会重发整份名册
          setVoiceState({ phase: 'up', attempt: 0 });
        }
        refreshRoster();
        refreshMeta();
      },
      onAudioBlocked: () => setAudioBlocked(true),
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
          setScreenOn(false);
          updateStageButtons();
          shell.setConn(!!voiceLine.engine?.connected(), connBoxMeta());
          scheduleRejoin('stage', 0);
        }
      },
      onLocalTrackEnded: (kind) => {
        if (kind === 'mic' && micOn()) {
          setMicOn(false);
          void voiceLine.engine?.setMic(false).catch(() => {});
          toast('麦克风设备断开了（iPhone 连续互通断开会这样），已自动闭麦', 'bad');
        }
        if (kind === 'camera' && cameraOn()) {
          setCameraOn(false);
          void stageEngine()?.setCamera(false).catch(() => {});
          toast('摄像头设备断开了，已自动关闭', 'bad');
        }
        if (kind === 'screen' && screenOn()) {
          // 浏览器原生「停止共享」：采集轨已结束，内核会自行撤发布，这里只对齐页面状态
          setScreenOn(false);
          void stageEngine()?.setScreen(false).catch(() => {});
          refreshMeta();
        }
        refreshRoster();
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

  // 自己被禁言（服务端收走发布权限 canPublish=false；持久禁言重进房时权限初始即为 false）。
  // 这个影子变量只用于「状态跳变时弹一次 toast」，按钮的禁言态从 roster() 派生，不看它
  let gagToasted = false;
  const isSelfGagged = () => voiceLine.engine?.participants().some((p) => p.isLocal && !p.canPublish) === true;

  function checkSelfGag() {
    const g = isSelfGagged();
    if (g === gagToasted) return;
    gagToasted = g;
    if (g) {
      // 服务端会顺带撤下已发布的麦克风轨道，本地 UI 同步收成闭麦
      if (micOn()) setMicOn(false);
      toast('你已被禁言，无法发言', 'bad');
    } else {
      toast('你的禁言已解除', 'ok');
    }
  }

  // ---- 控制按钮动作 ----

  function updateStageButtons() {
    setStageOk(!!stageEngine() && stageUp);
    setStageHint(stageLine ? '舞台线重连中' : '本服未启用舞台线（投屏/摄像头）');
  }

  async function toggleMic() {
    if (!voiceLine.engine) return;
    if (!micOn() && isSelfGagged()) {
      toast('你已被禁言，无法开麦', 'bad');
      return;
    }
    setMicOn((m) => !m);
    try {
      await voiceLine.engine.setMic(micOn());
    } catch (err) {
      if (micOn()) toast(captureErrorMsg('麦克风', err), 'bad');
      setMicOn((m) => !m);
    }
    refreshRoster();
  }

  function toggleDeaf() {
    setDeafened((d) => !d);
    applyAudioPrefs();
    toast(deafened() ? '已静音全部（仍可说话）' : '已恢复收听', '', 2000);
  }

  // ---- 键盘快捷键：M 切麦、D 静音全部、按住 Space 说话（Ctrl+Enter 发送在聊天输入框自己的 keydown 上）----
  const inTypingTarget = (ev: KeyboardEvent) => {
    const t = ev.target as HTMLElement | null;
    return !!t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable);
  };
  // 按住说话只在本轮按下期间开麦：失焦/松开都要能复位，否则麦克风会一直开着
  let pttHeld = false;
  const onHotkeyDown = (ev: KeyboardEvent) => {
    if (ev.repeat || ev.ctrlKey || ev.metaKey || ev.altKey) return; // repeat 防抖；带修饰键让位浏览器快捷键
    if (inTypingTarget(ev)) return;
    const key = ev.key.toLowerCase();
    if (key === 'm') {
      void toggleMic();
    } else if (key === 'd') {
      toggleDeaf();
    } else if (ev.key === ' ') {
      ev.preventDefault(); // 阻止页面滚动与聚焦按钮被激活
      if (pttHeld || micOn()) return; // 已开麦时按住 Space 不做多余翻转
      pttHeld = true;
      void toggleMic();
    }
  };
  const pttRelease = () => {
    if (!pttHeld) return;
    pttHeld = false;
    if (micOn()) void toggleMic();
  };
  const onHotkeyUp = (ev: KeyboardEvent) => {
    if (ev.key !== ' ') return;
    if (pttHeld) ev.preventDefault(); // 按住期间吞掉 keyup，避免触发聚焦按钮的 click
    pttRelease();
  };
  document.addEventListener('keydown', onHotkeyDown);
  document.addEventListener('keyup', onHotkeyUp);
  window.addEventListener('blur', pttRelease);

  // 自动播放被拦截：横幅点击是用户手势，两条线的引擎与增益链一起解锁
  async function resumeAllAudio() {
    setAudioBlocked(false);
    void audioCtx?.resume();
    for (const eng of new Set([voiceLine.engine, stageEngine()])) {
      if (eng) await eng.resumeAudio().catch(() => {});
    }
  }

  // 投屏中离开要先确认：投屏一断观众端就黑，误触代价大
  async function leaveRoom() {
    if (screenOn() && !(await confirmDialog({ title: '正在投屏，确定离开？', body: '离开房间会立刻中断投屏。', confirmText: '离开', danger: true }))) {
      return;
    }
    location.hash = '#/lobby';
  }

  async function toggleCamera() {
    const eng = stageEngine();
    if (!eng || !stageUp) {
      toast(stageHint(), '', 3000);
      return;
    }
    setCameraOn((c) => !c);
    try {
      await eng.setCamera(cameraOn());
      const p = loadPrefs();
      p.camera = cameraOn();
      savePrefs(p);
    } catch (err) {
      if (cameraOn()) toast(captureErrorMsg('摄像头', err), 'bad');
      setCameraOn((c) => !c);
    }
  }

  async function toggleScreen() {
    const eng = stageEngine();
    if (!eng || !stageUp) {
      toast(stageHint(), '', 3000);
      return;
    }
    setScreenOn((s) => !s);
    try {
      await eng.setScreen(screenOn());
    } catch (err) {
      setScreenOn((s) => !s); // 用户取消选择等情况回退
      const name = (err as DOMException)?.name ?? '';
      if (name !== 'NotAllowedError' && name !== 'AbortError') {
        toast(`投屏失败：${(err as Error)?.message ?? name}`, 'bad');
      }
    }
    refreshMeta();
  }

  // ---- 本地麦克风电平表：让说话的人确认自己有声音 ----
  let vuCtx: AudioContext | null = null;
  let vuAnalyser: AnalyserNode | null = null;
  let vuSrc: MediaStreamAudioSourceNode | null = null;
  let vuTrack: MediaStreamTrack | null = null;
  const vuTimer = window.setInterval(() => {
    const track = micOn() ? (voiceLine.engine?.localMicTrack() ?? null) : null;
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
    const bar = micOn() ? vuBarEl : undefined; // 闭麦时 VU 条不在 DOM 里
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

  // ---- 面板与聊天 ----

  function switchPanel(p: 'members' | 'chat' | '') {
    setPanel(p);
    if (p === 'chat') {
      setUnread(0);
      // 面板刚从 hidden 变可见，等 DOM 更新后再滚底
      queueMicrotask(() => {
        chatLogEl.scrollTop = chatLogEl.scrollHeight;
        // 触屏聚焦会弹出键盘把面板顶掉一半，只在有指针悬停的设备上抢焦点
        if (!window.matchMedia('(hover: none)').matches) chatInputEl.focus();
      });
    }
  }

  const chat = connectChat(channel, {
    onHistory: (messages) => {
      setHistoryLoaded(true);
      setMsgs(messages);
      setUnread(0);
    },
    onMessage: (m) => {
      setMsgs((list) => [...list, m]);
      if (panel() !== 'chat') setUnread((u) => u + 1);
    },
    onKicked: (code) => {
      if (leaving) return;
      bounce(code === 1001 ? '频道已删除' : '你已被移出该频道');
    },
    onState: (state) => {
      setChatPlaceholder(state === 'open' ? `发消息到 #${channel}` : '聊天重连中…');
    },
  });

  function sendChat(ev: Event) {
    ev.preventDefault();
    const content = chatInputEl.value.trim();
    if (!content) return;
    // ws 断开时 send 会静默丢消息：保留输入框内容，让用户重连后能再发一次
    if (!chat.connected()) {
      toast('聊天未连接，稍后再发', 'bad');
      return;
    }
    chat.send(content);
    chatInputEl.value = '';
    setChatReady(false);
  }

  // ---- 设置页偏好热应用 ----
  const onPrefs = async (ev: Event) => {
    const what = (ev as CustomEvent).detail as string;
    if (what === 'volume' || what === 'speaker') applyAudioPrefs();
    if (what === 'mirror') {
      const id = stageEngine()?.localIdentity() ?? '';
      const entry = videoEntries().find((e) => e.key === `${id}:camera`);
      if (entry) entry.video.style.transform = loadPrefs().mirror ? 'scaleX(-1)' : '';
    }
    if ((what === 'mic-device' || what === 'audio-chain') && micOn() && voiceLine.engine) {
      try {
        await voiceLine.engine.restartMic();
      } catch (err) {
        setMicOn(false);
        toast(`麦克风重启失败，已闭麦：${(err as Error)?.message ?? ''}`, 'bad');
      }
    }
    if (what === 'cam-device' && cameraOn()) {
      const p = loadPrefs();
      if (p.camDeviceId)
        void stageEngine()
          ?.switchCamera(p.camDeviceId)
          .catch((err) => toast(`切换摄像头失败：${(err as Error)?.message ?? ''}`, 'bad'));
    }
    if (what === 'screen' && screenOn()) applyScreenPrefsSoon();
  };
  prefsBus.addEventListener('prefs', onPrefs);

  // 投屏画质热应用：码率滑块每格都发事件，合并到一次；setParameters 不能并发，串成链
  let screenApplyTimer = 0;
  let screenApplyChain = Promise.resolve();
  function applyScreenPrefsSoon() {
    window.clearTimeout(screenApplyTimer);
    screenApplyTimer = window.setTimeout(() => {
      screenApplyChain = screenApplyChain.then(async () => {
        const eng = stageEngine();
        if (!eng || !screenOn()) return;
        try {
          if (await eng.applyScreenPrefs()) toast('投屏编码已切换，观众端会短暂重连', '', 3000);
        } catch (err) {
          toast(`投屏画质应用失败：${err instanceof Error ? err.message : String(err)}`, 'bad');
        }
        refreshMeta();
      });
    }, 200);
  }

  // ---- 视图组件 ----

  // 静态头像（聊天卡片等，不随信号变化）
  // 音量滑条 + 数值：成员行浮层与全屏控制条共用（右键菜单那份是命令式字符串，形态不同）
  const VolSlider = (p: { identity: string }) => (
    <>
      <input
        class="range"
        type="range"
        min="0"
        max="100"
        step="1"
        value={volumePctFor(p.identity)}
        onPointerDown={() => captureRestore(p.identity)}
        onInput={(ev) => setVolumePct(p.identity, Number(ev.currentTarget.value))}
      />
      <span class="mono vol-num">{volumePctFor(p.identity)}</span>
    </>
  );

  const Avatar = (p: { name: string; cls?: string; onClick?: (ev: MouseEvent) => void }) => {
    const a = el(avatarHtml(p.name, p.cls ?? 'avatar')) as HTMLElement;
    if (p.onClick) a.addEventListener('click', p.onClick);
    return a;
  };

  const VideoTileView = (p: { e: VideoEntry; spotlight: () => boolean; focusKey: () => string | null }) => {
    const e = p.e;
    let tileEl!: HTMLDivElement;
    const name = e.isLocal && e.source === 'camera' ? '你' : e.display;
    const isFs = () => fsKey() === e.key;
    return (
      <div
        ref={tileEl}
        class={'tile' + (e.source === 'screen' ? ' tile-screen' : '')}
        classList={{
          speaking: speaking().has(e.identity),
          pinned: pinnedKey() === e.key,
          featured: p.spotlight() && p.focusKey() === e.key,
          'faux-fs': isFs() && !document.fullscreenElement,
        }}
        data-identity={e.identity}
        data-tile-key={e.key}
        onClick={(ev) => {
          if (isFs()) return; // 全屏里点击不切置顶
          if ((ev.target as HTMLElement).closest('.tile-actions')) return;
          togglePin(e.key);
        }}
      >
        {e.video}
        <div class="tile-badges">
          <Show when={e.source === 'screen'}>
            <div class="live-badge">LIVE</div>
          </Show>
          <Show when={e.source === 'screen' && (e.encTag() || e.liveStats())}>
            <div class="spec-badge stat-strip mono" title={e.specTip()}>
              <Show when={e.encTag()}>
                <span class="stat">{el(licon('cpu'))}{e.encTag()}</span>
              </Show>
              <Show when={e.liveStats()}>
                {(s) => (
                  <>
                    <span class="stat">
                      {el(licon('monitor'))}
                      {s().width}×{s().height}
                    </span>
                    <span class="stat">
                      {el(licon('timer'))}
                      {Math.round(s().fps)}
                    </span>
                    <span class="stat">
                      {el(licon('gauge'))}
                      {(s().kbps / 1000).toFixed(1)}M
                    </span>
                  </>
                )}
              </Show>
            </div>
          </Show>
          <Show when={e.ingest}>
            <div class="spec-badge">{e.tag ? `OBS · ${e.tag}` : 'OBS · WHIP'}</div>
          </Show>
        </div>
        <div class="tile-label">
          <Avatar name={e.username} cls="avatar avatar-sm" />
          <span>{name + (e.source === 'screen' ? '的投屏' : '')}</span>
        </div>
        <div class="tile-actions">
          <button
            class="hit tact tact-pin"
            classList={{ pinned: pinnedKey() === e.key }}
            title={pinnedKey() === e.key ? '取消置顶' : '置顶'}
            onClick={(ev) => {
              ev.stopPropagation();
              togglePin(e.key);
            }}
          >
            {el(licon('pin', 15))}
          </button>
          <button
            class="hit tact"
            title={isFs() ? '退出全屏' : '全屏'}
            onClick={(ev) => {
              ev.stopPropagation();
              toggleFs(e.key, tileEl);
            }}
          >
            {el(icon('fullscreen', 15))}
          </button>
        </div>
        {/* 全屏控制条：远端卡片给音量滑条（本机卡片没有可调的声音）；退出全屏在右上角 */}
        <Show when={isFs() && !e.isLocal}>
          <div class="fs-bar" onClick={(ev) => ev.stopPropagation()}>
            <button
              class="hit fs-vol-mute"
              classList={{ 'muted-on': volumePctFor(e.identity) === 0 }}
              title={volumePctFor(e.identity) === 0 ? '恢复声音' : '屏蔽声音'}
              onClick={() => toggleVol(e.identity)}
            >
              {el(slashIcon('volume', 15, volumePctFor(e.identity) === 0, 'currentColor'))}
            </button>
            <VolSlider identity={e.identity} />
          </div>
        </Show>
      </div>
    );
  };

  const AudioTileView = (p: { e: AudioEntry; spotlight: () => boolean; focusKey: () => string | null }) => {
    const part = createMemo(() => roster().find((pp) => pp.identity === p.e.identity));
    const isSpeaking = createMemo(() => speaking().has(p.e.identity));
    const micOff = () => !(part()?.micOn ?? false) && !(part()?.ingest ?? false);
    return (
      <div
        class="tile tile-audio"
        classList={{
          speaking: isSpeaking(),
          muted: micOff() && !(part()?.isLocal ?? false),
          pinned: pinnedKey() === p.e.key,
          featured: p.spotlight() && p.focusKey() === p.e.key,
        }}
        data-identity={p.e.identity}
        onClick={() => togglePin(p.e.key)}
      >
        {el(avatarHtml(part()?.username ?? '', 'avatar avatar-xl' + (isSpeaking() ? ' speaking' : '')))}
        <div class="a-name">
          <span>{part()?.isLocal ? '你' : (part()?.display ?? '')}</span>
          {el(micIcon(13, micOff(), micOff() ? 'var(--red)' : isSpeaking() ? 'var(--ember)' : 'var(--text-2)'))}
        </div>
        <Show when={part()?.ingest}>
          <div style="position:absolute;top:12px;left:12px" class="tag tag-ember mono">
            OBS 推流{part()?.tag ? ` · ${part()?.tag}` : ''}
          </div>
        </Show>
        <div class="tile-actions">
          <button
            class="hit tact tact-pin"
            classList={{ pinned: pinnedKey() === p.e.key }}
            title={pinnedKey() === p.e.key ? '取消置顶' : '置顶'}
            onClick={(ev) => {
              ev.stopPropagation();
              togglePin(p.e.key);
            }}
          >
            {el(licon('pin', 15))}
          </button>
        </div>
      </div>
    );
  };

  const ChatMsgView = (p: { m: ChatMessage }) => {
    const mine = p.m.uid === myUid;
    const openMenu = (ev: MouseEvent) => showUserMenu(ev.clientX, ev.clientY, p.m.uid, p.m.username);
    return (
      <div
        class="chat-msg"
        onContextMenu={(ev) => {
          if (mine) return;
          ev.preventDefault();
          openMenu(ev);
        }}
      >
        <Avatar name={p.m.username} onClick={mine ? undefined : openMenu} />
        <div class="body">
          <div class="meta">
            <span class="who">{p.m.username}</span>
            <span class="at">{fmtClock(p.m.created_at)}</span>
          </div>
          <div class="text">{p.m.content}</div>
        </div>
      </div>
    );
  };

  const App = () => {
    // 无视频参与者补音频块：跟随名册与视频卡片增删，保持首次出现的顺序（对齐旧版 Map 语义）
    createEffect(() => {
      const ps = roster();
      const hasVideo = new Set(videoEntries().map((v) => v.identity));
      const wantIds = ps.filter((p) => !hasVideo.has(p.identity)).map((p) => p.identity);
      const wantSet = new Set(wantIds);
      const prev = untrack(audioEntries);
      const kept = prev.filter((e) => wantSet.has(e.identity));
      const haveIds = new Set(prev.map((e) => e.identity));
      const added = wantIds
        .filter((id) => !haveIds.has(id))
        .map((id) => ({ kind: 'audio' as const, key: `${id}:audio-tile`, seq: seq++, identity: id }));
      if (kept.length === prev.length && added.length === 0) return;
      // 被移除的音频块若被 pin 着，取消 pin
      const removed = prev.filter((e) => !wantSet.has(e.identity));
      if (removed.some((e) => e.key === untrack(pinnedKey))) setPinnedKey(null);
      setAudioEntries([...kept, ...added]);
    });

    // 名册差分 → 进出提示。基线快照就是上一次的 roster()，不另存副本；
    // 首次进房/重连成功后的第一份只重置基线（cueBaseline），断线期间不比较
    createEffect(
      on(roster, (cur, prev) => {
        if (!prev) return; // 首次运行没有可比的上一份，不消耗基线标志
        if (cueBaseline) {
          cueBaseline = false;
          return;
        }
        if (!voiceLine.engine?.connected()) return;
        diffRoster(prev, cur);
      }),
    );

    // 外壳连接框与顶栏 chip 同源（voiceState）；舞台线自己的变化仍走命令式 setConn
    createEffect(() => {
      const s = voiceState();
      shell.setConn(s.phase === 'up', s.phase === 'up' ? connBoxMeta() : s.phase === 'connecting' ? '正在协商…' : '正在重连…');
    });

    // 聊天日志 = 服务端消息 + 本地事件按时间合流；条目保持原对象引用，For 才不会整段重建
    const chatItems = createMemo<(ChatMessage | RoomEvent)[]>(() => {
      const at = (x: ChatMessage | RoomEvent) => ('sys' in x ? x.at : Date.parse(x.created_at) || 0);
      return [...msgs(), ...roomEvents()].sort((a, b) => at(a) - at(b));
    });

    // 新消息 / 历史 / 本地事件到达后滚到底（原实现每次 append 后滚动）
    createEffect(() => {
      chatItems();
      chatLogEl.scrollTop = chatLogEl.scrollHeight;
    });

    // 被禁言：按钮状态从名册派生（checkSelfGag 的影子变量只管跳变 toast）
    const selfGagged = createMemo(() => roster().some((p) => p.isLocal && !p.canPublish));

    const spotlight = createMemo(() => layoutPref() === 'spotlight');
    // 全部卡片按到达顺序（九宫格排位 = 旧版 DOM 插入顺序）
    const tileEntries = createMemo<TileEntry[]>(() => [...videoEntries(), ...audioEntries()].sort((a, b) => a.seq - b.seq));
    // 聚焦布局：pin > 投屏 > 发言人 > 第一块（fallback 顺序 = 视频优先，对齐旧版 allTiles 的遍历序）
    // pin 是用户的显式动作，压过投屏这条自动规则
    const focusKey = createMemo<string | null>(() => {
      const vids = videoEntries();
      const auds = audioEntries();
      if (vids.length + auds.length === 0) return null;
      const keys = [...vids.map((v) => v.key), ...auds.map((a) => a.key)];
      const pk = pinnedKey();
      if (pk && keys.includes(pk)) return pk;
      const scr = keys.find((k) => k.endsWith(':screen'));
      if (scr) return scr;
      const ls = lastSpeaker();
      if (ls) {
        for (const cand of [`${ls}:camera`, `${ls}:audio-tile`]) {
          if (keys.includes(cand)) return cand;
        }
      }
      return keys[0] ?? null;
    });
    const gridTiles = createMemo(() => (spotlight() ? tileEntries().filter((e) => e.key === focusKey()) : tileEntries()));
    const railTiles = createMemo(() => (spotlight() ? tileEntries().filter((e) => e.key !== focusKey()) : []));

    // 成员面板按设备维度平铺（与 tile/静音/踢出的操作粒度同构）：
    // 同人排序相邻，本机最前、推流设备靠后；人数另行统计
    const memberDevices = createMemo(() =>
      [...roster()].sort(
        (a, b) =>
          a.username.localeCompare(b.username) ||
          a.uid - b.uid ||
          Number(b.isLocal) - Number(a.isLocal) ||
          Number(a.ingest) - Number(b.ingest) ||
          a.identity.localeCompare(b.identity),
      ),
    );
    const memberUserCount = createMemo(() => new Set(roster().map(groupKey)).size);
    // 展示层按用户分组：单设备用户平铺一行，多设备用户展开树形设备子行
    const memberGroups = createMemo(() => {
      const groups = new Map<number | string, EPart[]>();
      for (const p of memberDevices()) {
        const k = groupKey(p);
        const g = groups.get(k);
        if (g) g.push(p);
        else groups.set(k, [p]);
      }
      return [...groups.values()];
    });

    const Tile = (p: { e: TileEntry }) =>
      p.e.kind === 'video' ? (
        <VideoTileView e={p.e} spotlight={spotlight} focusKey={focusKey} />
      ) : (
        <AudioTileView e={p.e} spotlight={spotlight} focusKey={focusKey} />
      );

    return (
      <div class="room-frame">
        <header class="topbar">
          {el(menuButtonHtml())}
          {el(icon('volume', 17, 'var(--ember)', 1.6))}
          <h1>{channel}</h1>
          <div class="vline"></div>
          <span class="sub">{metaText()}</span>
          <span
            class="conn-chip"
            classList={{ live: voiceState().phase === 'up', retry: voiceState().phase === 'retry' }}
          >
            <span class="dot"></span>
            <span>
              {voiceState().phase === 'up'
                ? '已连接'
                : voiceState().phase === 'connecting'
                  ? '连接中'
                  : voiceState().attempt > 0
                    ? `重连中·第 ${voiceState().attempt} 次`
                    : '重连中'}
            </span>
          </span>
          <div class="spacer"></div>
          <button
            class="hit btn btn-icon"
            classList={{ hidden: !canModerate() }}
            title="频道管理"
            onClick={() => openSettings('channel', settingsCtx)}
          >
            {el(icon('shield', 15, 'var(--text-1)', 1.6))}
          </button>
          <div class="seg-group" style="padding:3px;background:var(--bg-3)">
            <button
              class="hit seg"
              classList={{ on: !spotlight() }}
              style="display:flex;align-items:center;gap:6px;padding:5px 10px;font-size:12px"
              onClick={() => {
                const p = loadPrefs();
                p.layout = 'grid';
                savePrefs(p);
                setLayoutPref('grid');
              }}
            >
              {el(icon('grid', 14, 'currentColor', 1.7))}
              <span class="pill-label">九宫格</span>
            </button>
            <button
              class="hit seg"
              classList={{ on: spotlight() }}
              style="display:flex;align-items:center;gap:6px;padding:5px 10px;font-size:12px"
              onClick={() => {
                const p = loadPrefs();
                p.layout = 'spotlight';
                savePrefs(p);
                setLayoutPref('spotlight');
              }}
            >
              {el(icon('focus', 14, 'currentColor', 1.7))}
              <span class="pill-label">聚焦</span>
            </button>
          </div>
        </header>
        <div style="flex-grow:1;display:flex;min-height:0">
          <div style="flex-grow:1;display:flex;flex-direction:column;min-width:0;min-height:0">
            <div class="stage-area">
              <div class="stage-status">
                {statusText()}
                <Show when={voiceState().phase === 'retry'}>
                  <div class="stage-retry">
                    <button class="hit btn btn-sm" onClick={retryNow}>
                      立即重试
                    </button>
                  </div>
                </Show>
              </div>
              <Show when={audioBlocked()}>
                <button class="hit audio-blocked" onClick={() => void resumeAllAudio()}>
                  {el(icon('volume', 15, 'currentColor'))}
                  <span>浏览器拦截了自动播放，点击开启声音</span>
                </button>
              </Show>
              <div
                class="video-grid"
                classList={{ spotlight: spotlight() }}
                data-tiles={tileEntries().length}
                onContextMenu={(ev) => {
                  // 视频/音频卡片右键走同一个操作菜单
                  const identity = (ev.target as HTMLElement).closest<HTMLElement>('.tile')?.dataset.identity;
                  const p = parts().find((pp) => pp.identity === identity);
                  if (!p) return; // 对自己也弹（菜单内只放开本地静音项）
                  ev.preventDefault();
                  showUserMenu(ev.clientX, ev.clientY, p.uid, p.username, p.identity);
                }}
                onTouchStart={(ev) => {
                  // 触屏无右键：长按 500ms 弹同一个菜单（iOS Safari 不发 contextmenu）
                  const identity = (ev.target as HTMLElement).closest<HTMLElement>('.tile')?.dataset.identity;
                  const p = parts().find((pp) => pp.identity === identity);
                  if (!p) return;
                  const t = ev.touches[0];
                  longPressFired = false;
                  longPressTimer = window.setTimeout(() => {
                    longPressFired = true;
                    showUserMenu(t.clientX, t.clientY, p.uid, p.username, p.identity);
                  }, 500);
                }}
                onTouchMove={() => clearTimeout(longPressTimer)}
                onTouchEnd={(ev) => {
                  clearTimeout(longPressTimer);
                  // 长按已弹菜单：吞掉抬手的合成 click，否则会误点卡片底下的按钮（置顶/全屏等）
                  if (longPressFired) ev.preventDefault();
                }}
                onTouchCancel={() => clearTimeout(longPressTimer)}
              >
                <For each={gridTiles()}>{(e) => <Tile e={e} />}</For>
                <div class="rail">
                  <For each={railTiles()}>{(e) => <Tile e={e} />}</For>
                </div>
              </div>
              <div style="display:none" ref={audioBinEl}></div>
            </div>
            <div class="control-bar">
              <div class="group">
                <button
                  class="hit ctl-pill"
                  classList={{ on: micOn(), gagged: selfGagged() }}
                  title={selfGagged() ? '已被禁言' : micOn() ? '关闭麦克风' : '打开麦克风'}
                  onClick={() => void toggleMic()}
                >
                  {el(micIcon(17, !micOn(), 'currentColor'))}
                  <span class="pill-label">{micOn() ? '麦克风' : '麦克风已关'}</span>
                  <Show when={micOn()}>
                    <span class="mic-vu">
                      <i ref={(elm) => (vuBarEl = elm)}></i>
                    </span>
                  </Show>
                </button>
                <button
                  class="hit ctl-square"
                  classList={{ danger: deafened() }}
                  title={deafened() ? '已静音全部，点击恢复收听' : '静音全部（只影响自己听到的）'}
                  onClick={toggleDeaf}
                >
                  {el(slashIcon('speaker', 17, deafened(), deafened() ? 'var(--red)' : 'currentColor'))}
                </button>
                <button
                  class="hit ctl-square"
                  classList={{ on: cameraOn(), disabled: !stageOk() }}
                  title={stageOk() ? '摄像头' : stageHint()}
                  onClick={() => void toggleCamera()}
                >
                  {el(slashIcon('camera', 17, !cameraOn(), 'currentColor'))}
                </button>
                <button
                  class={'hit ctl-pill' + (canScreenShare ? '' : ' hidden')}
                  classList={{ on: screenOn(), disabled: !stageOk() }}
                  title={stageOk() ? '投屏' : stageHint()}
                  onClick={() => void toggleScreen()}
                >
                  {el(icon('screen', 17, 'currentColor'))}
                  <span class="pill-label">{screenOn() ? '投屏中' : '投屏'}</span>
                </button>
                <button class="hit ctl-square" title="投屏画质" onClick={() => openSettings('screen', settingsCtx)}>
                  {el(icon('sliders', 16, 'var(--text-1)', 1.6))}
                </button>
              </div>
              <div class="spacer"></div>
              <div class="group">
                <button
                  class="hit ctl-square"
                  classList={{ on: panel() === 'members' }}
                  title="成员"
                  onClick={() => switchPanel(panel() === 'members' ? '' : 'members')}
                >
                  {el(icon('users', 17, 'currentColor', 1.6))}
                </button>
                <button
                  class="hit ctl-square"
                  classList={{ on: panel() === 'chat' }}
                  title="聊天"
                  onClick={() => switchPanel(panel() === 'chat' ? '' : 'chat')}
                >
                  {el(icon('chat', 17, 'currentColor'))}
                  <span class="unread-badge" classList={{ hidden: unread() === 0 }}>
                    {unread() > 99 ? '99+' : unread()}
                  </span>
                </button>
                <button class="hit ctl-square" title="设置" onClick={() => openSettings('av', settingsCtx)}>
                  {el(icon('gear', 17, 'var(--text-1)'))}
                </button>
                <button class="hit ctl-pill danger" onClick={() => void leaveRoom()}>
                  {el(icon('leave', 17, 'var(--red)'))}
                  <span class="pill-label">离开房间</span>
                </button>
              </div>
            </div>
          </div>
          <aside class="side-panel" classList={{ hidden: panel() !== 'members' }}>
            <div class="panel-head">
              <span style="flex-grow:1">成员</span>
              <span class="mono" style="font-size:11px;color:var(--text-2)">
                {memberUserCount()}
              </span>
              <button
                class="hit mini-btn"
                style="width:28px;height:28px;border-radius:7px;display:flex;align-items:center;justify-content:center"
                onClick={() => switchPanel('')}
              >
                {el(icon('close', 15, 'var(--text-2)', 1.8))}
              </button>
            </div>
            <div
              class="panel-body"
              onContextMenu={(ev) => {
                const row = (ev.target as HTMLElement).closest<HTMLElement>('.member-row');
                if (!row?.dataset.uid) return;
                ev.preventDefault();
                showUserMenu(ev.clientX, ev.clientY, Number(row.dataset.uid), row.dataset.uname ?? '', row.dataset.identity);
              }}
            >
              <div>
                <div class="side-section-title">
                  在房 — {memberUserCount()} 人
                  <Show when={memberDevices().length > memberUserCount()}> · {memberDevices().length} 设备</Show>
                </div>
                <For each={memberGroups()}>
                  {(devices) => {
                    const first = devices[0];
                    const uname = first.username;
                    const isMe = first.uid === myUid;
                    const isOwner = uname === ownerName();
                    const anySpeaking = () => devices.some((d) => speaking().has(d.identity));
                    const devName = (p: EPart) =>
                      p.ingest ? `OBS 推流${p.tag ? ` · ${p.tag}` : ''}` : p.tag || p.identity;
                    const devSpeaking = (p: EPart) => speaking().has(p.identity);
                    const devMuted = (p: EPart) => volumePctFor(p.identity) === 0;
                    const devBits = (p: EPart) =>
                      [
                        p.sharing ? '投屏中' : '',
                        devSpeaking(p) ? '说话中' : !p.micOn && !p.ingest ? '已静音' : '',
                        !p.canPublish && !p.ingest ? '已禁言' : '',
                        !p.isLocal && devMuted(p)
                          ? '已本地屏蔽'
                          : !p.isLocal && volumePctFor(p.identity) !== 100
                            ? `音量 ${volumePctFor(p.identity)}%`
                            : '',
                      ]
                        .filter(Boolean)
                        .join(' · ');
                    // 音量按钮：点击 = 屏蔽/恢复；桌面 hover 弹出滑条（触屏走长按菜单）
                    const muteBtn = (p: EPart) => (
                      <Show when={!p.isLocal}>
                        <div class="volwrap">
                          <button
                            class="hit m-btn"
                            classList={{ 'muted-on': devMuted(p) }}
                            title={devMuted(p) ? '恢复该设备声音' : '屏蔽该设备声音'}
                            onClick={() => toggleVol(p.identity)}
                          >
                            {el(slashIcon('volume', 14, devMuted(p), devMuted(p) ? 'var(--red)' : 'var(--text-2)'))}
                          </button>
                          <div class="volpop">
                            <VolSlider identity={p.identity} />
                          </div>
                        </div>
                      </Show>
                    );
                    const kickBtn = (p: EPart) => {
                      const [kicking, setKicking] = createSignal(false);
                      return (
                        <Show when={!p.isLocal && (isMe || canModerate())}>
                          <button
                            class="hit m-btn"
                            classList={{ loading: kicking() }}
                            disabled={kicking()}
                            title="踢出该设备"
                            onClick={async () => {
                              const ok = await confirmDialog({
                                title: `确定踢出「${devName(p)}」？`,
                                body: '这台设备会被移出房间，之后仍可重新进入。',
                                confirmText: '踢出',
                                danger: true,
                              });
                              if (!ok) return;
                              setKicking(true);
                              try {
                                await kickUser(channel, p.uid, p.identity);
                                toast('已踢出该设备', 'ok');
                              } catch (err) {
                                toast((err as Error).message, 'bad');
                              } finally {
                                setKicking(false);
                              }
                            }}
                          >
                            {el(icon('leave', 14, 'var(--red)'))}
                          </button>
                        </Show>
                      );
                    };
                    // 单设备用户：一行平铺，状态行直接带设备名
                    if (devices.length === 1) {
                      const p = first;
                      return (
                        <div class="mgroup">
                          <div
                            class="member-row"
                            classList={{ 'owner-row': isOwner }}
                            data-identity={p.identity}
                            data-uid={p.uid}
                            data-uname={uname}
                          >
                            {el(avatarHtml(uname, 'avatar' + (devSpeaking(p) ? ' speaking' : '')))}
                            <div style="flex-grow:1;min-width:0">
                              <div class="m-name">
                                <span>{uname}</span>
                                <Show when={p.isLocal}>
                                  <span class="muted">（我）</span>
                                </Show>
                                <Show when={isOwner}>
                                  <span class="tag tag-ember">房主</span>
                                </Show>
                              </div>
                              <div class="m-status" classList={{ hot: devSpeaking(p) || p.sharing }}>
                                {devName(p)}
                                {devBits(p) ? ' · ' + devBits(p) : ''}
                              </div>
                            </div>
                            <button
                              class="hit m-btn"
                              title="更多操作"
                              onClick={(ev) => showUserMenu(ev.clientX, ev.clientY, p.uid, uname, p.identity)}
                            >
                              {el(icon('more', 16, 'var(--text-2)'))}
                            </button>
                            {muteBtn(p)}
                            {kickBtn(p)}
                          </div>
                        </div>
                      );
                    }
                    // 多设备用户：用户行 + 树形设备子行；用户行菜单作用于全部设备
                    return (
                      <div class="mgroup">
                        <div class="member-row muser" classList={{ 'owner-row': isOwner }} data-uid={first.uid} data-uname={uname}>
                          {el(avatarHtml(uname, 'avatar' + (anySpeaking() ? ' speaking' : '')))}
                          <div style="flex-grow:1;min-width:0">
                            <div class="m-name">
                              <span>{uname}</span>
                              <Show when={first.isLocal}>
                                <span class="muted">（我）</span>
                              </Show>
                              <Show when={isOwner}>
                                <span class="tag tag-ember">房主</span>
                              </Show>
                            </div>
                            <div class="m-status">{devices.length} 台设备</div>
                          </div>
                          <button
                            class="hit m-btn"
                            title="更多操作"
                            onClick={(ev) => showUserMenu(ev.clientX, ev.clientY, first.uid, uname)}
                          >
                            {el(icon('more', 16, 'var(--text-2)'))}
                          </button>
                        </div>
                        <For each={devices}>
                          {(p) => (
                            <div class="member-row mdev" data-identity={p.identity} data-uid={p.uid} data-uname={uname}>
                              <span class="d-ico" classList={{ local: p.isLocal }}>
                                {el(icon(p.ingest ? 'stream' : 'device', 13))}
                              </span>
                              <div style="flex-grow:1;min-width:0">
                                <div class="d-name">
                                  {devName(p)}
                                  <Show when={p.isLocal}>
                                    {' '}
                                    <span class="tag tag-ember">本机</span>
                                  </Show>
                                </div>
                                <div class="d-status" classList={{ hot: devSpeaking(p) || p.sharing }}>
                                  {devBits(p)}
                                </div>
                              </div>
                              {muteBtn(p)}
                              {kickBtn(p)}
                            </div>
                          )}
                        </For>
                      </div>
                    );
                  }}
                </For>
              </div>
            </div>
          </aside>
          <aside class="side-panel chat-panel" classList={{ hidden: panel() !== 'chat' }}>
            <div class="panel-head">
              <span class="mono" style="font-size:17px;color:var(--text-2);line-height:1">
                #
              </span>
              <span style="flex-grow:1">{channel}</span>
              <button
                class="hit mini-btn"
                style="width:28px;height:28px;border-radius:7px;display:flex;align-items:center;justify-content:center"
                onClick={() => switchPanel('')}
              >
                {el(icon('close', 15, 'var(--text-2)', 1.8))}
              </button>
            </div>
            <div class="chat-log" ref={chatLogEl}>
              <Show when={historyLoaded()}>
                <div class="chat-day">最近</div>
              </Show>
              <For each={chatItems()}>
                {(it) => ('sys' in it ? <div class="chat-sys">{it.text}</div> : <ChatMsgView m={it} />)}
              </For>
            </div>
            <div class="chat-input-wrap">
              <form class="chat-input-box" onSubmit={sendChat}>
                <input
                  ref={chatInputEl}
                  placeholder={chatPlaceholder()}
                  maxlength="2000"
                  autocomplete="off"
                  onInput={() => setChatReady(chatInputEl.value.trim().length > 0)}
                  onKeyDown={(ev) => {
                    if ((ev.ctrlKey || ev.metaKey) && ev.key === 'Enter') sendChat(ev);
                  }}
                />
                <button type="submit" class="hit send-btn" classList={{ ready: chatReady() }}>
                  {el(icon('back', 15, 'currentColor', 1.8))}
                </button>
              </form>
              <div class="chat-hint mono">仅保留最近 50 条历史</div>
            </div>
          </aside>
        </div>
      </div>
    );
  };

  const dispose = render(App, shell.content);
  const unwireMenu = wireMenuButton(root);

  // ---- 频道角色探测 ----
  void listChannels()
    .then((chs) => {
      const ch = chs.find((c) => c.name === channel);
      setMyRoleSig(ch?.my_role ?? '');
      setOwnerName(ch?.created_by ?? '');
    })
    .catch(() => {});

  // ---- 首次连接与清理 ----
  // 清理监听必须先于首次连接注册：连接期间用户离开时，清理块要能置 leaving 并释放已建好的部分
  const myHash = location.hash;
  const onHashChange = () => {
    if (location.hash !== myHash) {
      window.removeEventListener('hashchange', onHashChange);
      prefsBus.removeEventListener('prefs', onPrefs);
      navigator.mediaDevices?.removeEventListener?.('devicechange', onDeviceChange);
      document.removeEventListener('visibilitychange', onVisible);
      document.removeEventListener('fullscreenchange', onFsChange);
      document.removeEventListener('keydown', onFsKey);
      document.removeEventListener('keydown', onHotkeyDown);
      document.removeEventListener('keyup', onHotkeyUp);
      window.removeEventListener('blur', pttRelease);
      window.removeEventListener('online', retryNow);
      leaving = true;
      exitFs();
      clearTimeout(volSaveTimer);
      saveVolumes(volumes()); // 去抖的尾触可能还没落盘
      if (gainResume) {
        document.removeEventListener('pointerdown', gainResume);
        document.removeEventListener('keydown', gainResume);
      }
      srcNodes.forEach((s) => s.disconnect());
      srcNodes.clear();
      gainNodes.clear();
      void audioCtx?.close();
      clearInterval(vuTimer);
      tileTimers.forEach((t) => clearInterval(t));
      tileTimers.clear();
      void vuCtx?.close();
      clearTimeout(voiceLine.timer);
      if (stageLine && stageLine !== voiceLine) clearTimeout(stageLine.timer);
      chat.close();
      voiceLine.engine?.dispose();
      if (stageLine && stageLine !== voiceLine) stageLine.engine?.dispose();
      dispose();
      shell.destroy();
      unwireMenu();
    }
  };
  window.addEventListener('hashchange', onHashChange);

  updateStageButtons();
  await connectLines(true);
}
