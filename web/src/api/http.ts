// 与 server 交互的 REST 客户端底层：鉴权头、超时、401 清会话、版本探测。
import { inShell } from '../bridge';
import { setLeaveGuard } from '../nav';
import { toast } from '../ui';
import { deviceId } from './channel';

const TOKEN_KEY = 'hearth_token';
// 与 session.ts 的 USER_KEY 保持一致；401 清会话时不反向依赖 session，避免环依赖
const USER_KEY = 'hearth_user';
const SERVER_KEY = 'hearth_server';

// 桌面壳里服务器地址是用户填的（同一个安装包要能连任意一台 hearth），浏览器里仍是
// 构建期注入的那个。优先级：本机存档 > 壳注入的默认值 > 构建期变量。
// 模块加载时定一次，改地址的一方负责 reload——运行中换服务器等于换了整个会话。
declare global {
  interface Window {
    __HEARTH_SERVER__?: string;
  }
}

export function trimSlash(url: string): string {
  return url.replace(/\/+$/, '');
}

export function getServerURL(): string | null {
  return localStorage.getItem(SERVER_KEY);
}

export function setServerURL(url: string) {
  localStorage.setItem(SERVER_KEY, trimSlash(url));
}

// 忘记服务器（桌面壳的 #/server 页用）：清掉存档，下次启动重新问连哪台
export function clearServerURL() {
  localStorage.removeItem(SERVER_KEY);
}

export const SERVER_URL: string = trimSlash(
  getServerURL() ?? window.__HEARTH_SERVER__ ?? import.meta.env.VITE_SERVER_URL ?? 'http://localhost:8080',
);
// 显示给用户看的服务器主机名：壳里网页的 origin 是 tauri://localhost，
// location.host 只会显示 tauri.localhost，用户认的是自己连的那台服务器。
export function serverHost(): string {
  if (!inShell()) return location.host || 'localhost';
  try {
    return new URL(SERVER_URL).host;
  } catch {
    return SERVER_URL;
  }
}

export const LIVEKIT_URL_FALLBACK: string =
  import.meta.env.VITE_LIVEKIT_URL ?? 'ws://localhost:7880';

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function persistToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token);
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY);
}

// 请求失败的统一错误类型：status 是 HTTP 状态码，0 表示网络层失败（连不上 / 超时）。
// 调用方据此区分「服务器拒绝」与「压根没到服务器」，message 一律是可直接展示的中文。
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// 服务端没给 error 字段时的兜底文案
export function statusMessage(status: number): string {
  if (status === 403) return '没有权限执行此操作';
  if (status === 404) return '资源不存在';
  if (status === 429) return '操作太频繁，稍后再试';
  if (status >= 500) return '服务器出错了，稍后再试';
  return `请求失败 (${status})`;
}

// 登录后回跳用：401 时记下当前 hash，登录页取用后自行清除
const NEXT_KEY = 'hearth_next';
export const REQ_TIMEOUT_MS = 15000;

// 服务端升级探测：每个 /api/* 响应都带 X-Hearth-Version（见 server/internal/api/api.go 的
// versionHeader 中间件）。记住首次看到的值，后续值一变就说明服务端重启到了新版本——旧标签页的
// 前端代码还是旧的，提示用户刷新。只在拿到响应头（非空）时比较，网络层失败/拿不到头都不判断；
// 只弹一次，避免同一页面反复弹。
let knownVersion: string | null = null;
let versionUpdateNotified = false;

function checkVersionHeader(res: Response): void {
  const v = res.headers.get('X-Hearth-Version');
  if (!v) return;
  if (knownVersion === null) {
    knownVersion = v;
    return;
  }
  if (v === knownVersion || versionUpdateNotified) return;
  versionUpdateNotified = true;
  toast('服务已更新，点击此提示刷新页面以使用新版本', '', 20000, () => location.reload());
}

async function req<T>(path: string, options: { method?: string; body?: unknown } = {}): Promise<T> {
  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;
  // 设备头对普通会话无害（服务端只在会话绑定了设备时校验，即访客）
  headers['X-Device-Id'] = deviceId();
  // FormData（证书上传）不手写 Content-Type：浏览器要自己带上 multipart boundary
  const isForm = options.body instanceof FormData;
  if (options.body !== undefined && !isForm) headers['Content-Type'] = 'application/json';
  let res: Response;
  try {
    res = await fetch(`${SERVER_URL}${path}`, {
      method: options.method ?? 'GET',
      headers,
      body: options.body === undefined ? undefined : isForm ? (options.body as FormData) : JSON.stringify(options.body),
      signal: AbortSignal.timeout(REQ_TIMEOUT_MS),
    });
  } catch (e) {
    // fetch 只在网络层失败时 reject（HTTP 错误码走下面的 res.ok 分支）
    const timeout = (e as { name?: string } | null)?.name === 'TimeoutError';
    throw new ApiError(0, timeout ? '服务器响应超时，请稍后重试' : '连不上服务器，请检查网络或服务器地址');
  }
  checkVersionHeader(res);
  if (!res.ok) {
    const data = (await res.json().catch(() => null)) as { error?: string } | null;
    // 带着 token 却 401 说明会话已失效（区别于登录页密码错误——那时本地无 token）：
    // 清掉本地会话并跳回登录页，仍然 throw 让调用方停止后续逻辑
    if (res.status === 401 && token) {
      if (!location.hash.startsWith('#/login')) sessionStorage.setItem(NEXT_KEY, location.hash);
      clearToken();
      localStorage.removeItem(USER_KEY);
      setLeaveGuard(null); // 会话都没了，不该再被房间的离开确认拦住
      toast('登录已失效，请重新登录', 'bad');
      // replace 而非 push：否则用户按返回键会回到失效页再被踢回来，死循环
      location.replace('#/login');
    }
    throw new ApiError(res.status, data?.error ?? statusMessage(res.status));
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

// 供同源的其它模块复用鉴权/超时/401 处理（chat.ts 的消息接口）：形状与内部调用完全一致
export { req as apiRequest };

// 延迟标尺的时钟源（`GET /api/time`，无需登录）：不走 req()，标尺页可能根本没有会话，
// 也不该让一次时钟校准触发 401 的清会话跳转
export async function fetchServerTime(): Promise<number> {
  const res = await fetch(`${SERVER_URL}/api/time`, { signal: AbortSignal.timeout(5000) });
  if (!res.ok) throw new ApiError(res.status, statusMessage(res.status));
  const data = (await res.json()) as { now_ms?: number };
  if (typeof data.now_ms !== 'number') throw new ApiError(0, '服务器时间格式错误');
  return data.now_ms;
}
