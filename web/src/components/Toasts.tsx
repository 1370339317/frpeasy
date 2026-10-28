import { useApp } from '../store'
import { cx } from './ui'

/** 轻量 toast：底部居中浮动，错误多留 2 秒。 */
export function Toasts() {
  const { toasts, dismissToast } = useApp()
  if (!toasts.length) return null
  return (
    // 手机上必须左右留边且占满可用宽度：原先靠内容撑开，一句
    // 「浏览器不允许自动复制：已为你全选，长按即可复制」会被压成 5~6 行，
    // 挤在屏幕中间一小块，读起来很费劲。
    <div className="pointer-events-none fixed bottom-4 left-1/2 z-200 flex w-full max-w-[min(680px,92vw)] -translate-x-1/2 flex-col items-stretch gap-2 px-1 max-[820px]:bottom-3">
      {toasts.map((t) => (
        <div
          key={t.id}
          onClick={() => dismissToast(t.id)}
          className={cx(
            'pointer-events-auto cursor-pointer rounded-[11px] border px-5 py-2.5 text-[13.5px] leading-[1.5] shadow-panel max-[820px]:px-4 max-[820px]:text-[13px]',
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
