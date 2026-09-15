// 「OBS 联动」：用 obs-websocket 5.x 直接把本频道的 WHIP 地址与令牌写进本机 OBS 并开播。
// 只能配与浏览器同机的 OBS（地址限回环明文或 wss），密码只落本机 localStorage、不进日志与错误文案。
import { inShell } from '../../bridge';
import { createSignal, onCleanup, Show } from 'solid-js';
import {
  connectObs,
  OBS_WHIP_MIN_MAJOR,
  OBS_WS_DEFAULT_URL,
  obsMajor,
  whipServiceSettings,
  type ObsConn,
  type ObsStreamStatus,
  type ObsVersion,
} from '../../obsws';
import { confirmDialog, el, icon, toast } from '../../ui';

const LS_URL = 'hearth_obsws_url';
const LS_PASSWORD = 'hearth_obsws_password';

const lsGet = (k: string, dflt = ''): string => {
  try {
    return localStorage.getItem(k) ?? dflt;
  } catch {
    return dflt;
  }
};
const lsSet = (k: string, v: string): void => {
  try {
    localStorage.setItem(k, v);
  } catch {
    /* 隐私模式写不了就只在本次会话里有效 */
  }
};

const fmtDur = (ms: number): string => {
  const t = Math.max(0, Math.floor(ms / 1000));
  const p = (n: number) => String(n).padStart(2, '0');
  const h = Math.floor(t / 3600);
  return `${h ? `${h}:` : ''}${p(Math.floor((t % 3600) / 60))}:${p(t % 60)}`;
};

const fmtRate = (kbps: number): string => (kbps >= 1000 ? `${(kbps / 1000).toFixed(1)} Mbps` : `${Math.round(kbps)} kbps`);

export const ObsLink = (p: {
  server: () => string; // 已含频道 id 的完整 WHIP 地址，空 = 还没算出来
  token: () => string;
}) => {
  const [url, setUrl] = createSignal(lsGet(LS_URL) || OBS_WS_DEFAULT_URL);
  const [password, setPassword] = createSignal(lsGet(LS_PASSWORD));
  const [conn, setConn] = createSignal<ObsConn | null>(null);
  const [ver, setVer] = createSignal<ObsVersion | null>(null);
  const [status, setStatus] = createSignal<ObsStreamStatus | null>(null);
  const [kbps, setKbps] = createSignal(0);
  const [busy, setBusy] = createSignal<'' | 'test' | 'start' | 'stop'>('');
  const [err, setErr] = createSignal('');

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
      applyStatus(await c.request<ObsStreamStatus>('GetStreamStatus'));
    } catch {
      /* 轮询失败不打扰：连接真断了走 onLost */
    }
  };

  // 复用活连接；断了或还没连就重新握手
  const ensure = async (): Promise<ObsConn> => {
    const live = conn();
    if (live?.alive) return live;
    const c = await connectObs(url(), password(), undefined, undefined, inShell());
    c.onLost = () => {
      drop();
      setErr('OBS 连接已断开');
    };
    const v = await c.request<ObsVersion>('GetVersion');
    setVer(v);
    setConn(c);
    if (!poll) poll = setInterval(() => void refresh(), 2000);
    applyStatus(await c.request<ObsStreamStatus>('GetStreamStatus'));
    return c;
  };

  // 停播后 OBS 的输出要一小会儿才真正落下来，立刻 StartStream 会被拒
  const waitIdle = async (c: ObsConn) => {
    for (let i = 0; i < 20; i++) {
      const s = await c.request<ObsStreamStatus>('GetStreamStatus');
      applyStatus(s);
      if (!s.outputActive) return;
      await new Promise((r) => setTimeout(r, 300));
    }
    throw new Error('OBS 迟迟没有停下当前推流，请在 OBS 里手动停止后重试');
  };

  const run = async (what: 'test' | 'start' | 'stop', fn: (c: ObsConn) => Promise<void>) => {
    if (busy()) return;
    setBusy(what);
    setErr('');
    try {
      await fn(await ensure());
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy('');
    }
  };

  const test = () =>
    void run('test', async () => {
      const v = ver();
      if (v && obsMajor(v.obsVersion) < OBS_WHIP_MIN_MAJOR) toast(`OBS ${v.obsVersion} 没有 WHIP 输出`, 'bad');
      else toast('OBS 连上了', 'ok', 1600);
    });

  const start = () =>
    void run('start', async (c) => {
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
      await c.request('SetStreamServiceSettings', whipServiceSettings(p.server(), p.token()));
      await c.request('StartStream');
      await refresh();
      toast('OBS 已开始推流', 'ok');
    });

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
              lsSet(LS_URL, ev.currentTarget.value);
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
              lsSet(LS_PASSWORD, ev.currentTarget.value);
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
          classList={{ loading: busy() === 'test' }}
          disabled={busy() !== ''}
          onClick={test}
        >
          测试连接
        </button>
        <button
          type="button"
          id="obs-start"
          class="hit btn btn-sm btn-primary"
          classList={{ loading: busy() === 'start' }}
          disabled={busy() !== '' || !p.server() || !p.token()}
          onClick={start}
        >
          {el(icon('stream', 13, 'var(--on-ember)', 1.9))} 配置并开始推流
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

      <div class="ig-tip">
        需要 OBS 的「工具 → WebSocket 服务器设置」里勾上启用，并把这里的地址端口对上。密码只存在这台设备上。
        只能配和浏览器同一台机器的 OBS：明文地址限 <span class="mono ig-em">localhost</span> /{' '}
        <span class="mono ig-em">127.0.0.1</span>，远程请用 <span class="mono ig-em">wss://</span>。
      </div>
    </div>
  );
};
