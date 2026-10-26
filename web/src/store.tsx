import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { api, UnauthorizedError } from './lib/api'
import { applyTheme, readStoredTheme, type Theme } from './lib/theme'
import type {
  Agent,
  AccessHostSource,
  FrpsStatus,
  Me,
  NetworkResponse,
  Settings,
  Tunnel,
  TunnelsResponse,
  VersionsResponse,
} from './types'

/**
 * 全局数据层。
 *
 * 取代了旧单文件里的 `S` 大对象 + 一堆 renderXxx()：数据只有这一份，
 * 组件订阅自己需要的字段，后端一刷新所有消费方自动重渲染，
 * 不会再出现"改了设置但隧道页还是旧地址"这种漏渲染。
 */

// Android 只有一个入口。
//
// 原来分成 'android' 和 'androidRoot' 两个页签，两条命令唯一的差别是结尾有没有
// --elevate。但 Android 上的 agent 根本不做"安装"：不注册自启、不挂看护，
// 只是把二进制放下去跑起来（绿色运行）。所以"root 安装"这个说法本身是错的 ——
// --elevate 只是"以 root 身份运行"。要不要 root 是用户自己的事，
// 面板不该强迫他先选一个页签。CLI 的 --elevate 仍然保留，需要的人自己加。
export type CommandKind = 'linux' | 'win' | 'android'

interface ToastItem {
  id: number
  msg: string
  kind: 'ok' | 'bad'
}

interface AppData {
  me: Me | null
  tunnels: Tunnel[]
  agents: Agent[]
  frps: FrpsStatus | null
  settings: Settings | null
  versions: VersionsResponse | null
  net: NetworkResponse | null
  install: Record<CommandKind, string>
  /** 卸载命令：同样按平台分 */
  uninstall: Record<CommandKind, string>
  accessHost: string
  accessSource: AccessHostSource
  installBase: string
  panelPort: string
}

interface AppContextValue extends AppData {
  agentMap: Record<string, Agent>
  /** 数据是否至少加载过一次（用于首屏骨架） */
  ready: boolean
  loadAll: () => Promise<void>
  /** 返回最新隧道数组：轮询"是否全部恢复在线"时要用到返回值 */
  loadTunnels: () => Promise<Tunnel[]>
  loadAgents: () => Promise<void>
  loadFrps: () => Promise<void>
  loadMe: () => Promise<void>
  loadSettings: () => Promise<void>
  loadVersions: () => Promise<void>
  loadNet: () => Promise<void>
  /** 日夜主题：放在全局是因为顶栏和「设置」页都能切 */
  theme: Theme
  setTheme: (t: Theme) => void
  toast: (msg: string, kind?: 'ok' | 'bad') => void
  toasts: ToastItem[]
  dismissToast: (id: number) => void
  /** 会话失效时由 api 层抛出，这里转成登出 */
  onUnauthorized: () => void
}

const EMPTY_INSTALL: Record<CommandKind, string> = {
  linux: '',
  win: '',
  android: '',
}


const AppContext = createContext<AppContextValue | null>(null)

export function AppProvider({
  children,
  onUnauthorized,
}: {
  children: ReactNode
  onUnauthorized: () => void
}) {
  const [data, setData] = useState<AppData>({
    me: null,
    tunnels: [],
    agents: [],
    frps: null,
    settings: null,
    versions: null,
    net: null,
    install: EMPTY_INSTALL,
    uninstall: EMPTY_INSTALL,
    accessHost: '',
    accessSource: 'fallback',
    installBase: '',
    panelPort: '',
  })
  const [ready, setReady] = useState(false)
  const [theme, setThemeState] = useState<Theme>(() => readStoredTheme())
  const [toasts, setToasts] = useState<ToastItem[]>([])
  const toastId = useRef(0)

  // 主题落到 <html data-theme>，颜色由 CSS 变量驱动
  useEffect(() => {
    applyTheme(theme)
  }, [theme])

  const setTheme = useCallback((t: Theme) => setThemeState(t), [])

  const toast = useCallback((msg: string, kind: 'ok' | 'bad' = 'ok') => {
    const id = ++toastId.current
    setToasts((prev) => [...prev, { id, msg, kind }])
    // 错误信息给更长时间读；正常提示 2.4s 足够
    const ttl = kind === 'bad' ? 5200 : 2400
    window.setTimeout(() => setToasts((prev) => prev.filter((t) => t.id !== id)), ttl)
  }, [])

  const dismissToast = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id))
  }, [])

  const guard = useCallback(
    async <T,>(fn: () => Promise<T>): Promise<T | undefined> => {
      try {
        return await fn()
      } catch (e) {
        if (e instanceof UnauthorizedError) {
          onUnauthorized()
          return undefined
        }
        throw e
      }
    },
    [onUnauthorized],
  )

  const loadMe = useCallback(async () => {
    const me = await guard(() => api<Me>('GET', '/api/me'))
    if (me) setData((d) => ({ ...d, me }))
  }, [guard])

  /** 一次拿全：隧道 + 客户端 + frps 状态 + 四种安装命令 + 公网入口推导结果 */
  const loadTunnels = useCallback(async (): Promise<Tunnel[]> => {
    const r = await guard(() => api<TunnelsResponse>('GET', '/api/tunnels'))
    if (!r) return []
    setData((d) => ({
      ...d,
      tunnels: r.tunnels ?? [],
      // 必须用 `?? []` 而不是 `?? d.agents`：
      // Go 的空切片序列化成 null，`?? d.agents` 会把 null 当成「没返回」，
      // 于是保留旧列表 —— 删掉最后一台客户端后界面还显示着它，必须手动刷新。
      // 这个坑在 tunnels 上也踩过，两处统一成 `?? []`。
      agents: r.agents ?? [],
      frps: r.frps ?? d.frps,
      install: {
        linux: r.install_command || '',
        win: r.install_command_windows || '',
        android: r.install_command_android || '',
      },
      uninstall: {
        linux: r.uninstall_command || '',
        win: r.uninstall_command_windows || '',
        android: r.uninstall_command_android || '',
        // Android 的卸载与是否 root 无关（同一个脚本、同一套目录），复用一条
      },
      accessHost: r.access_host || '',
      accessSource: r.access_host_source || 'fallback',
      installBase: r.install_base || '',
      panelPort: r.panel_listen_port || '',
    }))
    return r.tunnels ?? []
  }, [guard])

  const loadAgents = useCallback(async () => {
    const r = await guard(() => api<{ agents: Agent[] }>('GET', '/api/agents'))
    if (r) setData((d) => ({ ...d, agents: r.agents ?? [] }))
  }, [guard])

  const loadFrps = useCallback(async () => {
    const r = await guard(() => api<FrpsStatus>('GET', '/api/frps'))
    if (r) setData((d) => ({ ...d, frps: r }))
  }, [guard])

  const loadSettings = useCallback(async () => {
    const r = await guard(() => api<Settings>('GET', '/api/settings'))
    if (r) setData((d) => ({ ...d, settings: r }))
  }, [guard])

  const loadVersions = useCallback(async () => {
    const r = await guard(() => api<VersionsResponse>('GET', '/api/system/versions'))
    if (r) setData((d) => ({ ...d, versions: r }))
  }, [guard])

  const loadNet = useCallback(async () => {
    try {
      const r = await guard(() => api<NetworkResponse>('GET', '/api/system/network'))
      if (r) setData((d) => ({ ...d, net: r }))
    } catch {
      setData((d) => ({ ...d, net: { nics: [], public_ip: '' } }))
    }
  }, [guard])

  const loadAll = useCallback(async () => {
    // 并行拉，任一失败不影响其它（guard 已把 401 转成登出）
    await Promise.all([
      loadMe(),
      // 显式拉一次客户端列表：不能指望 /api/tunnels 顺带回来的那份 ——
      // 之前就是这样，一旦那份是空的（null）就会保留旧值，删完客户端界面不更新。
      loadAgents(),
      loadTunnels().catch((e) => toast(String((e as Error).message), 'bad')),
      loadSettings(),
    ])
    setReady(true)
    // 版本列表要联网查 GitHub，慢，放最后不阻塞首屏
    void loadVersions()
  }, [loadMe, loadAgents, loadTunnels, loadSettings, loadVersions, toast])

  // 全局状态轮询。
  //
  // 面板不推事件，而「客户端在不在线」「隧道通不通」「frps 在不在跑」一直在变。
  // 原来只有客户端页和 frps 页各自写了轮询，概览页和隧道页**完全没有** ——
  // 用户停在概览上看到「1 台在线」，那台机器可能一小时前就离线了，只能手刷。
  // 各页自写还有两个副作用：间隔互不相同；frps 那个还带 40 次上限（3 秒 × 40），
  // 两分钟后就不再刷新，界面从此静止。
  //
  // 现在统一在这里跑，所有页面共享同一份新鲜数据。
  useEffect(() => {
    if (!ready) return
    let alive = true
    const tick = () => {
      if (!alive || document.visibilityState !== 'visible') return
      void loadFrps()
      // loadTunnels 一次带回隧道状态 + 客户端列表 + frps 概要，
      // 比分别调 /api/tunnels 和 /api/agents 省一个请求。
      void loadTunnels().catch(() => {})
    }
    const h = window.setInterval(tick, 5000)
    // 从后台切回前台立刻补一次：后台标签的定时器会被浏览器节流到分钟级，
    // 不补这一下，用户切回来看到的就是几分钟前的状态。
    const onVis = () => {
      if (document.visibilityState === 'visible') tick()
    }
    document.addEventListener('visibilitychange', onVis)
    return () => {
      alive = false
      window.clearInterval(h)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [ready, loadFrps, loadTunnels])

  const agentMap = useMemo(() => {
    const m: Record<string, Agent> = {}
    for (const a of data.agents) m[a.id] = a
    return m
  }, [data.agents])

  const value = useMemo<AppContextValue>(
    () => ({
      ...data,
      agentMap,
      ready,
      loadAll,
      loadTunnels,
      loadAgents,
      loadFrps,
      loadMe,
      loadSettings,
      loadVersions,
      loadNet,
      toast,
      toasts,
      dismissToast,
      onUnauthorized,
      theme,
      setTheme,
    }),
    [
      data,
      agentMap,
      ready,
      loadAll,
      loadTunnels,
      loadAgents,
      loadFrps,
      loadMe,
      loadSettings,
      loadVersions,
      loadNet,
      toast,
      toasts,
      dismissToast,
      onUnauthorized,
      theme,
      setTheme,
    ],
  )

  return <AppContext.Provider value={value}>{children}</AppContext.Provider>
}

export function useApp(): AppContextValue {
  const ctx = useContext(AppContext)
  if (!ctx) throw new Error('useApp 必须在 AppProvider 内使用')
  return ctx
}
