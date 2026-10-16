// 设置浮层的七个「个人」pane：账户 / 外观 / 语音与视频 / 投屏画质 / 推流 / 我的设备 / 邀请。
// 仍是命令式渲染（innerHTML + 事件绑定），由 settings.tsx 的骨架按需挂进容器；
// 有后台资源的 pane（语音与视频的电平表/预览）返回清理函数，切页时由骨架调用。
// 「邀请」只对 power 及以上出现（骨架按 getUser().role 过滤导航，这里不再自查）。
import { renderAccount } from './account';
import { renderAppearance } from './appearance';
import { renderAV } from './av';
import { renderScreen } from './screen';
import { renderStream } from './stream';
import { renderDevices } from './devices';
import { renderInvites } from './invites';

export type PersonalPane = 'av' | 'screen' | 'stream' | 'devices' | 'invites' | 'account' | 'appearance';

export const PERSONAL_PANES: { id: PersonalPane; label: string; icon: string; sub: string }[] = [
  { id: 'av', label: '语音与视频', icon: 'mic', sub: '输入输出设备与处理链' },
  { id: 'screen', label: '投屏画质', icon: 'screen', sub: '分辨率、帧率与码率联动' },
  { id: 'stream', label: '推流', icon: 'stream', sub: 'OBS 的 WHIP 地址与令牌' },
  { id: 'devices', label: '我的设备', icon: 'device', sub: '同账号在线的设备' },
  { id: 'invites', label: '邀请', icon: 'mail', sub: '发有时效的注册链接' },
  { id: 'account', label: '账户', icon: 'user', sub: '用户名、密码与登录状态' },
  { id: 'appearance', label: '外观', icon: 'moon', sub: '浅色 / 深色 / 跟随系统' },
];

export interface PaneHost {
  close(): void; // 退出登录等需要关掉整个浮层
  go(pane: PersonalPane): void; // pane 间跳转（投屏画质 → 推流）
}

// 把 pane 渲染进 body，返回清理函数（没有后台资源的 pane 返回 undefined）
export function renderPane(body: HTMLElement, pane: PersonalPane, host: PaneHost): (() => void) | undefined {
  switch (pane) {
    case 'account':
      renderAccount(body, host.close);
      return;
    case 'appearance':
      renderAppearance(body);
      return;
    case 'av':
      return renderAV(body);
    case 'screen':
      renderScreen(body, () => host.go('stream'));
      return;
    case 'stream':
      renderStream(body);
      return;
    case 'devices':
      renderDevices(body);
      return;
    case 'invites':
      renderInvites(body);
      return;
  }
}

export { flushScreenBitrateRange } from './screen';
export { inviteState } from './invites';
