// 触屏长按：右键菜单在触屏上没有对应手势（iOS Safari 连 contextmenu 都不发），
// 全站凡有 onContextMenu 的地方都配一份长按走同一个菜单。
// 计时 500ms，位移超 10px 或提前抬手即取消；触发后吞掉抬手合成的那次 click
// （既 preventDefault 该次 touchend，也记一个时间戳给挂在别处的 click 处理器查）。

const DEFAULT_MS = 500;
const MOVE_TOL = 10; // px：超过就当成滑动/滚动，不是长按
const CLICK_SWALLOW_MS = 300; // 长按刚触发后这段时间内的 click 一律忽略

let firedAt = 0;

// 刚刚由长按弹过菜单：click 处理器据此忽略这一次（有些浏览器的合成 click 不受 preventDefault 影响）
export function longPressJustFired(): boolean {
  return Date.now() - firedAt < CLICK_SWALLOW_MS;
}

export interface LongPressOpts {
  ms?: number;
}

// handler 收长按落点的视口坐标与按下时的目标元素（列表类在容器上绑一次，用 closest 找行）
export function wireLongPress(
  el: HTMLElement,
  handler: (x: number, y: number, target: HTMLElement) => void,
  opts: LongPressOpts = {},
): () => void {
  const ms = opts.ms ?? DEFAULT_MS;
  let timer = 0;
  let sx = 0;
  let sy = 0;
  let fired = false;

  const stop = () => {
    clearTimeout(timer);
    timer = 0;
  };

  const onStart = (ev: TouchEvent) => {
    stop();
    fired = false;
    if (ev.touches.length !== 1) return; // 双指是缩放/拖动，不是长按
    const t = ev.touches[0];
    const target = ev.target as HTMLElement;
    sx = t.clientX;
    sy = t.clientY;
    timer = window.setTimeout(() => {
      timer = 0;
      fired = true;
      firedAt = Date.now();
      handler(sx, sy, target);
    }, ms);
  };
  const onMove = (ev: TouchEvent) => {
    const t = ev.touches[0];
    if (!t) return;
    if (Math.abs(t.clientX - sx) > MOVE_TOL || Math.abs(t.clientY - sy) > MOVE_TOL) stop();
  };
  const onEnd = (ev: TouchEvent) => {
    stop();
    if (!fired) return;
    fired = false;
    ev.preventDefault(); // 菜单已经弹出来了，别让抬手再点到底下的按钮
  };
  const onCancel = () => {
    stop();
    fired = false;
  };

  el.addEventListener('touchstart', onStart, { passive: true });
  el.addEventListener('touchmove', onMove, { passive: true });
  el.addEventListener('touchend', onEnd);
  el.addEventListener('touchcancel', onCancel);

  return () => {
    stop();
    el.removeEventListener('touchstart', onStart);
    el.removeEventListener('touchmove', onMove);
    el.removeEventListener('touchend', onEnd);
    el.removeEventListener('touchcancel', onCancel);
  };
}
