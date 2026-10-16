// ---- 外观 ----
import { getTheme, setTheme } from '../../theme';
import type { Theme } from '../../theme';
import { installMode, isStandalone, onInstallAvailable, promptInstall } from '../../install';
import { inShell } from '../../bridge';
import { esc, icon, toast } from '../../ui';

// 安装状态文案：已装（standalone）无需任何操作；prompt 模式给一个能直接弹系统对话框的按钮；
// 其余平台（iOS / macOS Safari / 其他浏览器）没有编程接口，只给操作说明。不受大厅推荐卡的
// 「不再提示」影响——这是用户自己点进设置找的入口。
function paintInstallState(el: HTMLElement) {
  if (isStandalone()) {
    el.innerHTML = `<div style="display:flex;align-items:center;gap:8px;font-size:12.5px;color:var(--text-2)">${icon('check', 15, 'var(--ember)', 1.8)}<span>已作为应用运行。</span></div>`;
    return;
  }
  const mode = installMode();
  const desc =
    mode === 'prompt'
      ? '像应用一样独立打开，带图标和角标，通知更可靠。'
      : mode === 'ios'
        ? 'Safari 里点底部「分享」→「添加到主屏幕」。装好后从主屏幕打开，才能收到通知。'
        : mode === 'mac-safari'
          ? 'Safari 菜单「文件」→「添加到程序坞」。'
          : '在浏览器菜单里找「安装」或「添加到主屏幕」。';
  el.innerHTML = `
    <div style="display:flex;align-items:center;gap:12px">
      <span style="flex-grow:1;min-width:0;font-size:12px;line-height:1.6;color:var(--text-2);text-wrap:pretty">${esc(desc)}</span>
      ${mode === 'prompt' ? '<button type="button" class="hit btn btn-primary btn-sm" id="settings-install-btn" style="flex-shrink:0">安装</button>' : ''}
    </div>`;
  el.querySelector('#settings-install-btn')?.addEventListener('click', async () => {
    const outcome = await promptInstall();
    if (outcome === 'accepted') toast('已安装到桌面。', 'ok');
    paintInstallState(el);
  });
}

export function renderAppearance(body: HTMLElement) {
  const paint = () => {
    const theme = getTheme();
    const hint =
      theme === 'auto' ? '跟着设备的深浅色设置走，日落自动换。' : theme === 'dark' ? '始终用深色，不跟随系统。' : '始终用浅色，不跟随系统。';
    const tab = (id: Theme, label: string, ic: string) => {
      const on = theme === id;
      return `<button class="hit" data-theme-pick="${id}" style="flex-grow:1;display:flex;align-items:center;justify-content:center;gap:7px;padding:9px 0;border-radius:8px;font-size:12.5px;font-weight:${on ? 600 : 500};color:${on ? 'var(--ember)' : 'var(--text-1)'};background:${on ? 'var(--ember-tint)' : 'transparent'}">${icon(ic, 15, on ? 'var(--ember)' : 'var(--text-1)', 1.6)}${label}</button>`;
    };
    body.innerHTML = `
      <div class="pane-col" style="max-width:560px">
        <div style="display:flex;padding:4px;border-radius:11px;background:var(--bg-1);border:1px solid var(--line);gap:3px">
          ${tab('light', '浅色', 'sun')}${tab('dark', '深色', 'moon')}${tab('auto', '跟随系统', 'autoTheme')}
        </div>
        <div style="font-size:12px;line-height:1.6;color:var(--text-2)">${hint}</div>
        ${
          // 桌面壳自己就是应用，没有可安装的东西，整张卡不出
          inShell()
            ? ''
            : `<div class="card">
          <div style="font-size:13.5px;font-weight:600">安装为应用</div>
          <div id="install-state" style="margin-top:10px"></div>
        </div>`
        }
      </div>`;
    body.querySelectorAll<HTMLButtonElement>('[data-theme-pick]').forEach((btn) => {
      btn.addEventListener('click', () => {
        setTheme(btn.dataset.themePick as Theme);
        paint();
      });
    });
    const installEl = body.querySelector<HTMLElement>('#install-state');
    if (installEl) {
      paintInstallState(installEl);
      // beforeinstallprompt 理论上可能在这块渲染之后才到——到达时状态从「说明文字」变成「装」按钮
      onInstallAvailable(() => paintInstallState(installEl));
    }
  };
  paint();
}
