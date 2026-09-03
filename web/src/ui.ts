// UI 工具：SVG 图标、头像颜色、HTML 转义、toast、时间格式化。

export function esc(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// ---- SVG 图标（路径取自原型，20x20 viewBox 线性图标）----
const PATHS: Record<string, string> = {
  mic: 'M7.5 2.5h5v9h-5zM5 9.5a5 5 0 0010 0M10 14.5V17',
  micRect: '', // mic 用 rect 版单独渲染
  speaker: 'M4 12v-2a6 6 0 1112 0v2M2.5 11.5h3.5v5H2.5zM14 11.5h3.5v5H14z',
  volume: 'M4 8v4h3l4 3.5v-11L7 8H4z M14 7.5a4 4 0 010 5',
  camera: 'M2.5 5h11v10h-11zM13.5 9l4-2.5v7L13.5 11',
  screen: 'M2 3.5h16v11H2zM7 17.5h6',
  gear: 'M10 7.4a2.6 2.6 0 100 5.2 2.6 2.6 0 000-5.2M10 2.5v2M10 15.5v2M17.5 10h-2M4.5 10h-2M15.3 4.7l-1.4 1.4M6.1 13.9l-1.4 1.4M15.3 15.3l-1.4-1.4M6.1 6.1L4.7 4.7',
  chat: 'M3 5.5A2 2 0 015 3.5h10a2 2 0 012 2v6a2 2 0 01-2 2H8l-4 3.5V13.5H5a2 2 0 01-2-2v-6z',
  leave: 'M12 3.5h3a1.5 1.5 0 011.5 1.5v10a1.5 1.5 0 01-1.5 1.5h-3M8 13.5L4.5 10 8 6.5M4.5 10H12',
  back: 'M8 6.5L4.5 10 8 13.5M4.5 10H12M12 3.5h3a1.5 1.5 0 011.5 1.5v10a1.5 1.5 0 01-1.5 1.5h-3',
  close: 'M5 5l10 10M15 5L5 15',
  check: 'M4.5 10.5l3.5 3.5 7.5-8',
  plus: 'M10 4.5v11M4.5 10h11',
  copy: 'M7 7h7.5a2 2 0 012 2v7.5H9a2 2 0 01-2-2zM13 4.5A1.5 1.5 0 0011.5 3h-7A1.5 1.5 0 003 4.5v7A1.5 1.5 0 004.5 13',
  eye: 'M1.8 10S4.6 4.8 10 4.8 18.2 10 18.2 10 15.4 15.2 10 15.2 1.8 10 1.8 10z M10 7.8a2.2 2.2 0 100 4.4 2.2 2.2 0 000-4.4',
  eyeOff: 'M1.8 10S4.6 4.8 10 4.8 18.2 10 18.2 10 15.4 15.2 10 15.2 1.8 10 1.8 10z M10 7.8a2.2 2.2 0 100 4.4 2.2 2.2 0 000-4.4 M3.5 3.5l13 13',
  reset: 'M16.5 8.5A6.5 6.5 0 105.6 14.4M16.5 4.5v4h-4',
  user: 'M10 10.5a3.4 3.4 0 100-6.8 3.4 3.4 0 000 6.8M3.5 17c0-3.2 2.9-5 6.5-5s6.5 1.8 6.5 5',
  moon: 'M16 11.7A6.6 6.6 0 018.3 4a6.6 6.6 0 107.7 7.7z',
  sun: 'M10 6.6a3.4 3.4 0 100 6.8 3.4 3.4 0 000-6.8M10 2v2M10 16v2M18 10h-2M4 10H2M15.7 4.3l-1.4 1.4M5.7 14.3l-1.4 1.4M15.7 15.7l-1.4-1.4M5.7 5.7L4.3 4.3',
  autoTheme: 'M2.5 5.8A1.8 1.8 0 014.3 4h11.4a1.8 1.8 0 011.8 1.8v6.4a1.8 1.8 0 01-1.8 1.8H4.3a1.8 1.8 0 01-1.8-1.8zM7 17h6',
  stream: 'M10 11.5V17M6 3.5h8l-1.2 4.2 2.2 2.3H5l2.2-2.3L6 3.5z',
  device: 'M3 4h14v9.5H3zM1.5 16.5h17',
  phone: 'M7.5 2.5h5a2 2 0 012 2v11a2 2 0 01-2 2h-5a2 2 0 01-2-2v-11a2 2 0 012-2zM9 15h2',
  shield: 'M10 2.5l6 2.5v5c0 3.6-2.4 6.6-6 7.5-3.6-.9-6-3.9-6-7.5V5l6-2.5z',
  mail: 'M2.5 6.5l7.5 5 7.5-5M2.5 5.5h15v9h-15z',
  clock: 'M10 2.5a7.5 7.5 0 100 15 7.5 7.5 0 000-15M10 6.2V10l2.6 1.6',
  info: 'M10 2.5a7.5 7.5 0 100 15 7.5 7.5 0 000-15M10 9v4.5M10 6.6v.1',
  warn: 'M10 2.5a7.5 7.5 0 100 15 7.5 7.5 0 000-15M10 6.2v4.4M10 13.6v.1',
  search: 'M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11M13 13l4 4',
  trash: 'M4.5 6h11M8 6V4.5h4V6M6 6l.7 10h6.6L14 6',
  pulse: 'M3 10h3l2.5-5 3 10 2.5-5h3',
  grid: 'M2.5 2.5h6v6h-6zM11.5 2.5h6v6h-6zM2.5 11.5h6v6h-6zM11.5 11.5h6v6h-6z',
  focus: 'M2.5 3.5h10v13h-10zM14.5 3.5h3v4h-3zM14.5 9.5h3v4h-3z',
  chevDown: 'M5 8l5 5 5-5',
  chevUp: 'M5 12l5-5 5 5',
  users: 'M7.5 9.5a3 3 0 100-6 3 3 0 000 6M2.5 16.5c0-2.8 2.2-4.5 5-4.5s5 1.7 5 4.5M14 7.5a2.4 2.4 0 100-4.8M15.5 16.5c0-2 .5-3.2 2-3.9',
  cube: 'M10 2.5l7 4v7l-7 4-7-4v-7l7-4z',
  menu: 'M3 5.5h14M3 10h14M3 14.5h14',
  sliders: 'M2.5 6h6.5M15 6h2.5M2.5 14H4M8 14h9.5M15 6a2 2 0 11-4 0 2 2 0 014 0M8 14a2 2 0 11-4 0 2 2 0 014 0',
  fullscreen: 'M3.5 7.5v-4h4M12.5 3.5h4v4M16.5 12.5v4h-4M7.5 16.5h-4v-4',
  more: 'M3.4 10a1.1 1.1 0 102.2 0 1.1 1.1 0 10-2.2 0M8.9 10a1.1 1.1 0 102.2 0 1.1 1.1 0 10-2.2 0M14.4 10a1.1 1.1 0 102.2 0 1.1 1.1 0 10-2.2 0',
};

export function icon(name: string, size = 16, color = 'currentColor', strokeWidth = 1.7): string {
  const d = PATHS[name] ?? '';
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="${color}" stroke-width="${strokeWidth}" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="${d}"/></svg>`;
}

// ---- Lucide 精选（ISC，lucide-static v0.544.0，24 viewBox）----
// 统计徽章这类小尺寸场景用：图形语义更准（手绘那套没有对应的）
const LUCIDE: Record<string, string> = {
  monitor: '<rect width="20" height="14" x="2" y="3" rx="2"/><line x1="8" x2="16" y1="21" y2="21"/><line x1="12" x2="12" y1="17" y2="21"/>',
  timer: '<line x1="10" x2="14" y1="2" y2="2"/><line x1="12" x2="15" y1="14" y2="11"/><circle cx="12" cy="14" r="8"/>',
  gauge: '<path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/>',
  cpu: '<path d="M12 20v2"/><path d="M12 2v2"/><path d="M17 20v2"/><path d="M17 2v2"/><path d="M2 12h2"/><path d="M2 17h2"/><path d="M2 7h2"/><path d="M20 12h2"/><path d="M20 17h2"/><path d="M20 7h2"/><path d="M7 20v2"/><path d="M7 2v2"/><rect x="4" y="4" width="16" height="16" rx="2"/><rect x="8" y="8" width="8" height="8" rx="1"/>',
  pin: '<path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V6h1a2 2 0 0 0 0-4H8a2 2 0 0 0 0 4h1z"/>',
};

export function licon(name: string, size = 12, color = 'currentColor'): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="${color}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${LUCIDE[name] ?? ''}</svg>`;
}

// 麦克风（带可选斜杠）
export function micIcon(size = 16, slash = false, color = 'currentColor'): string {
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="${color}" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="7.5" y="2.5" width="5" height="9" rx="2.5"/><path d="M5 9.5a5 5 0 0010 0M10 14.5V17"/>${slash ? '<path d="M4 4l12 12"/>' : ''}</svg>`;
}

export function slashIcon(name: string, size = 16, slash = false, color = 'currentColor'): string {
  const d = PATHS[name] ?? '';
  return `<svg width="${size}" height="${size}" viewBox="0 0 20 20" fill="none" stroke="${color}" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="${d}"/>${slash ? '<path d="M3 3l14 14"/>' : ''}</svg>`;
}

// 炉火 logo
export function flameLogo(w = 18, h = 20): string {
  return `<svg width="${w}" height="${h}" viewBox="0 0 18 20" fill="none" aria-hidden="true"><path d="M9 1.5c3.2 3.1 4.6 5.4 4.6 7.7 0 1.5-.7 2.7-1.8 3.4.5-1.6.2-3.1-1-4.6.1 2.5-1.1 3.9-2.4 5.1-1.1 1-1.7 2-1.7 3.2 0 .9.3 1.7.9 2.3C4.4 17.7 2.6 15.4 2.6 12.4 2.6 8.2 6.1 5.8 9 1.5z" fill="var(--ember)"/><path d="M9 18.5c1.9 0 3.2-1.2 3.2-2.9 0-1.4-.9-2.3-2-3.3-.4 1.3-1.3 2-2.4 2.5-.9.4-1.4 1-1.4 1.9 0 1.1.9 1.8 2.6 1.8z" fill="var(--ember-soft)"/></svg>`;
}

// ---- 头像 ----
const AVATAR_COLORS: [string, string][] = [
  ['#8a4a2c', '#f8e3d6'],
  ['#4f5a3c', '#e8eeda'],
  ['#6a4630', '#f3ded0'],
  ['#7a4b58', '#f7dee5'],
  ['#514336', '#f0dfd0'],
  ['#3f4a52', '#d6e2ea'],
  ['#55483a', '#ece0d2'],
  ['#3d3a44', '#c9c2d6'],
];

export function avatarColor(name: string): [string, string] {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return AVATAR_COLORS[h % AVATAR_COLORS.length];
}

export function initialOf(name: string): string {
  const c = [...name][0] ?? '?';
  return /[a-z]/i.test(c) ? name.slice(0, 2).replace(/[^a-zA-Z0-9]/g, '') || c.toUpperCase() : c;
}

export function avatarHtml(name: string, cls = 'avatar'): string {
  const [bg, fg] = avatarColor(name);
  return `<div class="${cls}" style="background:${bg};color:${fg}">${esc(initialOf(name))}</div>`;
}

// ---- toast ----
let toastWrap: HTMLDivElement | null = null;

export function toast(msg: string, tone: 'ok' | 'bad' | '' = '', ms = 3200) {
  if (!toastWrap || !document.body.contains(toastWrap)) {
    toastWrap = document.createElement('div');
    toastWrap.className = 'toast-wrap';
    document.body.appendChild(toastWrap);
  }
  const el = document.createElement('div');
  el.className = `toast ${tone}`;
  const ic = tone === 'ok' ? icon('check', 15) : tone === 'bad' ? icon('warn', 15) : '';
  el.innerHTML = `${ic}<span></span>`;
  el.querySelector('span')!.textContent = msg;
  toastWrap.appendChild(el);
  setTimeout(() => el.remove(), ms);
}

// ---- 时间 ----
export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return '从未';
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return '—';
  const d = Date.now() - t;
  if (d < 90_000) return '现在';
  if (d < 3600_000) return `${Math.floor(d / 60_000)} 分钟前`;
  if (d < 86400_000) return `${Math.floor(d / 3600_000)} 小时前`;
  return `${Math.floor(d / 86400_000)} 天前`;
}

export function fmtClock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const p = (n: number) => String(n).padStart(2, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // 非安全上下文兜底
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    return ok;
  }
}

// 密码强度 0-4（与原型同款规则）
export function pwScore(pw: string): number {
  let score = 0;
  if (pw.length >= 8) score++;
  if (pw.length >= 12) score++;
  if (/[A-Z]/.test(pw) && /[a-z]/.test(pw)) score++;
  if (/[^A-Za-z0-9]/.test(pw) || /[0-9]/.test(pw)) score++;
  return score;
}

export function pwBarsHtml(score: number): string {
  const cls = score <= 1 ? 'bad' : score === 2 ? 'mid' : 'good';
  return Array.from({ length: 4 }, (_, i) => `<div class="${i < score ? cls : ''}"></div>`).join('');
}

// 把一段 HTML 变成节点：Solid 视图里挂 icon()/avatarHtml() 这类字符串产物用
export function el(html: string): Element {
  const t = document.createElement('template');
  t.innerHTML = html;
  return t.content.firstElementChild!;
}
