// 发版后仍开着的旧页面：壳还是旧的，动态 import 的 chunk 名却已随新产物换掉，一导入就 404。
// 这种失败重试多少次都不会好（服务端已经没有那个文件了），只有刷新能恢复；
// 但刷新必须一次会话只做一次，否则导入失败若另有原因就会变成刷新死循环。
const RELOADED = 'hearth_stale_reload';

// 「动态导入的模块拉不回来」各家浏览器措辞不同，只能按 message 认：
// Chrome/Edge「Failed to fetch dynamically imported module」、Firefox「error loading dynamically
// imported module」、Safari「Importing a module script failed」；ChunkLoadError 是打包器自己的名字。
const STALE_RE =
  /failed to fetch dynamically imported module|error loading dynamically imported module|importing a module script failed|chunkloaderror/i;

export function isStaleChunkError(e: unknown): boolean {
  const err = e as { name?: unknown; message?: unknown } | null;
  if (!err) return false;
  if (String(err.name ?? '') === 'ChunkLoadError') return true;
  return STALE_RE.test(String(err.message ?? ''));
}

/** 本次会话已经因为旧产物自动刷新过一次 */
export function staleReloadUsed(): boolean {
  try {
    return sessionStorage.getItem(RELOADED) === '1';
  } catch {
    return true; // 标记读不到就当刷过：宁可提示用户手动刷，也不能刷个没完
  }
}

/** 记下标记并刷新页面；返回 false = 标记落不下（刷了还会再刷），调用方改为提示用户 */
export function reloadForStale(): boolean {
  try {
    sessionStorage.setItem(RELOADED, '1');
    if (sessionStorage.getItem(RELOADED) !== '1') return false;
  } catch {
    return false;
  }
  location.reload();
  return true;
}
