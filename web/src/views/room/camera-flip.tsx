// 控制栏的「翻转摄像头」按钮：只有触屏设备才有前后摄像头之分，桌面不出这个键。
import { createSignal, onCleanup, Show } from 'solid-js';
import { el, icon, toast } from '../../ui';

// 指针粗细是媒体查询：接上外接鼠标、或平板切换输入方式都会变，跟着信号走而不是模块加载时定死
function coarsePointer() {
  const [coarse, setCoarse] = createSignal(false);
  const mq = window.matchMedia('(pointer: coarse)');
  setCoarse(mq.matches);
  const onChange = () => setCoarse(mq.matches);
  mq.addEventListener('change', onChange);
  onCleanup(() => mq.removeEventListener('change', onChange));
  return coarse;
}

export const CameraFlipButton = (p: { cameraOn: () => boolean; flip: () => Promise<void> }) => {
  const coarse = coarsePointer();
  const [busy, setBusy] = createSignal(false);
  const run = async () => {
    if (busy()) return;
    setBusy(true);
    try {
      await p.flip();
    } catch (err) {
      toast(`翻转失败：${(err as Error)?.message ?? ''}`, 'bad');
    } finally {
      setBusy(false);
    }
  };
  return (
    <Show when={coarse() && p.cameraOn()}>
      <button
        class="hit ctl-square"
        classList={{ loading: busy() }}
        disabled={busy()}
        title="翻转摄像头"
        aria-label="翻转摄像头"
        onClick={() => void run()}
      >
        {el(icon('reset', 17, 'currentColor'))}
        <span class="ctl-mobile-label">翻转</span>
      </button>
    </Show>
  );
};
