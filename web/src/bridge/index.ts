// 桌面壳（Tauri）的桥：网页按能力检测决定走不走原生流程，没有桥时全部返回空能力，
// 浏览器里的行为与从前完全一致。
//
// 命令集刻意保持最小：列采集源、开始/停止发布、低频统计。帧与音频不经 JS，
// 控制之外的东西不往这里加。
export interface BridgeCaps {
  native_publish: boolean; // 能不能走原生投屏发布
  platform: string;
  app_audio: boolean; // 原生投屏是否带所属应用的声音
}

export interface NativeSource {
  id: string;
  kind: 'display' | 'window';
  title: string;
  app: string;
}

export interface PublishStats {
  running: boolean;
  frames: number;
  bitrate_kbps: number;
  codec: string;
  error: string | null;
}

export interface PublishArgs {
  endpoint: string; // 完整 WHIP 地址（含频道）
  token: string; // 推流令牌
  source_id: string;
  bitrate_kbps: number;
  codec: 'h264' | 'h265';
}

const NO_BRIDGE: BridgeCaps = { native_publish: false, platform: 'web', app_audio: false };

type Invoke = <T>(cmd: string, args?: Record<string, unknown>) => Promise<T>;

function rawInvoke(): Invoke | null {
  const internals = (window as { __TAURI_INTERNALS__?: { invoke?: Invoke } }).__TAURI_INTERNALS__;
  if (!internals || typeof internals.invoke !== 'function') return null;
  return internals.invoke.bind(internals) as Invoke;
}

// 同步可判的「装在壳里」：服务器地址要在首屏之前决定，等不了异步能力检测。
// 这不等于有原生投屏能力——那由 capabilities() 说了算。
export function inShell(): boolean {
  return rawInvoke() !== null;
}

let capsOnce: Promise<BridgeCaps> | null = null;

// 有桥 = 壳存在**且** capabilities 调得通：壳与网页会版本错位，
// 老壳没有这条命令，不该被当成有能力。
export function capabilities(): Promise<BridgeCaps> {
  capsOnce ??= (async () => {
    const invoke = rawInvoke();
    if (!invoke) return NO_BRIDGE;
    try {
      return await invoke<BridgeCaps>('capabilities');
    } catch {
      return NO_BRIDGE;
    }
  })();
  return capsOnce;
}

async function call<T>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const invoke = rawInvoke();
  const caps = await capabilities();
  if (!invoke || !caps.native_publish) throw new Error('当前环境没有原生投屏能力');
  return invoke<T>(cmd, args);
}

export function listSources(): Promise<NativeSource[]> {
  return call<NativeSource[]>('list_sources');
}

export function startPublish(args: PublishArgs): Promise<void> {
  return call<void>('start_publish', { ...args });
}

export function stopPublish(): Promise<void> {
  return call<void>('stop_publish');
}

export function publishStats(): Promise<PublishStats | null> {
  return call<PublishStats | null>('publish_stats');
}
