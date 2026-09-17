// 投屏画质控件：设置浮层的「投屏画质」pane 与点「投屏」弹的浮窗共用这一份。
// 两处读写同一份 prefs、同一套码率约束（绝不各存一份），只有呈现分两档：
// 完整版是「我平时希望怎么投屏」，带说明与提示卡；紧凑版是开始投屏前的快速调整，少字、只摆常改项。
import {
  BITRATE_FLOOR,
  BITRATE_MIN_RATIO,
  BITRATE_STEP,
  BR_LIMITS,
  FPS_BY_RES,
  autoBitrate,
  autoBitrateMin,
  bitrateSliderBounds,
  loadPrefs,
  notifyPrefsChanged,
  probeHwEncode,
  savePrefs,
} from '../prefs';
import type { RoomPrefs, ScreenCodec, ScreenContent } from '../prefs';
import { capabilities, encoderDisplayName, inShell } from '../bridge';
import type { BridgeCaps } from '../bridge';
import { esc, icon, toast } from '../ui';

// 壳的能力探一次就够（Rust 侧也是缓存的）；拿到后重画一次编码那一行。
let shellCaps: BridgeCaps | null = null;

export interface ScreenQualityOpts {
  /** 紧凑版：浮窗里「开始投屏」前的快速调整 */
  compact?: boolean;
  /** 码率范围变过：只有重开发布才生效，调用方据此决定什么时候重开 */
  onBitrateRangeDirty?: () => void;
  /** 完整版底部的「走 OBS 推流」入口；紧凑版不摆 */
  goStream?: () => void;
}

// 两侧码率合成一个区间读数
function brLabel(p: { bitrateMin: number; bitrateMax: number }): string {
  // 破折号两侧不留空格：留了这行在紧凑版窄屏放不下会折成两行
  return `${p.bitrateMin.toFixed(1)}–${p.bitrateMax.toFixed(1)} Mbps`;
}

export function renderScreenQuality(body: HTMLElement, opts: ScreenQualityOpts = {}) {
  const prefs = loadPrefs();
  const compact = opts.compact === true;
  const markDirty = () => opts.onBitrateRangeDirty?.();

  // 自动档：上限按分辨率/帧率推，下限跟着上限走（改分辨率/帧率同样动到码率范围，一并记脏）
  const setAutoBitrate = (p: RoomPrefs, res: string, fps: number) => {
    const max = autoBitrate(res, fps);
    if (max !== p.bitrateMax) markDirty();
    p.bitrateMax = max;
    p.bitrateMin = autoBitrateMin(max);
    p.bitrateAuto = true;
  };

  const paint = () => {
    // 壳里投屏走原生管线，编码只有 h264/h265 两条路，标注一律用壳实测选中的编码器；
    // 浏览器那套 MediaCapabilities 预测说的是浏览器自己怎么编，与原生管线无关。
    const native = shellCaps?.native_publish === true;
    const nativeCodec: ScreenCodec = prefs.screenCodec === 'h265' ? 'h265' : 'h264';
    const codecOptions: Array<[string, string]> = native
      ? (shellCaps?.publish_codecs ?? []).map((c) => [
          c,
          compact
            ? c === 'h264'
              ? 'H.264'
              : 'H.265'
            : `${c === 'h264' ? 'H.264' : 'H.265'} · ${esc(encoderDisplayName(shellCaps?.publish_encoders?.[c]))}`,
        ])
      : compact
        ? [
            ['vp9', 'VP9'],
            ['av1', 'AV1'],
            ['h265', 'HEVC'],
            ['h264', 'H.264'],
          ]
        : [
            ['vp9', 'VP9 · SVC'],
            ['av1', 'AV1 · SVC'],
            ['h265', 'HEVC 单层'],
            ['h264', 'H.264 单层'],
          ];
    const codecOn = native ? nativeCodec : prefs.screenCodec;
    const lim = BR_LIMITS[prefs.res];
    const fpsAllowed = FPS_BY_RES[prefs.res] ?? [15, 30, 60];
    // 紧凑版只摆走得通的档位：够不着的分辨率/帧率留给设置页去解释
    const resOptions = compact ? ['720p', '1080p'] : ['720p', '1080p', '1440p', '4K'];
    const fpsOptions = compact ? fpsAllowed : [15, 30, 60, 120];
    // 建议区间常显在滑块下面（原来挂在「码率上限」行右侧，合成一行后没位置了）；顶住时临时换成提示
    const brHint = compact ? `建议 ${lim.min}–${lim.max}` : `${prefs.res} · ${prefs.fps}fps 建议 ${lim.min}–${lim.max}`;
    body.innerHTML = `
      <div class="${compact ? 'sq-compact' : 'pane-col pane-narrow'}">
        <div class="kv-line">
          <span class="k">分辨率</span>
          <div class="seg-group" style="flex-grow:1">
            ${resOptions
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
            ${fpsOptions
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
            ${codecOptions
              .map(([v, label]) => `<button class="hit seg ${codecOn === v ? 'on' : ''}" data-codec="${v}">${label}</button>`)
              .join('')}
          </div>
        </div>
        ${
          native && !compact
            ? `<div class="mono" style="padding-left:66px;font-size:10.5px;color:var(--text-3);margin-top:-8px">桌面端投屏由本机硬编直发，上面是壳实测选中的编码器。H.265 更省带宽，但观众端需支持 HEVC 解码，不支持的观众看不到画面。</div>`
            : ''
        }
        ${
          inShell() && shellCaps && !native && !compact
            ? `<div class="hint-card">
          ${icon('warn', 15, 'var(--text-2)')}
          <div>本机没有可用的原生硬编，投屏走浏览器：${esc(shellCaps.native_publish_error ?? '壳没有给出原因')}</div>
        </div>`
            : ''
        }
        <div class="kv-line">
          <span class="k">${compact ? '画面' : '内容类型'}</span>
          <div class="seg-group" style="flex-grow:1">
            ${(
              compact
                ? ([
                    ['game', '流畅优先'],
                    ['text', '清晰优先'],
                  ] as const)
                : ([
                    ['game', '流畅优先（默认）'],
                    ['text', '清晰优先（文档与代码）'],
                  ] as const)
            )
              .map(
                ([v, label]) =>
                  `<button class="hit seg ${prefs.screenContent === v ? 'on' : ''}" data-content="${v}">${label}</button>`,
              )
              .join('')}
          </div>
        </div>
        ${
          compact
            ? ''
            : `<div class="mono" style="padding-left:66px;font-size:10.5px;color:var(--text-3);margin-top:-8px">流畅优先：带宽不够时缩小画面、保住帧率。清晰优先反过来保住分辨率，帧率会掉——画面复杂时掉到个位数，只在滚动代码、看文档这类小字场景才用</div>`
        }
        <div class="kv-line">
          <span class="k">码率</span>
          <div class="br-range" id="br-range">
            <div class="br-rail"></div>
            <input class="range br-thumb br-lo" type="range" aria-label="码率下限" min="${BITRATE_FLOOR}" max="${lim.max}" step="${BITRATE_STEP}" value="${prefs.bitrateMin}" id="br-min" />
            <input class="range br-thumb br-hi" type="range" aria-label="码率上限" min="${BITRATE_FLOOR}" max="${lim.max}" step="${BITRATE_STEP}" value="${prefs.bitrateMax}" id="br-max" />
          </div>
          <span class="mono br-readout" id="br-label">${brLabel(prefs)}</span>
        </div>
        <div class="br-note" id="br-note">${brHint}</div>
        ${
          compact
            ? ''
            : `<div class="mono" style="padding-left:66px;font-size:10.5px;color:var(--text-3);margin-top:-8px">上限 = 网络好时最多发多少${prefs.bitrateAuto ? '（当前为自动推荐值）' : ''}；下限 = 网络差时最少也要发多少，低于它宁可丢包也不再降。下限设太高，真拥堵时画面就不是变糊而是花屏，一般留在上限的四成左右。投屏时关闭设置后会重开一次投屏，画面会断一下</div>`
        }
        ${
          compact
            ? ''
            : `<div class="hint-card">
          ${icon('cube', 15, 'var(--text-2)')}
          <div>VP9/AV1 走 SVC 分层：弱网观众自动降到低分辨率层，不拖累全场，也让上行带宽决定的观众数上限变成软性劣化；AV1 压缩率最高但软编极吃 CPU（实验）。H.264 单层兼容性最好。浏览器软编到 1080p60 为止——再往上是编码器的物理上限。<button class="hit" id="go-stream" style="color:var(--ember)">2K / 4K / 120fps 走 OBS 推流 →</button></div>
        </div>`
        }
      </div>`;

    body.querySelectorAll<HTMLButtonElement>('[data-res]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const r = btn.dataset.res!;
        if (r !== '720p' && r !== '1080p') {
          toast('浏览器投屏最高 1080p60，更高走 OBS 推流', '', 2600);
          return;
        }
        prefs.res = r;
        setAutoBitrate(prefs, r, prefs.fps);
        savePrefs(prefs);
        notifyPrefsChanged('screen');
        paint();
      });
    });
    // 按当前分辨率/帧率问浏览器：各编码档走不走硬件（MediaCapabilities 事前预测）。
    // 壳内投屏不经浏览器编码，这个预测会误导，不显示。
    if (!native)
      (['vp9', 'av1', 'h265', 'h264'] as ScreenCodec[]).forEach(async (c) => {
        const hw = await probeHwEncode(c);
        const btn = body.querySelector<HTMLButtonElement>(`[data-codec="${c}"]`);
        if (btn && hw !== null && !btn.querySelector('.enc-tag')) {
          btn.insertAdjacentHTML('beforeend', `<span class="enc-tag ${hw ? 'hw' : ''}">${hw ? '硬编' : '软编'}</span>`);
        }
      });
    body.querySelectorAll<HTMLButtonElement>('[data-codec]').forEach((btn) => {
      btn.addEventListener('click', () => {
        prefs.screenCodec = btn.dataset.codec as ScreenCodec;
        prefs.screenCodecAuto = false; // 手选后不再自动改
        savePrefs(prefs);
        notifyPrefsChanged('screen');
        paint();
      });
    });
    body.querySelectorAll<HTMLButtonElement>('[data-content]').forEach((btn) => {
      btn.addEventListener('click', () => {
        prefs.screenContent = btn.dataset.content as ScreenContent;
        savePrefs(prefs);
        notifyPrefsChanged('screen');
        paint();
      });
    });
    body.querySelectorAll<HTMLButtonElement>('[data-fps]').forEach((btn) => {
      btn.addEventListener('click', () => {
        const f = Number(btn.dataset.fps);
        if (!(FPS_BY_RES[prefs.res] ?? []).includes(f)) {
          toast('浏览器投屏最高 1080p60，更高走 OBS 推流', '', 2600);
          return;
        }
        prefs.fps = f;
        setAutoBitrate(prefs, prefs.res, f);
        savePrefs(prefs);
        notifyPrefsChanged('screen');
        paint();
      });
    });
    // 两个滑块共用一条轨道、同一量程（min/max/step 完全相同），拇指位置才能横向比较——
    // 所以边界不再写进 input 属性，而是拖动时夹住被拖的那一侧，另一侧一动不动。
    // 推挤（clampBitrateRange）只剩存档读取在用：单轨之后「拖一侧顶走另一侧」太刺眼。
    const brRange = body.querySelector<HTMLElement>('#br-range')!;
    const brMin = body.querySelector<HTMLInputElement>('#br-min')!;
    const brMax = body.querySelector<HTMLInputElement>('#br-max')!;
    const brLabelEl = body.querySelector<HTMLElement>('#br-label')!;
    const brNote = body.querySelector<HTMLElement>('#br-note')!;
    let noteTimer = 0;
    const pinned = (v: number, at: number) => Math.abs(v - at) < 0.05;
    // 轨道分四段画出「必须留出的余量」：余量段跟着上限实时移动，
    // 用户撞上限制之前就看得见限制在哪。三个百分比必须单调不减，否则色标翻转会画出乱色。
    const paintRail = () => {
      const span = lim.max - BITRATE_FLOOR;
      const pct = (v: number) => Math.min(100, Math.max(0, ((v - BITRATE_FLOOR) / span) * 100));
      // 按 input 的值画而不是 prefs：浏览器会把 value 夹到步进刻度上，照它画色段才与拇指严格对齐。
      // prefs 里的值现在也都落在刻度上（prefs.ts 的 roundStep），两者等价，但这里不依赖那个不变量
      const lo = pct(parseFloat(brMin.value));
      const cap = Math.max(lo, pct(parseFloat(brMax.value) * BITRATE_MIN_RATIO));
      const hi = Math.max(cap, pct(parseFloat(brMax.value)));
      brRange.style.setProperty('--lo', `${lo}%`);
      brRange.style.setProperty('--cap', `${cap}%`);
      brRange.style.setProperty('--hi', `${hi}%`);
      // 下限拇指恒在上限左侧，只有两个都被挤到刻度高位才会叠住：过了中点就让下限压在上面
      brRange.classList.toggle('min-top', lo > 50);
    };
    paintRail();
    const dragBitrate = (anchor: 'min' | 'max') => {
      // 边界按「拖动前」的另一侧值算，夹完写回 input：拖到头就停死，另一侧的值与拇指都不动
      const b = bitrateSliderBounds(prefs.bitrateMin, prefs.bitrateMax, lim);
      let hit = '';
      if (anchor === 'min') {
        const v = Math.min(parseFloat(brMin.value), b.minMax);
        if (v !== prefs.bitrateMin) markDirty();
        prefs.bitrateMin = v;
        brMin.value = String(v);
        if (pinned(v, b.minMax)) hit = '下限已到上限的八成——再高就没有降码率的余地了，先抬高上限';
      } else {
        const v = Math.max(parseFloat(brMax.value), b.maxMin);
        if (v !== prefs.bitrateMax) markDirty();
        prefs.bitrateMax = v;
        brMax.value = String(v);
        if (pinned(v, b.maxMin))
          hit =
            b.maxMinBy === 'floor'
              ? `上限已到 ${prefs.res} 的建议下界——再低这个分辨率就糊得没法看了`
              : '上限已被下限顶住——要再降先调低下限';
      }
      prefs.bitrateAuto = false;
      savePrefs(prefs);
      notifyPrefsChanged('screen');
      brLabelEl.textContent = brLabel(prefs);
      paintRail();
      brNote.textContent = hit || brHint;
      brNote.classList.toggle('on', !!hit);
      brLabelEl.classList.toggle('pinned', !!hit);
      window.clearTimeout(noteTimer);
      if (hit)
        noteTimer = window.setTimeout(() => {
          if (!body.isConnected) return;
          brNote.textContent = brHint;
          brNote.classList.remove('on');
          brLabelEl.classList.remove('pinned');
        }, 2200);
    };
    brMin.addEventListener('input', () => dragBitrate('min'));
    brMax.addEventListener('input', () => dragBitrate('max'));
    if (opts.goStream) body.querySelector('#go-stream')?.addEventListener('click', opts.goStream);
  };
  paint();
  if (inShell() && !shellCaps)
    void capabilities().then((caps) => {
      shellCaps = caps;
      if (body.isConnected) paint();
    });
}
