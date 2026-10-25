/** 展示层格式化。全部纯函数，便于单测。 */

export function fmtBytes(n: number | null | undefined): string {
  const v = Number(n) || 0
  if (v < 1024) return `${v} B`
  if (v < 1048576) return `${(v / 1024).toFixed(1)} KB`
  if (v < 1073741824) return `${(v / 1048576).toFixed(1)} MB`
  return `${(v / 1073741824).toFixed(2)} GB`
}

/**
 * 相对时间。
 *
 * 后端 Go 的零值 time.Time 会序列化成 "0001-01-01T00:00:00Z"，
 * 这种值必须显示「从未」而不是「-739000 天前」。
 */
export function fmtAgo(ts: string | null | undefined): string {
  if (!ts) return '从未'
  const d = new Date(ts)
  const t = d.getTime()
  if (Number.isNaN(t) || d.getUTCFullYear() < 1970) return '从未'
  const s = Math.floor((Date.now() - t) / 1000)
  if (s < 0) return '刚刚'
  if (s < 60) return `${s} 秒前`
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`
  return `${Math.floor(s / 86400)} 天前`
}

/** 剥掉协议、路径和端口，只留主机（IPv6 保留方括号）。 */
export function stripHostPort(raw: string | null | undefined): string {
  let h = (raw ?? '').replace(/^https?:\/\//i, '').replace(/\/.*$/, '')
  if (!h) return ''
  if (h.startsWith('[')) {
    const i = h.indexOf(']')
    return i >= 0 ? h.slice(0, i + 1) : h
  }
  const i = h.lastIndexOf(':')
  return i >= 0 ? h.slice(0, i) : h
}

/** UTC 构建时间 -> 本地「MM-DD HH:mm」。 */
export function fmtBuildTime(raw: string): string {
  if (!raw || raw === 'unknown') return '未注入构建时间'
  const d = new Date(raw)
  if (Number.isNaN(d.getTime())) return raw
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

export async function copyText(text: string): Promise<boolean> {
  if (!text || text === '—') return false
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}
