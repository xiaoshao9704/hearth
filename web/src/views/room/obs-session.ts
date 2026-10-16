// 本机 OBS 联动会话：进房时悄悄拨号、轮询推流状态、按需把本频道 WHIP 写进 OBS 并开播。
// 连接态信号仍由房间页持有（投屏按钮 / 面板要读），本模块只管生命周期与动作。
import type { Accessor, Setter } from 'solid-js';
import { getIngestToken, siteInfoCached } from '../../api';
import { obsPlatform, startObsStream } from '../../obsws';
import type { ObsConn, ObsStreamStatus, ObsVersion } from '../../obsws';
import { toast } from '../../ui';
import {
  connectStoredObs,
  OBS_READY_EVENT,
  OBS_SETUP_HINT,
  obsCaptureBlocker,
  whipServer,
} from './obs-capture';

export interface ObsSessionOpts {
  leaving: () => boolean;
  channelId: () => number;
  obsConn: Accessor<ObsConn | null>;
  setObsConn: Setter<ObsConn | null>;
  setObsVer: Setter<ObsVersion | null>;
  setObsStatus: Setter<ObsStreamStatus | null>;
  setScreenPick: Setter<boolean>;
  onRefreshMeta: () => void;
}

export interface ObsSession {
  dial: () => Promise<void>;
  drop: () => void;
  captureReady: () => void;
  startToChannel: () => Promise<void>;
  stopScreen: () => Promise<void>;
  dispose: () => void;
}

export function createObsSession(opts: ObsSessionOpts): ObsSession {
  let obsPoll: ReturnType<typeof setInterval> | undefined;
  let obsDialing = false;

  const drop = () => {
    clearInterval(obsPoll);
    obsPoll = undefined;
    opts.setObsConn(null);
    opts.setObsVer(null);
    opts.setObsStatus(null);
    opts.setScreenPick(false);
  };

  async function dial() {
    if (obsDialing || opts.obsConn()?.alive || opts.leaving()) return;
    obsDialing = true;
    try {
      const c = await connectStoredObs();
      if (!c) return;
      if (opts.leaving() || opts.obsConn()?.alive) return c.close();
      const ver = await c.request('GetVersion');
      if (obsCaptureBlocker(obsPlatform(ver.platform), ver.obsVersion)) return c.close();
      c.onLost = drop;
      opts.setObsVer(ver);
      opts.setObsConn(c);
      const poll = async () => {
        if (!c.alive) return;
        try {
          opts.setObsStatus(await c.request('GetStreamStatus'));
        } catch {
          /* 轮询失败不打扰：真断了走 onLost */
        }
      };
      await poll();
      obsPoll = setInterval(() => void poll(), 2000);
    } catch {
      /* OBS 没开或密码变了：静默降级 */
    } finally {
      obsDialing = false;
    }
  }

  const onObsReady = () => void dial();
  window.addEventListener(OBS_READY_EVENT, onObsReady);

  return {
    dial,
    drop,
    captureReady: () => toast(OBS_SETUP_HINT, '', 7000),
    async startToChannel() {
      const c = opts.obsConn();
      if (!c?.alive) throw new Error('OBS 连接已断开，请重新配置 OBS 联动');
      const id = opts.channelId();
      if (!id) throw new Error('频道还没就绪，稍等一下再试');
      const [info, site] = await Promise.all([getIngestToken(), siteInfoCached().catch(() => null)]);
      const server = whipServer(info, site, id);
      if (!server || !info.token) throw new Error('推流地址还没拿到，稍等一下再试');
      await startObsStream(c, server, info.token);
      opts.setObsStatus(await c.request('GetStreamStatus'));
      toast('OBS 已开始推流', 'ok');
      opts.onRefreshMeta();
    },
    async stopScreen() {
      const c = opts.obsConn();
      if (!c?.alive) return;
      try {
        await c.request('StopStream');
        opts.setObsStatus(await c.request('GetStreamStatus'));
        toast('已让 OBS 停止投屏', 'ok', 1600);
      } catch (err) {
        const msg = err instanceof Error ? err.message : typeof err === 'string' ? err : String(err);
        toast(`停止投屏失败：${msg}`, 'bad');
      }
      opts.onRefreshMeta();
    },
    dispose() {
      window.removeEventListener(OBS_READY_EVENT, onObsReady);
      clearInterval(obsPoll);
      opts.obsConn()?.close();
    },
  };
}
