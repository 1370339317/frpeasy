import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode, SelectHTMLAttributes } from 'react'
import { useEffect, useId, useRef } from 'react'

/** 极简 className 合并（避免为一行逻辑引入 clsx 依赖）。 */
export const cx = (...parts: (string | false | null | undefined)[]) => parts.filter(Boolean).join(' ')

export type Tone = 'ok' | 'bad' | 'warn' | 'info' | 'gray'

/* ---------------- 按钮 ---------------- */

type BtnVariant = 'primary' | 'ghost' | 'danger'

interface BtnProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: BtnVariant
  size?: 'md' | 'sm'
}

const BTN_VARIANT: Record<BtnVariant, string> = {
  primary: 'bg-accent text-white hover:brightness-110',
  ghost: 'bg-transparent border-border2 text-fg hover:border-accent hover:text-accent-fg',
  danger: 'bg-transparent border-border2 text-bad-fg hover:bg-bad-dim hover:border-bad',
}

export function Button({ variant = 'primary', size = 'md', className, ...rest }: BtnProps) {
  return (
    <button
      {...rest}
      className={cx(
        'inline-flex cursor-pointer items-center justify-center gap-1.5 rounded-[9px] border border-transparent font-semibold transition active:translate-y-px disabled:cursor-not-allowed disabled:opacity-50',
        size === 'sm' ? 'rounded-[7px] px-2.5 py-1 text-[12.5px]' : 'px-3.5 py-2 text-[13.5px]',
        BTN_VARIANT[variant],
        className,
      )}
    />
  )
}

/** 方框图标按钮（顶栏主题切换等）。 */
export function IconButton({ className, ...rest }: ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      {...rest}
      className={cx(
        'inline-flex h-8 w-8 cursor-pointer items-center justify-center rounded-[9px] border border-border2 bg-transparent text-[15px] leading-none text-muted transition hover:border-accent hover:text-fg',
        className,
      )}
    />
  )
}

/* ---------------- 容器 ---------------- */

export function Card({
  title,
  desc,
  actions,
  children,
  className,
  bodyClassName,
}: {
  title?: ReactNode
  desc?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  className?: string
  bodyClassName?: string
}) {
  return (
    <section
      className={cx(
        'mb-4 rounded-xl border border-border bg-card px-5 py-[18px] max-[820px]:px-4',
        className,
      )}
    >
      {(title || actions) && (
        // 窄屏改成"标题一行、描述另起一行"的堆叠：原来标题和描述共用一行，
        // 393px 宽时描述把标题挤成一列，四个字被拆成三行竖排。
        <div className="mb-3.5 flex items-center gap-2.5 max-[820px]:flex-wrap">
          {title && <h2 className="text-sm font-[650] whitespace-nowrap">{title}</h2>}
          {desc && (
            <span className="text-xs text-dim max-[820px]:order-last max-[820px]:w-full">
              {desc}
            </span>
          )}
          <div className="flex-1 max-[820px]:hidden" />
          {actions && <div className="max-[820px]:ml-auto">{actions}</div>}
        </div>
      )}
      <div className={bodyClassName}>{children}</div>
    </section>
  )
}

export function Stat({
  label,
  value,
  sub,
  led,
  labelSlot,
}: {
  label: ReactNode
  value: ReactNode
  sub?: ReactNode
  led?: ReactNode
  labelSlot?: ReactNode
}) {
  return (
    <div className="rounded-xl border border-border bg-card px-4.5 py-4">
      <div className="flex items-center gap-1.5 text-xs text-muted">
        {led}
        <span>{label}</span>
        {labelSlot}
      </div>
      <div className="mt-1.5 text-[22px] font-bold tracking-[0.3px] break-all">{value}</div>
      {sub && <div className="mt-0.5 text-[11.5px] text-dim">{sub}</div>}
    </div>
  )
}

export function Stats({ children }: { children: ReactNode }) {
  return (
    <div className="mb-4 grid gap-3.5 [grid-template-columns:repeat(auto-fit,minmax(190px,1fr))]">
      {children}
    </div>
  )
}

/* ---------------- 徽标 ---------------- */

const BADGE_TONE: Record<Tone, string> = {
  ok: 'bg-ok-dim text-ok-fg',
  bad: 'bg-bad-dim text-bad-fg',
  warn: 'bg-warn-dim text-warn-fg',
  info: 'bg-accent-dim text-accent-fg',
  gray: 'bg-gray-dim text-gray-fg',
}

export function Badge({
  tone = 'gray',
  children,
  title,
  className,
}: {
  tone?: Tone
  children: ReactNode
  title?: string
  className?: string
}) {
  return (
    <span
      title={title}
      className={cx(
        'inline-flex items-center gap-[5px] rounded-full px-2.5 py-0.5 text-[11.5px] leading-[18px] font-[650] whitespace-nowrap',
        BADGE_TONE[tone],
        className,
      )}
    >
      {children}
    </span>
  )
}

export function Led({ tone }: { tone: 'ok' | 'bad' | 'warn' }) {
  const bg = tone === 'ok' ? 'bg-ok shadow-[0_0_7px_var(--ok)]' : tone === 'bad' ? 'bg-bad' : 'bg-warn'
  return (
    <span
      className={cx('inline-block h-[7px] w-[7px] rounded-full', bg)}
      style={tone === 'warn' ? { animation: 'frp-pulse 1.2s infinite' } : undefined}
    />
  )
}

export function Alert({ tone, children }: { tone: Tone; children: ReactNode }) {
  const map: Record<string, string> = {
    bad: 'bg-bad-dim border-alert-bad-bd text-alert-bad-fg',
    warn: 'bg-warn-dim border-alert-warn-bd text-alert-warn-fg',
    info: 'bg-accent-dim border-alert-info-bd text-alert-info-fg',
    ok: 'bg-ok-dim border-ok text-ok-fg',
    gray: 'bg-gray-dim border-border text-muted',
  }
  return (
    <div
      className={cx(
        'mb-3.5 flex items-start gap-2.5 rounded-[10px] border px-3.5 py-2.5 text-[13px] leading-[1.55]',
        map[tone],
      )}
    >
      <span className="min-w-0">{children}</span>
    </div>
  )
}

/* ---------------- 表单 ---------------- */

export const inputCls =
  'w-full rounded-[9px] border border-border2 bg-input px-2.5 py-2 text-[13.5px] text-fg outline-none focus:border-accent focus:shadow-[0_0_0_3px_var(--accent-dim)]'

export function Field({
  label,
  hint,
  children,
  className,
}: {
  label?: ReactNode
  hint?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <div className={cx('mb-3.5', className)}>
      {label && <label className="mb-1.5 block text-[12.5px] text-muted">{label}</label>}
      {children}
      {hint && <div className="mt-1.5 text-xs leading-[1.5] text-dim">{hint}</div>}
    </div>
  )
}

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...rest} className={cx(inputCls, className)} />
}

export function Select({ className, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...rest} className={cx(inputCls, className)} />
}

export function Hint({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cx('mt-1.5 text-xs leading-[1.5] text-dim', className)}>{children}</div>
}

/** 可点选标签（镜像源、探测到的地址、客户端上报的服务都用它）。 */
export function Chip({
  selected,
  onClick,
  title,
  sub,
  children,
  static: isStatic,
}: {
  selected?: boolean
  onClick?: () => void
  title?: string
  sub?: ReactNode
  children: ReactNode
  static?: boolean
}) {
  return (
    <div
      onClick={isStatic ? undefined : onClick}
      // Chip 是可点击的 div。补上 role / tabIndex / 键盘响应，
      // 否则键盘与读屏用户完全用不到它（全站的镜像预设、策略与来源选项都是它）。
      role={isStatic ? undefined : 'button'}
      tabIndex={isStatic ? undefined : 0}
      aria-pressed={isStatic ? undefined : !!selected}
      onKeyDown={
        isStatic
          ? undefined
          : (e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault()
                onClick?.()
              }
            }
      }
      title={title}
      className={cx(
        'flex items-center gap-[7px] rounded-[9px] border px-3 py-1.5 text-[12.5px] transition',
        selected
          ? 'border-accent bg-accent-dim text-accent-fg'
          : 'border-border2 bg-chip hover:border-accent',
        isStatic ? 'cursor-default hover:border-border2' : 'cursor-pointer',
      )}
    >
      <span className="font-[650] whitespace-nowrap">{children}</span>
      {sub !== undefined && (
        <span
          className={cx(
            'font-mono text-[11.5px] whitespace-nowrap',
            selected ? 'text-accent-fg' : 'text-muted',
          )}
        >
          {sub}
        </span>
      )}
    </div>
  )
}

/** 分段页签。 */
export function Tabs<T extends string>({
  value,
  onChange,
  items,
}: {
  value: T
  onChange: (v: T) => void
  items: { key: T; label: string }[]
}) {
  return (
    <div className="inline-flex flex-wrap gap-[3px] rounded-[9px] border border-border bg-chip p-[3px]">
      {items.map((it) => (
        <button
          key={it.key}
          onClick={() => onChange(it.key)}
          className={cx(
            'cursor-pointer rounded-[7px] border-none px-[13px] py-1 text-[12.5px] transition',
            value === it.key
              ? 'bg-card2 font-semibold text-fg'
              : 'bg-transparent text-muted hover:text-fg',
          )}
        >
          {it.label}
        </button>
      ))}
    </div>
  )
}

/* ---------------- 表格 / 空态 ---------------- */

export function Table({ head, children }: { head: ReactNode[]; children: ReactNode }) {
  return (
    <div className="-mx-1 overflow-x-auto">
      <table className="w-full border-collapse text-[13.5px]">
        <thead>
          <tr>
            {head.map((h, i) => (
              <th
                key={i}
                className="border-b border-border px-2.5 py-[11px] text-left text-xs font-semibold whitespace-nowrap text-muted"
              >
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="[&>tr:hover]:bg-hover [&>tr:last-child>td]:border-b-0">{children}</tbody>
      </table>
    </div>
  )
}

export function Td({
  children,
  className,
  align,
}: {
  children?: ReactNode
  className?: string
  align?: 'right'
}) {
  return (
    <td
      className={cx(
        'border-b border-border px-2.5 py-[11px] align-middle',
        align === 'right' && 'text-right',
        className,
      )}
    >
      {children}
    </td>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="px-2.5 py-8 text-center text-[13px] text-dim">{children}</div>
}

/* ---------------- 弹窗 ---------------- */

export function Modal({
  open,
  title,
  onClose,
  footer,
  wide,
  children,
}: {
  open: boolean
  title: ReactNode
  onClose: () => void
  footer?: ReactNode
  /** 宽版：日志、表格这类"内容本身很宽"的弹窗用，默认 640px */
  wide?: boolean
  children: ReactNode
}) {
  // 焦点管理：打开时把焦点移进弹窗，关闭时还回原处。
  //
  // 不做这件事时键盘用户按 Tab 会跑到背后的页面上去（弹窗视觉上盖住了内容，
  // 但 DOM 顺序里它是兄弟节点），而 Esc 关闭之后焦点落回 body，
  // 用户得从头 Tab 一遍才能回到刚才那个按钮。
  const boxRef = useRef<HTMLDivElement | null>(null)
  const returnFocusRef = useRef<HTMLElement | null>(null)
  const titleID = useId()

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
      if (e.key !== 'Tab') return
      // 焦点陷阱：只在弹窗内循环，不跑到背景上。
      const box = boxRef.current
      if (!box) return
      const focusable = box.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])',
      )
      if (focusable.length === 0) return
      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      const active = document.activeElement
      if (!e.shiftKey && active === last) {
        e.preventDefault()
        first.focus()
      } else if (e.shiftKey && (active === first || !box.contains(active))) {
        e.preventDefault()
        last.focus()
      }
    }
    window.addEventListener('keydown', onKey)
    // 弹窗打开时锁滚动，避免背景跟着滑
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    returnFocusRef.current = document.activeElement as HTMLElement | null
    // 等一帧再移焦点：此时弹窗子树已经挂载。
    const raf = window.requestAnimationFrame(() => {
      const box = boxRef.current
      if (!box) return
      const first = box.querySelector<HTMLElement>(
        'button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), a[href]',
      )
      ;(first ?? box).focus()
    })
    return () => {
      window.cancelAnimationFrame(raf)
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
      const back = returnFocusRef.current
      returnFocusRef.current = null
      if (back && document.contains(back) && typeof back.focus === 'function') back.focus()
    }
  }, [open, onClose])

  if (!open) return null
  return (
    <div
      className="fixed inset-0 z-60 flex items-start justify-center overflow-y-auto bg-[rgba(6,9,15,0.6)] px-4 py-10"
      onClick={onClose}
    >
      <div
        ref={boxRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        tabIndex={-1}
        className={cx(
          'w-full rounded-2xl border border-border2 bg-panel shadow-panel outline-none',
          wide ? 'max-w-[980px]' : 'max-w-[640px]',
        )}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center border-b border-border px-5 py-4">
          <h3 id={titleID} className="text-[15px] font-[650]">
            {title}
          </h3>
          <div className="flex-1" />
          <button
            onClick={onClose}
            className="cursor-pointer border-none bg-transparent text-xl leading-none text-muted hover:text-fg"
            aria-label="关闭"
          >
            ×
          </button>
        </div>
        <div className="px-5 py-5">{children}</div>
        {footer && (
          <div className="flex justify-end gap-2.5 border-t border-border px-5 py-3.5">{footer}</div>
        )}
      </div>
    </div>
  )
}

/** 弹窗内的分区（带序号圆圈）。 */
export function Section({
  no,
  title,
  desc,
  children,
}: {
  no?: number | string
  title: ReactNode
  desc?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="mb-3.5 rounded-[11px] border border-border bg-card px-4 py-3.5">
      <div className="mb-1 flex items-center gap-2 text-[13px] font-[650]">
        {no !== undefined && (
          <span className="inline-flex h-[19px] w-[19px] items-center justify-center rounded-full bg-accent-dim text-[11px] font-bold text-accent-fg">
            {no}
          </span>
        )}
        {title}
      </div>
      {desc && <div className="mb-2.5 text-xs text-dim">{desc}</div>}
      {children}
    </div>
  )
}

/* ---------------- 代码块 ---------------- */

/**
 * 等宽文本块（命令、日志）。
 *
 * `selectAll` 用于"要整段拿走"的内容（安装命令、卸载命令）：点一下即全选。
 * 手机上复制按钮可能被浏览器内核禁掉，用户只能长按 —— 而长按选中的范围
 * 取决于落点，很容易只选中半行。`select-all` 让整个块成为一个选择单元，
 * 长按就是全选，这是移动端最省事的兜底。
 */
export function Pre({
  children,
  className,
  selectAll,
  ref,
}: {
  children: ReactNode
  className?: string
  selectAll?: boolean
  ref?: React.Ref<HTMLPreElement>
}) {
  return (
    <pre
      ref={ref}
      className={cx(
        'm-0 overflow-x-auto rounded-[10px] border border-border bg-pre px-3.5 py-3 font-mono text-[12.5px] whitespace-pre-wrap break-all text-fg',
        selectAll && 'select-all',
        className,
      )}
    >
      {children}
    </pre>
  )
}

/** 一行「值 + 复制按钮」。 */
export function CopyRow({ value, onCopy }: { value: string; onCopy: () => void }) {
  return (
    <div className="flex items-center gap-2.5">
      <div className="min-w-0 flex-1 overflow-x-auto rounded-lg border border-border bg-pre px-2.5 py-[7px] font-mono text-[12.5px] whitespace-nowrap">
        {value || '—'}
      </div>
      <Button variant="ghost" size="sm" onClick={onCopy}>
        复制
      </Button>
    </div>
  )
}
