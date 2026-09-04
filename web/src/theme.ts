// 主题管理：浅色 / 深色 / 跟随系统，存 localStorage，落到 html[data-theme]。
import { icon, toast } from './ui';

export type Theme = 'light' | 'dark' | 'auto';

export const THEME_ICONS: Record<Theme, string> = { light: 'sun', dark: 'moon', auto: 'autoTheme' };
const THEME_LABELS: Record<Theme, string> = { light: '浅色', dark: '深色', auto: '跟随系统' };

const KEY = 'hearth_theme';
const META_LIGHT = '#f7f3ee';
const META_DARK = '#0d0b0a';

export function getTheme(): Theme {
  const t = localStorage.getItem(KEY);
  return t === 'light' || t === 'dark' ? t : 'auto';
}

// index.html 里的两条 media 版 <meta name="theme-color"> 只覆盖系统默认；用户一旦显式选了
// 浅色/深色/跟随系统，这里插一条不带 media 的顶掉它们（浏览器同名 meta 取最后一条生效的）。
function syncMetaThemeColor(t: Theme) {
  const dark = t === 'dark' || (t === 'auto' && matchMedia('(prefers-color-scheme: dark)').matches);
  let meta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]:not([media])');
  if (!meta) {
    meta = document.createElement('meta');
    meta.setAttribute('name', 'theme-color');
    document.head.appendChild(meta);
  }
  meta.setAttribute('content', dark ? META_DARK : META_LIGHT);
}

export function setTheme(t: Theme) {
  localStorage.setItem(KEY, t);
  document.documentElement.dataset.theme = t;
  syncMetaThemeColor(t);
}

export function cycleTheme(): Theme {
  const order: Record<Theme, Theme> = { light: 'dark', dark: 'auto', auto: 'light' };
  const next = order[getTheme()];
  setTheme(next);
  return next;
}

// 主题切换按钮：统一图标/title/aria-label 与点击后的文字反馈，登录页/加入页/侧栏三处共用。
export function wireThemeButton(btn: HTMLButtonElement): void {
  const paint = (t: Theme) => {
    btn.innerHTML = icon(THEME_ICONS[t], 16, 'var(--text-1)', 1.6);
    btn.title = `外观：${THEME_LABELS[t]}`;
    btn.setAttribute('aria-label', `外观：${THEME_LABELS[t]}，点击切换`);
  };
  paint(getTheme());
  btn.addEventListener('click', () => {
    const next = cycleTheme();
    paint(next);
    toast(`外观：${THEME_LABELS[next]}`);
  });
}
