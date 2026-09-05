// AVEngine 的 LiveKit 实现：livekit-client 的全部使用收敛在此。
// 采集链（RNNoise / 设备选择 / 处理开关）与发布参数（语音码率、投屏编码/SVC）按 prefs 读取。
import {
  DisconnectReason,
  Participant,
  RemoteParticipant,
  RemoteTrack,
  Room,
  RoomEvent,
  Track,
} from 'livekit-client';
import type { AudioCaptureOptions, LocalVideoTrack, ScreenShareCaptureOptions, TrackPublishOptions, VideoCodec } from 'livekit-client';
import { RnnoisePipeline } from '../audio';
import { RES_DIMS, loadPrefs } from '../prefs';
import type { RoomPrefs, ScreenCodec } from '../prefs';
import type { AVEngine, EPart, EngineCallbacks, TrackSource, VideoStats } from './types';

const toSource = (s: Track.Source): TrackSource | null =>
  s === Track.Source.Camera ? 'camera' : s === Track.Source.ScreenShare ? 'screen' : null;

// 一条 candidate-pair 的格式化快照（endpoint 已按 `protocol/candidateType addr:port` 拼好）
interface IcePairSnapshot {
  state: string;
  nominated: boolean;
  local: string;
  remote: string;
}

// 一个 transport（pub/sub）的最新一次 getStats 快照：只保留结果，不逐条上报
interface IceTransportSnapshot {
  pairs: IcePairSnapshot[];
  local: string[]; // 去重后的本地候选
  remote: string[]; // 去重后的远端候选
  selected: string | null; // 当前选中 pair 的远端 endpoint
}

export class LiveKitEngine implements AVEngine {
  // 凭证是短时效入场券，断线后必须回房间层重新签发并重做入场判定。禁用 SDK 内部
  // resume 也避免无 Redis 的 stage 重启后，客户端拿已消失的 participant 状态反复
  // reconnect=1，卡在 STATE_MISMATCH 而永远不发起完整 join。
  private room = new Room({ reconnectPolicy: { nextRetryDelayInMs: () => null } });
  private cbs: EngineCallbacks;
  private rnnoise = new RnnoisePipeline();
  private rnnoiseBroken = false;
  private disposed = false;
  private resume = () => void this.rnnoise.resume();
  private iceProbeTimer: number | undefined;
  private snapshot: Record<'pub' | 'sub', IceTransportSnapshot | null> = { pub: null, sub: null };

  constructor(cbs: EngineCallbacks) {
    this.cbs = cbs;
    // AudioContext 自动播放策略：用户首次点击时恢复
    document.addEventListener('pointerdown', this.resume, false);
    this.wire();
  }

  private toPart(p: Participant): EPart {
    const micPub = p.getTrackPublication(Track.Source.Microphone);
    // 元数据是 hearth 下发的 rtc.Meta JSON（uid/username/kind/tag），进房令牌与推流发布
    // 两条路径都写。身份与展示全走它——identity 的主体是 user_id，本就不含用户名
    let meta: { uid?: number; username?: string; kind?: string; tag?: string } | null = null;
    if (p.metadata) {
      try {
        meta = JSON.parse(p.metadata);
      } catch {
        meta = null; // 非 JSON 元数据按普通参与者处理
      }
    }
    const ingest = meta?.kind === 'ingest';
    return {
      identity: p.identity,
      uid: meta?.uid ?? 0,
      username: meta?.username ?? p.name ?? '',
      display: p.name || p.identity,
      isLocal: p.identity === this.room.localParticipant.identity,
      micOn: !!micPub && !micPub.isMuted,
      canPublish: p.permissions?.canPublish !== false, // 服务端禁言会收走发布权限
      sharing: !!p.getTrackPublication(Track.Source.ScreenShare),
      ingest,
      tag: meta?.tag ?? '', // 浏览器参与者也有设备标签，展示设备名要用它
    };
  }

  private emitVideo(p: Participant, track: Track, isLocal: boolean) {
    const source = toSource(track.source);
    if (!source) return;
    const el = track.attach() as HTMLVideoElement;
    el.autoplay = true;
    if (el instanceof HTMLVideoElement) el.playsInline = true;
    if (isLocal) el.muted = true; // 本地画面静音避免回授
    this.cbs.onVideoTrack(this.toPart(p), source, el);
  }

  private removeTrack(p: Participant, track: Track) {
    if (track.kind === Track.Kind.Audio) {
      this.cbs.onAudioTrackRemoved(p.identity, track.detach());
      return;
    }
    const source = toSource(track.source);
    if (source) this.cbs.onVideoTrackRemoved(p.identity, source, track.detach());
  }

  private wire() {
    this.room
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack, _pub, p: RemoteParticipant) => {
        if (track.kind === Track.Kind.Audio) {
          this.cbs.onAudioTrack(p.identity, track.attach());
          return;
        }
        this.emitVideo(p, track, false);
      })
      .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack, _pub, p: RemoteParticipant) => this.removeTrack(p, track))
      // 视频静音即移除画面（无视频不占位），取消静音再插回；音频 mute 只影响状态展示
      .on(RoomEvent.TrackMuted, (pub, p) => {
        if (pub.track && pub.kind === Track.Kind.Video) this.removeTrack(p, pub.track);
        if (pub.source === Track.Source.Microphone) this.cbs.onRoster();
      })
      .on(RoomEvent.TrackUnmuted, (pub, p) => {
        if (pub.track && pub.kind === Track.Kind.Video) {
          this.emitVideo(p, pub.track, p.identity === this.room.localParticipant.identity);
        }
        if (pub.source === Track.Source.Microphone) this.cbs.onRoster();
      })
      .on(RoomEvent.ActiveSpeakersChanged, (speakers) => this.cbs.onSpeakers(speakers.map((s) => s.identity)))
      .on(RoomEvent.LocalTrackPublished, (pub) => {
        if (pub.track && pub.kind === Track.Kind.Video) this.emitVideo(this.room.localParticipant, pub.track, true);
        this.cbs.onRoster();
      })
      .on(RoomEvent.LocalTrackUnpublished, (pub) => {
        if (pub.track) this.removeTrack(this.room.localParticipant, pub.track);
        this.cbs.onRoster();
      })
      // 远端开始/停止投屏：名册里的 sharing 标记来自发布列表，订阅事件不覆盖「未订阅就撤了」的情况
      .on(RoomEvent.TrackPublished, () => this.cbs.onRoster())
      .on(RoomEvent.TrackUnpublished, () => this.cbs.onRoster())
      .on(RoomEvent.ParticipantConnected, () => this.cbs.onRoster())
      .on(RoomEvent.ParticipantDisconnected, () => this.cbs.onRoster())
      // 禁言/解禁（canPublish 变化）：走名册刷新，视图据此更新徽标与自我提示
      .on(RoomEvent.ParticipantPermissionsChanged, () => this.cbs.onRoster())
      // 自动播放被拦截：SDK 自己不会出提示，交给房间层弹「点击开启声音」
      .on(RoomEvent.AudioPlaybackStatusChanged, () => {
        if (!this.room.canPlaybackAudio) this.cbs.onAudioBlocked?.();
      })
      .on(RoomEvent.ConnectionStateChanged, (state) => this.cbs.onDiagnostic?.('connection_state', String(state)))
      .on(RoomEvent.SignalConnected, () => this.cbs.onDiagnostic?.('signal_connected', 'connected'))
      .on(RoomEvent.Reconnecting, () => this.cbs.onReconnecting())
      .on(RoomEvent.Reconnected, () => this.cbs.onReconnected())
      .on(RoomEvent.Disconnected, (reason) => {
        if (this.disposed) return;
        if (reason === DisconnectReason.CLIENT_INITIATED) return; // 自己调 disconnect
        if (reason === DisconnectReason.PARTICIPANT_REMOVED) return this.cbs.onEnded('kicked');
        if (reason === DisconnectReason.ROOM_DELETED) return this.cbs.onEnded('room-deleted');
        if (reason === DisconnectReason.DUPLICATE_IDENTITY) return this.cbs.onEnded('duplicate');
        this.cbs.onEnded('lost');
      });
  }

  // SDK 自己分别对 WebSocket 与 PeerConnection 做 15 秒超时，并在失败时关闭 engine。
  // 这里不能再套同期限的 Promise.race：外层先超时会绕过 SDK 的清理，下一次完整入场
  // 可能与尚未退出的 participant 重叠，被服务端判成 duplicate identity。
  async connect(url: string, token: string) {
    this.startIceProbe();
    try {
      await this.room.connect(url, token);
      await this.captureIceStats();
      this.emitSelectedServer();
    } catch (err) {
      // connect 失败时 SDK 会立即清理 PeerConnection；轮询负责在清理前留下候选，
      // 这里再尽力抓一次最终状态（PC 可能已经被清理，抓不到就用轮询期间攒下的快照）。
      await this.captureIceStats();
      this.emitIceFailed();
      throw err;
    } finally {
      this.stopIceProbe();
    }
  }

  private startIceProbe() {
    this.stopIceProbe();
    this.snapshot = { pub: null, sub: null };
    // RTCEngine/PCTransport 要等信令 JoinResponse 后才创建，连接期间轮询才能在
    // SDK 超时关闭 PeerConnection 之前留下失败候选；轮询只刷新快照，不上报。
    this.iceProbeTimer = window.setInterval(() => void this.captureIceStats(), 400);
  }

  private stopIceProbe() {
    if (this.iceProbeTimer !== undefined) window.clearInterval(this.iceProbeTimer);
    this.iceProbeTimer = undefined;
  }

  private emitSelectedServer() {
    const pub = this.snapshot.pub?.selected ?? '';
    const sub = this.snapshot.sub?.selected ?? '';
    this.cbs.onDiagnostic?.('selected_server', `pub ${pub}`, sub ? `sub ${sub}` : '');
  }

  private emitIceFailed() {
    const pub = this.snapshot.pub;
    const sub = this.snapshot.sub;
    const state = `pub ${pub?.pairs.length ?? 0} pairs, sub ${sub?.pairs.length ?? 0} pairs`;
    const fmt = (target: 'pub' | 'sub', pair: IcePairSnapshot) =>
      `${target} ${pair.state}${pair.nominated ? ' nominated' : ''} ${pair.local} -> ${pair.remote}`;
    const lines = [...(pub?.pairs ?? []).map((p) => fmt('pub', p)), ...(sub?.pairs ?? []).map((p) => fmt('sub', p))];
    const shown = lines.slice(0, 24);
    if (lines.length > shown.length) shown.push(`...(+${lines.length - shown.length})`);
    const localList = [...new Set([...(pub?.local ?? []), ...(sub?.local ?? [])])];
    const remoteList = [...new Set([...(pub?.remote ?? []), ...(sub?.remote ?? [])])];
    shown.push(`local: ${localList.join(', ')}`, `remote: ${remoteList.join(', ')}`);
    this.cbs.onDiagnostic?.('ice_failed', state, shown.join('\n').slice(0, 2000));
  }

  private async captureIceStats() {
    type StatsTransport = { getStats?: () => Promise<RTCStatsReport> | undefined };
    const manager = (this.room as unknown as {
      engine?: { pcManager?: { publisher?: StatsTransport; subscriber?: StatsTransport } };
    }).engine?.pcManager;
    if (!manager) return;
    await Promise.all([
      this.captureTransportStats('pub', manager.publisher),
      this.captureTransportStats('sub', manager.subscriber),
    ]);
  }

  private async captureTransportStats(target: 'pub' | 'sub', transport?: { getStats?: () => Promise<RTCStatsReport> | undefined }) {
    const pending = transport?.getStats?.();
    if (!pending) return;
    try {
      const report = await pending;
      const byID = new Map<string, Record<string, unknown>>();
      const pairs: Record<string, unknown>[] = [];
      let selectedPairID = '';
      report.forEach((raw) => {
        const stat = raw as unknown as Record<string, unknown>;
        const id = String(stat.id ?? '');
        if (id) byID.set(id, stat);
        if (stat.type === 'transport') selectedPairID = String(stat.selectedCandidatePairId ?? '');
        if (stat.type === 'candidate-pair') pairs.push(stat);
      });
      const endpoint = (stat: Record<string, unknown> | undefined) => {
        if (!stat) return 'unknown';
        // Safari/WebKit 的 RTCStats 仍可能只提供旧字段 ip；Chromium/Firefox 用 address。
        const address = String(stat.address ?? stat.ip ?? 'unknown');
        const host = address.includes(':') ? `[${address}]` : address;
        return `${String(stat.protocol ?? '?')}/${String(stat.candidateType ?? '?')} ${host}:${String(stat.port ?? '?')}`;
      };
      const local = new Set<string>();
      const remote = new Set<string>();
      for (const stat of byID.values()) {
        if (stat.type === 'local-candidate') local.add(endpoint(stat));
        if (stat.type === 'remote-candidate') remote.add(endpoint(stat));
      }
      const pairSnaps: IcePairSnapshot[] = pairs.map((pair) => ({
        state: String(pair.state ?? 'unknown'),
        nominated: pair.nominated === true,
        local: endpoint(byID.get(String(pair.localCandidateId ?? ''))),
        remote: endpoint(byID.get(String(pair.remoteCandidateId ?? ''))),
      }));
      const selectedPair =
        (selectedPairID ? byID.get(selectedPairID) : undefined) ??
        pairs.find((pair) => pair.selected === true) ??
        pairs.find((pair) => pair.nominated === true && pair.state === 'succeeded');
      const selected = selectedPair ? endpoint(byID.get(String(selectedPair.remoteCandidateId ?? ''))) : null;
      this.snapshot[target] = { pairs: pairSnaps, local: [...local], remote: [...remote], selected };
    } catch {
      // getStats 失败（如 PC 已关闭）：保留上一次快照，让失败上报仍有内容可看
    }
  }

  async resumeAudio() {
    await this.room.startAudio();
  }

  disconnect() {
    void this.room.disconnect();
  }

  connected() {
    return this.room.state === 'connected';
  }

  async screenEncoderInfo(): Promise<{ impl: string; hw: boolean | null } | null> {
    const track = this.room?.localParticipant.getTrackPublication(Track.Source.ScreenShare)?.track;
    const sender = (track as unknown as { sender?: RTCRtpSender } | undefined)?.sender;
    if (!sender) return null;
    let out: { impl: string; hw: boolean | null } | null = null;
    const stats = await sender.getStats();
    stats.forEach((s) => {
      const r = s as { type?: string; encoderImplementation?: string; powerEfficientEncoder?: boolean };
      if (r.type === 'outbound-rtp' && r.encoderImplementation) {
        out = { impl: r.encoderImplementation, hw: r.powerEfficientEncoder ?? null };
      }
    });
    return out;
  }

  // 实测统计：对相邻两次采样做字节差分得码率（bits/ms = kbps）
  private lastSample = new Map<string, { bytes: number; t: number }>();

  private pickVideoStats(report: RTCStatsReport | undefined, type: 'outbound-rtp' | 'inbound-rtp', key: string): VideoStats | null {
    if (!report) return null;
    let out: VideoStats | null = null;
    report.forEach((s) => {
      const r = s as {
        type?: string; kind?: string; bytesSent?: number; bytesReceived?: number;
        timestamp?: number; frameWidth?: number; frameHeight?: number; framesPerSecond?: number;
      };
      if (r.type !== type || r.kind !== 'video') return;
      const bytes = r.bytesSent ?? r.bytesReceived ?? 0;
      const t = r.timestamp ?? 0;
      const prev = this.lastSample.get(key);
      this.lastSample.set(key, { bytes, t });
      const kbps = prev && t > prev.t ? ((bytes - prev.bytes) * 8) / (t - prev.t) : 0;
      out = { width: r.frameWidth ?? 0, height: r.frameHeight ?? 0, fps: r.framesPerSecond ?? 0, kbps: Math.max(0, Math.round(kbps)) };
    });
    return out;
  }

  async screenStats(): Promise<VideoStats | null> {
    const track = this.room.localParticipant.getTrackPublication(Track.Source.ScreenShare)?.track;
    if (!track) return null;
    return this.pickVideoStats(await track.getRTCStatsReport(), 'outbound-rtp', 'local:screen');
  }

  async remoteVideoStats(identity: string, source: TrackSource): Promise<VideoStats | null> {
    const p = this.room.getParticipantByIdentity(identity);
    const src = source === 'screen' ? Track.Source.ScreenShare : Track.Source.Camera;
    const track = p?.getTrackPublication(src)?.track;
    if (!track) return null;
    return this.pickVideoStats(await track.getRTCStatsReport(), 'inbound-rtp', `${identity}:${source}`);
  }

  localMicTrack(): MediaStreamTrack | null {
    return this.room?.localParticipant.getTrackPublication(Track.Source.Microphone)?.track?.mediaStreamTrack ?? null;
  }

  localIdentity() {
    return this.room.localParticipant.identity;
  }

  participants(): EPart[] {
    return [this.room.localParticipant, ...this.room.remoteParticipants.values()].map((p) => this.toPart(p));
  }

  // ---- 麦克风：RNNoise 管线 / 浏览器内置处理 ----

  private micCaptureOptions(): AudioCaptureOptions {
    const p = loadPrefs();
    const music = p.musicMode;
    return {
      deviceId: p.micDeviceId ? { ideal: p.micDeviceId } : undefined,
      echoCancellation: music ? false : p.echoCancellation,
      noiseSuppression: music ? false : p.denoise === 'browser',
      autoGainControl: music ? false : p.autoGainControl,
    };
  }

  private micPublishOptions(): TrackPublishOptions {
    return { audioPreset: { maxBitrate: loadPrefs().voiceBitrate } };
  }

  private watchEnded(kind: 'mic' | 'camera' | 'screen', track: MediaStreamTrack | undefined) {
    track?.addEventListener('ended', () => this.cbs.onLocalTrackEnded(kind), { once: true });
  }

  async setMic(on: boolean) {
    if (!on) {
      await this.room.localParticipant.setMicrophoneEnabled(false);
      await this.rnnoise.stop();
      return;
    }
    const p = loadPrefs();
    if (p.denoise === 'rnnoise' && !p.musicMode && !this.rnnoiseBroken) {
      const raw = await navigator.mediaDevices.getUserMedia({ audio: this.micCaptureOptions() });
      try {
        const processed = await this.rnnoise.start(raw);
        await this.room.localParticipant.publishTrack(processed, {
          ...this.micPublishOptions(),
          source: Track.Source.Microphone,
        });
        this.watchEnded('mic', raw.getAudioTracks()[0]);
        return;
      } catch (err) {
        // RNNoise 管线不可用（wasm/worklet）：置灰回退浏览器路径
        console.warn('RNNoise 不可用，回退浏览器内置处理:', err);
        this.rnnoiseBroken = true;
        raw.getTracks().forEach((t) => t.stop());
        await this.rnnoise.stop();
      }
    }
    await this.room.localParticipant.setMicrophoneEnabled(true, this.micCaptureOptions(), this.micPublishOptions());
    this.watchEnded('mic', this.room.localParticipant.getTrackPublication(Track.Source.Microphone)?.track?.mediaStreamTrack);
  }

  async restartMic() {
    await this.setMic(false);
    await this.setMic(true);
  }

  async setCamera(on: boolean) {
    const p = loadPrefs();
    await this.room.localParticipant.setCameraEnabled(
      on,
      on && p.camDeviceId ? { deviceId: { ideal: p.camDeviceId } } : undefined,
    );
    if (on) {
      this.watchEnded('camera', this.room.localParticipant.getTrackPublication(Track.Source.Camera)?.track?.mediaStreamTrack);
    }
  }

  async switchCamera(deviceId: string) {
    await this.room.switchActiveDevice('videoinput', deviceId);
  }

  // ---- 投屏：h264 单层 / vp9·av1 SVC 分层 ----

  // 当前投屏轨发布时选的编码：与 prefs 对比决定热改能否就地完成。
  // 不拿 track.codec 比——SDK 对本机不支持的编码会静默回落，用它比会反复触发重发布
  private screenCodec: ScreenCodec | null = null;

  async setScreen(on: boolean) {
    const p = loadPrefs();
    const { capture, publish } = this.screenOptions(p);
    await this.room.localParticipant.setScreenShareEnabled(on, on ? capture : undefined, on ? publish : undefined);
    this.screenCodec = on ? p.screenCodec : null;
    if (on) this.watchEnded('screen', this.screenTrack()?.mediaStreamTrack);
  }

  private screenTrack(): LocalVideoTrack | undefined {
    return this.room.localParticipant.getTrackPublication(Track.Source.ScreenShare)?.videoTrack;
  }

  async applyScreenPrefs(): Promise<boolean> {
    const track = this.screenTrack();
    if (!track) return false;
    const p = loadPrefs();
    const { publish } = this.screenOptions(p);
    if (p.screenCodec !== this.screenCodec) {
      // 编码在 SDP 协商时定死，只能重新发布；stopOnUnpublish=false 留住采集轨，不用重选窗口
      await this.room.localParticipant.unpublishTrack(track, false);
      await this.room.localParticipant.publishTrack(track, publish);
      this.screenCodec = p.screenCodec;
      return true;
    }
    const d = RES_DIMS[p.res];
    await track.mediaStreamTrack.applyConstraints({ width: { ideal: d.width }, height: { ideal: d.height }, frameRate: { ideal: p.fps } });
    // 主编码与 h264 备份编码的发送参数一起改；SDK 的层开关只动 active，不会覆盖这里的码率
    const senders = [track.sender, ...[...track.simulcastCodecs.values()].map((c) => c.sender)];
    for (const sender of senders) {
      if (!sender) continue;
      const params = sender.getParameters();
      for (const e of params.encodings) {
        e.maxBitrate = Math.round(p.bitrate * 1e6);
        e.maxFramerate = p.fps;
      }
      await sender.setParameters(params);
    }
    return false;
  }

  private screenOptions(p: RoomPrefs): { capture: ScreenShareCaptureOptions; publish: TrackPublishOptions } {
    const d = RES_DIMS[p.res];
    const capture: ScreenShareCaptureOptions = {
      resolution: { width: d.width, height: d.height, frameRate: p.fps },
      contentHint: 'detail', // 屏幕内容以文字/细节为主
    };
    const encoding = { maxBitrate: Math.round(p.bitrate * 1e6), maxFramerate: p.fps };
    let publish: TrackPublishOptions;
    if (p.screenCodec === 'h264') {
      // 单层：H.264 无 SVC；simulcast 双编码会把软编 CPU 拖垮，维持单层
      publish = { videoCodec: 'h264', screenShareEncoding: encoding, screenShareSimulcastLayers: [] };
    } else if (p.screenCodec === 'h265') {
      // HEVC 单层（SDK 的 SVC 只认 vp9/av1）：发送端平台硬编、观众端硬解，
      // 同码率观感约为 H.264 的 1.5 倍；不支持 h265 的订阅端触发 h264 备份编码
      publish = {
        videoCodec: 'h265' as VideoCodec,
        screenShareEncoding: encoding,
        screenShareSimulcastLayers: [],
        backupCodec: { codec: 'h264' },
      };
    } else {
      // SVC：单编码器产分层码流，SFU 按观众带宽逐层转发；
      // 不支持该编码的订阅端会触发 h264 备份编码（按需才多一路编码）
      publish = {
        videoCodec: p.screenCodec as VideoCodec,
        scalabilityMode: 'L2T2_KEY',
        screenShareEncoding: encoding,
        backupCodec: { codec: 'h264' },
      };
    }
    return { capture, publish };
  }

  dispose() {
    this.disposed = true;
    this.stopIceProbe();
    document.removeEventListener('pointerdown', this.resume, false);
    void this.rnnoise.stop();
    void this.room.disconnect();
  }
}
