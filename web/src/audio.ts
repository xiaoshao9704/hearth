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
    const dest = ctx.createMediaStreamDestination();
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

// listAudioInputs 枚举麦克风设备（未授权时 label 为空，授权后再调用一次即可拿到名称）
export async function listAudioInputs(): Promise<MediaDeviceInfo[]> {
  const devices = await navigator.mediaDevices.enumerateDevices();
  return devices.filter((d) => d.kind === 'audioinput');
}
