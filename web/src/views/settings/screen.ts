// ---- 投屏画质 ----
import { screenRepublishHook } from '../../prefs';
import { renderScreenQuality } from '../screen-quality';
import { icon } from '../../ui';

// 码率范围（上限写编码参数、下限写会话描述）只在发布那一刻定死，拖动期间不重连：
// 这里只记脏，等设置浮层真正关闭时统一重开一次投屏，观众最多被打断一次。
// pane 之间跳转不经 closeSettings，因此不会误触发。
let bitrateRangeDirty = false;

/** 设置浮层关闭时调用：码率范围变过且正在投屏（房间视图登记了动作）才重开一次。 */
export function flushScreenBitrateRange() {
  if (!bitrateRangeDirty) return;
  bitrateRangeDirty = false;
  screenRepublishHook()?.();
}

// 控件本体与点「投屏」弹的浮窗共用（views/screen-quality.ts）：这里是完整版，
// 带说明与提示卡；浮窗那份是紧凑版。两处读写同一份 prefs。
export function renderScreen(body: HTMLElement, goStream: () => void) {
  body.innerHTML = `
    <div class="pane-col pane-narrow">
      <div class="sq-mount"></div>
      <div class="hint-card">
        ${icon('volume', 15, 'var(--text-2)')}
        <div>投屏带不带声音由浏览器自己的选择框决定：Chrome 只有共享「标签页」时才有那个勾选框，整屏与单个窗口没有。若共享后听到回音（自己这边传出去的语音又绕回来），把「语音与视频」里的扬声器切到与系统默认不同的输出设备，采集到的系统声音里就不再有别人的语音。</div>
      </div>
    </div>`;
  renderScreenQuality(body.querySelector<HTMLElement>('.sq-mount')!, {
    goStream,
    onBitrateRangeDirty: () => {
      bitrateRangeDirty = true;
    },
  });
}
