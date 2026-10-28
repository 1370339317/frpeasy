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

/**
 * 复制的三种结局。
 *
 * 为什么要区分 `manual` 与 `fail`：移动端（尤其国产内核 / 隐私模式 / 非安全上下文）
 * 经常两条通道都不可用。这时候**唯一的出路是把文本选中让用户长按**，
 * 而"已全选，长按即可复制"和"复制失败"对用户是完全不同的两句话 ——
 * 前者有明确下一步，后者只会让人以为坏了。
 */
export type CopyOutcome = 'ok' | 'manual' | 'fail'

/**
 * 复制到剪贴板。`target` 用于"两条通道都失败时自动全选这段文本"。
 *
 * 为什么不能只写 navigator.clipboard：它只在**安全上下文**（HTTPS 或 localhost）存在。
 * 这个面板最主要的部署形态恰恰是 `http://内网/公网IP:28080` —— 非安全上下文里
 * `navigator.clipboard` 是 undefined，那一条路径直接抛错。
 * 而「复制安装命令」就是主流程第一步：复制不出来 = 用户装不上客户端。
 *
 * 手机浏览器上还有第二层坑：execCommand('copy') 在部分内核里也是 false，
 * 所以最后必须有一条"选中 + 长按"的人工路径。
 */
export async function copyText(text: string, target?: HTMLElement | null): Promise<CopyOutcome> {
  if (!text || text === '—') return 'fail'
  // 1) 安全上下文优先 Clipboard API。
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return 'ok'
    } catch {
      /* 权限被拒 / 没有焦点：继续走兜底 */
    }
  }
  // 2) 兜底 execCommand。注意这一步是在 await 之后 —— 若上面抛过错，
  //    用户手势（transient activation）可能已经过期，所以它失败是正常的，
  //    不能因为"设计了兜底"就认为一定成功。
  if (copyByExecCommand(text)) return 'ok'
  // 3) 都失败：把目标文本选中，交给用户长按复制（移动端唯一稳的路）。
  if (target && selectElementText(target)) return 'manual'
  return 'fail'
}

/** 复制并在失败时给出可直接照做的提示（9 处复制按钮共用）。 */
export async function copyWithHint(
  text: string,
  target: HTMLElement | null,
  okMsg: string,
): Promise<{ msg: string; kind: 'ok' | 'bad' }> {
  const r = await copyText(text, target)
  if (r === 'ok') return { msg: okMsg, kind: 'ok' }
  if (r === 'manual') return { msg: '浏览器不允许自动复制：已为你全选，长按即可复制', kind: 'ok' }
  return { msg: '复制失败：请长按下面的命令手动复制', kind: 'bad' }
}

/** 选中元素内的全部文本。返回是否真的建立起了选区。 */
export function selectElementText(el: HTMLElement): boolean {
  try {
    const range = document.createRange()
    range.selectNodeContents(el)
    const sel = window.getSelection()
    if (!sel) return false
    sel.removeAllRanges()
    sel.addRange(range)
    return sel.toString().length > 0
  } catch {
    return false
  }
}

/**
 * 非安全上下文 / 移动端内核下的复制通道。
 *
 * 移动浏览器上有三个具体的坑，逐条规避：
 *  1. `opacity: 0` 或 `display: none` 会被部分内核视为"没有可复制内容" →
 *     用 0.01 保持渲染，`display` 绝不能动；
 *  2. 字号小于 16px 时 iOS Safari 会在 focus 的瞬间放大整个页面 →
 *     固定 16px（放大后还要手动缩回去，体感极差）；
 *  3. `readonly` 既挡掉软键盘弹出（否则一按复制屏幕下半截被键盘顶上来），
 *     又不影响 `select()`，所以保留。
 * 另外用 `preventScroll`，避免聚焦时页面跳到顶部、用户看不到自己点的是哪一条。
 */
function copyByExecCommand(text: string): boolean {
  if (typeof document === 'undefined' || typeof document.execCommand !== 'function') {
    return false
  }
  const ta = document.createElement('textarea')
  ta.value = text
  ta.setAttribute('readonly', '')
  ta.setAttribute('aria-hidden', 'true')
  ta.tabIndex = -1
  Object.assign(ta.style, {
    position: 'fixed',
    top: '0',
    left: '0',
    width: '1px',
    height: '1px',
    opacity: '0.01',
    fontSize: '16px',
    padding: '0',
    margin: '0',
    border: 'none',
    outline: 'none',
    boxShadow: 'none',
    background: 'transparent',
    // 盖在弹层之上：弹窗有焦点陷阱，元素被压在下面时 focus 不一定生效。
    zIndex: '2147483647',
  })
  const prevFocus = document.activeElement as HTMLElement | null
  document.body.appendChild(ta)
  let ok = false
  try {
    ta.focus({ preventScroll: true })
    ta.select()
    // 部分内核 select() 只选到可见部分（我们是 1px 宽），补一次显式范围。
    if (ta.selectionStart === ta.selectionEnd) {
      ta.setSelectionRange(0, ta.value.length)
    }
    ok = document.execCommand('copy')
  } catch {
    ok = false
  } finally {
    document.body.removeChild(ta)
    // 把焦点还回去：不还的话弹窗里的输入框会突然失焦。
    if (prevFocus && typeof prevFocus.focus === 'function') {
      try {
        prevFocus.focus({ preventScroll: true })
      } catch {
        /* 元素可能已被移除，忽略 */
      }
    }
  }
  return ok
}

/**
 * 比较两个语义化版本号（'0.7.34' / 'v0.7.34' / '0.7.34-beta'）。
 * 返回 <0 表示 a 比 b 旧。
 *
 * 只用于"要不要提示有新版"这种展示判断，不参与更新决策 ——
 * 真正的门禁在服务端（shared.CompareVersions），前端算错最多是提示不准，
 * 不能让它影响任何写操作。
 */
export function compareVersions(a: string, b: string): number {
  const norm = (v: string) =>
    v
      .trim()
      .replace(/^v/i, '')
      .split(/[.\-+_]/, 3)
  const pa = norm(a)
  const pb = norm(b)
  for (let i = 0; i < 3; i++) {
    const na = Number.parseInt(pa[i] ?? '0', 10)
    const nb = Number.parseInt(pb[i] ?? '0', 10)
    if (!Number.isFinite(na) || !Number.isFinite(nb)) return 0
    if (na !== nb) return na - nb
  }
  return 0
}
