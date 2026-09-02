// 房间页（Solid 渲染）：布局、面板、聊天与断线重连；音视频经 AVEngine 接口驱动。
// 双线模型：语音线（voice，权威名册/说话状态）+ 舞台线（stage：投屏/摄像头/OBS 及其伴音）。
// 两线同一内核时（combined）一条连接承担两种角色；舞台线缺席或断开只禁用投屏/摄像头，语音不受影响。
//
// 结构分两层：
// - 连接层（非 UI）：Line 双线模型、重连调度、引擎回调、聊天连接、离开清理——命令式，逻辑与旧版一致；
// - 视图层（Solid）：信号驱动，引擎回调只写信号，DOM 由 JSX 派生，消灭手工 refresh* 互相调用。
import { createEffect, createMemo, createSignal, untrack, For, Show } from 'solid-js';
import { render } from 'solid-js/web';
import { clearSession, fetchJoinCredentials, getUser, kickUser, listChannels, muteUser } from '../api';
import type { EngineCred } from '../api';
import { connectChat } from '../chat';
import type { ChatMessage } from '../chat';
import { createEngine } from '../engine';
import type { AVEngine, EPart, EngineCallbacks, TrackSource, VideoStats } from '../engine/types';
import { encoderIsHw, loadPrefs, prefsBus, savePrefs } from '../prefs';
import { menuButtonHtml, renderShell, wireMenuButton } from '../shell';
import { avatarHtml, esc, fmtClock, icon, licon, micIcon, slashIcon, toast } from '../ui';
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
  obs: boolean;
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

// ui.ts 的图标/头像返回 HTML 字符串（内容可信：路径常量 + 已转义的名字首字母），转成真实节点给 JSX 插入
function el(html: string): Element {
  const t = document.createElement('template');
  t.innerHTML = html;
  return t.content.firstElementChild!;
}

export async function renderRoom(root: HTMLElement, channel: string) {
  const prefs = loadPrefs();
  const canScreenShare = typeof navigator.mediaDevices?.getDisplayMedia === 'function';

  const shell = renderShell(root, { activeChannel: channel });
  shell.setConn(false, '正在协商…');

  const myUsername = getUser()?.username ?? '';
  const obsIdentity = `${myUsername}-obs`;

  // ---- 连接层状态（非响应式）----
  const voiceLine: Line = { role: 'voice', engine: null, engineName: '', attempts: 0, timer: 0, inflight: false };
  let stageLine: Line | null = null; // null = 无舞台线；combined 时与 voiceLine 同引擎
  let combined = false;
  let stageUp = false;
  let leaving = false;
  let bounced = false;
  let seq = 0; // 卡片到达顺序计数
  const audioEls = new Map<string, Set<SinkMedia>>();
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
  const [lastSpeaker, setLastSpeaker] = createSignal<string | null>(null);
  const [volumes, setVolumes] = createSignal<Map<string, number>>(new Map()); // 本地音量（0=屏蔽）
  const [panel, setPanel] = createSignal<'members' | 'chat' | ''>(
    window.matchMedia('(min-width: 1200px)').matches ? 'members' : '',
  );
  const [unread, setUnread] = createSignal(0);
  const [msgs, setMsgs] = createSignal<ChatMessage[]>([]);
  const [historyLoaded, setHistoryLoaded] = createSignal(false);
  const [chatReady, setChatReady] = createSignal(false); // 输入框非空 → 发送按钮点亮
  const [chatPlaceholder, setChatPlaceholder] = createSignal(`发消息到 #${channel}`);
  const [isOwnerSig, setIsOwnerSig] = createSignal(false);
  const [ownerName, setOwnerName] = createSignal('');

  // DOM ref（引擎产的命令式元素挂载点等）
  let audioBinEl!: HTMLDivElement;
  let chatLogEl!: HTMLDivElement;
  let chatInputEl!: HTMLInputElement;
  let vuBarEl: HTMLElement | undefined; // 麦克风 VU 条（micOn 时才在 DOM 里）

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

  const volumeFor = (identity: string) => volumes().get(identity) ?? (identity === obsIdentity ? 0 : 1);

  function applyAudioPrefs() {
    const p = loadPrefs();
    const master = deafened() ? 0 : p.volume / 100;
    audioEls.forEach((set, identity) => {
      const v = master * volumeFor(identity);
      set.forEach((elm) => {
        // iOS Safari 的 volume 只读（设置被静默忽略），静音必须走 muted 属性
        elm.muted = v === 0;
        elm.volume = v;
        if (p.speakerId && typeof elm.setSinkId === 'function') {
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
        obs: part.obs,
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
    maybeClearStatus();
    refreshMeta();
  }

  function togglePin(key: string) {
    setPinnedKey((k) => (k === key ? null : key));
  }

  // ---- 用户操作菜单（聊天卡片、成员行与视频卡片右键共用；挂 body，保持命令式）----
  // 管理操作（禁言/踢出）= 房主或管理员，与后端 requireModerator 一致
  const canModerate = () => isOwnerSig() || getUser()?.is_admin === true;

  let longPressTimer = 0; // 触屏长按弹菜单的定时器（tile 触摸事件共用）

  // identity 非空 = 设备模式（卡片/成员行入口）：菜单只控制这一台设备；
  // 空 = 用户模式（聊天头像入口）：列出该用户全部设备。禁言始终是用户级操作。
  function showUserMenu(x: number, y: number, username: string, identity?: string) {
    document.querySelector('.user-menu')?.remove();
    const isSelf = username === myUsername;
    const deviceMode = !!identity;
    const targets = parts().filter(
      (p) => !p.isLocal && p.username === username && (!deviceMode || p.identity === identity),
    );
    const muted = targets.some((p) => volumeFor(p.identity) === 0);
    const devName = (p: EPart) => (p.obs ? 'OBS 推流' : p.identity.slice(username.length + 1) || p.identity);
    // 禁言判定只看真人设备：OBS 推流参与者（ingress 自带发布权限）会污染 every() 推断
    const voiceTargets = targets.filter((p) => !p.obs);
    const gagged = voiceTargets.length > 0 && voiceTargets.every((p) => !p.canPublish);
    const gagBtn = (on: boolean, label: string) =>
      `<button class="hit um-item${on ? ' danger' : ''}" data-gag="${on}">${micIcon(14, on, on ? 'var(--red)' : 'currentColor')}<span>${label}</span></button>`;
    const menu = document.createElement('div');
    menu.className = 'user-menu';
    menu.innerHTML = `
      <div class="um-title">${esc(username)}${isSelf ? '（我）' : ''}${deviceMode && targets[0] ? ` · ${esc(devName(targets[0]))}` : ''}</div>
      ${
        targets.length > 1
          ? targets
              .map((p) => {
                const m = volumeFor(p.identity) === 0;
                return `<button class="hit um-item" data-mute-id="${esc(p.identity)}">${slashIcon('volume', 14, !m, m ? 'var(--red)' : 'currentColor')}<span>${m ? '恢复' : '屏蔽'} ${esc(devName(p))}</span></button>`;
              })
              .join('') +
            `<button class="hit um-item" data-act="mute">${slashIcon('volume', 14, !muted, 'currentColor')}<span>${muted ? '恢复全部声音' : '屏蔽全部声音'}</span></button>`
          : targets.length
            ? `<button class="hit um-item" data-act="mute">${slashIcon('volume', 14, !muted, 'currentColor')}<span>${muted ? '恢复声音' : '屏蔽声音'}</span></button>`
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
    menu.querySelector('[data-act="mute"]')?.addEventListener('click', () => {
      setVolumes((prev) => {
        const m = new Map(prev);
        targets.forEach((p) => m.set(p.identity, muted ? 1 : 0));
        return m;
      });
      applyAudioPrefs();
      close();
    });
    menu.querySelectorAll<HTMLButtonElement>('[data-gag]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        close();
        const on = btn.dataset.gag === 'true';
        try {
          await muteUser(channel, username, on);
          toast(on ? `已禁言 ${username}` : `已解除 ${username} 的禁言`, 'ok');
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      });
    });
    menu.querySelectorAll<HTMLButtonElement>('[data-mute-id]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const id = btn.dataset.muteId!;
        setVolumes((prev) => {
          const m = new Map(prev);
          m.set(id, volumeFor(id) === 0 ? 1 : 0);
          return m;
        });
        applyAudioPrefs();
        close();
      });
    });
    menu.querySelectorAll<HTMLButtonElement>('[data-kick-id]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        close();
        try {
          await kickUser(channel, username, btn.dataset.kickId!);
          toast('已踢出该设备', 'ok');
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      });
    });
    menu.querySelector('[data-act="kick"]')?.addEventListener('click', async () => {
      close();
      try {
        await kickUser(channel, username);
        toast(isSelf ? '已踢出你的全部设备（含本机与 OBS）' : `已把 ${username} 移出房间`, 'ok');
      } catch (err) {
        toast((err as Error).message, 'bad');
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
    if (first) setStatusText(`连接失败：${msg}`);
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
        setStatusText('');
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
        applyAudioPrefs();
      },
      onAudioTrackRemoved: (identity, els) => {
        els.forEach((elm) => {
          elm.remove();
          audioEls.get(identity)?.delete(elm as SinkMedia);
        });
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
          shell.setConn(false, '正在恢复连接…');
        }
      },
      onReconnected: () => {
        if (role === 'voice' || combined) {
          setStatusText('');
          shell.setConn(true, connBoxMeta());
        }
        refreshRoster();
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

  // 自己被禁言（服务端收走发布权限 canPublish=false；持久禁言重进房时权限初始即为 false）
  let selfGagged = false;
  const isSelfGagged = () => voiceLine.engine?.participants().some((p) => p.isLocal && !p.canPublish) === true;

  function checkSelfGag() {
    const g = isSelfGagged();
    if (g === selfGagged) return;
    selfGagged = g;
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
      const p = loadPrefs();
      p.mic = micOn();
      savePrefs(p);
    } catch (err) {
      if (micOn()) toast(captureErrorMsg('麦克风', err), 'bad');
      setMicOn((m) => !m);
    }
    refreshRoster();
  }

  function toggleDeaf() {
    setDeafened((d) => !d);
    applyAudioPrefs();
  }

  async function toggleCamera() {
    const eng = stageEngine();
    if (!eng || !stageUp) return;
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
    if (!eng || !stageUp) return;
    setScreenOn((s) => !s);
    try {
      await eng.setScreen(screenOn());
    } catch {
      setScreenOn((s) => !s); // 用户取消选择等情况回退
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
    if (content) {
      chat.send(content);
      chatInputEl.value = '';
      setChatReady(false);
    }
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
    if (what === 'mic' && loadPrefs().mic !== micOn()) {
      void toggleMic();
      return;
    }
    if ((what === 'mic-device' || what === 'audio-chain') && micOn() && voiceLine.engine) {
      try {
        await voiceLine.engine.restartMic();
      } catch {
        setMicOn(false);
      }
    }
    if (what === 'cam-device' && cameraOn()) {
      const p = loadPrefs();
      if (p.camDeviceId) void stageEngine()?.switchCamera(p.camDeviceId).catch(() => {});
    }
  };
  prefsBus.addEventListener('prefs', onPrefs);

  // ---- 视图组件 ----

  // 静态头像（聊天卡片等，不随信号变化）
  const Avatar = (p: { name: string; cls?: string; onClick?: (ev: MouseEvent) => void }) => {
    const a = el(avatarHtml(p.name, p.cls ?? 'avatar')) as HTMLElement;
    if (p.onClick) a.addEventListener('click', p.onClick);
    return a;
  };

  const VideoTileView = (p: { e: VideoEntry; spotlight: () => boolean; focusKey: () => string | null }) => {
    const e = p.e;
    const iv = e.video as IOSVideo;
    const fsOk = typeof iv.requestFullscreen === 'function' || typeof iv.webkitEnterFullscreen === 'function';
    const name = e.isLocal && e.source === 'camera' ? '你' : e.display;
    return (
      <div
        class={'tile' + (e.source === 'screen' ? ' tile-screen' : '')}
        classList={{
          speaking: speaking().has(e.identity),
          pinned: pinnedKey() === e.key,
          featured: p.spotlight() && p.focusKey() === e.key,
        }}
        data-identity={e.identity}
        onClick={(ev) => {
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
          <Show when={e.obs}>
            <div class="spec-badge">OBS · WHIP</div>
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
          <Show when={fsOk}>
            <button
              class="hit tact"
              title="全屏"
              onClick={(ev) => {
                ev.stopPropagation();
                if (typeof iv.requestFullscreen === 'function') void iv.requestFullscreen();
                else iv.webkitEnterFullscreen?.();
              }}
            >
              {el(icon('fullscreen', 15))}
            </button>
          </Show>
        </div>
      </div>
    );
  };

  const AudioTileView = (p: { e: AudioEntry; spotlight: () => boolean; focusKey: () => string | null }) => {
    const part = createMemo(() => roster().find((pp) => pp.identity === p.e.identity));
    const isSpeaking = createMemo(() => speaking().has(p.e.identity));
    const micOff = () => !(part()?.micOn ?? false) && !(part()?.obs ?? false);
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
        <Show when={part()?.obs}>
          <div style="position:absolute;top:12px;left:12px" class="tag tag-ember mono">
            OBS 推流
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
    const mine = p.m.username === myUsername;
    const openMenu = (ev: MouseEvent) => showUserMenu(ev.clientX, ev.clientY, p.m.username);
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

    // 新消息 / 历史到达后滚到底（原实现每次 append 后滚动）
    createEffect(() => {
      msgs();
      chatLogEl.scrollTop = chatLogEl.scrollHeight;
    });

    const spotlight = createMemo(() => layoutPref() === 'spotlight');
    // 全部卡片按到达顺序（九宫格排位 = 旧版 DOM 插入顺序）
    const tileEntries = createMemo<TileEntry[]>(() => [...videoEntries(), ...audioEntries()].sort((a, b) => a.seq - b.seq));
    // 聚焦布局：投屏 > pin > 发言人 > 第一块（fallback 顺序 = 视频优先，对齐旧版 allTiles 的遍历序）
    const focusKey = createMemo<string | null>(() => {
      const vids = videoEntries();
      const auds = audioEntries();
      if (vids.length + auds.length === 0) return null;
      const keys = [...vids.map((v) => v.key), ...auds.map((a) => a.key)];
      const scr = keys.find((k) => k.endsWith(':screen'));
      if (scr) return scr;
      const pk = pinnedKey();
      if (pk && keys.includes(pk)) return pk;
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

    // 成员按账号聚合（一个账号多台设备合一行）
    // 成员面板按设备维度平铺（与 tile/静音/踢出的操作粒度同构）：
    // 同人排序相邻，本机最前、OBS 靠后；人数另行统计
    const memberDevices = createMemo(() =>
      [...roster()].sort(
        (a, b) =>
          a.username.localeCompare(b.username) ||
          Number(b.isLocal) - Number(a.isLocal) ||
          Number(a.obs) - Number(b.obs) ||
          a.identity.localeCompare(b.identity),
      ),
    );
    const memberUserCount = createMemo(() => new Set(roster().map((p) => p.username)).size);
    // 展示层按用户分组：单设备用户平铺一行，多设备用户展开树形设备子行
    const memberGroups = createMemo(() => {
      const groups = new Map<string, EPart[]>();
      for (const p of memberDevices()) {
        const g = groups.get(p.username);
        if (g) g.push(p);
        else groups.set(p.username, [p]);
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
          <div class="spacer"></div>
          <button
            class="hit btn btn-icon"
            classList={{ hidden: !isOwnerSig() }}
            title="频道管理（新标签打开，不离开房间）"
            onClick={() => window.open(`#/manage/${encodeURIComponent(channel)}`, '_blank')}
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
              <div class="stage-status">{statusText()}</div>
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
                  showUserMenu(ev.clientX, ev.clientY, p.username, p.identity);
                }}
                onTouchStart={(ev) => {
                  // 触屏无右键：长按 500ms 弹同一个菜单（iOS Safari 不发 contextmenu）
                  const identity = (ev.target as HTMLElement).closest<HTMLElement>('.tile')?.dataset.identity;
                  const p = parts().find((pp) => pp.identity === identity);
                  if (!p) return;
                  const t = ev.touches[0];
                  longPressTimer = window.setTimeout(() => showUserMenu(t.clientX, t.clientY, p.username, p.identity), 500);
                }}
                onTouchMove={() => clearTimeout(longPressTimer)}
                onTouchEnd={() => clearTimeout(longPressTimer)}
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
                <button class="hit ctl-pill" classList={{ on: micOn() }} onClick={() => void toggleMic()}>
                  {el(micIcon(17, !micOn(), 'currentColor'))}
                  <span class="pill-label">{micOn() ? '麦克风' : '已静音'}</span>
                  <Show when={micOn()}>
                    <span class="mic-vu">
                      <i ref={(elm) => (vuBarEl = elm)}></i>
                    </span>
                  </Show>
                </button>
                <button
                  class="hit ctl-square"
                  classList={{ danger: deafened() }}
                  title="全体静音（只影响自己）"
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
                <button class="hit ctl-square" title="投屏画质" onClick={() => openSettings('screen', { backLabel: `返回 ${channel}` })}>
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
                <button class="hit ctl-square" title="设置" onClick={() => openSettings('devices', { backLabel: `返回 ${channel}` })}>
                  {el(icon('gear', 17, 'var(--text-1)'))}
                </button>
                <button
                  class="hit ctl-pill danger"
                  onClick={() => {
                    location.hash = '#/lobby';
                  }}
                >
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
                if (!row?.dataset.uname) return;
                ev.preventDefault();
                showUserMenu(ev.clientX, ev.clientY, row.dataset.uname, row.dataset.identity);
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
                    const isMe = uname === myUsername;
                    const isOwner = uname === ownerName();
                    const anySpeaking = () => devices.some((d) => speaking().has(d.identity));
                    const devName = (p: EPart) =>
                      p.obs ? 'OBS 推流' : p.identity.slice(uname.length + 1) || p.identity;
                    const devSpeaking = (p: EPart) => speaking().has(p.identity);
                    const devMuted = (p: EPart) => volumeFor(p.identity) === 0;
                    const devBits = (p: EPart) =>
                      [
                        p.sharing ? '投屏中' : '',
                        devSpeaking(p) ? '说话中' : !p.micOn && !p.obs ? '已静音' : '',
                        !p.canPublish && !p.obs ? '已禁言' : '',
                        devMuted(p) && !p.isLocal ? '已本地屏蔽' : '',
                      ]
                        .filter(Boolean)
                        .join(' · ');
                    const muteBtn = (p: EPart) => (
                      <Show when={!p.isLocal}>
                        <button
                          class="hit m-btn"
                          classList={{ 'muted-on': devMuted(p) }}
                          title={devMuted(p) ? '恢复该设备声音' : '屏蔽该设备声音'}
                          onClick={() => {
                            setVolumes((prev) => {
                              const m = new Map(prev);
                              m.set(p.identity, devMuted(p) ? 1 : 0);
                              return m;
                            });
                            applyAudioPrefs();
                          }}
                        >
                          {el(slashIcon('volume', 14, devMuted(p), devMuted(p) ? 'var(--red)' : 'var(--text-2)'))}
                        </button>
                      </Show>
                    );
                    const kickBtn = (p: EPart) => (
                      <Show when={!p.isLocal && (isMe || canModerate())}>
                        <button
                          class="hit m-btn"
                          title="踢出该设备"
                          onClick={async () => {
                            try {
                              await kickUser(channel, uname, p.identity);
                              toast('已踢出该设备', 'ok');
                            } catch (err) {
                              toast((err as Error).message, 'bad');
                            }
                          }}
                        >
                          {el(icon('leave', 14, 'var(--red)'))}
                        </button>
                      </Show>
                    );
                    // 单设备用户：一行平铺，状态行直接带设备名
                    if (devices.length === 1) {
                      const p = first;
                      return (
                        <div class="mgroup">
                          <div
                            class="member-row"
                            classList={{ 'owner-row': isOwner }}
                            data-identity={p.identity}
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
                              onClick={(ev) => showUserMenu(ev.clientX, ev.clientY, uname, p.identity)}
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
                        <div class="member-row muser" classList={{ 'owner-row': isOwner }} data-uname={uname}>
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
                            onClick={(ev) => showUserMenu(ev.clientX, ev.clientY, uname)}
                          >
                            {el(icon('more', 16, 'var(--text-2)'))}
                          </button>
                        </div>
                        <For each={devices}>
                          {(p) => (
                            <div class="member-row mdev" data-identity={p.identity} data-uname={uname}>
                              <span class="d-ico" classList={{ local: p.isLocal }}>
                                {el(icon(p.obs ? 'stream' : 'device', 13))}
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
              <For each={msgs()}>{(m) => <ChatMsgView m={m} />}</For>
            </div>
            <div class="chat-input-wrap">
              <form class="chat-input-box" onSubmit={sendChat}>
                <input
                  ref={chatInputEl}
                  placeholder={chatPlaceholder()}
                  autocomplete="off"
                  onInput={() => setChatReady(chatInputEl.value.trim().length > 0)}
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
  wireMenuButton(root);

  // ---- 房主探测 ----
  void listChannels()
    .then((chs) => {
      const ch = chs.find((c) => c.name === channel);
      setIsOwnerSig(ch?.is_owner === true);
      setOwnerName(ch?.created_by ?? '');
    })
    .catch(() => {});

  // ---- 首次连接与清理 ----
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
    }
  };
  window.addEventListener('hashchange', onHashChange);
}
