import { useCallback, useEffect, useState } from 'react'
import { api, UnauthorizedError } from './lib/api'
import { AppProvider, useApp } from './store'
import { Login } from './components/Login'
import { Layout } from './components/Layout'
import { Toasts } from './components/Toasts'
import { Alert } from './components/ui'
import { isView, type View } from './views'
import { Overview } from './pages/Overview'
import { Tunnels } from './pages/Tunnels'
import { Clients } from './pages/Clients'
import { Frps } from './pages/Frps'
import { Files } from './pages/Files'
import { Ports } from './pages/Ports'
import { SettingsPage } from './pages/Settings'

/**
 * 视图与 URL hash 同步。
 *
 * 用 hash 而不是 History API：这样刷新页面能停在当前页，也不会给 Go 侧的
 * 静态文件服务增加「任意路径都要回退到 index.html」的要求 —— 二进制里的
 * FileServer 保持最简，没有 SPA fallback 这种容易出错的逻辑。
 */
function useHashView(): [View, (v: View) => void] {
  const read = (): View => {
    const h = window.location.hash.replace(/^#\/?/, '')
    return isView(h) ? h : 'overview'
  }
  const [view, setView] = useState<View>(read)

  useEffect(() => {
    const onHash = () => setView(read())
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  const go = useCallback((v: View) => {
    window.location.hash = `#/${v}`
    setView(v)
  }, [])

  return [view, go]
}

function Alerts() {
  const { me, tunnels } = useApp()
  const out: { tone: 'bad' | 'info' | 'warn'; text: React.ReactNode }[] = []
  if (me?.needs_password_change)
    out.push({
      tone: 'bad',
      text: (
        <>
          ⚠ 你还在用初始口令 <b>admin</b>。这台面板能开隧道直通内网，放到公网前请先到「设置」改掉。
        </>
      ),
    })
  if (me && !me.public_addr_configured)
    out.push({
      tone: 'info',
      text: '未设置「公网地址」，安装命令会自动用你当前访问面板的地址。装到公网服务器后建议到「设置」里显式填写。',
    })
  if (tunnels.some((t) => t.proxy_state === 'waiting_server'))
    out.push({
      tone: 'warn',
      text: 'frps 没在运行，所以这些隧道目前都不通（状态显示「等待服务端」而不是「在线」）。到「服务端 frps」页启动它。',
    })
  return (
    <>
      {out.map((a, i) => (
        <Alert key={i} tone={a.tone}>
          {a.text}
        </Alert>
      ))}
    </>
  )
}

function Shell({ onLogout }: { onLogout: () => void }) {
  const { ready, loadAll } = useApp()
  const [view, go] = useHashView()

  useEffect(() => {
    void loadAll()
  }, [loadAll])

  if (!ready) {
    return (
      <div className="flex h-screen items-center justify-center text-sm text-dim">加载中…</div>
    )
  }

  return (
    <Layout view={view} onNavigate={go} onLogout={onLogout} alerts={<Alerts />}>
      {view === 'overview' && <Overview />}
      {view === 'tunnels' && <Tunnels />}
      {view === 'clients' && <Clients />}
      {view === 'server' && <Frps />}
      {view === 'ports' && <Ports />}
      {view === 'files' && <Files />}
      {view === 'settings' && <SettingsPage />}
    </Layout>
  )
}

type Auth = 'checking' | 'in' | 'out'

export default function App() {
  const [auth, setAuth] = useState<Auth>('checking')

  // 会话探测：能拿到隧道列表就说明 cookie 还有效
  useEffect(() => {
    let alive = true
    ;(async () => {
      try {
        await api('GET', '/api/tunnels')
        if (alive) setAuth('in')
      } catch (e) {
        if (!alive) return
        setAuth(e instanceof UnauthorizedError ? 'out' : 'out')
      }
    })()
    return () => {
      alive = false
    }
  }, [])

  const logout = useCallback(async () => {
    try {
      await api('POST', '/api/logout')
    } catch {
      /* 登出失败也要回登录页 */
    }
    setAuth('out')
  }, [])

  if (auth === 'checking') {
    return <div className="flex h-screen items-center justify-center text-sm text-dim">连接面板…</div>
  }

  if (auth === 'out') {
    // 注意：这里不能挂 <Toasts /> —— 它依赖 AppProvider（useApp），
    // 而登录页在 Provider 之外，一旦渲染就直接抛异常、整页白屏。
    // 登录失败的信息由 Login 组件自己在表单里显示。
    return <Login onSuccess={() => setAuth('in')} />
  }

  return (
    <AppProvider onUnauthorized={() => setAuth('out')}>
      <Shell onLogout={() => void logout()} />
      <Toasts />
    </AppProvider>
  )
}
