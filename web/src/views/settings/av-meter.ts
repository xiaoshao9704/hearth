// 麦克风电平表（设置页试音可视化）
export function createMicMeter() {
  let micStream: MediaStream | null = null;
  let analyser: AnalyserNode | null = null;
  let audioCtx: AudioContext | null = null;
  let rafId = 0;

  async function start(deviceId: string, bars: NodeListOf<HTMLDivElement>): Promise<boolean> {
    stop();
    try {
      micStream = await navigator.mediaDevices.getUserMedia({
        audio: deviceId ? { deviceId: { ideal: deviceId } } : true,
      });
      audioCtx = new AudioContext();
      void audioCtx.resume();
      const src = audioCtx.createMediaStreamSource(micStream);
      analyser = audioCtx.createAnalyser();
      analyser.fftSize = 512;
      src.connect(analyser);
      const buf = new Uint8Array(analyser.fftSize);
      const tick = () => {
        if (!analyser) return;
        analyser.getByteTimeDomainData(buf);
        let peak = 0;
        for (let i = 0; i < buf.length; i++) peak = Math.max(peak, Math.abs(buf[i] - 128) / 128);
        const lit = Math.min(12, Math.round(peak * 18));
        bars.forEach((b, i) => {
          b.className = i < lit ? (i > 9 ? 'lit hot' : 'lit') : '';
        });
        rafId = requestAnimationFrame(tick);
      };
      tick();
      return true;
    } catch {
      // 未授权时电平表保持熄灭
      return false;
    }
  }

  function stop() {
    cancelAnimationFrame(rafId);
    analyser = null;
    micStream?.getTracks().forEach((t) => t.stop());
    micStream = null;
    void audioCtx?.close();
    audioCtx = null;
  }

  return { start, stop };
}
