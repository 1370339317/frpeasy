import { useApp } from '../store'
import { cx } from './ui'

/** 轻量 toast：右下浮动，错误多留 2 秒。 */
export function Toasts() {
  const { toasts, dismissToast } = useApp()
  if (!toasts.length) return null
  return (
    <div className="pointer-events-none fixed bottom-6 left-1/2 z-200 flex -translate-x-1/2 flex-col items-center gap-2">
      {toasts.map((t) => (
        <div
          key={t.id}
          onClick={() => dismissToast(t.id)}
          className={cx(
            'pointer-events-auto max-w-[min(680px,92vw)] cursor-pointer rounded-[11px] border px-5 py-2.5 text-[13.5px] leading-[1.5] shadow-panel',
            t.kind === 'bad'
              ? 'border-bad bg-card2 text-bad-fg'
              : 'border-border2 bg-card2 text-fg',
          )}
        >
          {t.msg}
        </div>
      ))}
    </div>
  )
}
