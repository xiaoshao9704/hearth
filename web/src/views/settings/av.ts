// ---- 语音与视频 ----
import { loadPrefs, notifyPrefsChanged, savePrefs } from '../../prefs';
import type { DenoiseMode } from '../../prefs';
import { armNotifyPermission, notifyState } from '../../notify';
import {
  setPushOptOut,
  state as pushState,
  subscribe as pushSubscribe,
  unsubscribe as pushUnsubscribe,
  unsupportedReason,
} from '../../push';
import { inShell } from '../../bridge';
import { icon, slashIcon, toast } from '../../ui';
import {
  createCamPreview,
  loadAVDevices,
  paintDevicePicker,
  type MediaDeviceOpt,
} from './av-devices';
import { createMicMeter } from './av-meter';

export function renderAV(body: HTMLElement): () => void {
  const prefs = loadPrefs();
  let openPicker = '';
  let devices: Record<string, MediaDeviceOpt[]> = { mic: [], out: [], cam: [] };
  const meter = createMicMeter();
  const cam = createCamPreview();

  body.innerHTML = `
    <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:22px">
      <div style="display:flex;flex-direction:column;gap:16px">
        <div style="display:flex;flex-direction:column;gap:9px">
          <div class="section-label">麦克风</div>
          <button class="hit card" id="auto-mic-row" style="display:flex;align-items:center;gap:12px;padding:13px 15px;width:100%;text-align:left">
            <div style="flex-grow:1">
              <div style="font-size:13px;font-weight:500">进房自动开麦</div>
              <div style="font-size:11px;color:var(--text-2);margin-top:3px">下次进入频道时自动打开麦克风</div>
            </div>
            <div class="switch ${prefs.mic ? 'on' : ''}" id="auto-mic-switch"><div class="knob"></div></div>
          </button>
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
        <button class="hit switch-row" id="join-cue-row" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">进出房间提示音</div>
            <div class="s-desc">有人进出频道时播放一声短提示</div>
          </div>
          <div class="switch ${prefs.joinCue ? 'on' : ''}" id="join-cue-switch"><div class="knob"></div></div>
        </button>
        <button class="hit switch-row" id="chat-cue-row" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">聊天提示音</div>
            <div class="s-desc">收到他人聊天消息时播放一声短提示</div>
          </div>
          <div class="switch ${prefs.chatCue ? 'on' : ''}" id="chat-cue-switch"><div class="knob"></div></div>
        </button>
        <button class="hit switch-row" id="mention-cue-row" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">被 @ 提示音</div>
            <div class="s-desc">有人 @ 你时播放一声更醒目的提示（不受聊天提示音的节流影响）</div>
          </div>
          <div class="switch ${prefs.mentionCue ? 'on' : ''}" id="mention-cue-switch"><div class="knob"></div></div>
        </button>
        <div class="section-label">系统通知</div>
        <div class="notify-hint" id="notify-hint"></div>
        <button class="hit switch-row" id="notify-msg-row" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">新消息</div>
            <div class="s-desc">页面在后台时，有人发言就弹一条系统通知</div>
          </div>
          <div class="switch ${prefs.notifyMessages ? 'on' : ''}" id="notify-msg-switch"><div class="knob"></div></div>
        </button>
        <button class="hit switch-row" id="notify-at-row" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">被 @ 提到</div>
            <div class="s-desc">消息里 @ 了你的用户名时单独提醒</div>
          </div>
          <div class="switch ${prefs.notifyMentions ? 'on' : ''}" id="notify-at-switch"><div class="knob"></div></div>
        </button>
        <button class="hit switch-row" id="notify-join-row" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">有人进房</div>
            <div class="s-desc">默认关——人来人往比消息吵</div>
          </div>
          <div class="switch ${prefs.notifyJoins ? 'on' : ''}" id="notify-join-switch"><div class="knob"></div></div>
        </button>
        <button class="hit switch-row" id="notify-push-row" style="width:100%;text-align:left">
          <div style="flex-grow:1">
            <div class="s-title">离线推送（被 @ 和回复）</div>
            <div class="s-desc" id="notify-push-desc">授权通知后自动开启；页面关着也能收到别人 @ 你或回复你的消息，普通消息不推。关掉后这台设备不再收离线推送</div>
          </div>
          <div class="switch" id="notify-push-switch"><div class="knob"></div></div>
        </button>
        <div class="opt-list" id="audio-chain"></div>
        <div class="kv-line">
          <span class="k">离开状态</span>
          <input class="afk-min mono" type="number" min="0" max="240" step="1" id="afk-min" value="${prefs.afkMinutes}" />
          <span style="font-size:11.5px;color:var(--text-2)">分钟无操作后，名册里标记为「离开」；0 = 不标记</span>
        </div>
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
    paintDevicePicker(kind, pickers, devices, openPicker, (k) => { openPicker = k; }, paintAllPickers);
  }
  const paintAllPickers = () => {
    paintPicker('mic');
    paintPicker('out');
    paintPicker('cam');
  };

  async function refreshDevices() {
    devices = await loadAVDevices();
    paintAllPickers();
  }

  // 电平表：拉一路本地麦克风流做可视化
  async function startMeter() {
    const bars = body.querySelectorAll<HTMLDivElement>('#level-bars > div');
    const ok = await meter.start(prefs.micDeviceId, bars);
    if (ok) await refreshDevices(); // 授权后能拿到设备名
  }
  function stopMeter() {
    meter.stop();
  }

  async function startCamPreview() {
    const box = body.querySelector<HTMLDivElement>('#cam-preview')!;
    const ok = await cam.start(prefs.camDeviceId, prefs.mirror, box);
    if (ok) await refreshDevices();
  }
  function stopCam() {
    cam.stop();
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

  const autoMicSwitch = body.querySelector<HTMLDivElement>('#auto-mic-switch')!;
  body.querySelector('#auto-mic-row')!.addEventListener('click', () => {
    prefs.mic = !prefs.mic;
    autoMicSwitch.classList.toggle('on', prefs.mic);
    save('mic-auto');
  });

  const joinCueSwitch = body.querySelector<HTMLDivElement>('#join-cue-switch')!;
  body.querySelector('#join-cue-row')!.addEventListener('click', () => {
    prefs.joinCue = !prefs.joinCue;
    joinCueSwitch.classList.toggle('on', prefs.joinCue);
    save('join-cue');
  });

  const chatCueSwitch = body.querySelector<HTMLDivElement>('#chat-cue-switch')!;
  body.querySelector('#chat-cue-row')!.addEventListener('click', () => {
    prefs.chatCue = !prefs.chatCue;
    chatCueSwitch.classList.toggle('on', prefs.chatCue);
    save('chat-cue');
  });

  const afkMin = body.querySelector<HTMLInputElement>('#afk-min')!;
  afkMin.addEventListener('change', () => {
    const n = Math.round(Number(afkMin.value));
    prefs.afkMinutes = Number.isFinite(n) && n >= 0 && n <= 240 ? n : 10;
    afkMin.value = String(prefs.afkMinutes);
    save('afk');
  });

  const mentionCueSwitch = body.querySelector<HTMLDivElement>('#mention-cue-switch')!;
  body.querySelector('#mention-cue-row')!.addEventListener('click', () => {
    prefs.mentionCue = !prefs.mentionCue;
    mentionCueSwitch.classList.toggle('on', prefs.mentionCue);
    save('mention-cue');
  });

  // 系统通知的三个开关：没授权时开关照常记偏好，只是提示条说明还没授权
  const notifyHint = body.querySelector<HTMLDivElement>('#notify-hint')!;
  function paintNotifyHint() {
    const state = notifyState();
    notifyHint.textContent =
      state === 'unsupported'
        ? inShell()
          ? '桌面端的系统通知稍后提供，这几个开关暂不生效。'
          : '这个浏览器不支持系统通知，开关不会生效。'
        : state === 'denied'
          ? '浏览器已拒绝本站的通知权限，要在地址栏的站点设置里改回「允许」。'
          : state === 'granted'
            ? '已获得通知权限。页面在前台时不发通知，只响提示音。'
            : '首次收到消息时才请求通知权限，同意后才会弹。';
    notifyHint.classList.toggle('bad', state === 'denied' || state === 'unsupported');
  }
  paintNotifyHint();
  (
    [
      ['#notify-msg-row', '#notify-msg-switch', 'notifyMessages'],
      ['#notify-at-row', '#notify-at-switch', 'notifyMentions'],
      ['#notify-join-row', '#notify-join-switch', 'notifyJoins'],
    ] as const
  ).forEach(([row, sw, key]) => {
    const knob = body.querySelector<HTMLDivElement>(sw)!;
    body.querySelector(row)!.addEventListener('click', () => {
      prefs[key] = !prefs[key];
      knob.classList.toggle('on', prefs[key]);
      if (prefs[key]) armNotifyPermission();
      paintNotifyHint();
      save(key);
    });
  });

  // 离线推送开关：状态以浏览器里的订阅为准（见 push.ts），本地不存副本
  const pushSwitch = body.querySelector<HTMLDivElement>('#notify-push-switch')!;
  const pushDesc = body.querySelector<HTMLDivElement>('#notify-push-desc')!;
  const pushRow = body.querySelector<HTMLButtonElement>('#notify-push-row')!;
  const pushBlocked = unsupportedReason();
  if (pushBlocked) {
    pushDesc.textContent = pushBlocked;
    pushDesc.classList.add('bad');
    pushRow.disabled = true;
  } else {
    void pushState().then((on) => pushSwitch.classList.toggle('on', on));
  }
  let pushBusy = false;
  pushRow.addEventListener('click', async () => {
    if (pushBlocked || pushBusy) return;
    pushBusy = true;
    const on = pushSwitch.classList.contains('on');
    try {
      if (on) {
        await pushUnsubscribe();
        setPushOptOut(true); // 用户主动关：自动订阅路径（授权瞬间/启动补订阅）以后不再打扰
      } else if (Notification.permission === 'denied') {
        throw new Error('浏览器已拒绝通知，需在浏览器站点设置里恢复');
      } else {
        setPushOptOut(false); // 重新打开：清掉"关过"的标记
        await pushSubscribe(); // 权限是 default 时会顺带触发浏览器的授权申请
      }
      toast(on ? '已关闭离线推送' : '已开启离线推送', 'ok');
    } catch (err) {
      toast((err as Error).message, 'bad');
    } finally {
      pushBusy = false;
      pushSwitch.classList.toggle('on', await pushState()); // 一律以浏览器的实际订阅收尾
    }
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

  return () => {
    stopMeter();
    stopCam();
  };
}
