import type { ReactNode } from 'react'
import { useApp } from '../store'
import { fmtBuildTime } from '../lib/format'
import { Button, IconButton, Led, cx } from './ui'
import { VIEW_GROUPS, VIEW_TITLES, type View } from '../views'
import { ownBundleHash } from '../lib/uidrift'

/**
 * 应用外壳：左侧导航 + 顶栏 + 内容区。
 *
 * 左下角固定显示「版本 · 构建时间」——前端是编译期嵌进二进制的，
 * 构建时间没变就说明跑的还是旧二进制，这是分辨"功能没做"和"没重新编译"
 * 唯一的直接证据，必须一直可见。
 */
export function Layout({
  view,
  onNavigate,
  onLogout,
  alerts,
  children,
}: {
  view: View
  onNavigate: (v: View) => void
  onLogout: () => void
  alerts: ReactNode
  children: ReactNode
}) {
  const { me, frps, tunnels, theme, setTheme } = useApp()
  const frpsOn = !!frps?.running && !!frps.port_alive
  const tone = frps?.starting ? 'warn' : frpsOn ? 'ok' : 'bad'
  const frpsLabel = frps?.starting ? '拉取中' : frpsOn ? '运行中' : '已停止'

  return (
    <div className="flex h-screen overflow-hidden">
      <aside className="flex w-[200px] flex-none flex-col border-r border-border bg-panel max-[820px]:w-16">
        <div className="flex h-14 items-center gap-2.5 border-b border-border px-[18px] font-bold tracking-[0.4px] max-[820px]:justify-center max-[820px]:px-0">
          <span className="h-2 w-2 rounded-full bg-accent shadow-[0_0_10px_var(--accent)]" />
          <span className="max-[820px]:hidden">frpeasy</span>
        </div>

        {/* 分组渲染 + 可滚动：条目以后只会更多，窗口不高时不能让下面的
            组直接掉出屏幕（那会变成「菜单怎么不见了」）。窄屏只留图标时
            分组标题隐藏，但 title 属性保证悬停仍能看清是什么。 */}
        <nav className="flex min-h-0 flex-1 flex-col gap-3.5 overflow-y-auto px-2.5 py-2.5">
          {VIEW_GROUPS.map((g) => (
            <div key={g.key}>
              <div className="mb-1 px-3 text-[10.5px] font-semibold tracking-[0.1em] text-dim uppercase max-[820px]:hidden">
                {g.title}
              </div>
              <div className="flex flex-col gap-0.5">
                {g.items.map((it) => (
                  <button
                    key={it.key}
                    onClick={() => onNavigate(it.key)}
                    data-view={it.key}
                    title={it.label}
                    className={cx(
                      'flex w-full cursor-pointer items-center gap-2.5 rounded-[9px] border-none px-3 py-[9px] text-left text-[13.5px] transition',
                      view === it.key
                        ? 'bg-accent-dim font-semibold text-accent-fg'
                        : 'bg-transparent text-muted hover:bg-card2 hover:text-fg',
                    )}
                  >
                    <span className="w-[18px] text-center text-sm opacity-90">{it.icon}</span>
                    <span className="max-[820px]:hidden">{it.label}</span>
                  </button>
                ))}
              </div>
            </div>
          ))}
        </nav>

        <div className="mt-auto border-t border-border p-3 text-xs text-dim">
          <div className="flex items-center gap-1.5">
            <Led tone={tone} />
            frps {frpsLabel} · {tunnels.length} 条隧道
          </div>
          <div
            className="mt-1.5 border-t border-dashed border-border pt-1.5 font-mono text-[10.5px] leading-[1.5] break-all opacity-80"
            title={
              '面板 = 服务器上那个二进制的版本（来自 /api/me）；' +
              '界面 = 当前这个标签页实际在跑的前端产物哈希。' +
              '两者对不上就说明页面是旧的（通常发生在面板刚更新之后），刷新一次即可。'
            }
          >
            {me?.panel_version ?? 'dev'} · {fmtBuildTime(me?.panel_built ?? '')}
            <br />
            界面 {ownBundleHash()}
            {me?.ui_source === 'dir' ? ' · UI取磁盘' : ''}
          </div>
        </div>
      </aside>

      <main className="min-w-0 flex-1 overflow-y-auto">
        <div className="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border bg-panel px-6">
          <h1 className="text-base font-[650]">{VIEW_TITLES[view]}</h1>
          <div className="flex-1" />
          <span
            className={cx(
              'inline-flex items-center gap-[5px] rounded-full px-2.5 py-0.5 text-[11.5px] font-[650] whitespace-nowrap',
              tone === 'ok'
                ? 'bg-ok-dim text-ok-fg'
                : tone === 'warn'
                  ? 'bg-warn-dim text-warn-fg'
                  : 'bg-bad-dim text-bad-fg',
            )}
          >
            frps {frpsLabel}
          </span>
          <span className="text-[12.5px] text-dim">{me?.user ?? ''}</span>
          <IconButton
            onClick={() => setTheme(theme === 'light' ? 'dark' : 'light')}
            title="切换日/夜主题"
          >
            {theme === 'light' ? '☾' : '☀'}
          </IconButton>
          <Button variant="ghost" size="sm" onClick={onLogout}>
            退出
          </Button>
        </div>

        <div className="max-w-[1180px] px-6 pt-[22px] pb-16">
          {alerts}
          {children}
        </div>
      </main>
    </div>
  )
}
