// 「OBS 联动」：用 obs-websocket 5.x 直接把本频道的 WHIP 地址与令牌写进本机 OBS 并开播，
// 顺带读写 OBS 的输出画质（分辨率 / 帧率 / 码率）——那是 OBS 自己的设置，与 hearth 的投屏画质无关。
// 只能配与浏览器同机的 OBS（地址限回环明文或 wss），密码只落本机 localStorage、不进日志与错误文案。
import { inShell } from '../../bridge';
import { createMemo, createSignal, For, onCleanup, Show } from 'solid-js';
import {
  connectObs,
  encoderLabel,
  OBS_WHIP_MIN_MAJOR,
  OBS_WS_DEFAULT_URL,
  obsMajor,
  startObsStream,
  type ObsConn,
  type ObsStreamStatus,
  type ObsVersion,
  type ObsVideoSettings,
} from '../../obsws';
import {
  LS_OBS_PASSWORD,
  LS_OBS_READY,
  LS_OBS_URL,
  OBS_READY_EVENT,
  obsLsGet,
  obsLsSet,
  ObsCaptureSection,
} from './obs-capture';
import { confirmDialog, el, icon, toast } from '../../ui';

const fmtDur = (ms: number): string => {
  const t = Math.max(0, Math.floor(ms / 1000));
  const p = (n: number) => String(n).padStart(2, '0');
  const h = Math.floor(t / 3600);
  return `${h ? `${h}:` : ''}${p(Math.floor((t % 3600) / 60))}:${p(t % 60)}`;
};

const fmtRate = (kbps: number): string => (kbps >= 1000 ? `${(kbps / 1000).toFixed(1)} Mbps` : `${Math.round(kbps)} kbps`);

// 输出分辨率候选：只列常用档，OBS 里现设的值若不在表内会另行补进去
const RES_OPTIONS = ['1920x1080', '1600x900', '1280x720', '960x540', '854x480'];
// 帧率用 分子/分母 作值：OBS 的 fps 本来就是分数，59.94 这类值不能四舍五入回写
const FPS_OPTIONS = ['60/1', '50/1', '30/1', '24/1'];
const fpsLabel = (v: string): string => {
  const [n, d] = v.split('/').map(Number);
  if (!n || !d) return v;
  return d === 1 ? `${n}` : (n / d).toFixed(2);
};

const PRESETS = [
  { label: '1080p60 · 8000k', w: 1920, h: 1080, fps: 60, kbps: 8000 },
  { label: '1080p30 · 6000k', w: 1920, h: 1080, fps: 30, kbps: 6000 },
  { label: '720p60 · 4000k', w: 1280, h: 720, fps: 60, kbps: 4000 },
];

const SIMPLE_OUT = 'SimpleOutput';

export const ObsLink = (p: {
  server: () => string; // 已含频道 id 的完整 WHIP 地址，空 = 还没算出来
  token: () => string;
}) => {
  const [url, setUrl] = createSignal(obsLsGet(LS_OBS_URL) || OBS_WS_DEFAULT_URL);
  const [password, setPassword] = createSignal(obsLsGet(LS_OBS_PASSWORD));
  const [conn, setConn] = createSignal<ObsConn | null>(null);
  const [ver, setVer] = createSignal<ObsVersion | null>(null);
  const [status, setStatus] = createSignal<ObsStreamStatus | null>(null);
  const [kbps, setKbps] = createSignal(0);
  const [busy, setBusy] = createSignal<'' | 'test' | 'start' | 'stop' | 'quality'>('');
  const [err, setErr] = createSignal('');
  // 浏览器的本地网络权限提示挂着时，这条连接会一直等（不断开提示就不会消失），得说清楚在等什么
  const [waitingLna, setWaitingLna] = createSignal(false);
  // 画质：都以 OBS 里的现值为准，本地不留第二份真相
  const [video, setVideo] = createSignal<ObsVideoSettings | null>(null);
  const [bitrate, setBitrate] = createSignal('');
  const [encoder, setEncoder] = createSignal('');
  const [advOut, setAdvOut] = createSignal(false);

  // 码率 obs-websocket 不直接给，从相邻两次 outputBytes/outputDuration 的增量推
  let prevSample: { bytes: number; ms: number } | null = null;
  let poll: ReturnType<typeof setInterval> | undefined;

  const drop = () => {
    clearInterval(poll);
    poll = undefined;
    setConn(null);
    setVer(null);
    setStatus(null);
    setKbps(0);
    setVideo(null);
    setBitrate('');
    setEncoder('');
    setAdvOut(false);
    prevSample = null;
  };

  // 改了地址或密码就把旧连接扔掉，免得按钮还连在上一台 OBS 上
  const resetConn = () => {
    conn()?.close();
    if (conn()) drop();
  };

  const applyStatus = (s: ObsStreamStatus) => {
    if (!s.outputActive) {
      prevSample = null;
      setKbps(0);
    } else {
      if (prevSample && s.outputDuration > prevSample.ms)
        setKbps(((s.outputBytes - prevSample.bytes) * 8) / (s.outputDuration - prevSample.ms));
      prevSample = { bytes: s.outputBytes, ms: s.outputDuration };
    }
    setStatus(s);
  };

  const refresh = async () => {
    const c = conn();
    if (!c?.alive) return;
    try {
      applyStatus(await c.request('GetStreamStatus'));
    } catch {
      /* 轮询失败不打扰：连接真断了走 onLost */
    }
  };

  // 取 profile 参数：没设过就用 OBS 给的默认值
  const profileParam = async (c: ObsConn, category: string, name: string): Promise<string> => {
    const r = await c.request('GetProfileParameter', { parameterCategory: category, parameterName: name });
    return r.parameterValue ?? r.defaultParameterValue ?? '';
  };

  const loadQuality = async (c: ObsConn) => {
    setVideo(await c.request('GetVideoSettings'));
    // 高级输出模式下码率藏在各编码器自己的设置里，形状因编码器而异，只提示不代写
    const adv = (await profileParam(c, 'Output', 'Mode')) === 'Advanced';
    setAdvOut(adv);
    setEncoder(await profileParam(c, SIMPLE_OUT, 'StreamEncoder'));
    setBitrate(adv ? '' : await profileParam(c, SIMPLE_OUT, 'VBitrate'));
  };

  // 复用活连接；断了或还没连就重新握手
  const ensure = async (): Promise<ObsConn> => {
    const live = conn();
    if (live?.alive) return live;
    const c = await connectObs(url(), password(), inShell(), { onWaiting: () => setWaitingLna(true) });
    c.onLost = () => {
      drop();
      setErr('OBS 连接已断开');
    };
    setVer(await c.request('GetVersion'));
    setConn(c);
    // 连通过一次才算「这台设备配过 OBS 联动」：房间页据此决定要不要自动连，
    // 没配过的人不该被去敲一遍 localhost:4455。
    obsLsSet(LS_OBS_READY, '1');
    // 房间页只在进房时自动连一次，首次在这里配好的得靠这一声才不用退出重进
    window.dispatchEvent(new CustomEvent(OBS_READY_EVENT));
    if (!poll) poll = setInterval(() => void refresh(), 2000);
    applyStatus(await c.request('GetStreamStatus'));
    // 画质是附加能力：读不到（老 obs-websocket、profile 参数缺失）也不该挡住推流那条主路
    try {
      await loadQuality(c);
    } catch {
      setVideo(null);
    }
    return c;
  };

  // 停播后 OBS 的输出要一小会儿才真正落下来，立刻 StartStream 会被拒
  const waitIdle = async (c: ObsConn) => {
    for (let i = 0; i < 20; i++) {
      const s = await c.request('GetStreamStatus');
      applyStatus(s);
      if (!s.outputActive) return;
      await new Promise((r) => setTimeout(r, 300));
    }
    throw new Error('OBS 迟迟没有停下当前推流，请在 OBS 里手动停止后重试');
  };

  const run = async (what: Exclude<ReturnType<typeof busy>, ''>, fn: (c: ObsConn) => Promise<void>) => {
    if (busy()) return;
    setBusy(what);
    setErr('');
    setWaitingLna(false);
    try {
      await fn(await ensure());
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy('');
      setWaitingLna(false);
    }
  };

  const test = () =>
    void run('test', async () => {
      const v = ver();
      if (v && obsMajor(v.obsVersion) < OBS_WHIP_MIN_MAJOR) toast(`OBS ${v.obsVersion} 没有 WHIP 输出`, 'bad');
      else toast('OBS 连上了', 'ok', 1600);
    });

  // 写直播服务设置并开播
  const startOn = async (c: ObsConn) => {
    if (!p.server() || !p.token()) throw new Error('推流地址还没拿到，稍等一下再试');
    const v = ver();
    if (v && obsMajor(v.obsVersion) < OBS_WHIP_MIN_MAJOR)
      throw new Error(`OBS ${v.obsVersion} 没有 WHIP 输出，需要 OBS ${OBS_WHIP_MIN_MAJOR} 或更新的版本`);
    if (status()?.outputActive) {
      const ok = await confirmDialog({
        title: 'OBS 正在推流',
        body: '要停掉 OBS 当前的推流，改推到本频道吗？',
        confirmText: '停掉并改推',
        danger: true,
      });
      if (!ok) return;
      await c.request('StopStream');
      await waitIdle(c);
    }
    await startObsStream(c, p.server(), p.token());
    await refresh();
    toast('OBS 已开始推流', 'ok');
  };

  const start = () => void run('start', startOn);

  const stop = () =>
    void run('stop', async (c) => {
      await c.request('StopStream');
      await refresh();
      toast('已让 OBS 停止推流', 'ok', 1600);
    });

  onCleanup(() => {
    clearInterval(poll);
    conn()?.close();
  });

  const lowVersion = () => {
    const v = ver();
    return !!v && obsMajor(v.obsVersion) < OBS_WHIP_MIN_MAJOR;
  };
  const live = () => status()?.outputActive === true;

  // ---- 画质 ----
  const resValue = () => {
    const v = video();
    return v ? `${v.outputWidth}x${v.outputHeight}` : '';
  };
  const resList = createMemo(() => {
    const cur = resValue();
    return cur && !RES_OPTIONS.includes(cur) ? [cur, ...RES_OPTIONS] : RES_OPTIONS;
  });
  const fpsValue = () => {
    const v = video();
    return v ? `${v.fpsNumerator}/${v.fpsDenominator}` : '';
  };
  const fpsList = createMemo(() => {
    const cur = fpsValue();
    return cur && !FPS_OPTIONS.includes(cur) ? [cur, ...FPS_OPTIONS] : FPS_OPTIONS;
  });
  // 分辨率与帧率 OBS 在推流中一律拒改，禁用比让它报错好懂
  const videoLocked = () => live();
  const qualityBusy = () => busy() !== '';

  const setVideoBits = (c: ObsConn, bits: { outputWidth?: number; outputHeight?: number; fpsNumerator?: number; fpsDenominator?: number }) =>
    c.request('SetVideoSettings', bits);

  const writeBitrate = (c: ObsConn, kbpsValue: number) =>
    c.request('SetProfileParameter', {
      parameterCategory: SIMPLE_OUT,
      parameterName: 'VBitrate',
      parameterValue: String(kbpsValue),
    });

  const onRes = (v: string) =>
    void run('quality', async (c) => {
      const [w, h] = v.split('x').map(Number);
      if (!w || !h) return;
      await setVideoBits(c, { outputWidth: w, outputHeight: h });
      setVideo(await c.request('GetVideoSettings'));
    });

  const onFps = (v: string) =>
    void run('quality', async (c) => {
      const [n, d] = v.split('/').map(Number);
      if (!n || !d) return;
      await setVideoBits(c, { fpsNumerator: n, fpsDenominator: d });
      setVideo(await c.request('GetVideoSettings'));
    });

  const onBitrate = (raw: string) =>
    void run('quality', async (c) => {
      const n = Math.round(Number(raw));
      if (!Number.isFinite(n) || n <= 0) throw new Error('码率要填正整数（单位 kbps）');
      await writeBitrate(c, n);
      setBitrate(await profileParam(c, SIMPLE_OUT, 'VBitrate'));
    });

  const applyPreset = (preset: (typeof PRESETS)[number]) =>
    void run('quality', async (c) => {
      await setVideoBits(c, {
        outputWidth: preset.w,
        outputHeight: preset.h,
        fpsNumerator: preset.fps,
        fpsDenominator: 1,
      });
      setVideo(await c.request('GetVideoSettings'));
      if (!advOut()) {
        await writeBitrate(c, preset.kbps);
        setBitrate(await profileParam(c, SIMPLE_OUT, 'VBitrate'));
      }
      toast(advOut() ? `已套用 ${preset.label} 的画面设置（码率见下方提示）` : `已套用 ${preset.label}`, 'ok', 1800);
    });

  return (
    <div class="ig-field obs-link">
      <div class="section-label">OBS 联动 · 直接配本机 OBS</div>
      <div class="obs-inputs">
        <div class="field obs-in">
          <input
            id="obs-url"
            value={url()}
            placeholder={OBS_WS_DEFAULT_URL}
            autocomplete="off"
            spellcheck={false}
            aria-label="obs-websocket 地址"
            onInput={(ev) => {
              setUrl(ev.currentTarget.value);
              obsLsSet(LS_OBS_URL, ev.currentTarget.value);
              resetConn();
            }}
          />
        </div>
        <div class="field obs-in">
          <input
            id="obs-password"
            type="password"
            value={password()}
            placeholder="密码（没设就留空）"
            autocomplete="new-password"
            aria-label="obs-websocket 密码"
            onInput={(ev) => {
              setPassword(ev.currentTarget.value);
              obsLsSet(LS_OBS_PASSWORD, ev.currentTarget.value);
              resetConn();
            }}
          />
        </div>
      </div>

      <div class="obs-actions">
        <button
          type="button"
          id="obs-test"
          class="hit btn btn-sm"
          classList={{ loading: busy() === 'test', 'btn-primary': !conn() }}
          disabled={busy() !== ''}
          onClick={test}
        >
          测试连接
        </button>
        <button
          type="button"
          id="obs-start"
          class="hit btn btn-sm"
          // 未连接时它仍然可点（点了会先握手），但主按钮让给「测试连接」：
          // 连不上时错误只会从这里冒出来，不如让人先按那条更短的路。
          classList={{ loading: busy() === 'start', 'btn-primary': !!conn() }}
          disabled={busy() !== '' || !p.server() || !p.token()}
          title={conn() ? '' : '还没连上 OBS，点这里会先连一次再配置推流'}
          onClick={start}
        >
          {/* 主次样式会随连接态切换，图标跟着按钮文字色走 */}
          {el(icon('stream', 13, 'currentColor', 1.9))} 配置并开始推流
        </button>
        <button
          type="button"
          id="obs-stop"
          class="hit btn btn-sm btn-danger"
          classList={{ loading: busy() === 'stop' }}
          disabled={busy() !== '' || !live()}
          onClick={stop}
        >
          停止推流
        </button>
      </div>

      <div class="obs-status">
        <span class="obs-dot" classList={{ on: live(), idle: !!conn() && !live() }} />
        <Show when={conn()} fallback={<span>未连接 OBS</span>}>
          <span>
            {live() ? `推流中 · ${fmtDur(status()!.outputDuration)}` : '已连接 · 未推流'}
            <Show when={live() && status()!.outputReconnecting}>
              <span class="obs-warn"> · 重连中</span>
            </Show>
            <Show when={live() && kbps() > 0}>
              <span class="mono"> · {fmtRate(kbps())}</span>
            </Show>
          </span>
        </Show>
        <Show when={ver()}>
          <span class="obs-ver mono">
            OBS {ver()!.obsVersion} · ws {ver()!.obsWebSocketVersion}
          </span>
        </Show>
      </div>

      <Show when={waitingLna()}>
        <div class="hint-card">
          <span class="ig-note">
            Chrome 正在询问是否允许本站访问本地网络，请在地址栏旁的提示里点「允许」，连接会自动继续。
          </span>
        </div>
      </Show>
      <Show when={err()}>
        <div class="notice-bad">
          <span class="ig-note">{err()}</span>
        </div>
      </Show>
      <Show when={lowVersion()}>
        <div class="notice-bad">
          <span class="ig-note">
            OBS {ver()!.obsVersion} 没有 WHIP 输出（OBS {OBS_WHIP_MIN_MAJOR} 起才有），升级后才能一键推流。
          </span>
        </div>
      </Show>

      <Show when={conn() && ver()}>
        <ObsCaptureSection
          conn={conn()!}
          obsVersion={ver()!.obsVersion}
          platform={ver()!.platform}
          busy={busy() !== ''}
          start={() => startOn(conn()!)}
        />
      </Show>

      <Show when={video()}>
        <div class="obs-quality">
          <div class="section-label">OBS 画质 · 改的是 OBS 自己的输出设置</div>
          <div class="obs-q-row">
            <label class="obs-q-item">
              <span class="obs-q-label">分辨率</span>
              <select
                id="obs-res"
                class="hit obs-select"
                value={resValue()}
                disabled={qualityBusy() || videoLocked()}
                onChange={(ev) => onRes(ev.currentTarget.value)}
              >
                <For each={resList()}>{(o) => <option value={o}>{o.replace('x', ' × ')}</option>}</For>
              </select>
            </label>
            <label class="obs-q-item">
              <span class="obs-q-label">帧率</span>
              <select
                id="obs-fps"
                class="hit obs-select"
                value={fpsValue()}
                disabled={qualityBusy() || videoLocked()}
                onChange={(ev) => onFps(ev.currentTarget.value)}
              >
                <For each={fpsList()}>{(o) => <option value={o}>{fpsLabel(o)} fps</option>}</For>
              </select>
            </label>
            <label class="obs-q-item">
              <span class="obs-q-label">码率</span>
              <div class="field obs-q-bitrate">
                <input
                  id="obs-bitrate"
                  value={bitrate()}
                  inputmode="numeric"
                  placeholder="kbps"
                  autocomplete="off"
                  disabled={qualityBusy() || advOut()}
                  aria-label="推流码率（kbps）"
                  onChange={(ev) => onBitrate(ev.currentTarget.value)}
                />
                <span class="obs-q-unit">kbps</span>
              </div>
            </label>
          </div>

          <div class="obs-presets">
            <For each={PRESETS}>
              {(preset) => (
                <button
                  type="button"
                  class="hit btn btn-sm"
                  disabled={qualityBusy() || videoLocked()}
                  onClick={() => applyPreset(preset)}
                >
                  {preset.label}
                </button>
              )}
            </For>
          </div>

          <Show when={videoLocked()}>
            <div class="ig-tip">推流中改不了分辨率与帧率，停止推流后才能改。</div>
          </Show>
          <Show when={advOut()}>
            <div class="ig-tip">OBS 现在是高级输出模式，码率在编码器设置里，请在 OBS 里改。</div>
          </Show>
          <Show when={encoder()}>
            <div class="ig-tip">
              编码器：<span class="ig-em">{encoderLabel(encoder())}</span>，在 OBS 设置 → 输出里改。
            </div>
          </Show>
        </div>
      </Show>

      <div class="ig-tip">
        需要 OBS 的「工具 → WebSocket 服务器设置」里勾上启用，并把这里的地址端口对上。密码只存在这台设备上。
        只能配和浏览器同一台机器的 OBS：明文地址限 <span class="mono ig-em">localhost</span> /{' '}
        <span class="mono ig-em">127.0.0.1</span>，远程请用 <span class="mono ig-em">wss://</span>。
      </div>
    </div>
  );
};
