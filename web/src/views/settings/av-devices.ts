// 设备枚举与选择器 / 摄像头预览（语音与视频 pane 的设备侧）
import { esc, icon, slashIcon } from '../../ui';

export interface MediaDeviceOpt {
  id: string;
  name: string;
  meta: string;
}

export async function enumerate(kind: MediaDeviceKind): Promise<MediaDeviceOpt[]> {
  try {
    const devs = await navigator.mediaDevices.enumerateDevices();
    return devs
      .filter((d) => d.kind === kind)
      .map((d, i) => ({
        id: d.deviceId,
        name: d.label || `${kind === 'audioinput' ? '麦克风' : kind === 'audiooutput' ? '扬声器' : '摄像头'} ${i + 1}`,
        meta: d.deviceId === 'default' ? '系统默认' : d.deviceId.slice(0, 8),
      }));
  } catch {
    return [];
  }
}

export async function loadAVDevices(): Promise<Record<'mic' | 'out' | 'cam', MediaDeviceOpt[]>> {
  const rawOut = await enumerate('audiooutput');
  // Windows 上 Chrome 会暴露 communications 虚拟设备——游戏声卡的聊天/通讯通道走它
  const hasComms = rawOut.some((d) => d.id === 'communications');
  return {
    mic: await enumerate('audioinput'),
    out: [
      { id: '', name: '系统默认设备', meta: '跟随系统默认输出' },
      ...(hasComms ? [{ id: 'communications', name: '默认通话设备', meta: '通讯通道（游戏声卡分离输出）' }] : []),
      ...rawOut.filter((d) => d.id !== 'default' && d.id !== 'communications'),
    ],
    cam: await enumerate('videoinput'),
  };
}

export type PickerKind = 'mic' | 'out' | 'cam';

export interface PickerBinding {
  el: HTMLElement;
  icon: string;
  get: () => string;
  set: (id: string) => void;
}

/** 设备下拉：渲染当前值与展开列表，点选回调走 binding.set。 */
export function paintDevicePicker(
  kind: PickerKind,
  pickers: Record<string, PickerBinding>,
  devices: Record<string, MediaDeviceOpt[]>,
  openPicker: string,
  setOpenPicker: (k: string) => void,
  paintAll: () => void,
) {
  const p = pickers[kind];
  const list = devices[kind];
  const cur = list.find((d) => d.id === p.get()) ?? list[0];
  const open = openPicker === kind;
  p.el.innerHTML = `
      <button class="hit picker-field ${open ? 'open' : ''}" data-toggle="${kind}" style="width:100%">
        ${icon(p.icon, 16, open ? 'var(--ember)' : 'var(--text-1)')}
        <span class="cur">${esc(cur?.name ?? '默认设备')}</span>
        ${icon(open ? 'chevUp' : 'chevDown', 15, 'var(--text-2)', 1.8)}
      </button>
      ${
        open
          ? `<div class="picker-drop">${
              list.length
                ? list
                    .map(
                      (d) => `
              <button class="hit picker-opt ${d.id === (cur?.id ?? '') ? 'on' : ''}" data-pick="${kind}:${esc(d.id)}" style="width:100%;text-align:left">
                <div style="flex-grow:1;min-width:0">
                  <div class="o-name">${esc(d.name)}</div>
                  <div class="o-meta mono">${esc(d.meta)}</div>
                </div>
                ${d.id === (cur?.id ?? '') ? icon('check', 15, 'var(--ember)', 2.2) : ''}
              </button>`,
                    )
                    .join('')
                : '<div class="picker-opt muted">未授权或没有设备（先点一次开麦/开摄像头授权）</div>'
            }</div>`
          : ''
      }`;
  p.el.querySelector(`[data-toggle="${kind}"]`)!.addEventListener('click', () => {
    setOpenPicker(open ? '' : kind);
    paintAll();
  });
  p.el.querySelectorAll<HTMLButtonElement>('[data-pick]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const [k, ...rest] = btn.dataset.pick!.split(':');
      setOpenPicker('');
      pickers[k].set(rest.join(':'));
      paintAll();
    });
  });
}

/** 摄像头本地预览：拉一路本地视频流挂到预览盒。 */
export function createCamPreview() {
  let camStream: MediaStream | null = null;

  async function start(deviceId: string, mirror: boolean, box: HTMLDivElement): Promise<boolean> {
    stop();
    try {
      camStream = await navigator.mediaDevices.getUserMedia({
        video: deviceId ? { deviceId: { ideal: deviceId } } : true,
      });
      const video = document.createElement('video');
      video.autoplay = true;
      video.muted = true;
      video.playsInline = true;
      video.srcObject = camStream;
      video.className = mirror ? 'mirror' : '';
      box.innerHTML = '';
      box.appendChild(video);
      return true;
    } catch {
      box.innerHTML = `<div class="cam-off">${slashIcon('camera', 28, true, 'var(--text-3)')}<span>摄像头不可用或未授权</span></div>`;
      return false;
    }
  }

  function stop() {
    camStream?.getTracks().forEach((t) => t.stop());
    camStream = null;
  }

  return { start, stop };
}
