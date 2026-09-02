// AVEngine 的 ember 实现：hearth 进程内纯音频 SFU 的客户端。
// 信令走同源 WebSocket（自有 JSON 协议），媒体是与服务器（公网 ICE-Lite）的单条 PC——
// 客户端不需要 STUN/TURN。视频类操作在本引擎不支持（由舞台线承担）。
import { deviceId } from '../api';
import { loadPrefs } from '../prefs';
import { captureMic } from './capture';
import type { MicCapture } from './capture';
import type { AVEngine, EPart, EngineCallbacks } from './types';

interface SigMsg {
  type: string;
  sdp?: string;
  mids?: Record<string, string>;
  identity?: string;
  peers?: { identity: string; name: string; micOn: boolean; muted?: boolean }[];
  on?: boolean;
  speakers?: string[];
  reason?: string;
}

const usernameOf = (identity: string, name: string) => name || identity.split('-')[0];

export class EmberEngine implements AVEngine {
  private cbs: EngineCallbacks;
  private ws: WebSocket | null = null;
  private pc: RTCPeerConnection | null = null;
  private micTx: RTCRtpTransceiver | null = null;
  private capture: MicCapture | null = null;
  private selfId = '';
  private micOn = false;
  private gagged = false; // 服务端禁言：welcome.on 带入，gag 消息实时更新
  private roster = new Map<string, { name: string; micOn: boolean; muted: boolean }>();
  private midMap: Record<string, string> = {};
  private trackEls = new Map<string, HTMLMediaElement[]>();
  private closed = false;
  private ended = false;

  constructor(cbs: EngineCallbacks) {
    this.cbs = cbs;
  }

  connected() {
    return this.ws?.readyState === WebSocket.OPEN && this.pc?.connectionState === 'connected';
  }

  async screenEncoderInfo(): Promise<{ impl: string; hw: boolean | null } | null> {
    return null; // 语音线无视频
  }

  async screenStats() {
    return null; // 语音线无视频
  }

  async remoteVideoStats() {
    return null; // 语音线无视频
  }

  localMicTrack(): MediaStreamTrack | null {
    return this.capture?.track ?? null;
  }

  localIdentity() {
    return this.selfId;
  }

  participants(): EPart[] {
    const out: EPart[] = [
      {
        identity: this.selfId,
        username: usernameOf(this.selfId, ''),
        display: usernameOf(this.selfId, ''),
        isLocal: true,
        micOn: this.micOn,
        canPublish: !this.gagged,
        sharing: false,
        obs: false,
      },
    ];
    this.roster.forEach((v, identity) => {
      out.push({
        identity,
        username: usernameOf(identity, v.name),
        display: v.name || identity,
        isLocal: false,
        micOn: v.micOn,
        canPublish: !v.muted,
        sharing: false,
        obs: false,
      });
    });
    return out;
  }

  private send(m: SigMsg) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m));
  }

  private lost() {
    if (this.closed || this.ended) return;
    this.ended = true;
    this.cbs.onEnded('lost');
  }

  // 解绑并关掉当前 ws/pc：重连前必须调，否则旧连接被服务端判 duplicate 时
  // 残留的 onmessage 会把 bye 当成新连接的结局
  private teardown() {
    this.pcReady = false;
    if (this.ws) {
      this.ws.onmessage = null;
      this.ws.onclose = null;
      this.ws.onerror = null;
      this.ws.close();
      this.ws = null;
    }
    this.pc?.close();
    this.pc = null;
    this.micTx = null;
  }

  async connect(url: string, token: string): Promise<void> {
    this.teardown();
    this.ended = false;
    this.midMap = {};
    this.roster.clear();
    const full = `${url}&token=${encodeURIComponent(token)}&device_id=${encodeURIComponent(deviceId())}`;
    const ws = new WebSocket(full);
    this.ws = ws;

    await new Promise<void>((resolve, reject) => {
      const fail = (why: string) => {
        if (this.ws === ws) this.teardown(); // 超时/失败的连接不留僵尸
        reject(new Error(why));
      };
      const timer = setTimeout(() => fail('语音信令连接超时'), 12000);

      ws.onerror = () => {};
      ws.onclose = () => {
        clearTimeout(timer);
        if (!this.ended && this.pcReady) this.lost();
        else fail('语音信令连接失败');
      };
      ws.onmessage = async (ev) => {
        const m = JSON.parse(ev.data as string) as SigMsg;
        try {
          switch (m.type) {
            case 'welcome': {
              this.selfId = m.identity ?? '';
              this.gagged = m.on === true; // 持久禁言：入会即被告知
              (m.peers ?? []).forEach((p) => this.roster.set(p.identity, { name: p.name, micOn: p.micOn, muted: p.muted === true }));
              await this.setupPC();
              break;
            }
            case 'answer': // 初始 offer 的应答
              await this.pc?.setRemoteDescription({ type: 'answer', sdp: m.sdp! });
              this.pcReady = true;
              clearTimeout(timer);
              this.cbs.onRoster();
              resolve();
              break;
            case 'offer': {
              // 服务端重协商（有人进出）
              Object.assign(this.midMap, m.mids ?? {});
              if (!this.pc) break;
              await this.pc.setRemoteDescription({ type: 'offer', sdp: m.sdp! });
              const answer = await this.pc.createAnswer();
              await this.pc.setLocalDescription(answer);
              this.send({ type: 'answer', sdp: this.pc.localDescription!.sdp });
              break;
            }
            case 'roster':
              this.roster.clear();
              (m.peers ?? []).forEach((p) => this.roster.set(p.identity, { name: p.name, micOn: p.micOn, muted: p.muted === true }));
              this.pruneGone();
              this.cbs.onRoster();
              break;
            case 'gag': // 服务端禁言/解禁自己
              this.gagged = m.on === true;
              if (this.gagged && this.micOn) await this.setMic(false);
              this.cbs.onRoster();
              break;
            case 'speakers':
              this.cbs.onSpeakers(m.speakers ?? []);
              break;
            case 'bye':
              this.ended = true;
              clearTimeout(timer);
              if (m.reason === 'kicked') this.cbs.onEnded('kicked');
              else if (m.reason === 'duplicate') this.cbs.onEnded('duplicate');
              else this.cbs.onEnded('lost');
              break;
          }
        } catch (err) {
          console.warn('ember 信令处理失败:', err);
        }
      };
    });
  }

  private pcReady = false;

  private async setupPC() {
    const pc = new RTCPeerConnection({}); // 服务器公网 ICE-Lite：无需 STUN
    this.pc = pc;
    this.micTx = pc.addTransceiver('audio', { direction: 'sendonly' });
    pc.ontrack = (ev) => {
      const mid = ev.transceiver.mid ?? '';
      const identity = this.midMap[mid] ?? mid;
      const el = new Audio();
      el.srcObject = new MediaStream([ev.track]);
      el.autoplay = true;
      const list = this.trackEls.get(identity) ?? [];
      list.push(el);
      this.trackEls.set(identity, list);
      this.cbs.onAudioTrack(identity, el);
    };
    pc.onconnectionstatechange = () => {
      console.info('ember PC:', pc.connectionState);
      if (pc.connectionState === 'connected') this.cbs.onReconnected(); // 首次连上也走这里刷新 UI
      if (pc.connectionState === 'failed') this.lost();
    };
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    // 服务端 ICE-Lite（被动方），客户端 offer 不依赖本端 candidates；
    // gathering 在部分环境会长期不完成，最多等 1s 就发
    await new Promise<void>((r) => {
      const t = setTimeout(r, 1000);
      const h = () => {
        if (pc.iceGatheringState === 'complete') {
          clearTimeout(t);
          pc.removeEventListener('icegatheringstatechange', h);
          r();
        }
      };
      pc.addEventListener('icegatheringstatechange', h);
      h();
    });
    this.send({ type: 'offer', sdp: pc.localDescription!.sdp });
  }

  // roster 缩水时清掉离场者的音频元素
  private pruneGone() {
    for (const [identity, els] of this.trackEls) {
      if (!this.roster.has(identity) && identity !== this.selfId) {
        this.cbs.onAudioTrackRemoved(identity, els);
        this.trackEls.delete(identity);
      }
    }
  }

  async setMic(on: boolean) {
    this.micOn = on;
    if (!on) {
      await this.micTx?.sender.replaceTrack(null);
      await this.capture?.stop();
      this.capture = null;
      this.send({ type: 'mute', on: true });
      return;
    }
    const cap = await captureMic();
    this.capture = cap;
    cap.raw.addEventListener('ended', () => this.cbs.onLocalTrackEnded('mic'), { once: true });
    await this.micTx?.sender.replaceTrack(cap.track);
    try {
      const params = this.micTx!.sender.getParameters();
      params.encodings = [{ maxBitrate: loadPrefs().voiceBitrate }];
      await this.micTx!.sender.setParameters(params);
    } catch {
      // 码率参数不支持时忽略
    }
    this.send({ type: 'mute', on: false });
  }

  async restartMic() {
    if (!this.micOn) return;
    await this.capture?.stop();
    this.capture = null;
    await this.setMic(true);
  }

  async setCamera(): Promise<void> {
    throw new Error('语音线不支持摄像头');
  }

  async setScreen(): Promise<void> {
    throw new Error('语音线不支持投屏');
  }

  async switchCamera() {
    // 语音线无摄像头
  }

  disconnect() {
    this.teardown();
  }

  dispose() {
    this.closed = true;
    void this.capture?.stop();
    this.capture = null;
    this.disconnect();
  }
}
