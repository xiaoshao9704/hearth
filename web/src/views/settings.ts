// 设置（全屏接管浮层）：账户 / 外观 / 语音与视频 / 投屏画质 / 推流 / 我的设备。
// 以浮层形式挂在 body 上，房间内打开不会断开通话；偏好改动经 prefsBus 通知房间热应用。
import {
  clearSession,
  deleteMyDevice,
  deviceId,
  getIngress,
  getUser,
  listChannels,
  listMyDevices,
  logout,
  resetIngress,
  updatePassword,
  updateUsername,
} from '../api';
import {
  BR_LIMITS,
  FPS_BY_RES,
  autoBitrate,
  loadPrefs,
  notifyPrefsChanged,
  probeHwEncode,
  savePrefs,
} from '../prefs';
import type { DenoiseMode, ScreenCodec } from '../prefs';
import { getTheme, setTheme } from '../theme';
import type { Theme } from '../theme';
import { avatarHtml, copyText, esc, icon, pwBarsHtml, pwScore, slashIcon, timeAgo, toast } from '../ui';

type Pane = 'account' | 'appearance' | 'av' | 'screen' | 'stream' | 'devices';

const PANES: { id: Pane; label: string; icon: string; sub: string }[] = [
  { id: 'account', label: '账户', icon: 'user', sub: '用户名、密码与登录状态' },
  { id: 'appearance', label: '外观', icon: 'moon', sub: '浅色 / 深色 / 跟随系统' },
  { id: 'av', label: '语音与视频', icon: 'mic', sub: '输入输出设备与处理链' },
  { id: 'screen', label: '投屏画质', icon: 'screen', sub: '分辨率、帧率与码率联动' },
  { id: 'stream', label: '推流', icon: 'stream', sub: 'OBS 的 WHIP 地址与密钥' },
  { id: 'devices', label: '我的设备', icon: 'device', sub: '同账号在线的设备' },
];

let overlay: HTMLDivElement | null = null;
let cleanupPane: (() => void) | null = null;

export function openSettings(pane: Pane = 'account', context?: { backLabel?: string }) {
  if (overlay) closeSettings();
  overlay = document.createElement('div');
  overlay.className = 'settings-overlay';
  document.body.appendChild(overlay);
  const backLabel = context?.backLabel ?? '返回';
  renderFrame(overlay, pane, backLabel);
}

export function closeSettings() {
  cleanupPane?.();
  cleanupPane = null;
  overlay?.remove();
  overlay = null;
}

function renderFrame(root: HTMLDivElement, pane: Pane, backLabel: string) {
  const meta = PANES.find((p) => p.id === pane)!;
  root.innerHTML = `
    <div class="settings-nav">
      <div class="nav-head">设置</div>
      <div class="nav-body">
        ${PANES.map(
          (p) => `
          <button class="hit nav-row ${p.id === pane ? 'on' : ''}" data-pane="${p.id}">
            ${icon(p.icon, 16, p.id === pane ? 'var(--ember)' : 'var(--text-2)', 1.6)}
            <span class="label" style="flex-grow:1;text-align:left">${p.label}</span>
          </button>`,
        ).join('')}
      </div>
      <div class="nav-foot" style="display:flex;flex-direction:column;gap:8px">
        ${
          getUser()?.is_admin
            ? `<button class="hit back-row" id="settings-admin" style="width:100%;border-color:var(--ember-line);background:var(--ember-weak)">
                 ${icon('shield', 15, 'var(--ember)', 1.6)}
                 <span class="back-label" style="flex-grow:1;text-align:left;color:var(--ember)">管理后台</span>
               </button>`
            : ''
        }
        <button class="hit back-row" id="settings-back" style="width:100%">
          ${icon('back', 15, 'var(--text-1)')}
          <span class="back-label" style="flex-grow:1;text-align:left">${esc(backLabel)}</span>
        </button>
      </div>
    </div>
    <div class="settings-main">
      <header class="topbar">
        <h1>${meta.label}</h1>
        <span class="sub">${meta.sub}</span>
        <div class="spacer"></div>
        <button class="hit btn btn-icon" id="settings-close">${icon('close', 16, 'var(--text-1)', 1.8)}</button>
      </header>
      <div class="settings-body" id="pane-body"></div>
    </div>
  `;

  root.querySelectorAll<HTMLButtonElement>('[data-pane]').forEach((btn) => {
    btn.addEventListener('click', () => {
      cleanupPane?.();
      cleanupPane = null;
      renderFrame(root, btn.dataset.pane as Pane, backLabel);
    });
  });
  root.querySelector('#settings-back')!.addEventListener('click', closeSettings);
  root.querySelector('#settings-close')!.addEventListener('click', closeSettings);
  root.querySelector('#settings-admin')?.addEventListener('click', () => {
    closeSettings();
    location.hash = '#/admin';
  });

  const body = root.querySelector<HTMLDivElement>('#pane-body')!;
  switch (pane) {
    case 'account':
      renderAccount(body);
      break;
    case 'appearance':
      renderAppearance(body);
      break;
    case 'av':
      renderAV(body);
      break;
    case 'screen':
      renderScreen(body, () => renderFrame(root, 'stream', backLabel));
      break;
    case 'stream':
      renderStream(body);
      break;
    case 'devices':
      renderDevices(body);
      break;
  }
}

// ---- 账户 ----

function renderAccount(body: HTMLElement) {
  const user = getUser();
  body.innerHTML = `
    <div class="pane-col pane-narrow">
      <div class="card">
        <div style="display:flex;align-items:center;gap:14px">
          ${avatarHtml(user?.username ?? '?', 'avatar avatar-lg')}
          <div style="flex-grow:1;min-width:0">
            <div style="font-size:14px;font-weight:600" id="acc-name">${esc(user?.username ?? '')}</div>
            <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:3px">user_id · ${user?.id ?? '?'}</div>
          </div>
          ${user?.is_admin ? '<span class="tag tag-ember" style="font-size:10.5px;padding:4px 9px">管理员</span>' : ''}
        </div>
        <div style="margin-top:13px;padding-top:13px;border-top:1px solid var(--line-soft);font-size:11.5px;line-height:1.65;color:var(--text-2);text-wrap:pretty">系统内部一律按 <span class="mono" style="color:var(--text-1)">user_id</span> 认人：改用户名不会动它，历史消息、设备档案、每个频道的推流 key 都还挂在同一个 id 上。</div>
      </div>

      <div class="card">
        <div style="font-size:13.5px;font-weight:600">用户名</div>
        <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">别人在频道里看到的名字，也是你登录时用的</div>
        <div style="display:flex;align-items:center;gap:10px;margin-top:13px">
          <div class="field" style="flex-grow:1;height:42px"><input id="name-input" value="${esc(user?.username ?? '')}" /></div>
          <button class="hit btn btn-primary disabled" id="name-save" style="height:42px;padding:0 18px">保存</button>
        </div>
        <div id="name-hint" style="margin-top:9px;font-size:11.5px;color:var(--text-2)">和当前用户名相同</div>
      </div>

      <div class="card">
        <div style="font-size:13.5px;font-weight:600">修改密码</div>
        <div style="font-size:11.5px;color:var(--text-2);margin-top:4px">改完其他设备上的会话会全部退出，需要重新登录</div>
        <div style="display:flex;flex-direction:column;gap:11px;margin-top:13px">
          <div>
            <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">当前密码</div>
            <div class="field" style="height:42px;background:var(--bg-2)"><input id="pw-cur" type="password" placeholder="验证是你本人" autocomplete="current-password" /></div>
          </div>
          <div style="display:flex;gap:11px;flex-wrap:wrap">
            <div style="flex-grow:1;min-width:180px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">新密码</div>
              <div class="field" style="height:42px;background:var(--bg-2)"><input id="pw-new" type="password" placeholder="至少 8 位" autocomplete="new-password" /></div>
            </div>
            <div style="flex-grow:1;min-width:180px">
              <div style="font-size:11px;color:var(--text-2);margin-bottom:6px">确认新密码</div>
              <div class="field" style="height:42px;background:var(--bg-2)"><input id="pw-conf" type="password" placeholder="再输一次" autocomplete="new-password" /></div>
            </div>
          </div>
          <div class="pw-bars" id="acc-pw-bars">${pwBarsHtml(0)}</div>
          <div style="display:flex;align-items:center;gap:12px">
            <div id="pw-hint" style="font-size:11.5px;color:var(--text-2)"></div>
            <div class="spacer"></div>
            <button class="hit btn btn-primary disabled" id="pw-save">修改密码</button>
          </div>
        </div>
      </div>

      <button class="hit card" id="acc-logout" style="display:flex;align-items:center;gap:10px;padding:14px 18px;border-color:var(--red-line);text-align:left;width:100%">
        ${icon('leave', 16, 'var(--red)')}
        <div style="flex-grow:1">
          <div style="font-size:13px;font-weight:600;color:var(--red-text)">退出登录</div>
          <div style="font-size:11.5px;color:var(--text-2);margin-top:3px">只退这台设备，其他设备不受影响</div>
        </div>
      </button>
    </div>
  `;

  const nameInput = body.querySelector<HTMLInputElement>('#name-input')!;
  const nameSave = body.querySelector<HTMLButtonElement>('#name-save')!;
  const nameHint = body.querySelector<HTMLDivElement>('#name-hint')!;
  const NAME_RE = /^[a-zA-Z0-9_-]{2,32}$/;

  function syncName() {
    const v = nameInput.value.trim();
    const valid = NAME_RE.test(v);
    const changed = v !== (getUser()?.username ?? '');
    nameSave.classList.toggle('disabled', !valid || !changed);
    nameHint.textContent = !valid ? '用户名需 2–32 位字母数字 - _' : changed ? '改完别人看到的名字会立刻变' : '和当前用户名相同';
    nameHint.style.color = valid ? 'var(--text-2)' : 'var(--red-text)';
  }
  nameInput.addEventListener('input', syncName);
  nameSave.addEventListener('click', async () => {
    const v = nameInput.value.trim();
    if (nameSave.classList.contains('disabled')) return;
    try {
      const u = await updateUsername(v);
      body.querySelector('#acc-name')!.textContent = u.username;
      toast(`用户名已改成「${u.username}」，user_id 没变。`, 'ok');
      syncName();
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  });

  const pwCur = body.querySelector<HTMLInputElement>('#pw-cur')!;
  const pwNew = body.querySelector<HTMLInputElement>('#pw-new')!;
  const pwConf = body.querySelector<HTMLInputElement>('#pw-conf')!;
  const pwSave = body.querySelector<HTMLButtonElement>('#pw-save')!;
  const pwHint = body.querySelector<HTMLDivElement>('#pw-hint')!;

  function syncPw() {
    body.querySelector('#acc-pw-bars')!.innerHTML = pwBarsHtml(pwScore(pwNew.value));
    const ready = pwCur.value.length > 0 && pwNew.value.length >= 8 && pwNew.value === pwConf.value;
    let hint = '';
    let tone = 'var(--text-2)';
    if (pwNew.value && pwNew.value.length < 8) {
      hint = `新密码还差 ${8 - pwNew.value.length} 位`;
      tone = 'var(--red-text)';
    } else if (pwConf.value && pwNew.value !== pwConf.value) {
      hint = '两次输入不一样';
      tone = 'var(--red-text)';
    } else if (pwNew.value && !pwCur.value) {
      hint = '还要填当前密码';
    } else if (ready) {
      hint = '可以改了';
      tone = 'var(--sage)';
    }
    pwHint.textContent = hint;
    pwHint.style.color = tone;
    pwSave.classList.toggle('disabled', !ready);
  }
  [pwCur, pwNew, pwConf].forEach((el) => el.addEventListener('input', syncPw));
  pwSave.addEventListener('click', async () => {
    if (pwSave.classList.contains('disabled')) return;
    try {
      await updatePassword(pwCur.value, pwNew.value);
      pwCur.value = pwNew.value = pwConf.value = '';
      syncPw();
      toast('密码已更新，其他设备上的会话已全部退出。', 'ok');
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
  });

  body.querySelector('#acc-logout')!.addEventListener('click', async () => {
    try {
      await logout();
    } catch {
      clearSession();
    }
    closeSettings();
    location.hash = '#/login';
  });
}

// ---- 外观 ----

function renderAppearance(body: HTMLElement) {
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
      </div>`;
    body.querySelectorAll<HTMLButtonElement>('[data-theme-pick]').forEach((btn) => {
      btn.addEventListener('click', () => {
        setTheme(btn.dataset.themePick as Theme);
        paint();
      });
    });
  };
  paint();
}

// ---- 语音与视频 ----

interface MediaDeviceOpt {
  id: string;
  name: string;
  meta: string;
}

async function enumerate(kind: MediaDeviceKind): Promise<MediaDeviceOpt[]> {
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

function renderAV(body: HTMLElement) {
  const prefs = loadPrefs();
  let openPicker = '';
  let micStream: MediaStream | null = null;
  let camStream: MediaStream | null = null;
  let analyser: AnalyserNode | null = null;
  let audioCtx: AudioContext | null = null;
  let rafId = 0;
  let devices: Record<string, MediaDeviceOpt[]> = { mic: [], out: [], cam: [] };

  body.innerHTML = `
    <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:22px">
      <div style="display:flex;flex-direction:column;gap:16px">
        <div style="display:flex;flex-direction:column;gap:9px">
          <div class="section-label">麦克风</div>
          <div class="picker" id="picker-mic"></div>
          <div style="display:flex;align-items:center;gap:12px">
            <span style="font-size:11.5px;color:var(--text-2);width:46px">电平</span>
            <div class="level-bars" id="level-bars">${'<div></div>'.repeat(12)}</div>
          </div>
        </div>
        <div style="display:flex;flex-direction:column;gap:9px">
          <div class="section-label">扬声器</div>
          <div class="picker" id="picker-out"></div>
          <div style="display:flex;align-items:center;gap:12px">
            <span style="font-size:11.5px;color:var(--text-2);width:46px">音量</span>
            <input class="range" type="range" min="0" max="100" step="1" id="vol-range" value="${prefs.volume}" />
            <span class="mono" style="font-size:11.5px;color:var(--text-1);width:28px;text-align:right" id="vol-label">${prefs.volume}</span>
          </div>
        </div>
        <div class="opt-list" id="audio-chain"></div>
      </div>
      <div style="display:flex;flex-direction:column;gap:9px">
        <div class="section-label">摄像头</div>
        <div class="picker" id="picker-cam"></div>
        <div class="cam-preview" id="cam-preview">
          <div class="cam-off">${slashIcon('camera', 28, true, 'var(--text-3)')}<span>摄像头未开启</span></div>
        </div>
        <button class="hit card" id="mirror-row" style="display:flex;align-items:center;gap:12px;padding:13px 15px;width:100%;text-align:left">
          <div style="flex-grow:1">
            <div style="font-size:13px;font-weight:500">镜像预览</div>
            <div style="font-size:11px;color:var(--text-2);margin-top:3px">只影响你自己看到的画面</div>
          </div>
          <div class="switch ${prefs.mirror ? 'on' : ''}" id="mirror-switch"><div class="knob"></div></div>
        </button>
        <div id="music-note"></div>
      </div>
    </div>
  `;

  const pickers: Record<string, { el: HTMLElement; icon: string; get: () => string; set: (id: string) => void }> = {
    mic: {
      el: body.querySelector('#picker-mic')!,
      icon: 'mic',
      get: () => prefs.micDeviceId,
      set: (id) => {
        prefs.micDeviceId = id;
        save('mic-device');
        void startMeter();
      },
    },
    out: {
      el: body.querySelector('#picker-out')!,
      icon: 'speaker',
      get: () => prefs.speakerId,
      set: (id) => {
        prefs.speakerId = id;
        save('speaker');
      },
    },
    cam: {
      el: body.querySelector('#picker-cam')!,
      icon: 'camera',
      get: () => prefs.camDeviceId,
      set: (id) => {
        prefs.camDeviceId = id;
        save('cam-device');
        void startCamPreview();
      },
    },
  };

  function save(what: string) {
    savePrefs(prefs);
    notifyPrefsChanged(what);
  }

  function paintPicker(kind: 'mic' | 'out' | 'cam') {
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
      openPicker = open ? '' : kind;
      paintAllPickers();
    });
    p.el.querySelectorAll<HTMLButtonElement>('[data-pick]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const [k, ...rest] = btn.dataset.pick!.split(':');
        openPicker = '';
        pickers[k].set(rest.join(':'));
        paintAllPickers();
      });
    });
  }
  const paintAllPickers = () => {
    paintPicker('mic');
    paintPicker('out');
    paintPicker('cam');
  };

  async function refreshDevices() {
    const rawOut = await enumerate('audiooutput');
    // Windows 上 Chrome 会暴露 communications 虚拟设备——游戏声卡的聊天/通讯通道走它
    const hasComms = rawOut.some((d) => d.id === 'communications');
    devices = {
      mic: await enumerate('audioinput'),
      out: [
        { id: '', name: '系统默认设备', meta: '跟随系统默认输出' },
        ...(hasComms ? [{ id: 'communications', name: '默认通话设备', meta: '通讯通道（游戏声卡分离输出）' }] : []),
        ...rawOut.filter((d) => d.id !== 'default' && d.id !== 'communications'),
      ],
      cam: await enumerate('videoinput'),
    };
    paintAllPickers();
  }

  // 电平表：拉一路本地麦克风流做可视化
  async function startMeter() {
    stopMeter();
    try {
      micStream = await navigator.mediaDevices.getUserMedia({
        audio: prefs.micDeviceId ? { deviceId: { ideal: prefs.micDeviceId } } : true,
      });
      audioCtx = new AudioContext();
      void audioCtx.resume();
      const src = audioCtx.createMediaStreamSource(micStream);
      analyser = audioCtx.createAnalyser();
      analyser.fftSize = 512;
      src.connect(analyser);
      const buf = new Uint8Array(analyser.fftSize);
      const bars = body.querySelectorAll<HTMLDivElement>('#level-bars > div');
      const tick = () => {
        if (!analyser) return;
        analyser.getByteTimeDomainData(buf);
        let peak = 0;
        for (let i = 0; i < buf.length; i++) peak = Math.max(peak, Math.abs(buf[i] - 128) / 128);
        const lit = Math.min(12, Math.round(peak * 18));
        bars.forEach((b, i) => {
          b.className = i < lit ? (i > 9 ? 'lit hot' : 'lit') : '';
        });
        rafId = requestAnimationFrame(tick);
      };
      tick();
      await refreshDevices(); // 授权后能拿到设备名
    } catch {
      // 未授权时电平表保持熄灭
    }
  }
  function stopMeter() {
    cancelAnimationFrame(rafId);
    analyser = null;
    micStream?.getTracks().forEach((t) => t.stop());
    micStream = null;
    void audioCtx?.close();
    audioCtx = null;
  }

  async function startCamPreview() {
    stopCam();
    const box = body.querySelector<HTMLDivElement>('#cam-preview')!;
    try {
      camStream = await navigator.mediaDevices.getUserMedia({
        video: prefs.camDeviceId ? { deviceId: { ideal: prefs.camDeviceId } } : true,
      });
      const video = document.createElement('video');
      video.autoplay = true;
      video.muted = true;
      video.playsInline = true;
      video.srcObject = camStream;
      video.className = prefs.mirror ? 'mirror' : '';
      box.innerHTML = '';
      box.appendChild(video);
      await refreshDevices();
    } catch {
      box.innerHTML = `<div class="cam-off">${slashIcon('camera', 28, true, 'var(--text-3)')}<span>摄像头不可用或未授权</span></div>`;
    }
  }
  function stopCam() {
    camStream?.getTracks().forEach((t) => t.stop());
    camStream = null;
  }

  // 音频处理链：降噪三选一 + 回声消除/自动增益/音乐模式
  function paintChain() {
    const music = prefs.musicMode;
    const denoiseOpts: { id: DenoiseMode; title: string; tag: string; desc: string }[] = [
      { id: 'rnnoise', title: 'RNNoise 降噪', tag: 'AI', desc: '神经网络降噪，键盘声 / 风扇声压得最狠' },
      { id: 'browser', title: '浏览器自带', tag: '', desc: 'WebRTC 内置，省 CPU，效果一般' },
      { id: 'off', title: '不降噪', tag: '', desc: '原始音频上行' },
    ];
    const switches = [
      { key: 'echoCancellation' as const, title: '回声消除', desc: '外放时必开' },
      { key: 'autoGainControl' as const, title: '自动增益', desc: '离麦远近自动补齐音量' },
    ];
    body.querySelector('#audio-chain')!.innerHTML = `
      <div class="opt-head">降噪 · 三选一</div>
      ${denoiseOpts
        .map((d) => {
          const on = !music && prefs.denoise === d.id;
          return `
        <button class="hit opt-row ${music ? 'dim' : ''}" data-denoise="${d.id}" style="width:100%;text-align:left">
          <div class="radio ${on ? 'on' : ''}"><div class="dot"></div></div>
          <div style="flex-grow:1;min-width:0">
            <div style="display:flex;align-items:center;gap:7px">
              <span class="o-title ${on ? 'on' : ''}">${d.title}</span>
              ${d.tag ? '<span class="tag tag-ember" style="font-weight:700">AI</span>' : ''}
            </div>
            <div class="o-desc">${music ? '音乐模式已接管' : d.desc}</div>
          </div>
        </button>`;
        })
        .join('')}
      ${switches
        .map((s) => {
          const eff = music ? false : prefs[s.key];
          return `
        <button class="hit switch-row ${music ? 'dim' : ''}" data-flip="${s.key}" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">${s.title}</div>
            <div class="s-desc">${music ? '已由音乐模式接管' : s.desc}</div>
          </div>
          <div class="switch ${eff ? 'on' : ''}"><div class="knob"></div></div>
        </button>`;
        })
        .join('')}
      <button class="hit switch-row" data-flip="musicMode" style="width:100%;text-align:left;background:var(--bg-3)">
        <div style="flex-grow:1">
          <div class="s-title">音乐模式</div>
          <div class="s-desc">旁路全部处理，保留立体声与动态（语音码率提到 128k）</div>
        </div>
        <div class="switch ${music ? 'on' : ''}"><div class="knob"></div></div>
      </button>`;

    body.querySelectorAll<HTMLButtonElement>('[data-denoise]').forEach((btn) => {
      btn.addEventListener('click', () => {
        if (prefs.musicMode) return;
        prefs.denoise = btn.dataset.denoise as DenoiseMode;
        save('audio-chain');
        paintChain();
      });
    });
    body.querySelectorAll<HTMLButtonElement>('[data-flip]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const key = btn.dataset.flip as 'echoCancellation' | 'autoGainControl' | 'musicMode';
        if (key !== 'musicMode' && prefs.musicMode) return;
        prefs[key] = !prefs[key];
        if (key === 'musicMode') prefs.voiceBitrate = prefs.musicMode ? 128000 : 64000;
        save('audio-chain');
        paintChain();
        paintMusicNote();
      });
    });
  }

  function paintMusicNote() {
    const on = prefs.musicMode;
    body.querySelector('#music-note')!.innerHTML = `
      <div style="display:flex;gap:9px;padding:11px 13px;border-radius:9px;border:1px solid ${on ? 'var(--ember-line)' : 'var(--ember-line)'};background:${on ? 'var(--ember-tint)' : 'var(--ember-weak)'}">
        ${icon('info', 15, 'var(--ember)')}
        <div style="font-size:11.5px;line-height:1.6;color:var(--text-1);text-wrap:pretty">${
          on ? '音乐模式已开：降噪、回声消除、增益全部旁路。' : '开启音乐模式后，左边的降噪与增益会一并停用——独奏 / 放歌时才建议开。'
        }</div>
      </div>`;
  }

  const volRange = body.querySelector<HTMLInputElement>('#vol-range')!;
  volRange.addEventListener('input', () => {
    prefs.volume = Number(volRange.value);
    body.querySelector('#vol-label')!.textContent = volRange.value;
    save('volume');
  });

  const mirrorSwitch = body.querySelector<HTMLDivElement>('#mirror-switch')!;
  body.querySelector('#mirror-row')!.addEventListener('click', () => {
    prefs.mirror = !prefs.mirror;
    mirrorSwitch.classList.toggle('on', prefs.mirror);
    body.querySelector('#cam-preview video')?.classList.toggle('mirror', prefs.mirror);
    save('mirror');
  });

  paintAllPickers();
  paintChain();
  paintMusicNote();
  void refreshDevices();
  void startMeter();
  void startCamPreview();

  cleanupPane = () => {
    stopMeter();
    stopCam();
  };
}

// ---- 投屏画质 ----

function renderScreen(body: HTMLElement, goStream: () => void) {
  const prefs = loadPrefs();

  const paint = () => {
    const lim = BR_LIMITS[prefs.res];
    const fpsAllowed = FPS_BY_RES[prefs.res] ?? [15, 30, 60];
    body.innerHTML = `
      <div class="pane-col pane-narrow">
        <div class="kv-line">
          <span class="k">分辨率</span>
          <div class="seg-group" style="flex-grow:1">
            ${['720p', '1080p', '1440p', '4K']
              .map((r) => {
                const enabled = r === '720p' || r === '1080p';
                return `<button class="hit seg ${prefs.res === r ? 'on' : ''} ${enabled ? '' : 'off'}" data-res="${r}">${r}</button>`;
              })
              .join('')}
          </div>
        </div>
        <div class="kv-line">
          <span class="k">帧率</span>
          <div class="seg-group" style="flex-grow:1">
            ${[15, 30, 60, 120]
              .map(
                (f) =>
                  `<button class="hit seg ${prefs.fps === f ? 'on' : ''} ${fpsAllowed.includes(f) ? '' : 'off'}" data-fps="${f}">${f}</button>`,
              )
              .join('')}
          </div>
        </div>
        <div class="kv-line">
          <span class="k">编码</span>
          <div class="seg-group" style="flex-grow:1">
            ${([
              ['vp9', 'VP9 · SVC'],
              ['av1', 'AV1 · SVC'],
              ['h264', 'H.264 单层'],
            ] as const)
              .map(([v, label]) => `<button class="hit seg ${prefs.screenCodec === v ? 'on' : ''}" data-codec="${v}">${label}</button>`)
              .join('')}
          </div>
        </div>
        <div class="kv-line">
          <span class="k">码率</span>
          <input class="range" type="range" min="${lim.min}" max="${lim.max}" step="0.5" value="${prefs.bitrate}" id="br-range" />
          <span class="mono" style="font-size:11.5px;color:var(--text-1);width:70px;text-align:right" id="br-label">${prefs.bitrate.toFixed(1)} Mbps</span>
        </div>
        <div class="mono" style="padding-left:66px;font-size:10.5px;color:var(--text-3);margin-top:-8px">${prefs.res} · ${prefs.fps}fps 建议 ${lim.min}–${lim.max} Mbps${prefs.bitrateAuto ? '（当前为自动推荐值）' : ''}</div>
        <div class="hint-card">
          ${icon('cube', 15, 'var(--text-2)')}
          <div>VP9/AV1 走 SVC 分层：弱网观众自动降到低分辨率层，不拖累全场，也让家宽上行的观众数上限变成软性劣化；AV1 压缩率最高但软编极吃 CPU（实验）。H.264 单层兼容性最好。浏览器软编到 1080p60 为止——再往上是编码器的物理上限。<button class="hit" id="go-stream" style="color:var(--ember)">2K / 4K / 120fps 走 OBS 推流 →</button></div>
        </div>
      </div>`;

    body.querySelectorAll<HTMLButtonElement>('[data-res]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const r = btn.dataset.res!;
        if (r !== '720p' && r !== '1080p') return;
        prefs.res = r;
        prefs.bitrate = autoBitrate(r, prefs.fps);
        prefs.bitrateAuto = true;
        savePrefs(prefs);
        notifyPrefsChanged('screen');
        paint();
      });
    });
    // 按当前分辨率/帧率问浏览器：各编码档走不走硬件（MediaCapabilities 事前预测）
    (['vp9', 'av1', 'h264'] as ScreenCodec[]).forEach(async (c) => {
      const hw = await probeHwEncode(c);
      const btn = body.querySelector<HTMLButtonElement>(`[data-codec="${c}"]`);
      if (btn && hw !== null && !btn.querySelector('.enc-tag')) {
        btn.insertAdjacentHTML('beforeend', `<span class="enc-tag ${hw ? 'hw' : ''}">${hw ? '硬编' : '软编'}</span>`);
      }
    });
    body.querySelectorAll<HTMLButtonElement>('[data-codec]').forEach((btn) => {
      btn.addEventListener('click', () => {
        prefs.screenCodec = btn.dataset.codec as ScreenCodec;
        savePrefs(prefs);
        notifyPrefsChanged('screen');
        paint();
      });
    });
    body.querySelectorAll<HTMLButtonElement>('[data-fps]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const f = Number(btn.dataset.fps);
        if (!(FPS_BY_RES[prefs.res] ?? []).includes(f)) return;
        prefs.fps = f;
        prefs.bitrate = autoBitrate(prefs.res, f);
        prefs.bitrateAuto = true;
        savePrefs(prefs);
        notifyPrefsChanged('screen');
        paint();
      });
    });
    const brRange = body.querySelector<HTMLInputElement>('#br-range')!;
    brRange.addEventListener('input', () => {
      prefs.bitrate = parseFloat(brRange.value);
      prefs.bitrateAuto = false;
      savePrefs(prefs);
      notifyPrefsChanged('screen');
      body.querySelector('#br-label')!.textContent = `${prefs.bitrate.toFixed(1)} Mbps`;
    });
    body.querySelector('#go-stream')!.addEventListener('click', goStream);
  };
  paint();
}

// ---- 推流 ----

function renderStream(body: HTMLElement) {
  let channels: string[] = [];
  let current = '';
  let url = '';
  let key = '';
  let reveal = false;
  let confirming = false;
  let notice = '';

  body.innerHTML = '<div class="muted">加载频道…</div>';

  const paint = () => {
    if (channels.length === 0) {
      body.innerHTML = '<div class="pane-col pane-wide"><div class="hint-card">还没有频道。先在大厅建一个，推流地址是「每人每频道」一把 key。</div></div>';
      return;
    }
    const base = url ? url.slice(0, url.lastIndexOf('/') + 1) : '获取中…';
    const masked = key ? (reveal ? key : `${'•'.repeat(Math.max(0, key.length - 4))}${key.slice(-4)}`) : '获取中…';
    body.innerHTML = `
      <div class="pane-col pane-wide">
        <div style="display:flex;align-items:center;gap:14px;flex-wrap:wrap">
          <div style="font-size:12.5px;color:var(--text-1)">频道</div>
          <div class="seg-group">
            ${channels.map((c) => `<button class="hit seg ${c === current ? 'on' : ''}" data-ch="${esc(c)}">${esc(c)}</button>`).join('')}
          </div>
        </div>
        <div style="display:flex;flex-direction:column;gap:7px">
          <div class="section-label" style="letter-spacing:0.1em">服务器地址</div>
          <div class="copy-line">
            <span class="val mono">${esc(base)}</span>
            <button class="hit btn btn-sm" data-copy="url">${icon('copy', 13)} 复制</button>
          </div>
        </div>
        <div style="display:flex;flex-direction:column;gap:7px">
          <div class="section-label" style="letter-spacing:0.1em">推流密钥 · ${esc(current)}</div>
          <div class="copy-line">
            <span class="val mono">${esc(masked)}</span>
            <button class="hit mini-btn" data-act="reveal" style="width:30px;height:30px;border-radius:7px;display:flex;align-items:center;justify-content:center">${icon(reveal ? 'eye' : 'eyeOff', 15, reveal ? 'var(--ember)' : 'var(--text-2)', 1.6)}</button>
            <button class="hit btn btn-sm" data-copy="key">${icon('copy', 13)} 复制</button>
            <button class="hit btn btn-sm btn-danger" data-act="ask-reset">${icon('reset', 13, 'var(--red)')} 重置</button>
          </div>
        </div>
        ${
          confirming
            ? `<div class="notice-bad" style="gap:12px">
                 <span style="flex-grow:1;font-size:12px;line-height:1.55">重置「${esc(current)}」的 key？旧 key 立即失效，OBS 里要重新填一次。</span>
                 <button class="hit btn btn-sm" data-act="cancel-reset">取消</button>
                 <button class="hit btn btn-sm btn-danger-solid" data-act="do-reset">确认重置</button>
               </div>`
            : ''
        }
        ${notice ? `<div class="notice-ok">${icon('check', 15, 'var(--sage)', 1.8)}<span>${esc(notice)}</span></div>` : ''}
        <div class="hint-card" style="border-color:var(--line-soft)">
          ${icon('check', 16, 'var(--sage)')}
          <div style="display:flex;flex-direction:column;gap:6px">
            <div style="font-size:12.5px;font-weight:600;color:var(--text-0)">OBS 里怎么填</div>
            <div style="font-size:12px;line-height:1.7">设置 → 直播 → 服务选 <span class="mono" style="color:var(--text-1)">WHIP</span>，服务器填上面那行，Bearer Token 填密钥。编码器用硬编，规格随便挑——服务端不转码，<span style="color:var(--sage)">2K / 4K / 120fps 原样透传</span>。</div>
          </div>
        </div>
      </div>`;

    body.querySelectorAll<HTMLButtonElement>('[data-ch]').forEach((btn) => {
      btn.addEventListener('click', () => {
        current = btn.dataset.ch!;
        reveal = false;
        confirming = false;
        notice = '';
        url = key = '';
        paint();
        void load(false);
      });
    });
    body.querySelectorAll<HTMLButtonElement>('[data-copy]').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const text = btn.dataset.copy === 'url' ? url.slice(0, url.lastIndexOf('/') + 1) : key;
        if (!text) return;
        if (await copyText(text)) toast('已复制', 'ok', 1400);
      });
    });
    body.querySelector('[data-act="reveal"]')?.addEventListener('click', () => {
      reveal = !reveal;
      paint();
    });
    body.querySelector('[data-act="ask-reset"]')?.addEventListener('click', () => {
      confirming = true;
      notice = '';
      paint();
    });
    body.querySelector('[data-act="cancel-reset"]')?.addEventListener('click', () => {
      confirming = false;
      paint();
    });
    body.querySelector('[data-act="do-reset"]')?.addEventListener('click', () => {
      confirming = false;
      void load(true);
    });
  };

  async function load(reset: boolean) {
    try {
      const info = reset ? await resetIngress(current) : await getIngress(current);
      url = info.url;
      key = info.stream_key;
      if (reset) {
        reveal = false;
        notice = '已生成新的 key，旧 key 已失效。';
        setTimeout(() => {
          notice = '';
          paint();
        }, 2600);
      }
    } catch (err) {
      toast((err as Error).message, 'bad');
    }
    paint();
  }

  void (async () => {
    try {
      channels = (await listChannels()).map((c) => c.name);
    } catch {
      channels = [];
    }
    current = channels[0] ?? '';
    paint();
    if (current) void load(false);
  })();
}

// ---- 我的设备 ----

function renderDevices(body: HTMLElement) {
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
                <div style="display:flex;align-items:center;gap:8px">
                  <span style="font-size:13.5px;font-weight:500">${esc(d.tag || '未知设备')}</span>
                  ${isThis ? '<span class="tag tag-sage">本机</span>' : ''}
                </div>
                <div class="mono" style="font-size:11px;color:var(--text-2);margin-top:3px">dev_${esc(d.device_id)} · ${timeAgo(d.last_seen)}</div>
              </div>
              ${isThis ? '<span style="font-size:12px;color:var(--text-3)">当前设备</span>' : `<button class="hit btn btn-sm" data-del="${esc(d.device_id)}">移除档案</button>`}
            </div>`;
                  })
                  .join('')
          }
        </div>
        <div class="hint-card">${icon('info', 15, 'var(--text-2)')}<span>这里是设备档案（进房时记录），不是登录会话。移除档案不会把设备踢下线；要让别的设备退出登录，改一次密码即可。</span></div>
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
