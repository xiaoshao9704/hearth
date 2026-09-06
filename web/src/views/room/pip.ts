import { createSignal } from 'solid-js';

// 画中画：把投屏画面（可选连带浮动名册）弹到独立窗口，切到别的应用也能看着画面说话。
//
// 两级能力，按「能不能承载额外 DOM」分档，运行期逐级回退：
// 1. Document Picture-in-Picture：整个窗口是一个可写的 document，画面与名册都能放进去；
// 2. 元素级 requestPictureInPicture：只有画面，浏览器自己画窗口；
// 两级都没有就不显示按钮（supported() 返回 ''）。
//
// 画面元素是引擎产的命令式节点：进出 PiP 一律**搬运同一个元素**，绝不克隆或重建，
// 否则轨道会断。搬出时在原位留一个占位块，搬回时按占位块定位（原卡片已被拆掉就直接丢弃，
// 反正新的轨道会带来新的元素）。

// Document PiP 尚未进 lib.dom，按用到的最小面声明
interface DocumentPiP {
  requestWindow(options?: { width?: number; height?: number }): Promise<Window>;
  window: Window | null;
}

function docPip(): DocumentPiP | null {
  const w = window as unknown as { documentPictureInPicture?: DocumentPiP };
  const d = w.documentPictureInPicture;
  return d && typeof d.requestWindow === 'function' ? d : null;
}

function elemPipReady(): boolean {
  return document.pictureInPictureEnabled && 'requestPictureInPicture' in HTMLVideoElement.prototype;
}

export type PipKind = 'document' | 'video' | '';

export interface PipOpts {
  // 当前要弹出的画面（投屏优先）；没有画面时返回 null
  getVideo: () => HTMLVideoElement | null;
  // 在 PiP 窗口里额外挂点东西（浮动名册），返回卸载函数；只有 Document PiP 会调用
  mountExtras?: (container: HTMLElement) => () => void;
  onNotice?: (msg: string) => void;
}

export interface PipCtl {
  supported: () => PipKind;
  active: () => boolean;
  toggle: () => Promise<void>;
  close: () => void;
  dispose: () => void;
}

// 样式跨 document 不自动继承：能读到规则的表整段复制，跨源表退回 <link> 让 PiP 窗口自己拉。
function copyStyleSheets(target: Document) {
  for (const sheet of Array.from(document.styleSheets)) {
    try {
      const css = Array.from(sheet.cssRules)
        .map((r) => r.cssText)
        .join('\n');
      const style = target.createElement('style');
      style.textContent = css;
      target.head.append(style);
    } catch {
      if (!sheet.href) continue;
      const link = target.createElement('link');
      link.rel = 'stylesheet';
      link.href = sheet.href;
      target.head.append(link);
    }
  }
}

export function createPipCtl(opts: PipOpts): PipCtl {
  const kind: PipKind = docPip() ? 'document' : elemPipReady() ? 'video' : '';
  const [live, setLive] = createSignal(false);
  let win: Window | null = null;
  let moved: HTMLVideoElement | null = null; // 被搬进 PiP 窗口的画面元素
  let placeholder: HTMLElement | null = null; // 原位占位块，搬回时的锚点
  let unmountExtras: (() => void) | null = null;
  let elemVideo: HTMLVideoElement | null = null; // 元素级 PiP 中的画面

  const notice = (m: string) => opts.onNotice?.(m);

  function restoreVideo() {
    if (!moved) return;
    const v = moved;
    moved = null;
    if (placeholder?.isConnected) placeholder.replaceWith(v);
    else v.remove(); // 原卡片在 PiP 期间已经消失（轨道结束），元素跟着废弃
    placeholder = null;
  }

  function closeDoc() {
    unmountExtras?.();
    unmountExtras = null;
    restoreVideo();
    const w = win;
    win = null;
    w?.close();
  }

  function close() {
    if (win) closeDoc();
    if (elemVideo) {
      elemVideo = null;
      if (document.pictureInPictureElement) void document.exitPictureInPicture().catch(() => {});
    }
    setLive(false);
  }

  async function openDoc(video: HTMLVideoElement): Promise<boolean> {
    const api = docPip();
    if (!api) return false;
    const ratio = video.videoWidth && video.videoHeight ? video.videoHeight / video.videoWidth : 9 / 16;
    const width = 480;
    let w: Window;
    try {
      w = await api.requestWindow({ width, height: Math.round(width * ratio) });
    } catch {
      return false; // 用户手势缺失 / 策略禁用：让位给元素级 PiP
    }
    win = w;
    copyStyleSheets(w.document);
    w.document.documentElement.dataset.theme = document.documentElement.dataset.theme ?? '';
    w.document.body.className = 'pip-body';

    placeholder = document.createElement('div');
    placeholder.className = 'pip-away';
    placeholder.textContent = '画面已弹到画中画窗口';
    video.replaceWith(placeholder);
    moved = video;

    const stage = w.document.createElement('div');
    stage.className = 'pip-stage';
    stage.append(video);
    w.document.body.append(stage);

    if (opts.mountExtras) {
      const host = w.document.createElement('div');
      host.className = 'pip-extras';
      w.document.body.append(host);
      unmountExtras = opts.mountExtras(host);
    }
    // 用户关掉 PiP 窗口（也包括「返回标签页」）：把画面搬回原位
    w.addEventListener('pagehide', () => {
      if (!win) return;
      closeDoc();
      setLive(false);
    });
    setLive(true);
    return true;
  }

  async function openElem(video: HTMLVideoElement): Promise<boolean> {
    if (!elemPipReady() || video.disablePictureInPicture) return false;
    try {
      await video.requestPictureInPicture();
    } catch {
      return false;
    }
    elemVideo = video;
    video.addEventListener(
      'leavepictureinpicture',
      () => {
        elemVideo = null;
        setLive(false);
      },
      { once: true },
    );
    setLive(true);
    return true;
  }

  async function toggle() {
    if (live()) {
      close();
      return;
    }
    const video = opts.getVideo();
    if (!video) {
      notice('当前没有可弹出的画面');
      return;
    }
    if (await openDoc(video)) return;
    if (await openElem(video)) return;
    notice('这个浏览器不支持画中画');
  }

  return {
    supported: () => kind,
    active: live,
    toggle,
    close,
    dispose: close,
  };
}
