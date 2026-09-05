// 麦克风采集/处理工具：RNNoise(WASM) 降噪管线、输入设备枚举。
// RNNoise 用 @sapphi-red/web-noise-suppressor（AudioWorklet + WASM，无需 COOP/COEP）。
import { RnnoiseWorkletNode, loadRnnoise } from '@sapphi-red/web-noise-suppressor';
// worklet 脚本与 wasm 以静态资源方式由 vite 打包（?url）
import rnnoiseWorkletUrl from '@sapphi-red/web-noise-suppressor/rnnoiseWorklet.js?url';
import rnnoiseWasmUrl from '@sapphi-red/web-noise-suppressor/rnnoise.wasm?url';
import rnnoiseWasmSimdUrl from '@sapphi-red/web-noise-suppressor/rnnoise_simd.wasm?url';

// RnnoisePipeline：getUserMedia 原始流 -> AudioWorklet(RNNoise) -> MediaStreamDestination，
// 发布处理后的音轨而非原始麦克风 track。
export class RnnoisePipeline {
  private ctx: AudioContext | null = null;
  private node: RnnoiseWorkletNode | null = null;
  private raw: MediaStream | null = null;

  get active(): boolean {
    return this.ctx !== null;
  }

  // start 返回处理后的音轨；加载失败抛错（调用方降级到普通采集路径）
  async start(raw: MediaStream): Promise<MediaStreamTrack> {
    const ctx = new AudioContext({ sampleRate: 48000 }); // RNNoise 固定 48kHz
    await ctx.audioWorklet.addModule(rnnoiseWorkletUrl);
    const wasmBinary = await loadRnnoise({ url: rnnoiseWasmUrl, simdUrl: rnnoiseWasmSimdUrl });
    const node = new RnnoiseWorkletNode(ctx, { maxChannels: 1, wasmBinary });
    // 目标节点强制单声道：RNNoise 只处理一路，默认的立体声 destination 会让处理后的
    // 音轨只填左声道、右声道空，发布出去别人就只有左耳有声。单声道音轨在播放端两边都出。
    const dest = ctx.createMediaStreamDestination();
    dest.channelCount = 1;
    dest.channelCountMode = 'explicit';
    dest.channelInterpretation = 'speakers';
    ctx.createMediaStreamSource(raw).connect(node);
    node.connect(dest);
    const track = dest.stream.getAudioTracks()[0];
    if (!track) {
      await ctx.close();
      throw new Error('RNNoise 输出音轨为空');
    }
    this.ctx = ctx;
    this.node = node;
    this.raw = raw;
    void this.resume(); // 无手势时可能仍 suspended，等用户首次点击再恢复
    return track;
  }

  // resume 处理浏览器自动播放策略（有用户手势后调用才真正生效）
  async resume(): Promise<void> {
    if (this.ctx && this.ctx.state === 'suspended') {
      await this.ctx.resume().catch(() => {});
    }
  }

  // stop 释放整条管线：停原始麦克风、销毁 worklet、关 AudioContext
  async stop(): Promise<void> {
    this.raw?.getTracks().forEach((t) => t.stop());
    this.node?.destroy();
    this.node?.disconnect();
    await this.ctx?.close().catch(() => {});
    this.ctx = null;
    this.node = null;
    this.raw = null;
  }
}

// ---- 进出房间提示音（WebAudio 合成，不引资源文件）----

// 两声短音共用一个上下文：房间页每次进出都要响，反复建上下文会被浏览器数量上限卡住
let cueCtx: AudioContext | null = null;

// 单音：5ms attack + 60ms release，直接切方波会爆音
function cueNote(ctx: AudioContext, freq: number, at: number) {
  const osc = ctx.createOscillator();
  const gain = ctx.createGain();
  osc.type = 'sine';
  osc.frequency.value = freq;
  gain.gain.setValueAtTime(0, at);
  gain.gain.linearRampToValueAtTime(0.15, at + 0.005);
  gain.gain.setValueAtTime(0.15, at + 0.03);
  gain.gain.linearRampToValueAtTime(0, at + 0.09);
  osc.connect(gain).connect(ctx.destination);
  osc.start(at);
  osc.stop(at + 0.1);
}

// 消息提示音：单音，比进出音更短更柔——5ms attack，总长 70ms，峰值增益 0.1
function messageNote(ctx: AudioContext, at: number) {
  const osc = ctx.createOscillator();
  const gain = ctx.createGain();
  osc.type = 'sine';
  osc.frequency.value = 880;
  gain.gain.setValueAtTime(0, at);
  gain.gain.linearRampToValueAtTime(0.1, at + 0.005);
  gain.gain.setValueAtTime(0.1, at + 0.02);
  gain.gain.linearRampToValueAtTime(0, at + 0.07);
  osc.connect(gain).connect(ctx.destination);
  osc.start(at);
  osc.stop(at + 0.08);
}

// playCue 播放进入（两个上行音）/ 离开（两个下行音）/ 消息（一声轻音）提示；
// 自动播放策略下上下文可能是 suspended，恢复失败就静默跳过（提示音不值得打扰用户）
export function playCue(kind: 'join' | 'leave' | 'message') {
  try {
    if (!cueCtx) cueCtx = new AudioContext();
    const ctx = cueCtx;
    const play = () => {
      if (kind === 'message') {
        messageNote(ctx, ctx.currentTime);
        return;
      }
      const [a, b] = kind === 'join' ? [660, 880] : [660, 440];
      cueNote(ctx, a, ctx.currentTime);
      cueNote(ctx, b, ctx.currentTime + 0.09);
    };
    if (ctx.state === 'suspended') {
      void ctx.resume().then(play).catch(() => {});
      return;
    }
    play();
  } catch {
    // 不支持 WebAudio 或上下文创建失败：不出声即可
  }
}

// listAudioInputs 枚举麦克风设备（未授权时 label 为空，授权后再调用一次即可拿到名称）
export async function listAudioInputs(): Promise<MediaDeviceInfo[]> {
  const devices = await navigator.mediaDevices.enumerateDevices();
  return devices.filter((d) => d.kind === 'audioinput');
}
