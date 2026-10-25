/** 日夜主题：只改 <html data-theme>，颜色全部由 CSS 变量驱动。 */

export type Theme = 'dark' | 'light'

const KEY = 'frpeasy_theme'

export function readStoredTheme(): Theme {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'light' || v === 'dark') return v
  } catch {
    /* 隐私模式下 localStorage 可能抛异常，忽略 */
  }
  // 没存过就跟随系统
  if (typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: light)').matches) {
    return 'light'
  }
  return 'dark'
}

export function applyTheme(t: Theme): void {
  document.documentElement.setAttribute('data-theme', t)
  try {
    localStorage.setItem(KEY, t)
  } catch {
    /* 存不进去也不影响本次生效 */
  }
}
