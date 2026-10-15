// 房间连接层：进房拿凭证、按线连接、断线退避重连。
// 可变线路状态由房间页持有（本模块读写同一份 bag），引擎回调工厂与聊天补历史由参数注入。
import type { Accessor, Setter } from 'solid-js';
import { ApiError, fetchJoinCredentials } from '../../api';
import type { DataLine, EngineCred } from '../../api';
import { createEngine } from '../../engine';
import type { AVEngine, EngineCallbacks } from '../../engine/types';
import { isStaleChunkError, reloadForStale, staleReloadUsed } from '../../stale';
import { esc, icon, toast } from '../../ui';
import type { RoomDiag } from './diag';

export type Role = 'voice' | 'stage';

export interface Line {
  role: Role;
  engine: AVEngine | null;
  engineName: string;
  attempts: number;
  timer: number;
  inflight: boolean;
}

export type VoiceState = { phase: 'connecting' | 'up' | 'retry'; attempt: number };

/** 连接层可变状态：房间页与本模块共享同一对象，无第二真相源 */
export interface ConnectBag {
  voice: Line;
  stage: Line | null;
  combined: boolean;
  stageUp: boolean;
  stageIssue: string;
  leaving: boolean;
  bounced: boolean;
  appliedSpeakerId: string;
  cueBaseline: boolean;
}

export interface RoomConnectOpts {
  channel: string;
  channelId: () => number;
  bag: ConnectBag;
  diag: RoomDiag;
  endpointOf: (raw: string) => string;
  setStatusText: Setter<string>;
  setVoiceState: Setter<VoiceState>;
  setDataLine: Setter<DataLine>;
  micOn: Accessor<boolean>;
  setMicOn: Setter<boolean>;
  cameraOn: Accessor<boolean>;
  setCameraOn: Setter<boolean>;
  screenOn: Accessor<boolean>;
  setScreenOn: Setter<boolean>;
  nativeScreen: () => boolean;
  setConn: (up: boolean, meta: string) => void;
  clearLeavePrompt: () => void;
  updateStageButtons: () => void;
  refreshRoster: () => void;
  refreshMeta: () => void;
  checkSelfGag: () => void;
  captureErrorMsg: (kind: '麦克风' | '摄像头', err: unknown) => string;
  callbacksFor: (role: Role) => EngineCallbacks;
  syncMessages: (first: boolean) => void;
}

export interface RoomConnect {
  connectLines: (first: boolean, only?: Role) => Promise<void>;
  scheduleRejoin: (role: Role, delay?: number) => void;
  bounce: (msg: string) => void;
  retryNow: () => void;
  lineFor: (role: Role) => Line | null;
  connBoxMeta: () => string;
}

export function createRoomConnect(opts: RoomConnectOpts): RoomConnect {
  const bag = opts.bag;

  function bounce(msg: string) {
    if (bag.bounced) return;
    bag.bounced = true;
    opts.clearLeavePrompt();
    opts.setStatusText(msg);
    toast(msg, 'bad');
    setTimeout(() => {
      location.hash = '#/lobby';
    }, 1500);
  }

  function lineFor(role: Role): Line | null {
    return role === 'voice' ? bag.voice : bag.combined ? bag.voice : bag.stage;
  }

  function connBoxMeta(): string {
    const chip = (ic: string, label: string) =>
      `<span class="conn-chip">${icon(ic, 11, 'var(--text-2)', 1.7)}<span>${esc(label)}</span></span>`;
    let h = chip('volume', bag.voice.engineName || '—');
    if (bag.stage && !bag.combined) h += chip('screen', bag.stageUp ? bag.stage.engineName : '重连中');
    return h;
  }

  function scheduleRejoin(role: Role, delay?: number) {
    if (bag.leaving || bag.bounced) return;
    const line = lineFor(role);
    if (!line) return;
    clearTimeout(line.timer);
    const wait = delay ?? Math.min(30000, 2000 * 2 ** Math.min(line.attempts, 4)) * (0.7 + Math.random() * 0.6);
    if (role === 'voice' || bag.combined) {
      opts.setStatusText(line.attempts === 0 ? '连接断开，正在重连…' : `连接断开，正在重连…（第 ${line.attempts + 1} 次）`);
      opts.setVoiceState({ phase: 'retry', attempt: line.attempts + 1 });
    }
    line.timer = window.setTimeout(() => void connectLines(false, role), wait);
  }

  function handleCredsError(err: unknown, first: boolean) {
    const msg = (err as Error).message ?? '';
    if (first) opts.setStatusText(`连接失败：${msg}`);
    if (err instanceof ApiError) {
      if (err.status === 401) {
        bag.bounced = true;
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
    const attempt = line.attempts;
    const started = performance.now();
    const details = { engine: cred.engine, endpoint: opts.endpointOf(cred.url), attempt };
    opts.diag('info', 'connect_start', role, details);
    try {
      if (!line.engine || cred.engine !== line.engineName) {
        line.engine?.dispose();
        line.engine = await createEngine(cred.engine, opts.callbacksFor(role));
        line.engineName = cred.engine;
        bag.appliedSpeakerId = '';
        if (bag.leaving) {
          line.engine.dispose();
          line.engine = null;
          return;
        }
      }
      await line.engine.connect(cred.url, cred.token);
      if (bag.leaving) {
        line.engine.dispose();
        line.engine = null;
        return;
      }
      line.attempts = 0;
      opts.diag('info', 'connect_ok', role, { ...details, elapsed_ms: performance.now() - started });
      if (!first) opts.syncMessages(false);
      if (role === 'voice') {
        opts.setStatusText('');
        bag.cueBaseline = true;
        opts.setVoiceState({ phase: 'up', attempt: 0 });
        if (!first) toast('语音已重新连接', 'ok', 1800);
        opts.checkSelfGag();
        if (opts.micOn()) {
          try {
            await line.engine.setMic(true);
          } catch (err) {
            opts.setMicOn(false);
            if (first) toast(opts.captureErrorMsg('麦克风', err), 'bad');
          }
        }
      }
      if (role === 'stage' || bag.combined) {
        bag.stageUp = true;
        if (opts.cameraOn()) {
          try {
            await (role === 'stage' ? line.engine : bag.voice.engine)!.setCamera(true);
          } catch {
            opts.setCameraOn(false);
          }
        }
        if (opts.screenOn() && !opts.nativeScreen()) {
          opts.setScreenOn(false);
          toast('重连后投屏需要重新发起', '', 4000);
        }
      }
      opts.refreshRoster();
      opts.refreshMeta();
    } catch (err) {
      opts.diag('error', 'connect_failed', role, { ...details, elapsed_ms: performance.now() - started, error: err });
      if (isStaleChunkError(err)) {
        const stale = '页面版本已过期，请刷新页面';
        if (!staleReloadUsed()) {
          opts.clearLeavePrompt();
          if (reloadForStale()) return;
        }
        opts.setStatusText(stale);
        toast(stale, 'bad', 8000);
        return;
      }
      if (role === 'voice') {
        handleCredsError(err, first);
      } else {
        bag.stageUp = false;
        if (first) toast(`舞台线连接失败（投屏/摄像头暂不可用）：${(err as Error).message}`, 'bad', 4000);
        scheduleRejoin('stage');
      }
    } finally {
      line.inflight = false;
      opts.updateStageButtons();
    }
  }

  async function connectLines(first: boolean, only?: Role) {
    if (bag.leaving || bag.bounced) return;
    let creds;
    const started = performance.now();
    opts.diag('info', 'credentials_start', only, { attempt: lineFor(only ?? 'voice')?.attempts ?? 0 });
    try {
      creds = await fetchJoinCredentials(opts.channel, opts.channelId() || undefined);
    } catch (err) {
      opts.diag('error', 'credentials_failed', only, { elapsed_ms: performance.now() - started, error: err });
      handleCredsError(err, first);
      return;
    }
    opts.diag('info', 'credentials_ok', only, {
      elapsed_ms: performance.now() - started,
      state: creds.combined ? 'combined' : creds.stage ? 'split' : 'voice-only',
    });
    if (bag.leaving || bag.bounced) return;
    bag.combined = creds.combined;
    opts.setDataLine(creds.data_line ?? 'auto');
    if (!only || only === 'voice') {
      await connectLine(bag.voice, creds.voice, first, 'voice');
      if (bag.leaving) return;
    }
    if (bag.combined) {
      bag.stage = bag.voice;
      bag.stageUp = !!bag.voice.engine?.connected();
      bag.stageIssue = '';
    } else if (creds.stage) {
      bag.stageIssue = '';
      if (!bag.stage || bag.stage === bag.voice) {
        bag.stage = { role: 'stage', engine: null, engineName: '', attempts: 0, timer: 0, inflight: false };
      }
      if (!only || only === 'stage') {
        await connectLine(bag.stage, creds.stage, first, 'stage');
        if (bag.leaving) return;
      }
    } else {
      bag.stage = null;
      bag.stageUp = false;
      bag.stageIssue = creds.stage_error ?? '';
    }
    opts.updateStageButtons();
    opts.setConn(!!bag.voice.engine?.connected(), connBoxMeta());
  }

  const retryNow = () => {
    if (bag.leaving || bag.bounced) return;
    (['voice', 'stage'] as Role[]).forEach((role) => {
      const line = lineFor(role);
      if (role === 'stage' && (bag.combined || !line)) return;
      if (line && !line.engine?.connected()) {
        clearTimeout(line.timer);
        line.attempts = 0;
        if (role === 'voice') {
          opts.setStatusText('正在重新进入语音…');
          opts.setVoiceState({ phase: 'connecting', attempt: 0 });
        }
        void connectLines(false, role);
      }
    });
  };

  return { connectLines, scheduleRejoin, bounce, retryNow, lineFor, connBoxMeta };
}
