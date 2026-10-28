import { useEffect, useState, type ReactNode } from 'react'
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
 *
 * 手机（≤820px）上侧栏改成抽屉：原先是压到 64px 宽的图标条，结果底部
 * 那段"版本 · 构建时间"被挤进 64px 里逐字竖排，顶栏也放不下标题+徽章+用户名，
 * 整个页面在 393px 宽的手机上几乎没法看。抽屉让内容拿到全宽，代价是多一次点击。
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

  // 窄屏抽屉。桌面端完全不受影响：下面所有 max-[820px]: 类只在窄屏生效。
  const [navOpen, setNavOpen] = useState(false)
  const go = (v: View) => {
    onNavigate(v)
    setNavOpen(false)
  }
  // 视口放大回桌面时把抽屉状态清掉，否则下次缩窄会直接是打开态。
  useEffect(() => {
    const mq = window.matchMedia('(min-width: 821px)')
    const on = () => {
      if (mq.matches) setNavOpen(false)
    }
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])

  return (
    <div className="flex h-screen overflow-hidden max-[820px]:h-[100dvh]">
      {/* 抽屉遮罩：点一下关闭 */}
      {navOpen && (
        <div
          className="fixed inset-0 z-30 bg-[rgba(6,9,15,0.55)] min-[821px]:hidden"
          onClick={() => setNavOpen(false)}
          aria-hidden="true"
        />
      )}

      <aside
        className={cx(
          'flex w-[200px] flex-none flex-col border-r border-border bg-panel',
          // 窄屏：固定定位的抽屉，从左侧滑入
          'max-[820px]:fixed max-[820px]:inset-y-0 max-[820px]:left-0 max-[820px]:z-40 max-[820px]:w-[216px] max-[820px]:shadow-panel max-[820px]:transition-transform max-[820px]:duration-200',
          navOpen ? 'max-[820px]:translate-x-0' : 'max-[820px]:-translate-x-full',
        )}
      >
        <div className="flex h-14 items-center gap-2.5 border-b border-border px-[18px] font-bold tracking-[0.4px]">
          <span className="h-2 w-2 rounded-full bg-accent shadow-[0_0_10px_var(--accent)]" />
          <span>frpeasy</span>
          <div className="flex-1" />
          {/* 抽屉里的关闭按钮，仅窄屏 */}
          <button
            onClick={() => setNavOpen(false)}
            aria-label="收起菜单"
            className="hidden cursor-pointer border-none bg-transparent text-lg leading-none text-muted hover:text-fg max-[820px]:block"
          >
            ×
          </button>
        </div>

        {/* 分组渲染 + 可滚动：条目以后只会更多，窗口不高时不能让下面的
            组直接掉出屏幕（那会变成「菜单怎么不见了」）。 */}
        <nav className="flex min-h-0 flex-1 flex-col gap-3.5 overflow-y-auto px-2.5 py-2.5">
          {VIEW_GROUPS.map((g) => (
            <div key={g.key}>
              <div className="mb-1 px-3 text-[10.5px] font-semibold tracking-[0.1em] text-dim uppercase">
                {g.title}
              </div>
              <div className="flex flex-col gap-0.5">
                {g.items.map((it) => (
                  <button
                    key={it.key}
                    onClick={() => go(it.key)}
                    data-view={it.key}
                    title={it.label}
                    className={cx(
                      'flex w-full cursor-pointer items-center gap-2.5 rounded-[9px] border-none px-3 py-[9px] text-left text-[13.5px] transition max-[820px]:py-3',
                      view === it.key
                        ? 'bg-accent-dim font-semibold text-accent-fg'
                        : 'bg-transparent text-muted hover:bg-card2 hover:text-fg',
                    )}
                  >
                    <span className="w-[18px] text-center text-sm opacity-90">{it.icon}</span>
                    <span>{it.label}</span>
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
        <div className="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border bg-panel px-6 max-[820px]:gap-2 max-[820px]:px-3">
          {/* 汉堡：仅窄屏。用外层 span 控制显隐 —— IconButton 自带 inline-flex，
              直接在本体上叠 hidden 会和它抢 display。 */}
          <span className="hidden max-[820px]:block">
            <IconButton onClick={() => setNavOpen(true)} title="打开菜单">
              ☰
            </IconButton>
          </span>
          <h1 className="text-base font-[650] whitespace-nowrap">{VIEW_TITLES[view]}</h1>
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
          {/* 手机上用户名让位给状态徽章：多一个字符标题就被挤换行 */}
          <span className="text-[12.5px] text-dim max-[520px]:hidden">{me?.user ?? ''}</span>
          <IconButton
            onClick={() => setTheme(theme === 'light' ? 'dark' : 'light')}
            title="切换日/夜主题"
          >
            {theme === 'light' ? '☾' : '☀'}
          </IconButton>
          <Button variant="ghost" size="sm" onClick={onLogout} className="whitespace-nowrap">
            退出
          </Button>
        </div>

        <div className="max-w-[1180px] px-6 pt-[22px] pb-16 max-[820px]:px-3 max-[820px]:pt-4 max-[820px]:pb-12">
          {alerts}
          {children}
        </div>
      </main>
    </div>
  )
}
