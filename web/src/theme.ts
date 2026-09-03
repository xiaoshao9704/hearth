// 主题管理：浅色 / 深色 / 跟随系统，存 localStorage，落到 html[data-theme]。
export type Theme = 'light' | 'dark' | 'auto';

export const THEME_ICONS: Record<Theme, string> = { light: 'sun', dark: 'moon', auto: 'autoTheme' };

const KEY = 'hearth_theme';

export function getTheme(): Theme {
  const t = localStorage.getItem(KEY);
  return t === 'light' || t === 'dark' ? t : 'auto';
}

export function setTheme(t: Theme) {
  localStorage.setItem(KEY, t);
  document.documentElement.dataset.theme = t;
}

export function cycleTheme(): Theme {
  const order: Record<Theme, Theme> = { light: 'dark', dark: 'auto', auto: 'light' };
  const next = order[getTheme()];
  setTheme(next);
  return next;
}
