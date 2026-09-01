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
import type { AudioCaptureOptions, ScreenShareCaptureOptions, TrackPublishOptions, VideoCodec } from 'livekit-client';
import { RnnoisePipeline } from '../audio';
import { RES_DIMS, loadPrefs } from '../prefs';
import type { AVEngine, EPart, EngineCallbacks, TrackSource } from './types';

const toSource = (s: Track.Source): TrackSource | null =>
  s === Track.Source.Camera ? 'camera' : s === Track.Source.ScreenShare ? 'screen' : null;

export class LiveKitEngine implements AVEngine {
  private room = new Room();
  private cbs: EngineCallbacks;
  private rnnoise = new RnnoisePipeline();
  private rnnoiseBroken = false;
  private disposed = false;
  private resume = () => void this.rnnoise.resume();

  constructor(cbs: EngineCallbacks) {
    this.cbs = cbs;
    // AudioContext 自动播放策略：用户首次点击时恢复
    document.addEventListener('pointerdown', this.resume, false);
    this.wire();
  }

  private toPart(p: Participant): EPart {
    const micPub = p.getTrackPublication(Track.Source.Microphone);
    return {
      identity: p.identity,
      username: p.name || p.identity.split('-')[0],
      display: p.name || p.identity,
      isLocal: p.identity === this.room.localParticipant.identity,
      micOn: !!micPub && !micPub.isMuted,
      sharing: !!p.getTrackPublication(Track.Source.ScreenShare),
      obs: p.identity.endsWith('-obs'),
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
      .on(RoomEvent.ParticipantConnected, () => this.cbs.onRoster())
      .on(RoomEvent.ParticipantDisconnected, () => this.cbs.onRoster())
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

  async connect(url: string, token: string) {
    await this.room.connect(url, token);
  }

  disconnect() {
    void this.room.disconnect();
  }

  connected() {
    return this.room.state === 'connected';
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

  private watchEnded(kind: 'mic' | 'camera', track: MediaStreamTrack | undefined) {
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

  async setScreen(on: boolean) {
    const p = loadPrefs();
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
    await this.room.localParticipant.setScreenShareEnabled(on, on ? capture : undefined, on ? publish : undefined);
  }

  dispose() {
    this.disposed = true;
    document.removeEventListener('pointerdown', this.resume, false);
    void this.rnnoise.stop();
    void this.room.disconnect();
  }
}
