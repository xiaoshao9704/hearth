// ---- 我的设备 ----
import { deleteMyDevice, deviceId, listMyDevices } from '../../api';
import { esc, icon, timeAgo, toast } from '../../ui';

export function renderDevices(body: HTMLElement) {
  const myDevice = deviceId();
  body.innerHTML = '<div class="muted">加载设备档案…</div>';

  async function paint() {
    let devices;
    try {
      devices = await listMyDevices();
    } catch (err) {
      body.innerHTML = `<div class="error-text">${esc((err as Error).message)}</div>`;
      return;
    }
    body.innerHTML = `
      <div class="pane-col pane-wide">
        <div style="font-size:12.5px;color:var(--text-2)">同一账号可以在多台设备同时在线，设备档案落在服务端数据库里。</div>
        <div class="list-box">
          ${
            devices.length === 0
              ? '<div class="table-empty">还没有设备档案——进一次房间就会建档。</div>'
              : devices
                  .map((d) => {
                    const isThis = d.device_id === myDevice;
                    const isPhone = /iphone|ipad|android/.test(d.tag);
                    return `
            <div class="list-row">
              <div style="width:34px;height:34px;flex-shrink:0;border-radius:9px;display:flex;align-items:center;justify-content:center;background:${isThis ? 'var(--ember-tint)' : 'var(--bg-4)'}">
                ${icon(isPhone ? 'phone' : 'device', 17, isThis ? 'var(--ember)' : 'var(--text-1)', 1.6)}
              </div>
              <div style="flex-grow:1;min-width:0">
                <div style="display:flex;align-items:center;gap:8px;min-width:0">
                  <span style="font-size:13.5px;font-weight:500;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(d.tag || '未知设备')}</span>
                  ${isThis ? '<span class="tag tag-sage" style="flex-shrink:0">本机</span>' : ''}
                </div>
                <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:3px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">dev_${esc(d.device_id)} · ${timeAgo(d.last_seen)}</div>
              </div>
              ${isThis ? '<span style="font-size:12px;color:var(--text-3);flex-shrink:0">当前设备</span>' : `<button class="hit btn btn-sm" data-del="${esc(d.device_id)}" style="flex-shrink:0">移除档案</button>`}
            </div>`;
                  })
                  .join('')
          }
        </div>
        <div class="hint-card">${icon('info', 15, 'var(--text-2)')}<span>这里是设备档案（进房时记录），不是登录会话。移除档案不会把设备踢下线；要让别的设备退出登录，去「账户」里下线那条登录会话。</span></div>
      </div>`;
    body.querySelectorAll<HTMLButtonElement>('[data-del]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        try {
          await deleteMyDevice(btn.dataset.del!);
          await paint();
        } catch (err) {
          toast((err as Error).message, 'bad');
        }
      });
    });
  }
  void paint();
}
