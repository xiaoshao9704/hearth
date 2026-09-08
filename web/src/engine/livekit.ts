// AVEngine 的 LiveKit 实现：livekit-client 的全部使用收敛在此。
// 采集链（RNNoise / 设备选择 / 处理开关）与发布参数（语音码率、投屏编码/SVC）按 prefs 读取。
import {
  ConnectionQuality,
  DisconnectReason,
  Participant,
  RemoteParticipant,
  RemoteTrack,
  Room,
  RoomEvent,
  Track,
} from 'livekit-client';
import type {
  AudioCaptureOptions,
  LocalVideoTrack,
  ScreenShareCaptureOptions,
  TrackPublishOptions,
  VideoCaptureOptions,
  VideoCodec,
} from 'livekit-client';
import { RnnoisePipeline } from '../audio';
import { RES_DIMS, loadPrefs } from '../prefs';
import type { RoomPrefs, ScreenCodec } from '../prefs';
import { DATA_TOPIC_FILE, DATA_TOPIC_TEXT } from './types';
import type { AVEngine, EPart, EngineCallbacks, LineStats, TrackSource, VideoStats } from './types';

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
  line: LineStats; // 本 transport 的连接读数（RTT/抖动/丢包/传输方式）
}

// 连上之后快照的刷新间隔：读数面板与 60 秒诊断都取这份快照，不另开 getStats 轮询
const LINE_PROBE_MS = 5000;

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
  // 本地属性镜像：setAttribute 先写这里再尽力广播，本机名册不等服务端回执
  private localAttrs: Record<string, string> = {};
  private snapshot: Record<'pub' | 'sub', IceTransportSnapshot | null> = { pub: null, sub: null };
  // 线路丢包要看区间差分（累计值会把开局那几个包一直摊到最后）：按 transport 记上一次的累计数
  private lastLine: Record<'pub' | 'sub', { lost: number; total: number } | null> = { pub: null, sub: null };

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
    // 自己的 afk 先看本地镜像：属性广播要服务端令牌授予 canUpdateOwnMetadata，
    // 没授予时远端收不到，但本机的名册仍应如实显示自己已被判为离开
    const isLocal = p.identity === this.room.localParticipant.identity;
    return {
      identity: p.identity,
      uid: meta?.uid ?? 0,
      username: meta?.username ?? p.name ?? '',
      display: p.name || p.identity,
      isLocal,
      micOn: !!micPub && !micPub.isMuted,
      canPublish: p.permissions?.canPublish !== false, // 服务端禁言会收走发布权限
      sharing: !!p.getTrackPublication(Track.Source.ScreenShare),
      ingest,
      tag: meta?.tag ?? '', // 浏览器参与者也有设备标签，展示设备名要用它
      afk: isLocal ? this.localAttrs.afk === '1' : p.attributes?.afk === '1',
      quality: p.connectionQuality === ConnectionQuality.Unknown ? undefined : p.connectionQuality,
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
    // Data Streams 的 handler 必须在 connect 之前注册，否则 join 后先到的流没人接：
    // 和下面的事件订阅一样只在构造时做一次
    this.room.registerTextStreamHandler(DATA_TOPIC_TEXT, (reader, from) => {
      void reader
        .readAll()
        .then((text) => this.cbs.onText?.(DATA_TOPIC_TEXT, text, from.identity))
        .catch(() => {}); // 单条流读失败不该掀翻房间：发送方会重发或走 REST 补齐
    });
    this.room.registerByteStreamHandler(DATA_TOPIC_FILE, (reader, from) => {
      const info = reader.info;
      void reader
        .readAll()
        .then((chunks) => {
          const total = chunks.reduce((n, c) => n + c.byteLength, 0);
          const bytes = new Uint8Array(total);
          let off = 0;
          for (const c of chunks) {
            bytes.set(c, off);
            off += c.byteLength;
          }
          this.cbs.onFile?.(
            DATA_TOPIC_FILE,
            { name: info.name, mime: info.mimeType, size: info.size ?? total, attrs: info.attributes ?? {} },
            bytes,
            from.identity,
          );
        })
        .catch(() => {}); // 传输中断：接收方卡片停在「传输中」，不伪造一个坏文件
    });
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
      // 参与者属性（afk 等纯展示态）变化：同样只是重绘名册
      .on(RoomEvent.ParticipantAttributesChanged, () => this.cbs.onRoster())
      // 连接质量（内核按 RTCP 回报算，全员的更新都会到）：并进 EPart，走同一条名册通路
      .on(RoomEvent.ConnectionQualityChanged, () => this.cbs.onRoster())
      // 自动播放被拦截：SDK 自己不会出提示，交给房间层弹「点击开启声音」
      .on(RoomEvent.AudioPlaybackStatusChanged, () => {
        if (!this.room.canPlaybackAudio) this.cbs.onAudioBlocked?.();
      })
      .on(RoomEvent.ConnectionStateChanged, (state) => this.cbs.onDiagnostic?.('connection_state', String(state)))
      .on(RoomEvent.SignalConnected, () => this.cbs.onDiagnostic?.('signal_connected', 'connected'))
      .on(RoomEvent.Reconnecting, () => this.cbs.onReconnecting())
      .on(RoomEvent.Reconnected, () => this.cbs.onReconnected())
      .on(RoomEvent.Disconnected, (reason) => {
        this.stopIceProbe();
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
    this.snapshot = { pub: null, sub: null };
    this.lastLine = { pub: null, sub: null };
    this.startIceProbe();
    try {
      await this.room.connect(url, token);
      await this.captureIceStats();
      this.emitSelectedServer();
      this.startIceProbe(LINE_PROBE_MS);
    } catch (err) {
      // connect 失败时 SDK 会立即清理 PeerConnection；轮询负责在清理前留下候选，
      // 这里再尽力抓一次最终状态（PC 可能已经被清理，抓不到就用轮询期间攒下的快照）。
      await this.captureIceStats();
      this.emitIceFailed();
      this.stopIceProbe();
      throw err;
    }
  }

  // RTCEngine/PCTransport 要等信令 JoinResponse 后才创建，连接期间的快速轮询才能在
  // SDK 超时关闭 PeerConnection 之前留下失败候选；连上后转 LINE_PROBE_MS 慢速，
  // 同一个定时器同一条采集路径继续刷新快照（读数与诊断都读它）。轮询只刷新快照，不上报。
  private startIceProbe(intervalMs = 400) {
    this.stopIceProbe();
    this.iceProbeTimer = window.setInterval(() => void this.captureIceStats(), intervalMs);
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
      const endpoint = (stat: Record<string, unknown> | undefined) => (stat ? this.endpointOf(stat) : 'unknown');
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
      this.snapshot[target] = {
        pairs: pairSnaps,
        local: [...local],
        remote: [...remote],
        selected,
        line: this.deriveLine(target, byID, selectedPair),
      };
    } catch {
      // getStats 失败（如 PC 已关闭）：保留上一次快照，让失败上报仍有内容可看
    }
  }

  // 从一份 getStats 派生本 transport 的线路读数：
  // RTT 取选中候选对（没有再退回服务端回报的 remote-inbound-rtp）；
  // 抖动/丢包收侧看 inbound-rtp（自己实际收到的），发侧看服务端回报的 remote-inbound-rtp；
  // 传输方式按候选对的 protocol，远端候选是中继时直接记 relay（走了 TURN 才是真正的兜底）
  private deriveLine(
    target: 'pub' | 'sub',
    byID: Map<string, Record<string, unknown>>,
    selectedPair: Record<string, unknown> | undefined,
  ): LineStats {
    const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);
    const out: LineStats = { at: Date.now() };

    if (selectedPair) {
      const rtt = num(selectedPair.currentRoundTripTime);
      if (rtt !== undefined) out.rtt_ms = Math.round(rtt * 1000);
      const localCand = byID.get(String(selectedPair.localCandidateId ?? ''));
      const remoteCand = byID.get(String(selectedPair.remoteCandidateId ?? ''));
      const proto = String(localCand?.protocol ?? remoteCand?.protocol ?? '');
      const remoteType = String(remoteCand?.candidateType ?? '');
      out.transport = remoteType === 'relay' ? 'relay' : proto === 'udp' || proto === 'tcp' ? proto : 'unknown';
      if (localCand) out.local = this.endpointOf(localCand);
      if (remoteCand) out.remote = this.endpointOf(remoteCand);
    }

    let jitter: number | undefined;
    let lostSum = 0;
    let recvSum = 0;
    let haveInbound = false;
    let remoteRTT: number | undefined;
    let remoteJitter: number | undefined;
    let fractionLost: number | undefined;
    for (const stat of byID.values()) {
      if (stat.type === 'inbound-rtp') {
        haveInbound = true;
        const j = num(stat.jitter);
        if (j !== undefined) jitter = Math.max(jitter ?? 0, j);
        lostSum += num(stat.packetsLost) ?? 0;
        recvSum += num(stat.packetsReceived) ?? 0;
      }
      if (stat.type === 'remote-inbound-rtp') {
        const rtt = num(stat.roundTripTime);
        if (rtt !== undefined) remoteRTT = Math.max(remoteRTT ?? 0, rtt);
        const j = num(stat.jitter);
        if (j !== undefined) remoteJitter = Math.max(remoteJitter ?? 0, j);
        const f = num(stat.fractionLost);
        if (f !== undefined) fractionLost = Math.max(fractionLost ?? 0, f);
      }
    }
    if (out.rtt_ms === undefined && remoteRTT !== undefined) out.rtt_ms = Math.round(remoteRTT * 1000);

    if (haveInbound) {
      const prev = this.lastLine[target];
      this.lastLine[target] = { lost: lostSum, total: lostSum + recvSum };
      if (jitter !== undefined) out.jitter_ms = Math.round(jitter * 10000) / 10;
      if (prev) {
        const dl = Math.max(0, lostSum - prev.lost);
        const dt = Math.max(0, lostSum + recvSum - prev.total);
        if (dt > 0) out.loss_pct = Math.round((dl / dt) * 1000) / 10;
      }
    } else {
      if (remoteJitter !== undefined) out.jitter_ms = Math.round(remoteJitter * 10000) / 10;
      if (fractionLost !== undefined) out.loss_pct = Math.round(fractionLost * 1000) / 10;
    }
    return out;
  }

  // Safari/WebKit 的 RTCStats 仍可能只提供旧字段 ip；Chromium/Firefox 用 address
  private endpointOf(stat: Record<string, unknown>): string {
    const address = String(stat.address ?? stat.ip ?? 'unknown');
    const host = address.includes(':') ? `[${address}]` : address;
    return `${String(stat.protocol ?? '?')}/${String(stat.candidateType ?? '?')} ${host}:${String(stat.port ?? '?')}`;
  }

  // 两个 transport 的读数合成一条线：RTT/传输方式取有选中候选对的那个（订阅侧优先，
  // 合并形态下它一定在），抖动与丢包优先用收侧（用户真正感受到的那一路）
  lineStats(): LineStats | null {
    if (!this.connected()) return null;
    const sub = this.snapshot.sub?.line;
    const pub = this.snapshot.pub?.line;
    const base = sub?.rtt_ms !== undefined ? sub : pub?.rtt_ms !== undefined ? pub : (sub ?? pub);
    if (!base) return null;
    return {
      ...base,
      jitter_ms: sub?.jitter_ms ?? pub?.jitter_ms,
      loss_pct: sub?.loss_pct ?? pub?.loss_pct,
    };
  }

  async resumeAudio() {
    await this.room.startAudio();
  }

  async sendText(topic: string, text: string) {
    await this.room.localParticipant.sendText(text, { topic });
  }

  // 用 sendBytes 而不是 SDK 的 sendFile：后者的 SendFileOptions 只取 topic/mimeType/
  // destinationIdentities，attributes 会被丢掉，而卡片关联全靠 attrs.message_id。
  // 代价是整包读进内存——文件本来就有大小上限，可接受。
  async sendFile(file: File, topic: string, attrs: Record<string, string>, onProgress?: (p: number) => void) {
    const bytes = new Uint8Array(await file.arrayBuffer());
    await this.room.localParticipant.sendBytes(bytes, {
      topic,
      name: file.name,
      mimeType: file.type || 'application/octet-stream',
      attributes: attrs,
      onProgress,
    });
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

  // 实测统计：对相邻两次采样做字节差分得码率（bits/ms = kbps）。
  // jb/decode/encode 也按区间差分（累计值除以累计帧数会被开局那几帧长期拖住）
  private lastSample = new Map<
    string,
    { bytes: number; t: number; packets: number; lost: number; jb: number; jbCount: number; work: number; frames: number }
  >();

  private pickVideoStats(report: RTCStatsReport | undefined, type: 'outbound-rtp' | 'inbound-rtp', key: string): VideoStats | null {
    if (!report) return null;
    let out: VideoStats | null = null;
    report.forEach((s) => {
      const r = s as {
        type?: string; kind?: string; bytesSent?: number; bytesReceived?: number;
        timestamp?: number; frameWidth?: number; frameHeight?: number; framesPerSecond?: number;
        packetsReceived?: number; packetsLost?: number;
        jitterBufferDelay?: number; jitterBufferEmittedCount?: number;
        totalDecodeTime?: number; framesDecoded?: number; framesDropped?: number;
        totalEncodeTime?: number; framesEncoded?: number; qualityLimitationReason?: string;
      };
      if (r.type !== type || r.kind !== 'video') return;
      const bytes = r.bytesSent ?? r.bytesReceived ?? 0;
      const t = r.timestamp ?? 0;
      const packets = r.packetsReceived ?? 0;
      const lost = r.packetsLost ?? 0;
      // 收侧看解码，发侧看编码：两侧各一对「累计耗时 / 累计帧数」，同一组差分算区间均值
      const work = (type === 'inbound-rtp' ? r.totalDecodeTime : r.totalEncodeTime) ?? 0;
      const frames = (type === 'inbound-rtp' ? r.framesDecoded : r.framesEncoded) ?? 0;
      const jb = r.jitterBufferDelay ?? 0;
      const jbCount = r.jitterBufferEmittedCount ?? 0;
      const prev = this.lastSample.get(key);
      this.lastSample.set(key, { bytes, t, packets, lost, jb, jbCount, work, frames });
      const kbps = prev && t > prev.t ? ((bytes - prev.bytes) * 8) / (t - prev.t) : 0;
      // 丢包只对接收侧有意义，且要看区间差分——累计值会把开局那几个包一直摊到最后
      let loss: number | undefined;
      if (type === 'inbound-rtp' && prev) {
        const dl = Math.max(0, lost - prev.lost);
        const dp = Math.max(0, packets - prev.packets);
        if (dl + dp > 0) loss = Math.round((dl / (dl + dp)) * 1000) / 10;
      }
      const avgMs = (dv: number, dn: number): number | undefined =>
        dn > 0 ? Math.round((dv / dn) * 10000) / 10 : undefined;
      const frameMs = prev ? avgMs(work - prev.work, frames - prev.frames) : undefined;
      const limitation = r.qualityLimitationReason;
      out = {
        width: r.frameWidth ?? 0,
        height: r.frameHeight ?? 0,
        fps: r.framesPerSecond ?? 0,
        kbps: Math.max(0, Math.round(kbps)),
        loss,
        jitter_buffer_ms: type === 'inbound-rtp' && prev ? avgMs(jb - prev.jb, jbCount - prev.jbCount) : undefined,
        decode_ms: type === 'inbound-rtp' ? frameMs : undefined,
        frames_dropped: type === 'inbound-rtp' ? r.framesDropped : undefined,
        encode_ms: type === 'outbound-rtp' ? frameMs : undefined,
        limitation:
          type === 'outbound-rtp' && limitation
            ? limitation === 'none' || limitation === 'cpu' || limitation === 'bandwidth'
              ? limitation
              : 'other'
            : undefined,
      };
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
      channelCount: { ideal: 1 }, // 语音单声道：立体声麦克风若只有一路有声，发布出去就成了单耳
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

  // 手机翻转前后摄像头。朝向以采集轨的 settings 为准，拿不到（部分安卓浏览器不报）
  // 才退回自己记的上一次值
  private facing: 'user' | 'environment' = 'user';

  async flipCamera() {
    const track = this.room.localParticipant.getTrackPublication(Track.Source.Camera)?.videoTrack;
    if (!track) throw new Error('摄像头未开启');
    const settings = track.mediaStreamTrack.getSettings();
    const cur = settings.facingMode === 'environment' || settings.facingMode === 'user' ? settings.facingMode : this.facing;
    const next: 'user' | 'environment' = cur === 'environment' ? 'user' : 'environment';
    try {
      // 必须 exact：ideal 在只报一个 facingMode 的机器上会静默返回原摄像头，按钮就成了摆设。
      // SDK 的 VideoCaptureOptions.facingMode 只到字面量，约束对象要绕过类型
      await track.restartTrack({ facingMode: { exact: next } } as unknown as VideoCaptureOptions);
    } catch {
      // 没有对应朝向的摄像头（多数桌面、部分外接摄像头）：退回按设备列表切下一个
      const cams = await Room.getLocalDevices('videoinput');
      const curId = settings.deviceId ?? '';
      const idx = cams.findIndex((d) => d.deviceId === curId);
      const nextDev = cams[(idx + 1) % Math.max(cams.length, 1)];
      if (cams.length < 2 || !nextDev || nextDev.deviceId === curId) throw new Error('没有可切换的第二个摄像头');
      await this.switchCamera(nextDev.deviceId);
    }
    this.facing = next;
    // 采集轨换了新对象，原来挂的 ended 监听跟着走了，重挂一次
    this.watchEnded('camera', this.room.localParticipant.getTrackPublication(Track.Source.Camera)?.track?.mediaStreamTrack);
  }

  async setAttribute(key: string, value: string) {
    if (this.localAttrs[key] === value) return;
    this.localAttrs[key] = value;
    this.cbs.onRoster(); // 本机先反映，广播成不成功都不改变自己看到的状态
    await this.room.localParticipant.setAttributes({ ...this.localAttrs });
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
    // restrictOwnAudio 还没普及：浏览器不认的约束一律不传，免得整个 getDisplayMedia 直接 TypeError
    const supported = navigator.mediaDevices?.getSupportedConstraints?.() as
      | (MediaTrackSupportedConstraints & { restrictOwnAudio?: boolean })
      | undefined;
    const capture: ScreenShareCaptureOptions = {
      resolution: { width: d.width, height: d.height, frameRate: p.fps },
      contentHint: 'detail', // 屏幕内容以文字/细节为主
      systemAudio: 'include', // 让浏览器把系统声音摆进可选源；不支持的浏览器忽略
      selfBrowserSurface: 'exclude', // 别把 hearth 自己这个标签页列为候选（选中就成了镜中镜）
      preferCurrentTab: false,
      // 系统声音是音乐/游戏音效，不是人声：回声消除/降噪/自动增益会把它嚼烂，
      // 声道数也和麦克风相反——麦克风降到单声道避免单耳，这里要保住左右声场。
      // restrictOwnAudio 把本页面自己播放的声音（也就是别人的语音）剔出采集，避免回音
      audio: p.screenAudio
        ? {
            echoCancellation: false,
            noiseSuppression: false,
            autoGainControl: false,
            channelCount: 2,
            ...(supported?.restrictOwnAudio ? { restrictOwnAudio: true } : {}),
          }
        : false,
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
    // 这次发布连带的投屏音轨（有就发）：128k 立体声，关 DTX 免得静音段断流吞掉音乐尾音，
    // 关 RED 免得为冗余多占上行。只作用于本次投屏发布，麦克风走 micPublishOptions
    return { capture, publish: { ...publish, audioPreset: { maxBitrate: 128_000 }, dtx: false, red: false } };
  }

  dispose() {
    this.disposed = true;
    this.stopIceProbe();
    document.removeEventListener('pointerdown', this.resume, false);
    void this.rnnoise.stop();
    void this.room.disconnect();
  }
}
