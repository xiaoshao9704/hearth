// 剧场浮动名册：剧场模式下侧栏让位，谁在房、谁在说话靠这个半透明浮窗兜住。
// 可拖到四角（松手吸附最近的角，落 prefs）、可折叠成一个小按钮。
// 停靠角与折叠态是模块级信号：主窗口与画中画窗口各挂一份视图，共享同一份真相，
// 一边拖动另一边跟着走（两者都只是 prefs 的镜像，不是第二真相源）。
import { createMemo, createSignal, For, Show } from 'solid-js';
import { render } from 'solid-js/web';
import type { EPart } from '../../engine/types';
import { loadPrefs, savePrefs, type TheaterCorner } from '../../prefs';
import { avatarHtml, el, icon, micIcon } from '../../ui';

const initial = loadPrefs();
const [corner, setCornerSig] = createSignal<TheaterCorner>(initial.theaterCorner);
const [folded, setFoldedSig] = createSignal(initial.theaterRosterFold);

function setCorner(c: TheaterCorner) {
  setCornerSig(c);
  const p = loadPrefs();
  p.theaterCorner = c;
  savePrefs(p);
}

function setFolded(v: boolean) {
  setFoldedSig(v);
  const p = loadPrefs();
  p.theaterRosterFold = v;
  savePrefs(p);
}

export interface FloatingRosterProps {
  parts: () => EPart[];
  speaking: () => Set<string>;
  // 右键/长按弹用户菜单（与名册面板、视频卡片同一个菜单）；画中画窗口里不接
  onMenu?: (x: number, y: number, p: EPart) => void;
  // 画中画窗口内：位置由窗口本身决定，不吸附、不拖动
  pip?: boolean;
}

// 按用户聚合（uid 拿不到时退回 identity，与名册面板同一套分组键）：
// 一行一个人，任一设备在说话就整行高亮
interface RosterRow {
  key: string;
  name: string;
  parts: EPart[];
}

export function FloatingRoster(props: FloatingRosterProps) {
  let boxEl!: HTMLDivElement;
  const [drag, setDrag] = createSignal<{ dx: number; dy: number } | null>(null);

  const rows = createMemo<RosterRow[]>(() => {
    const map = new Map<string, RosterRow>();
    for (const p of [...props.parts()].sort((a, b) => a.username.localeCompare(b.username) || a.uid - b.uid)) {
      const key = p.uid > 0 ? `u${p.uid}` : p.identity;
      const ex = map.get(key);
      if (ex) ex.parts.push(p);
      else map.set(key, { key, name: p.username || p.display || '—', parts: [p] });
    }
    return [...map.values()];
  });

  const rowSpeaking = (r: RosterRow) => r.parts.some((p) => props.speaking().has(p.identity));
  const rowMicOn = (r: RosterRow) => r.parts.some((p) => p.micOn);
  const rowSharing = (r: RosterRow) => r.parts.some((p) => p.sharing || p.ingest);

  // 拖拽：移动只改 transform（不写 prefs），松手按盒子中心吸附最近的角。
  // 不用 setPointerCapture：捕获会把随后的 click 改派到捕获元素上，头部的折叠按钮就点不动了
  const onGrab = (ev: PointerEvent) => {
    if (props.pip || ev.button !== 0) return;
    if ((ev.target as HTMLElement).closest('button')) return; // 头部的按钮不参与拖拽
    const startX = ev.clientX;
    const startY = ev.clientY;
    const onMove = (m: PointerEvent) => setDrag({ dx: m.clientX - startX, dy: m.clientY - startY });
    const onUp = () => {
      window.removeEventListener('pointermove', onMove);
      window.removeEventListener('pointerup', onUp);
      window.removeEventListener('pointercancel', onUp);
      const d = drag();
      // 先量位置再清 transform：清完 DOM 已经弹回原角，量到的就不是松手处了
      const r = boxEl.getBoundingClientRect();
      setDrag(null);
      if (!d || (Math.abs(d.dx) < 6 && Math.abs(d.dy) < 6)) return; // 视为点击，不改停靠角
      const cx = r.left + r.width / 2;
      const cy = r.top + r.height / 2;
      setCorner(`${cy < window.innerHeight / 2 ? 't' : 'b'}${cx < window.innerWidth / 2 ? 'l' : 'r'}` as TheaterCorner);
    };
    window.addEventListener('pointermove', onMove);
    window.addEventListener('pointerup', onUp);
    window.addEventListener('pointercancel', onUp);
  };

  return (
    <div
      ref={boxEl}
      class={'float-roster fr-' + (props.pip ? 'pip' : corner())}
      classList={{ dragging: !!drag(), folded: folded() && !props.pip }}
      style={drag() ? `transform:translate(${drag()!.dx}px,${drag()!.dy}px)` : undefined}
    >
      <Show
        when={!(folded() && !props.pip)}
        fallback={
          <button class="hit fr-unfold" title={`展开名册（${rows().length} 人）`} onClick={() => setFolded(false)}>
            {el(icon('users', 16, 'currentColor', 1.7))}
            <span class="mono">{rows().length}</span>
          </button>
        }
      >
        <div class="fr-head" onPointerDown={onGrab}>
          {el(icon('users', 14, 'currentColor', 1.7))}
          <span class="fr-count mono">{rows().length}</span>
          <span class="spacer"></span>
          <Show when={!props.pip}>
            <button class="hit fr-btn" title="折叠名册" onClick={() => setFolded(true)}>
              {el(icon('close', 13, 'currentColor', 1.9))}
            </button>
          </Show>
        </div>
        <div class="fr-list">
          <For each={rows()}>
            {(r) => (
              <div
                class="fr-row"
                classList={{ speaking: rowSpeaking(r) }}
                onContextMenu={(ev) => {
                  if (!props.onMenu) return;
                  ev.preventDefault();
                  props.onMenu(ev.clientX, ev.clientY, r.parts[0]);
                }}
              >
                {el(avatarHtml(r.name, 'avatar avatar-sm'))}
                <span class="fr-name">{r.name}</span>
                <Show when={rowSharing(r)}>
                  <span class="fr-tag">{el(icon('screen', 12, 'currentColor', 1.8))}</span>
                </Show>
                <Show when={!rowMicOn(r)}>
                  <span class="fr-mute">{el(micIcon(12, true, 'currentColor'))}</span>
                </Show>
              </div>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}

// 画中画窗口里再挂一份名册视图（同一批信号，跨 document 的 DOM 操作照常生效）。
// 画面元素必须搬运，名册这种纯派生 DOM 直接重画更稳——不会在关窗时留下悬空引用。
export function mountPipRoster(container: HTMLElement, props: FloatingRosterProps): () => void {
  return render(() => <FloatingRoster {...props} pip />, container);
}
