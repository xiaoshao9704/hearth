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

// 发布状态推送：壳侧看门狗判定失败时推一次（ICE 没建起来、迟迟没有画面、管线报错）。
// 不做前台轮询——WKWebView 进后台会挂起 JS 定时器，轮询等于没有。
export interface PublishState {
  running: boolean;
  error: string | null;
}

// 服务器探测结果。reason：untrusted / unreachable / not_https / not_hearth / bad_url
export interface CheckResult {
  ok: boolean;
  reason: string;
  detail: string;
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
type TransformCallback = (cb: (payload: unknown) => void) => number;

interface Internals {
  invoke?: Invoke;
  transformCallback?: TransformCallback;
}

function internals(): Internals | null {
  return (window as { __TAURI_INTERNALS__?: Internals }).__TAURI_INTERNALS__ ?? null;
}

function rawInvoke(): Invoke | null {
  const api = internals();
  if (!api || typeof api.invoke !== 'function') return null;
  return api.invoke.bind(api) as Invoke;
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

// 壳侧事件订阅（Tauri 的 event 插件）：不引 @tauri-apps/api，桥本来就只经 __TAURI_INTERNALS__ 通信。
export async function onPublishState(cb: (s: PublishState) => void): Promise<() => void> {
  const api = internals();
  const invoke = rawInvoke();
  if (!invoke || typeof api?.transformCallback !== 'function') return () => {};
  const handler = api.transformCallback((payload) => cb((payload as { payload: PublishState }).payload));
  try {
    const id = await invoke<number>('plugin:event|listen', {
      event: 'publish-state',
      target: { kind: 'Any' },
      handler,
    });
    return () => {
      void invoke('plugin:event|unlisten', { event: 'publish-state', eventId: id }).catch(() => {});
    };
  } catch {
    return () => {};
  }
}

// 深链订阅（浏览器跳转登录用）：壳只把 hearth:// 开头的 URL 透传过来。
// 形状同 onPublishState——事件经 Tauri 的 event 插件，不引 @tauri-apps/api。
export async function onDeepLink(cb: (url: string) => void): Promise<() => void> {
  const api = internals();
  const invoke = rawInvoke();
  if (!invoke || typeof api?.transformCallback !== 'function') return () => {};
  const handler = api.transformCallback((payload) => cb((payload as { payload: { url: string } }).payload.url));
  try {
    const id = await invoke<number>('plugin:event|listen', {
      event: 'deep-link',
      target: { kind: 'Any' },
      handler,
    });
    return () => {
      void invoke('plugin:event|unlisten', { event: 'deep-link', eventId: id }).catch(() => {});
    };
  } catch {
    return () => {};
  }
}

// openExternal 把外链交给系统默认浏览器。WebView 里 target=_blank 与 window.open
// 什么都不发生，直接导航又会把本地打包的页面顶掉、回不来。
// 返回是否已经接手：false = 不在壳里（或壳太老没有这个插件），调用方按网页原样走。
export async function openExternal(url: string): Promise<boolean> {
  const invoke = rawInvoke();
  if (!invoke) return false;
  try {
    await invoke<null>('plugin:opener|open_url', { url });
    return true;
  } catch {
    return false;
  }
}

// ---- 应用内信任（只在桌面壳里可用，浏览器里这几条命令不存在）----
// 与投屏能力无关：没有采集能力的机器也要能连服务器，所以不走 call() 的能力门槛。
function shellCall<T>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const invoke = rawInvoke();
  if (!invoke) return Promise.reject(new Error('当前环境不是桌面壳'));
  return invoke<T>(cmd, args);
}

export function checkServer(url: string): Promise<CheckResult> {
  return shellCall<CheckResult>('check_server', { url });
}

// fingerprint 是从管理员那里另行取得的根证书 SHA-256，不是从本页下载的证书上抄来的
export function pairServer(url: string, fingerprint: string): Promise<void> {
  return shellCall<void>('pair_server', { url, fingerprint_sha256: fingerprint });
}

export function forgetServer(url: string): Promise<void> {
  return shellCall<void>('forget_server', { url });
}
