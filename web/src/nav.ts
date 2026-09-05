// 路由离房守卫：房间页注册后，任何切走当前 hash 的站内跳转都要先问一句。
// 判定必须同步——route() 在 hashchange 里当场决定画不画新视图，ui.ts 的确认对话框是异步的，
// 所以这里用浏览器原生 confirm。

let guard: (() => boolean) | null = null;
let guardedHash = '';

export function setLeaveGuard(fn: (() => boolean) | null) {
  guard = fn;
  guardedHash = fn ? location.hash : '';
}

// 撤守卫要报上自己注册的那个函数：房间 A 切房间 B 时 B 先注册、A 的清理后跑，
// 不比对就会把 B 刚装好的守卫误清掉
export function clearLeaveGuard(fn: () => boolean) {
  if (guard === fn) setLeaveGuard(null);
}

// route() 最前面调用：返回 false 表示用户放弃离开，调用方不得切视图。
export function allowLeave(): boolean {
  if (!guard || location.hash === guardedHash) return true;
  if (guard()) return true;
  // 还原地址栏用 replaceState 而不是改 location.hash：不会再触发一轮 hashchange，
  // 房间视图自己的清理监听（排在 route() 之后）读到的仍是原房间 hash，房间原地不动、不重连
  history.replaceState(null, '', guardedHash);
  return false;
}
