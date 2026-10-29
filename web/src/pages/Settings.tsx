import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { compareVersions, copyWithHint } from '../lib/format'
import {
  Alert,
  Button,
  Card,
  Chip,
  Field,
  Hint,
  Input,
  cx,
} from '../components/ui'
import type {
  ProbeResult,
  RankInfo,
  Settings as SettingsShape,
  SettingsBody,
  SettingsSaveResult,
  DiagnoseResponse,
  SourceEvent,
  SourceEventsResponse,
  TuningCongestion,
  TuningResponse,
} from '../types'

// 内置候选源。用户也可以自己填前缀 —— 镜像会挂、会改名，写死清单不现实。
const SOURCE_PRESETS: { token: string; label: string; note: string }[] = [
  { token: 'panel', label: '面板中继', note: '只有它时客户端不出网' },
  { token: 'github', label: 'GitHub 直连', note: '无前缀' },
  { token: 'https://ghfast.top', label: 'ghfast', note: '国内快' },
  { token: 'https://ghproxy.com', label: 'ghproxy', note: '' },
  { token: 'https://gh-proxy.com', label: 'gh-proxy', note: '' },
]

/** 把一个 token 说成人话。 */
function sourceLabel(tok: string): string {
  if (tok === 'panel') return '面板中继'
  if (tok === 'github') return 'GitHub 直连'
  try {
    return new URL(tok).host
  } catch {
    return tok
  }
}

function parseSources(v: string): string[] {
  return v
    .split(',')
    .map((x) => x.trim().replace(/\/+$/, ''))
    .filter(Boolean)
}

/**
 * 有序源列表编辑器。
 *
 * 用"上移/下移"而不是拖拽：拖拽在窄屏和键盘操作下都难用，而这里最多四项 ——
 * 点两下就调好了，不值得为它引入一个拖拽库和一堆边界情况。
 */
function SourceListEditor({
  value,
  onChange,
  allowPanel,
  probe,
}: {
  value: string
  onChange: (v: string) => void
  allowPanel: boolean
  probe: Record<string, ProbeResult>
}) {
  const list = parseSources(value)
  const set = (next: string[]) => onChange(next.join(','))
  const { toast } = useApp()
  const move = (i: number, d: number) => {
    const next = [...list]
    const j = i + d
    if (j < 0 || j >= next.length) return
    ;[next[i], next[j]] = [next[j], next[i]]
    set(next)
  }
  const remove = (i: number) => set(list.filter((_, k) => k !== i))
  // 加一项时当场拦下两种"加了但没生效"：重复的源（服务端会去重，界面却显示两行）
  // 和缺协议的写法（旧代码直接 return，点了什么提示都没有）。
  const add = (raw: string): boolean => {
    const v = raw.trim().replace(/\/+$/, '')
    if (!v) return false
    if (!/^https?:\/\//.test(v)) {
      toast('前缀要写完整，以 http:// 或 https:// 开头，例如 https://ghfast.top', 'bad')
      return false
    }
    if (list.includes(v)) {
      toast('这个源已经在列表里了', 'bad')
      return false
    }
    set([...list, v])
    return true
  }
  const [custom, setCustom] = useState('')
  const addable = SOURCE_PRESETS.filter(
    (p) => (allowPanel || p.token !== 'panel') && !list.includes(p.token),
  )

  return (
    <div>
      <div className="overflow-hidden rounded-[9px] border border-border">
        {list.length === 0 && (
          <div className="px-3 py-2.5 text-[12.5px] text-bad-fg">
            至少要有一个源，否则客户端无从取到任何文件。
          </div>
        )}
        {list.map((tok, i) => {
          const r = probe[tok]
          return (
            <div
              key={tok}
              className={cx(
                'flex items-center gap-2 px-3 py-1.5 text-[12.5px]',
                i > 0 && 'border-t border-border',
              )}
            >
              <span className="w-4 shrink-0 text-dim">{i + 1}</span>
              <span className="shrink-0 font-medium">{sourceLabel(tok)}</span>
              {tok !== 'panel' && tok !== 'github' && (
                <span className="min-w-0 flex-1 truncate font-mono text-[11.5px] text-dim">{tok}</span>
              )}
              {r && (
                // 不只是速度：延迟、抖动、综合分都要露出来。
                // 只显示 "483KB/s" 时，一个忽快忽慢的源看起来和稳定的一样好，
                // 而实际下载卡住的往往正是前者。
                <span
                  className={cx(
                    'ml-auto shrink-0 whitespace-nowrap text-[11.5px] tabular-nums',
                    r.ok || r.skipped ? 'text-dim' : 'text-bad-fg',
                  )}
                  title={
                    r.skipped
                      ? `${r.label}：走面板自己，没有可测的外部地址`
                      : r.ok
                        ? `${r.label}：首字节 ${r.ttfb_ms}ms，速度 ${Math.round(r.speed_kbps)}KB/s，抖动 ${r.jitter}，评分 ${r.score}`
                        : `${r.label}：${r.error || '不可用'}`
                  }
                >
                  {r.skipped
                    ? '不用测'
                    : r.ok
                      ? `${Math.round(r.speed_kbps)}KB/s · ${r.ttfb_ms}ms · 抖${r.jitter} · ${r.score}分`
                      : '不可用'}
                </span>
              )}
              <span className="ml-auto flex shrink-0 items-center gap-1">
                <Btn title="上移" disabled={i === 0} onClick={() => move(i, -1)}>↑</Btn>
                <Btn title="下移" disabled={i === list.length - 1} onClick={() => move(i, 1)}>↓</Btn>
                <Btn title="移除" disabled={list.length <= 1} onClick={() => remove(i)}>✕</Btn>
              </span>
            </div>
          )
        })}
      </div>

      <div className="mt-2 flex flex-wrap items-center gap-2">
        {addable.map((p) => (
          <Chip key={p.token} sub={p.note} onClick={() => set([...list, p.token])}>
            + {p.label}
          </Chip>
        ))}
        <Input
          value={custom}
          onChange={(e) => setCustom(e.target.value)}
          placeholder="自定义前缀，如 https://your-mirror/"
          className="max-w-[260px]"
        />
        <Button
          variant="ghost"
          size="sm"
          disabled={!custom.trim()}
          onClick={() => {
            if (add(custom)) setCustom('')
          }}
        >
          添加
        </Button>
      </div>
    </div>
  )
}

/** 列表行里的小按钮（上移/下移/移除）。 */
function Btn({
  children,
  onClick,
  disabled,
  title,
}: {
  children: ReactNode
  onClick: () => void
  disabled?: boolean
  title: string
}) {
  return (
    <button
      type="button"
      title={title}
      disabled={disabled}
      onClick={onClick}
      className={cx(
        'h-[22px] w-[22px] rounded-[5px] border border-border bg-card2 text-[12px] leading-none',
        disabled ? 'cursor-not-allowed opacity-40' : 'cursor-pointer text-dim hover:text-fg',
      )}
    >
      {children}
    </button>
  )
}

/**
 * 表单草稿：留在模块作用域里，切走页面（组件卸载）再回来不会丢。
 *
 * 设置页的输入是"一次性工作"：加一个源、再去别的页面看一眼隧道、回来继续 ——
 * 这是最自然的操作。而 SPA 切页会卸载组件，所有 useState 一起消失，
 * 表现就是「刚加的那个源没了」「刚才测出来的数据没了」，且没有任何提示。
 * 草稿只影响显示，**不写库**；要不要落库仍然只由「保存」决定。
 */
type SettingsDraft = {
  publicAddr: string
  serverSources: string
  clientSources: string
  pStart: string
  pEnd: string
  bindPort: string
  dashPort: string
  mode: string
  probe: Record<string, ProbeResult>
  clientProbe: Record<string, ProbeResult>
  // 隧道传输层（'1'/'0' 与库里的写法保持一致，避免两套表示来回转换）
  tcpMux: string
  poolCount: string
  compress: string
  speedEnabled: string
  speedInterval: string
}

let draft: SettingsDraft | null = null

/**
 * 设置项的归属分区。
 *
 * 每一块都搬到"用它的那个页面"上去了，这里只留全局项（公网地址 / 外观 / 口令）。
 * 判断标准只有一条：**用户产生这个疑问时，正站在哪个页面**。
 *   - 「隧道为什么慢」→ 在隧道页 → 传输参数就放隧道页
 *   - 「服务器带宽够不够」→ 在 frps 页 → 自检与内核调优就放 frps 页
 *   - 「这台机器到面板多快」→ 在客户端页 → 测速策略就放客户端页
 * 放在设置页等于要求用户先知道"该去哪"，而那正是他缺的信息。
 */
export type SettingsSection = 'global' | 'tunnel' | 'server' | 'client' | 'files' | 'ports'

export function SettingsPage({ section = 'global' }: { section?: SettingsSection }) {
  const {
    settings,
    net,
    theme,
    setTheme,
    agents,
    loadSettings,
    loadMe,
    loadTunnels,
    loadNet,
    toast,
  } = useApp()

  // 表单是受控的：只在 settings 真的刷新时回填，避免每次渲染把用户输入冲掉。
  // 初值优先取草稿（上次离开这个页面时的样子），其次才是库里的默认。
  const [publicAddr, setPublicAddr] = useState(draft?.publicAddr ?? '')
  // 两个源列表（有序、逗号分隔）。以前的单值 mirror_base 已被它们取代。
  const [serverSources, setServerSources] = useState(draft?.serverSources ?? 'github')
  const [clientSources, setClientSources] = useState(draft?.clientSources ?? 'panel')
  // 逐项测速结果，key 是源 token。两个列表各一份 ——
  // ① 是面板自己的链路，测完可以据此重排；② 测的是"这个前缀能不能取出文件"，
  // 结论以面板网络为准，**不重排**（排序交给客户端按自己的网络测）。
  const [probe, setProbe] = useState<Record<string, ProbeResult>>(draft?.probe ?? {})
  const [clientProbe, setClientProbe] = useState<Record<string, ProbeResult>>(draft?.clientProbe ?? {})
  // 下载策略。auto = 每次真要下载前临时测速排优（推荐）；manual = 严格按列表顺序。
  const [mode, setMode] = useState(draft?.mode ?? 'manual')
  // 最近一次排优结果 + 最近的排优/下载记录。都是"事后追查"用的，
  // 所以从接口读而不是只依赖本次页面会话里点了什么。
  const [rank, setRank] = useState<RankInfo | null>(null)
  // 客户端列表那侧的「上次测速」（面板经该前缀取文件的结果），与上面那份分开存。
  const [clientRank, setClientRank] = useState<RankInfo | null>(null)
  const [events, setEvents] = useState<SourceEvent[]>([])
  const [pStart, setPStart] = useState(draft?.pStart ?? '')
  const [pEnd, setPEnd] = useState(draft?.pEnd ?? '')
  const [bindPort, setBindPort] = useState(draft?.bindPort ?? '')
  const [dashPort, setDashPort] = useState(draft?.dashPort ?? '')
  // 隧道传输层：三个值都由面板统一下发给客户端（frp 的 tcpMux 两端必须一致，
  // 所以它只能有一个来源）。初值同样优先取草稿。
  const [tcpMux, setTcpMux] = useState<'1' | '0'>((draft?.tcpMux as '1' | '0') ?? '1')
  const [poolCount, setPoolCount] = useState(draft?.poolCount ?? '0')
  const [compress, setCompress] = useState<'1' | '0'>((draft?.compress as '1' | '0') ?? '1')
  const [tuning, setTuning] = useState<TuningResponse | null>(null)
  const [tuningBusy, setTuningBusy] = useState(false)
  // 客户端测速调度。每次拉 1MB，间隔直接决定流量成本，所以跟着一起在这里配。
  const [speedEnabled, setSpeedEnabled] = useState<'1' | '0'>(
    (draft?.speedEnabled as '1' | '0') ?? '1',
  )
  const [speedInterval, setSpeedInterval] = useState(draft?.speedInterval ?? '30')
  const [speedBusy, setSpeedBusy] = useState(false)
  // 链路自检结果。一次约 12 秒（两个目标各 6 秒上限），期间按钮转圈。
  const [diag, setDiag] = useState<DiagnoseResponse | null>(null)
  const [diagBusy, setDiagBusy] = useState(false)
  const [token, setToken] = useState('')
  const [pass, setPass] = useState('')
  const [saving, setSaving] = useState(false)
  // 上一次保存失败的原因。
  //
  // 只用 toast 是不够的：toast 几秒后消失，用户切个页面回来就看不到发生了什么，
  // 只剩下"我明明点了保存，怎么刷新后还是旧值"。这类反馈必须留在页面上，
  // 直到用户看见或下次保存成功为止 —— 它对应的正是"保存了但没生效"的困惑。
  const [saveError, setSaveError] = useState('')
  const [testing, setTesting] = useState(false)
  const [testingClient, setTestingClient] = useState(false)

  // 每次改动都记进草稿。切页面回来时从这里恢复，而不是从库里 ——
  // 用户没点保存的输入不该因为换了个页面就消失。
  useEffect(() => {
    draft = {
      publicAddr,
      serverSources,
      clientSources,
      pStart,
      pEnd,
      bindPort,
      dashPort,
      mode,
      probe,
      clientProbe,
      tcpMux,
      poolCount,
      compress,
      speedEnabled,
      speedInterval,
    }
  }, [
    publicAddr,
    serverSources,
    clientSources,
    pStart,
    pEnd,
    bindPort,
    dashPort,
    mode,
    probe,
    clientProbe,
  ])

  // 表单只在「第一次拿到设置」时回填一次。
  //
  // 早期写法是 useEffect(..., [settings]) —— 而 store 每次 loadSettings 都会
  // 放一个**新的对象**进来，于是依赖每次都变、表单被反复重置。用户正在输入
  // 公网地址时，只要期间发生过一次设置刷新（切页面回来、保存别的项），
  // 输入框就被冲回库里的旧值；如果那次保存发生在这之后，提交上去的就是空。
  // 用户看到的现象就是「填了没生效 / 更新后配置消失」。
  //
  // hydrated 标记保证：除非保存成功后再显式重新回填，否则不动用户的输入。
  // 有草稿时直接当作已回填：草稿就是用户正在编辑的那份。
  const hydrated = useRef(draft !== null)

  // fillForm 把「库里的值」灌进表单。抽出来是为了让「放弃修改」也能复用：
  // 那个按钮原来只是重新 loadSettings，而 loadSettings 会因为 hydrated 已置位
  // 不再回填，于是点了没反应。
  const fillForm = useCallback((s: SettingsShape) => {
    setPublicAddr(s.public_addr ?? '')
    setMode(s.source_mode || 'manual')
    setServerSources(s.server_sources || 'github')
    setClientSources(s.client_sources || 'panel')
    setPStart(s.port_range_start ?? '')
    setPEnd(s.port_range_end ?? '')
    setBindPort(s.frps_bind_port ?? '')
    setDashPort(s.dashboard_port ?? '')
    // 库里的默认是 '1'；写成 '0' 才算关闭，其它任何值都当作开（保守方向）
    setTcpMux((s.tunnel_tcp_mux ?? '1') === '0' ? '0' : '1')
    setPoolCount(s.tunnel_pool_count ?? '0')
    setCompress((s.tunnel_compression ?? '1') === '0' ? '0' : '1')
    setSpeedEnabled((s.speedtest_enabled ?? '1') === '0' ? '0' : '1')
    setSpeedInterval(s.speedtest_interval_min || '30')
    setToken('')
    setPass('')
  }, [])

  useEffect(() => {
    if (!settings || hydrated.current) return
    hydrated.current = true
    fillForm(settings)
  }, [settings, fillForm])

  useEffect(() => {
    void loadNet()
  }, [loadNet])

  // 系统调优现状（当前拥塞算法 / BBR 是否可用）。
  //
  // 单独一个接口：它读 /proc，和设置 KV 不是一回事。老面板没有它，
  // 所以这里失败要静默 —— 不能因为多问了一个新接口就让整个设置页报错。
  const loadTuning = useCallback(async () => {
    try {
      setTuning(await api<TuningResponse>('GET', '/api/system/tuning'))
    } catch {
      /* 老版本面板没有该接口，忽略 */
    }
  }, [])

  useEffect(() => {
    void loadTuning()
  }, [loadTuning])

  // ---- 隧道传输层的「生效状态」与「护栏状态」 ----
  //
  // 这两个值解决同一类困惑："我保存了，到底生效没有？为什么保存不了？"
  // 之前只有表单值和一句 toast，用户没法自己判断，只能反复刷新猜。
  const muxMinVer = tuning?.min_agent_version ?? ''

  /** 版本不足以安全关闭 mux 的客户端（与后端 staleAgentsForTransport 同一判据）。 */
  const staleForMux = (() => {
    if (!muxMinVer) return []
    return (agents ?? []).filter((a) => {
      const v = (a.agent_version || '').trim().replace(/^v/i, '')
      // 版本号认不出来（dev / 本地构建）同样算不合格 —— 后端也是这么判的：
      // 不赌一个未知版本的解码行为。
      if (!/^\d+\.\d+/.test(v)) return true
      return compareVersions(v, muxMinVer) < 0
    })
  })()

  /** 当前**已经生效**的传输层参数（frps 正在用的那一份）。 */
  const applied = tuning?.transport

  /** 表单与生效值是否有差异 —— 有就说明"改了但还没保存"。 */
  const transportDirty =
    !!applied &&
    ((applied.tcp_mux ? '1' : '0') !== tcpMux ||
      (applied.compression ? '1' : '0') !== compress ||
      (tcpMux === '0' && applied.pool_count !== (Number.parseInt(poolCount, 10) || 0)))

  // 切换拥塞控制。
  //
  // 会写 /etc/sysctl.d/99-frpeasy-bbr.conf 并立即生效 —— 这是真正的系统变更，
  // 所以先让用户确认一次，并说明可撤销（删文件 + 重启即回系统默认）。
  async function toggleBBR(enable: boolean) {
    if (tuningBusy) return
    if (
      enable &&
      !confirm(
        '在当前服务器上启用 BBR 拥塞控制？\n\n' +
          '会写入 /etc/sysctl.d/99-frpeasy-bbr.conf 并立即生效。' +
          '已经建立的连接不受影响，新建的连接才会用 BBR。\n\n' +
          '跨境链路上这一步通常是 5~13 倍的差别。随时可以撤销。',
      )
    )
      return
    setTuningBusy(true)
    try {
      const r = await api<{ congestion: TuningCongestion }>('POST', '/api/system/tuning', {
        action: enable ? 'enable_bbr' : 'disable_bbr',
      })
      setTuning((t) => (t ? { ...t, congestion: r.congestion } : t))
      toast(enable ? '已启用 BBR，现有隧道连接会在重连后切换' : '已撤销 BBR 配置')
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setTuningBusy(false)
    }
  }

  // 链路自检：让服务器自己测一遍到国内/境外的速度。
  //
  // 这一步回答的是"服务器出网有没有问题"，排除掉这一层之后，
  // 隧道慢就只能在客户端那段链路或 frp 传输参数里找原因了。
  async function runDiagnose() {
    if (diagBusy) return
    setDiagBusy(true)
    try {
      setDiag(await api<DiagnoseResponse>('POST', '/api/system/diagnose', {}))
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setDiagBusy(false)
    }
  }

  // 让客户端立刻测一次。只是排队：客户端下次心跳（约 15 秒）后执行。
  async function triggerSpeedTest() {
    if (speedBusy) return
    setSpeedBusy(true)
    try {
      const r = await api<{ queued: number; message: string }>(
        'POST',
        '/api/agents/speedtest',
        {},
      )
      toast(`已通知 ${r.queued} 台在线客户端测速，约 15 秒后出结果`)
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setSpeedBusy(false)
    }
  }

  const save = useCallback(async () => {
    if (saving) return
    // 设置还没加载出来时禁止保存。此时表单是一片空白，提交上去就等于
    // 把库里的真实配置全部覆盖成空 —— 这正是「更新后配置消失」的成因。
    if (!settings) {
      toast('设置尚未加载完成，请稍候再保存（必要时刷新页面）', 'bad')
      return
    }
    setSaving(true)
    try {
      // 只提交真正改动过的字段。
      //
      // 原来是把整份表单原样回传：只要某个字段因为任何原因显示成空
      // （最典型的就是设置还没加载完、或输入被冲掉），保存就会把它在库里的
      // 值覆盖成空，而用户完全看不出是哪一步弄丢的。改成逐字段比对后，
      // 「没动过的字段」永远不会被写，丢配置这条路就被堵死了。
      //
      // 注意：新增字段除了在这里比对，**还必须加进下面 useCallback 的依赖数组**。
      // 漏了依赖，save 会捕获旧的 state 值 —— 表现是"界面上选了、保存后没变"，
      // 而且完全没有报错（source_mode 就这样漏过一次）。
      const trim = (v: string) => v.trim()
      const body: SettingsBody = {}

      if (trim(publicAddr) !== trim(settings.public_addr ?? '')) {
        body.public_addr = trim(publicAddr)
        // 清空公网地址是破坏性操作（安装命令和客户端连 frps 的地址都失去基准），
        // 必须显式带上 clear 标记，服务端才允许写入空值。
        if (!body.public_addr) body.clear_public_addr = '1'
      }
      if (mode !== (settings.source_mode || 'manual')) body.source_mode = mode
      if (serverSources !== (settings.server_sources || 'github')) body.server_sources = serverSources
      if (clientSources !== (settings.client_sources || 'panel')) body.client_sources = clientSources
      if (trim(pStart) !== trim(settings.port_range_start ?? '')) body.port_range_start = trim(pStart)
      if (trim(pEnd) !== trim(settings.port_range_end ?? '')) body.port_range_end = trim(pEnd)
      if (trim(bindPort) !== trim(settings.frps_bind_port ?? '')) body.frps_bind_port = trim(bindPort)
      if (trim(dashPort) !== trim(settings.dashboard_port ?? '')) body.dashboard_port = trim(dashPort)
      // 隧道传输层。tcpMux 只有在真的变了才提交 —— 它是唯一会重启 frps 的项，
      // 而 UI 每次保存都会回传全部字段，不比对就会「改个无关设置也把隧道断一次」。
      if (compress !== (settings.tunnel_compression ?? '1')) body.tunnel_compression = compress
      if (tcpMux !== (settings.tunnel_tcp_mux ?? '1')) body.tunnel_tcp_mux = tcpMux
      // 连接池只在 mux 关闭时才有意义，开着 mux 提交它只会让服务端白做一次校验
      if (tcpMux === '0' && trim(poolCount) !== trim(settings.tunnel_pool_count ?? '0')) {
        body.tunnel_pool_count = trim(poolCount)
      }
      if (speedEnabled !== (settings.speedtest_enabled ?? '1')) body.speedtest_enabled = speedEnabled
      if (trim(speedInterval) !== trim(settings.speedtest_interval_min ?? '30')) {
        body.speedtest_interval_min = trim(speedInterval)
      }
      // 令牌输入框拿到的是脱敏串，只有用户重新输入时才提交
      if (token.trim()) body.frps_token = token.trim()
      if (pass) body.new_password = pass

      if (Object.keys(body).length === 0) {
        toast('没有改动，无需保存')
        return
      }

      const r = await api<SettingsSaveResult>('POST', '/api/settings', body)
      setSaveError('')
      setToken('')
      setPass('')
      // 保存后重新回填：服务端会规范化取值（例如去掉公网地址末尾的斜杠），
      // 让表单显示真正落库的值，避免"看起来没保存成功"。
      hydrated.current = false
      // 必须重拉隧道：访问地址是按公网地址实时推导的，不刷新表格里还是旧地址
      await Promise.all([loadSettings(), loadMe(), loadTunnels()])

      if (r.public_addr_changed && (r.affected_tunnels ?? 0) > 0) {
        toast(
          `公网地址已改：${r.affected_tunnels} 条已绑定隧道将在客户端下次拉取配置（约 ${r.client_pickup_second ?? 15} 秒）后改用新地址重连；若新地址不可达，这些隧道会变为离线。`,
          'bad',
        )
      } else if (r.frps_restarting) {
        toast('设置已保存，frps 正在重启')
      } else {
        toast('设置已保存')
      }
    } catch (e) {
      const msg = (e as Error).message
      setSaveError(msg)
      toast(msg, 'bad')
    } finally {
      setSaving(false)
    }
  }, [
    saving,
    settings,
    publicAddr,
    mode,
    serverSources,
    clientSources,
    pStart,
    pEnd,
    bindPort,
    dashPort,
    tcpMux,
    poolCount,
    compress,
    speedEnabled,
    speedInterval,
    token,
    pass,
    loadSettings,
    loadMe,
    loadTunnels,
    toast,
  ])

  // 排优记录与两侧「上次测速」。打开页面时取一次，测速后再取一次。
  //
  // 两侧各取一次：`last_rank` 是按侧存的（面板自己测的 / 面板测客户端列表的），
  // 只取一次会把另一侧的结论丢掉。事件列表合并展示 —— 记录区本来就要看两边。
  const loadSourceLog = useCallback(async () => {
    const [srv, cli] = await Promise.all([
      api<SourceEventsResponse>('GET', '/api/sources/events?side=server&limit=30'),
      api<SourceEventsResponse>('GET', '/api/sources/events?side=client&limit=30'),
    ])
    setRank(srv.last_rank ?? null)
    setClientRank(cli.last_rank ?? null)
    const merged = [...(srv.events ?? []), ...(cli.events ?? [])]
      .sort((a, b) => (a.at < b.at ? 1 : a.at > b.at ? -1 : 0))
      .slice(0, 30)
    setEvents(merged)

    // 把上次的逐项结果铺回列表行上。
    //
    // 原来这些结果只存在组件状态里，切走页面（组件卸载）就没了 ——
    // 回来只看到"没有测速数据"，而结论其实还在服务端（rank 缓存）。
    // 排优缓存过期也照样铺回去，rank 块上会标「已超过 10 分钟」。
    const seed = (rs?: ProbeResult[] | null) => {
      const m: Record<string, ProbeResult> = {}
      for (const x of rs ?? []) if (x.token) m[x.token] = x
      return m
    }
    const sp = seed(srv.last_rank?.results)
    if (Object.keys(sp).length) setProbe(sp)
    const cp = seed(cli.last_rank?.results)
    if (Object.keys(cp).length) setClientProbe(cp)
  }, [])

  useEffect(() => {
    void loadSourceLog().catch(() => {
      /* 记录读不到不影响改设置，静默即可 */
    })
  }, [loadSourceLog])

  // 测速并按实测结果重排。
  //
  // 这是"自动择优"的手动版：同一套探测（可用性 / 稳定性 / 速度），
  // 区别只在排序结果要不要立刻写回列表。面板只测服务端侧 ——
  // 客户端那台机器的网络面板看不到，替它排序等于凭空造一个顺序。
  //
  // **必须把界面上这份列表一起发过去**：按钮就在列表正下方，用户常常是先
  // 「+ 一个源」再点测速，此时库里还是上一版列表。原来服务端只读库，
  // 于是只测了库里的那一项，再拿结果覆盖表单 —— 刚加的那项直接消失。
  async function probeAndRank() {
    if (testing) return
    if (parseSources(serverSources).length === 0) {
      toast('先加一个源再测速（github 或镜像前缀）', 'bad')
      return
    }
    setTesting(true)
    try {
      const r = await api<{ results: ProbeResult[]; order: string[]; summary: string; applied: boolean }>(
        'POST',
        '/api/sources/probe',
        { side: 'server', apply: true, sources: serverSources },
      )
      const next: Record<string, ProbeResult> = {}
      for (const x of r.results ?? []) next[x.token] = x
      setProbe(next)
      // 返回的顺序就是刚测过的那份列表的排序，用它回填等于"重排"，
      // 不会多出或丢掉任何一项。
      setServerSources(r.order.join(','))
      await Promise.all([loadSettings(), loadSourceLog()])
      const bad = (r.results ?? []).filter((x) => !x.ok)
      toast(
        bad.length
          ? `测了 ${r.results.length} 个源并已按实测重排；${bad.length} 个不可用（${bad.map((x) => x.label).join('、')}）`
          : `测了 ${r.results.length} 个源，已按速度与稳定性重排并保存`,
        'ok',
      )
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setTesting(false)
    }
  }

  // 测客户端列表里的前缀：只判"能不能把文件取下来"（404 = 前缀配错），
  // 不排序、不写库。面板测不到客户端那台机器的速度，但"前缀是死的"这种错误
  // 面板能查出来，就不该让用户等到客户端装不上才发现。
  async function probeClientSources() {
    if (testingClient) return
    if (parseSources(clientSources).length === 0) {
      toast('先加一个源再测速（面板中继 / github / 镜像前缀）', 'bad')
      return
    }
    setTestingClient(true)
    try {
      const r = await api<{
        results: ProbeResult[]
        order: string[]
        summary: string
        applied: boolean
        caveat?: string
      }>('POST', '/api/sources/probe', { side: 'client', sources: clientSources })
      const next: Record<string, ProbeResult> = {}
      for (const x of r.results ?? []) next[x.token] = x
      setClientProbe(next)
      await loadSourceLog()
      const real = (r.results ?? []).filter((x) => !x.skipped)
      const bad = real.filter((x) => !x.ok)
      toast(
        bad.length
          ? `${bad.length} 个前缀取不到文件（${bad.map((x) => x.label).join('、')}）：检查前缀拼得对不对`
          : real.length
            ? `${real.length} 个前缀都能取出文件（速度是面板这边的，不代表客户端）`
            : '这份列表只有面板中继，不需要测',
        bad.length ? 'bad' : 'ok',
      )
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setTestingClient(false)
    }
  }

  // 最近一次「客户端自己测」的记录：只有 agent 上报的那条才有 agent_name
  // （面板替客户端列表测的那条没有）。用它证明客户端确实在做本地排优。
  const agentRank = events.find((e) => e.kind === 'rank' && (e.agent_name ?? '').trim() !== '') ?? null

  const nicChips = [
    ...(net?.public_ip
      ? [{ ip: net.public_ip, name: '公网出口 IP · 推荐', tone: 'info' as const }]
      : []),
    ...(net?.nics ?? []).map((n) => ({ ip: n.ip, name: n.name, tone: 'gray' as const })),
  ]

  return (
    <>
      {/* 设置页只留"全局项"。其余设置都搬到了用它的那个页面上 ——
          判断标准是"用户产生这个疑问时正站在哪"：在隧道页问"为什么慢"，
          就该在隧道页能改传输参数，而不是先学会"去设置里找"。
          这里给一份索引，避免老用户找不到东西。 */}
      {section === 'global' && (
        <Card title="其他设置在哪" desc="按「在哪用就在哪配」重新分过了，这里是索引">
          <div className="grid gap-1 text-[12.5px] leading-[1.9]">
            {[
              ['隧道', 'tunnels', '压缩 / 关闭 tcpMux / 连接池'],
              ['frps 服务端', 'server', '系统调优（BBR）、链路自检、frps 端口与令牌'],
              ['客户端', 'clients', '测速的间隔与开关、立即测全部'],
              ['端口与防火墙', 'ports', '端口池（自动分配的公网端口范围）'],
              ['文件与更新', 'files', '二进制下载源与下载策略'],
            ].map(([label, view, what]) => (
              <div key={view} className="flex flex-wrap items-baseline gap-2">
                <a
                  className="cursor-pointer font-semibold text-accent-fg hover:underline"
                  onClick={() => (window.location.hash = `#/${view}`)}
                >
                  {label}
                </a>
                <span className="text-dim">— {what}</span>
              </div>
            ))}
          </div>
        </Card>
      )}

      {/* 保存失败的持久提示。放在最上面：无论失败原因属于哪一块设置，
          用户一进这个页面就能看到，不用翻卡片找。 */}
      {saveError && (
        <Alert tone="bad">
          <div className="flex flex-wrap items-start gap-2">
            <span className="min-w-0 flex-1">
              <b>上次保存没有成功</b>，所以这些设置仍是原来的值：{saveError}
            </span>
            <Button size="sm" variant="ghost" className="shrink-0" onClick={() => setSaveError('')}>
              知道了
            </Button>
          </div>
        </Alert>
      )}

      {/* 公网地址是全局身份，留在设置页 */}
      {section === 'global' && (<>
      <Card title="公网地址" desc="客户端连接 frps 用的地址，也是生成安装命令的依据">
        <Field
          label="公网地址（域名或 IP，可带面板端口）"
          hint={
            <>
              只填<b>主机名/IP</b>（要写面板端口就写 <span className="font-mono">1.2.3.4:28080</span>）。
              <b>不要填 frps 的接入端口</b> —— 那个端口面板会自动拼。这里改了，安装命令、客户端连 frps
              的地址、以及所有隧道的访问地址会一起跟着变；客户端约 15 秒后自动重连新地址。
            </>
          }
        >
          <Input
            value={publicAddr}
            onChange={(e) => setPublicAddr(e.target.value)}
            placeholder="如 1.2.3.4 或 nas.example.com"
          />
        </Field>
        <Hint>
          留空时自动使用你当前访问面板的地址（本地体验没问题；部署在公网请显式填写，否则从内网地址访问面板会生成错误的客户端地址）。
        </Hint>

        <div className="mt-3">
          <label className="mb-1.5 block text-[12.5px] text-muted">探测到的地址（点一下直接填入）</label>
          <div className="flex flex-wrap gap-2">
            {nicChips.length === 0 ? (
              <span className="text-xs text-dim">
                没探测到可用地址（本机可能只有环回网卡，或外网探测被阻断），可直接手填。
              </span>
            ) : (
              nicChips.map((c) => (
                <Chip
                  key={c.ip}
                  sub={c.name}
                  onClick={() => {
                    setPublicAddr(c.ip)
                    toast(`已填入 ${c.name}`)
                  }}
                >
                  {c.ip}
                </Chip>
              ))
            )}
          </div>
        </div>
        <Hint>
          上面是本机的物理网卡 IP 与公网出口 IP，只供你挑选；不会混入 127.0.0.1
          之类的环回地址，也不涉及任何端口。
        </Hint>
      </Card>
      </>)}

      {/* 二进制下载源属于文件与更新，放在那一页 */}
      {section === 'files' && (<>
      <Card
        title="二进制下载"
        desc="先选策略；两个有序列表从左往右试，第一个能用的就停"
      >
        <Field label="下载策略（① 面板自己取、② 客户端取，两个列表共用这一个策略）">
          <div className="flex flex-wrap gap-2">
            <Chip
              selected={mode === 'auto'}
              sub="下载前各自测速排优，推荐"
              onClick={() => setMode('auto')}
            >
              自动择优
            </Chip>
            <Chip
              selected={mode === 'manual'}
              sub="严格按下面两个列表的顺序"
              onClick={() => setMode('manual')}
            >
              手动排序
            </Chip>
          </div>
        </Field>
        <Hint>
          <b>自动择优</b>：每次真要下载二进制之前，临时把候选源挨个测一遍（能不能下、
          快不快、稳不稳），按这一次的实测结果排序，再按这个顺序回退重试。
          测速结果与最终顺序都会记进下面的记录，出问题能追。
          代价是下载前多花几秒（结果缓存 10 分钟，连续下载不会重复测）。
          <br />
          <b>手动排序</b>：完全按你排的顺序走，一秒不多等。
          <br />
          两种策略下<b>列表本身都还有用</b>：手动时它就是顺序；自动时它是候选集合，
          同时作为同分时的偏好。
          <br />
          <b>策略对两侧都生效，但测速是各测各自的</b>：面板测面板这台机器的链路，
          客户端测它自己那台机器的（两块网络可能完全不同）。所以 ② 那份列表的
          「自动」不在面板上执行，而是随配置下发、由客户端本地执行 ——
          这正是下面 ② 里那块状态说明在讲的事。
        </Hint>

        <div className="mb-2 mt-4 text-[13px] font-semibold text-fg">
          ① 面板自己取 frps / frpc / 自己的更新
        </div>
        <SourceListEditor
          value={serverSources}
          onChange={setServerSources}
          allowPanel={false}
          probe={probe}
        />
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={testing} onClick={() => void probeAndRank()}>
            {testing ? '测速中…' : '测这份列表并按实测重排'}
          </Button>
          <span className="text-xs text-dim">
            实下 512KB × 2 轮：能看出「能不能下」「多快」「稳不稳」，只测一次的峰值会骗人
          </span>
        </div>
        <Hint>
          这是<b>面板这台机器</b>自己出网取二进制时的顺序（frps、frpc 缓存、agent 缓存、面板自更新）。
          没有 panel 选项：面板不能给自己中继。
          <br />
          测速测的就是<b>上面这份列表（含你刚加、还没点保存的项）</b>，测完按实测顺序重排并保存。
        </Hint>

        <div className="mb-2 mt-6 text-[13px] font-semibold text-fg">② 客户端取 agent / frpc</div>
        <SourceListEditor
          value={clientSources}
          onChange={setClientSources}
          allowPanel
          probe={clientProbe}
        />
        {/* 缺「面板中继」是一个很容易被忽略、但后果很重的配置错误：
            客户端会在自己那台机器上直连 GitHub / 镜像，而国内环境经常出不去 ——
            表现是「客户端一直在线，但 frpc 永远拉不到、隧道全离线」，
            面板侧完全没有错误信息（2026-10-28 线上卡了 4 小时就是这么来的）。 */}
        {!parseSources(clientSources).includes('panel') && (
          <Alert tone="warn">
            <div className="flex flex-wrap items-center gap-2">
              <span className="min-w-0 flex-1">
                当前列表里没有<b>「面板」中继</b>。客户端会在它那台机器上直连
                GitHub / 镜像，一旦那里出网受限（国内很常见），frpc 就取不到 ——
                现象是「客户端在线、隧道却一直离线」，面板上看不到任何报错。
              </span>
              <Button
                size="sm"
                variant="ghost"
                className="shrink-0"
                onClick={() =>
                  setClientSources(
                    ['panel', ...parseSources(clientSources).filter((x) => x !== 'panel')].join(','),
                  )
                }
              >
                把面板加到第一位
              </Button>
            </div>
          </Alert>
        )}

        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={testingClient} onClick={() => void probeClientSources()}>
            {testingClient ? '测速中…' : '测这份列表能不能取到文件'}
          </Button>
          <span className="text-xs text-dim">
            面板会经每个前缀真下一段文件：404 = 前缀拼错了
          </span>
        </div>
        <Hint>
          这是<b>客户端那台机器</b>从哪儿取。两种通道：
          <b>面板中继</b>（客户端只连面板，由面板转发；任何环境都能用，但占面板出口带宽）
          和<b>镜像前缀</b>（客户端经该前缀自己取 GitHub 上的文件，不占面板带宽，要求它能出网）。
          最后一项可以是「GitHub 直连」——没配任何前缀时它最慢。
          <br />
          上面那个测速<b>按面板这条链路算</b>：能判「这个前缀是不是死的、拼没拼错」，
          但速度不代表客户端那台机器，也<b>不会替你排序</b>。
          <br />
          改完下一轮心跳就生效，<b>不用重装客户端</b>。安装那一步固定经面板（装的时候 agent
          还没跑起来、这份顺序还没下发，而且装不上比走慢一点的通道严重得多）。
        </Hint>

        {/* 这一块是他反复问的问题：「客户端到底按不按这个顺序、会不会自己测」。
            把当前策略下客户端的确切行为写出来，并把真机器上报的结果摆在下面。 */}
        <div className="mt-2 rounded-[9px] border border-border bg-chip px-3 py-2 text-[12.5px] leading-relaxed">
          {mode === 'auto' ? (
            <>
              当前策略 <b>自动择优</b>：客户端每次真要下载前，<b>在它自己那台机器上</b>
              把候选源测一遍（能不能下 / 中位速度 / 抖动，2 轮 × 512KB），按这次实测排序后
              逐个回退，结果缓存 10 分钟。排优与每次下载尝试都会上报到这里。
            </>
          ) : (
            <>
              当前策略 <b>手动排序</b>：客户端<b>严格按上面这个顺序</b>逐个试，<b>不测速</b>。
              想让它自己做稳定性测试与择优，把上面的「下载策略」切成「自动择优」——
              切换后下一轮心跳（约 15 秒）就下发，不用重装。
            </>
          )}
        </div>
        {agentRank && (
          <div className="mt-1.5 text-[12px] text-dim">
            最近一次<b>客户端自己测</b>：{agentRank.agent_name} ·{' '}
            {new Date(agentRank.at).toLocaleString()} · {agentRank.detail || agentRank.source || '—'}
          </div>
        )}
        {clientRank && (
          <div className="mt-1 text-[12px] text-dim">
            上次在<b>面板这边</b>测这份列表：{clientRank.summary || '—'}
            {clientRank.stale && '（已超过 10 分钟，仅供参考）'}
          </div>
        )}

        {rank && (
          <div className="mt-4 rounded-[9px] border border-border bg-chip px-3 py-2 text-[12.5px]">
            <div className="flex flex-wrap items-center gap-2">
              <b>最近一次排优</b>
              <span className="text-dim">{new Date(rank.at).toLocaleString()}</span>
              {rank.stale && <span className="text-warn-fg">（已超过 10 分钟，仅供参考）</span>}
              <span className="text-dim">顺序：{rank.order.join(' → ') || '—'}</span>
            </div>
            <div className="mt-1 text-dim">{rank.summary}</div>
          </div>
        )}

        <details className="mt-3">
          <summary className="cursor-pointer text-[12.5px] text-dim">
            排优 / 下载记录（最近 {events.length} 条，出问题先看这里）
          </summary>
          <div className="mt-2 max-h-[320px] overflow-y-auto rounded-[9px] border border-border">
            {events.length === 0 ? (
              <div className="px-3 py-2.5 text-[12px] text-dim">还没有记录。</div>
            ) : (
              events.map((e) => (
                <div
                  key={e.id}
                  className="flex flex-wrap items-baseline gap-x-2 border-b border-border px-3 py-1.5 text-[12px] last:border-b-0"
                >
                  <span className="text-dim tabular-nums">{e.at}</span>
                  <span className={e.kind === 'rank' ? 'text-info-fg' : e.ok ? 'text-ok-fg' : 'text-bad-fg'}>
                    {e.kind === 'rank' ? '排优' : e.ok ? '下载成功' : '下载失败'}
                  </span>
                  <span className="text-dim">{e.side === 'client' ? e.agent_name || '客户端' : '面板'}</span>
                  <span>{e.what}</span>
                  {e.source && <span className="font-mono text-[11px]">{e.source}</span>}
                  {e.elapsed_ms > 0 && <span className="text-dim">{e.elapsed_ms}ms</span>}
                  <span className="w-full text-dim">{e.detail || e.error}</span>
                </div>
              ))
            )}
          </div>
        </details>
      </Card>
      </>)}

      {/* 端口池属于端口与防火墙，放在那一页 */}
      {section === 'ports' && (<>
      <Card title="端口池" desc="自动分配公网端口的范围">
        <div className="grid grid-cols-2 gap-3.5 max-[820px]:grid-cols-1">
          <Field label="起始端口" className="mb-0">
            <Input
              type="number"
              value={pStart}
              onChange={(e) => setPStart(e.target.value)}
            />
          </Field>
          <Field label="结束端口" className="mb-0">
            <Input type="number" value={pEnd} onChange={(e) => setPEnd(e.target.value)} />
          </Field>
        </div>
        <Hint>改端口池不会影响已建隧道。记得在服务器防火墙 / 安全组放行这些端口。</Hint>
      </Card>
      </>)}

      {/* ---------------- 隧道传输 ---------------- */}
      {/* 这块存在的理由：frp 的 tcpMux 在 frps 与 frpc 必须一致，一边开一边关
          会握手后被直接断开（客户端只有 EOF、服务端一条错误都不记）。所以它
          不能放给用户改客户端配置，必须由面板统一决定并同时下发到两端。 */}
      {/* 隧道传输参数只影响隧道，放在隧道页 */}
      {section === 'tunnel' && (<>
      <Card
        title="隧道传输"
        desc="由面板统一决定并下发给所有客户端，两端必须一致"
      >
        {/* 生效状态条。
            用户最容易卡住的一步不是"怎么改"，而是"改完到底生效没有" ——
            表单值只是"我填的"，这里显示的是 frps 与心跳**正在用的那一份**。
            两者不同还给出明确解释：不是保存失败，而是还没点保存。 */}
        {applied && (
          <div
            className={cx(
              'mb-3 rounded-lg border px-2.5 py-1.5 text-[12px] leading-[1.6]',
              transportDirty
                ? 'border-warn-fg/40 bg-warn-dim text-warn-fg'
                : 'border-ok-fg/30 bg-ok-dim text-ok-fg',
            )}
          >
            {transportDirty ? (
              <>
                表单已改但<b>还没保存</b>：下面显示的是你填的值，当前实际生效的仍是 ——
                多路复用 {applied.tcp_mux ? '开' : '关'} · 压缩{' '}
                {applied.compression ? '开' : '关'} · 连接池 {applied.pool_count}。点「保存」后才会下发。
              </>
            ) : (
              <>
                当前生效：多路复用 <b>{applied.tcp_mux ? '开' : '关'}</b> · 压缩{' '}
                <b>{applied.compression ? '开' : '关'}</b> · 连接池{' '}
                <b>{applied.pool_count}</b>
                <span className="ml-1 opacity-80">
                  （与下面的表单一致；客户端会在下次心跳约 15 秒内按此重连）
                </span>
              </>
            )}
          </div>
        )}

        <div className="grid gap-3.5">
          <label className="flex cursor-pointer items-start gap-2.5">
            <input
              type="checkbox"
              className="mt-0.5 h-4 w-4 shrink-0 accent-[var(--accent)]"
              checked={compress === '1'}
              onChange={(e) => setCompress(e.target.checked ? '1' : '0')}
            />
            <span className="min-w-0">
              <b className="block text-[13.5px]">压缩传输内容</b>
              <span className="mt-0.5 block text-xs leading-[1.6] text-dim">
                跨境 / 小带宽链路上通常能提升 3~5 倍 —— 隧道里的 HTML/JS/JSON 会被压一遍。
                对图片、zip 这类已经压缩过的内容没有收益，只是多花一点 CPU。
              </span>
            </span>
          </label>

          <label className="flex cursor-pointer items-start gap-2.5">
            <input
              type="checkbox"
              className="mt-0.5 h-4 w-4 shrink-0 accent-[var(--accent)]"
              checked={tcpMux === '0'}
              onChange={(e) => setTcpMux(e.target.checked ? '0' : '1')}
            />
            <span className="min-w-0">
              <b className="block text-[13.5px]">
                关闭 TCP 多路复用（tcpMux）
                {tcpMux === '0' && <span className="ml-1.5 text-[11.5px] text-ok-fg">已关闭</span>}
              </b>
              <span className="mt-0.5 block text-xs leading-[1.6] text-dim">
                <b className="text-fg">保持开启（默认）。实测关掉会让速度变成两档乱跳。</b>
                {' '}2026-10-29 做了 18 次对照（同一台机器、同一条跨境隧道）：
                开启时 3 轮共 18 次<b className="text-ok-fg">全部稳定在 7 MB/s 左右</b>；
                关闭时会出现两档 —— 快档 3~6 MB/s，<b className="text-bad-fg">慢档只有 20~60 KB/s</b>，
                而且大约一半的请求会落到慢档，还会夹着 8 秒超时、0 字节。
                <br />
                原因：关闭后每个用户连接都要单独新建一条 work 连接，每次重新从
                TCP 慢启动开始；链路上存在丢包时（你这条实测 10%），新建连接有很高
                概率在第一轮就把窗口打崩，之后一直起不来。开启时所有流量复用一条
                长期有流量的连接，窗口与 BBR 估计能一直维持在高位。
                <br />
                理论上的确是 yamux 单流窗口 256KB 会限制吞吐（≈256KB ÷ RTT），
                但在有丢包的跨境链路上这条上限根本达不到，实测结论是相反的
                —— 这也是为什么 frp 把它设为默认开启。
                <b className="text-warn-fg">
                  {' '}要求所有客户端 ≥ {muxMinVer || '0.7.34'}；切换时隧道会中断十几秒。
                </b>
              </span>

              {/* 已经关掉的人给一条明确的回退提示：这是实测里最容易踩、
                  代价最大的一项配置，不说清楚用户只会以为"隧道就是慢"。 */}
              {tcpMux === '0' && (
                <span className="mt-1.5 block rounded-md border border-bad-fg/40 bg-bad-dim px-2 py-1.5 text-[11.5px] leading-[1.6] text-bad-fg">
                  你现在是<b>关闭</b>状态。除「客户端与服务端在同一内网、延迟 &lt; 5ms」
                  这种特殊情况外，关闭只会让隧道慢 100~200 倍并随机卡死。
                  建议取消勾选后保存，等约 15 秒隧道自动恢复，再去「隧道」页测速对比。
                </span>
              )}

              {/* 护栏前置到界面上。
                  之前这条判断只在保存时由后端返回 409，用户点了保存、看到一句
                  toast、刷新后又发现没变 —— 完全不知道是被什么挡住了。
                  现在提前点名是哪几台机器，用户不用试就知道该先做什么。 */}
              {staleForMux.length > 0 && (
                <span className="mt-1.5 block rounded-md border border-bad-fg/40 bg-bad-dim px-2 py-1.5 text-[11.5px] leading-[1.6] text-bad-fg">
                  现在<b>关不掉</b>：{staleForMux.length} 台客户端的版本低于 {muxMinVer}
                  —— {staleForMux.slice(0, 3).map((a) => a.name).join('、')}
                  {staleForMux.length > 3 && ` 等 ${staleForMux.length} 台`}。
                  它们仍会用 mux=开，而 frps 改成关之后这些隧道会全部连不上，
                  且两边日志都不留错误。请先在「客户端」页把它们升级上去。
                </span>
              )}

              {/* 改完怎么验证 —— 之前完全没写，用户只能凭感觉。
                  实测手段就是隧道页的「测速」：它从服务器经隧道压一次数据回来。 */}
              <span className="mt-1.5 block text-[11.5px] leading-[1.6] text-dim">
                改完怎么验证：去「隧道」页点某条隧道的<b>测速</b>，记下数字 → 再切换这个开关 →
                等隧道恢复（约 15 秒）→ 再测一次。两次的差别就是这个开关在你这条链路上的实际收益。
              </span>
            </span>
          </label>

          {tcpMux === '0' && (
            <Field
              label={`连接池大小（0~${tuning?.max_pool_count ?? 32}，仅多路复用关闭时生效）`}
              className="mb-0 max-w-[220px]"
            >
              <Input
                type="number"
                min={0}
                max={tuning?.max_pool_count ?? 32}
                value={poolCount}
                onChange={(e) => setPoolCount(e.target.value)}
                placeholder="0"
              />
            </Field>
          )}
        </div>
        <Hint>
          改动点「保存」后生效：客户端在下一轮心跳（约 15 秒）拿到新配置并重连。
          tcpMux 变化时面板会重启 frps 以重写它的配置。
        </Hint>
      </Card>
      </>)}

      {/* 拥塞控制是服务器内核的事，放在 frps 服务端页 */}
      {section === 'server' && (<>
      {/* ---------------- 系统调优 ---------------- */}
      <Card
        title="系统调优（拥塞控制）"
        desc="决定这条链路上 TCP 能跑多快；跨境丢包链路上差别可达数倍"
      >
        {tuning?.congestion.supported ? (
          <div className="flex flex-wrap items-center gap-3">
            <div className="min-w-0">
              <div className="text-[13px]">
                当前拥塞控制：
                <b
                  className={cx(
                    'ml-1 font-mono',
                    tuning.congestion.cc === 'bbr' ? 'text-ok-fg' : 'text-warn-fg',
                  )}
                >
                  {tuning.congestion.cc || '未知'}
                </b>
              </div>
              <div className="mt-1 text-xs leading-[1.6] text-dim">
                {tuning.congestion.cc === 'bbr'
                  ? 'BBR 已启用：不依赖丢包信号判断拥塞，跨境丢包链路上表现明显优于 CUBIC。'
                  : tuning.congestion.bbr_available
                    ? 'CUBIC 会把丢包当成拥塞信号，一路收缩窗口 —— 跨境链路上实测吞吐会塌到 KB/s 量级。建议启用 BBR。'
                    : '当前内核没有可用的 BBR 模块，无法切换。'}
              </div>
            </div>
            <div className="ml-auto flex shrink-0 gap-2">
              {tuning.congestion.cc === 'bbr' ? (
                <Button
                  variant="ghost"
                  disabled={tuningBusy}
                  onClick={() => void toggleBBR(false)}
                >
                  撤销 BBR
                </Button>
              ) : (
                <Button
                  disabled={tuningBusy || !tuning.congestion.bbr_available}
                  onClick={() => void toggleBBR(true)}
                >
                  {tuningBusy ? '处理中…' : '启用 BBR'}
                </Button>
              )}
            </div>
          </div>
        ) : (
          <div className="text-xs leading-[1.6] text-dim">
            {tuning ? tuning.congestion.error || '当前平台不支持在面板内调整' : '读取中…'}
            {tuning && !tuning.congestion.supported && (
              <span className="mt-1 block">
                面板只管理服务器上的 frps，而拥塞控制是服务器内核的事，所以在非 Linux 面板上这栏不适用。
              </span>
            )}
          </div>
        )}
      </Card>
      </>)}

      {/* ---------------- 链路自检 ---------------- */}
      {/* 隧道慢的时候，第一个要回答的问题是「服务器出网有没有问题」。
          以前这一步只能登服务器手工 curl 几个目标对比，现在做进面板：
          一次点击，服务器自己测一遍到国内/境外的速度，并给出可读结论。 */}
      {/* 链路自检诊断的是服务器出网，放在 frps 服务端页 */}
      {section === 'server' && (<>
      <Card
        title="链路自检"
        desc="从服务器侧实测到国内 / 境外的速度，用一句话判断瓶颈在哪"
      >
        <div className="flex flex-wrap items-center gap-3">
          <div className="min-w-0 flex-1 text-xs leading-[1.6] text-dim">
            排除「服务器本身出不去」这一层。若这里都正常，隧道慢的原因就在
            <b className="text-fg"> 客户端到服务器这一段</b>，或者 frp 的传输参数。
          </div>
          <Button className="shrink-0" disabled={diagBusy} onClick={() => void runDiagnose()}>
            {diagBusy ? '检测中…（约 15 秒）' : '开始自检'}
          </Button>
        </div>

        {/* 这里解释的是用户必然会问的那个问题："为什么同一个目标，第一次测出来的
            数字比后面小好几倍？" —— 答案是 TCP 的物理规律，不是测量出错。
            不写清楚的话，任何人都会认为这个功能不准。 */}
        <div className="mt-2.5 space-y-2 rounded-lg border border-border2 bg-chip px-3 py-2.5 text-[12px] leading-[1.7] text-dim">
          <p>
            <b className="text-fg">两个数字分别是什么：</b>
            {' '}
            <b className="text-fg">首次</b> = 新建连接去拿（含 DNS + TCP + TLS
            握手、以及拥塞窗口从初值涨起来的过程），这就是你第一次打开页面时的体感；
            <b className="text-fg">复用</b> = 紧接着在同一条连接上再拿一次，握手没有了、
            窗口已涨满，若还有 CDN 边缘缓存，数据从就近节点直接返回。
          </p>
          <p>
            <b className="text-fg">为什么两者能差好几倍：</b>
            {' '}跨境的每个往返要 100~200ms，握手占 3~4 个往返就是 300~800ms，
            这段时间几乎没有数据传输；之后窗口还要爬升。复用把这两笔开销全省了。
            {' '}<b className="text-fg">两个数都是真的，只是量的是不同的事</b>
            —— 所以这里都报出来，让你自己看差多少。
          </p>
          <p>
            <b className="text-fg">这种机制怎么用在正常业务上：</b>
            浏览器和 frp 默认就在复用连接，不用你操心；自己写的程序要开 keep-alive。
            隧道这边对应的是
            <b className="text-fg">「连接池」</b>：关掉多路复用后预建 N 条连接，
            新请求不必现握手。但要说清一句：
            <b className="text-warn-fg">隧道里拿不到 CDN 缓存带来的那部分加速</b>
            —— 你的流量是私有的、动态的，没有第三方边缘节点帮你存。
          </p>
        </div>

        {diag && (
          <div className="mt-3">
            <div className="grid gap-1.5">
              {diag.items.map((it) => (
                <div
                  key={it.key}
                  className="flex flex-wrap items-center gap-2 rounded-[9px] border border-border2 bg-chip px-2.5 py-1.5"
                >
                  <span className="min-w-0 flex-1 truncate text-[12.5px]">{it.name}</span>
                  {it.ok ? (
                    <span
                      className="flex shrink-0 items-baseline gap-1 font-mono text-[12.5px]"
                      title="首次 = 新连接（含握手与慢启动）的实际体感；复用 = 同一条连接上第二次请求的速度，两者差多少就是「连接复用 + 缓存」省下的"
                    >
                      <span className="text-[11px] text-dim">首次</span>
                      <b
                        className={cx(
                          it.suspect
                            ? 'text-warn-fg'
                            : it.speed >= 1 << 20
                              ? 'text-ok-fg'
                              : 'text-warn-fg',
                        )}
                      >
                        {(it.speed / 1024 / 1024).toFixed(2)}
                      </b>
                      {/* 复用速度只在明显更快时才显示 —— 一样的话没必要占位置 */}
                      {it.speed_reuse > it.speed * 1.5 && (
                        <>
                          <span className="ml-1.5 text-[11px] text-dim">复用</span>
                          <b className="text-warn-fg">
                            {(it.speed_reuse / 1024 / 1024).toFixed(2)}
                          </b>
                        </>
                      )}
                      <span className="text-[11px] text-dim">MB/s</span>
                      {it.suspect && <span className="ml-1 text-[11px] text-warn-fg">疑似缓存</span>}
                    </span>
                  ) : (
                    <span className="shrink-0 text-[12px] text-bad-fg">{it.error || '失败'}</span>
                  )}
                  {/* 建连成本单列：它是"第一次为什么慢"的量化答案。
                      握手（3~4 个 RTT）与慢启动起步都在这里，通常几百毫秒。 */}
                  <span
                    className="shrink-0 font-mono text-[11px] text-dim"
                    title="建连 = DNS + TCP + TLS 握手，这就是第一次访问要额外等的时间"
                  >
                    建连 {it.warm_ms}ms · {(it.bytes / 1024 / 1024).toFixed(1)}MB
                  </span>
                </div>
              ))}
            </div>
            <div className="mt-2.5 rounded-lg border border-accent/30 bg-accent-dim px-3 py-2 text-[12.5px] leading-[1.65]">
              {diag.advice}
            </div>
          </div>
        )}
      </Card>
      </>)}

      {/* ---------------- 客户端测速 ---------------- */}
      {/* 这一项有真实流量成本：每次拉 1MB。10 分钟一次是每台每月 4.2GB，
          30 分钟一次是 1.4GB。对走 CDT / 按量带宽的机器不是可以随便定的数字，
          所以间隔要能改、能关，也要能手动测一次看当下。 */}
      {/* 客户端测速是客户端的策略：放在客户端页，看结果时顺便就能调 */}
      {section === 'client' && (<>
      <Card
        title="客户端测速"
        desc="让客户端自己测「本机 → 面板」的速度 —— 这段才是隧道的真实链路"
      >
        <div className="grid gap-3">
          <label className="flex cursor-pointer items-start gap-2.5">
            <input
              type="checkbox"
              className="mt-0.5 h-4 w-4 shrink-0 accent-[var(--accent)]"
              checked={speedEnabled === '1'}
              onChange={(e) => setSpeedEnabled(e.target.checked ? '1' : '0')}
            />
            <span className="min-w-0">
              <b className="block text-[13.5px]">定期自测</b>
              <span className="mt-0.5 block text-xs leading-[1.6] text-dim">
                关掉后客户端不再自动测速，界面上的速度会停在最后一次结果。
                仍可以用下面的按钮手动测一次。
              </span>
            </span>
          </label>

          <div className="flex flex-wrap items-end gap-3">
            <Field label="间隔（分钟）" className="mb-0 w-[150px]">
              <Input
                type="number"
                min={5}
                max={1440}
                value={speedInterval}
                onChange={(e) => setSpeedInterval(e.target.value)}
                disabled={speedEnabled === '0'}
              />
            </Field>
            <div className="mb-1 text-xs leading-[1.6]">
              {(() => {
                const n = Number.parseInt(speedInterval, 10)
                const mins = Number.isFinite(n) && n > 0 ? n : 30
                // 每天 1440 分钟 / 间隔 = 次数；每次 1MB；按 30 天算
                const timesPerDay = 1440 / mins
                const gbPerMonth = (timesPerDay * 30) / 1024
                return (
                  <span className={speedEnabled === '0' ? 'text-dim' : 'text-muted'}>
                    每台每月约{' '}
                    <b className={gbPerMonth > 3 ? 'text-warn-fg' : 'text-fg'}>
                      {gbPerMonth.toFixed(1)} GB
                    </b>
                    （{timesPerDay.toFixed(0)} 次/天，每次 1MB）
                    {gbPerMonth > 3 && <span className="text-warn-fg"> · 偏多，考虑调大间隔</span>}
                  </span>
                )
              })()}
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" disabled={speedBusy} onClick={() => void triggerSpeedTest()}>
              {speedBusy ? '下发中…' : '立即让所有在线客户端测一次'}
            </Button>
            <span className="text-xs text-dim">
              只排一次队，客户端下次心跳（约 15 秒）后执行；关掉自动测速也照样能手动测。
            </span>
          </div>
        </div>
        <Hint>
          测速走的是客户端到面板的 HTTP 通道，和隧道（frps 的裸 TCP）不完全等价，
          但同一台机器、同一条线路，量级上可参考。改完记得点「保存」。
        </Hint>
      </Card>
      </>)}

{/* 外观是浏览器本地偏好，不属于任何"对象"，留在设置页。 */}
      {section === 'global' && (
        <Card title="外观" desc="只影响当前这台浏览器，不会同步到其它设备">
          <Field label="面板主题" className="mb-0">
            <div className="flex gap-2 pt-0.5">
              <Button
                variant={theme === 'light' ? 'primary' : 'ghost'}
                size="sm"
                onClick={() => setTheme('light')}
              >
                ☀ 日间
              </Button>
              <Button
                variant={theme === 'dark' ? 'primary' : 'ghost'}
                size="sm"
                onClick={() => setTheme('dark')}
              >
                ☾ 夜间
              </Button>
            </div>
          </Field>
        </Card>
      )}

      {/* frps 的运行参数跟着服务端走：改端口要重启 frps、令牌要下发给所有客户端，
          都属于"服务器本身怎么跑"，留在设置页会让人找不到。 */}
      {section === 'server' && (
        <Card title="frps 运行参数" desc="改端口会自动重启 frps，正在连的客户端会自动重连">
          <div className="grid grid-cols-2 gap-3.5 max-[820px]:grid-cols-1">
            <Field label="frps 监听端口（改后自动重启 frps）" className="mb-0">
              <Input type="number" value={bindPort} onChange={(e) => setBindPort(e.target.value)} />
            </Field>
            <Field label="frps 令牌（留空保持不变）" className="mb-0">
              <Input
                type="password"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="留空则不变"
                autoComplete="new-password"
              />
            </Field>
          </div>
          <Field label="frps 状态端口（dashboard，仅 127.0.0.1）" className="mt-3.5 mb-0 max-w-[280px]">
            <Input type="number" value={dashPort} onChange={(e) => setDashPort(e.target.value)} />
          </Field>
          <Hint>dashboard 只监听本机，面板读它拿实时流量；不要把它暴露到公网。</Hint>
        </Card>
      )}

      {/* 管理员口令属于账号本身，留在设置页 */}
      {section === 'global' && (<>
      <Card title="管理员口令">
        <Field
          label="新口令（留空不改）"
          hint="面板能开隧道直通内网，暴露到公网前一定要把初始口令改掉。"
        >
          <Input
            type="password"
            value={pass}
            onChange={(e) => setPass(e.target.value)}
            autoComplete="new-password"
          />
        </Field>
      </Card>
      </>)}
      {/* 版本与更新、以及各种二进制文件，都搬到了「这台服务器 → 文件与更新」。
          两处分开时，用户看到「有新版」却要去另一页看文件是否就绪，很别扭。
          这里只留一个入口，别在这里再挂一份（挂了就会出现两个更新按钮，
          状态还各自轮询、互相打架）。 */}
      {section === 'global' && (
        <Hint>
          面板与客户端的版本、以及 frps / frpc / agent 各平台的二进制文件，都在左侧{' '}
        <a className="cursor-pointer font-semibold text-accent-fg underline" onClick={() => (window.location.hash = '#/files')}>
            这台服务器 → 文件与更新
          </a>
          。
        </Hint>
      )}
      <div className="flex flex-wrap items-center gap-2.5">
        <Button onClick={() => void save()} disabled={saving || !settings}>
          {saving ? '保存中…' : '保存设置'}
        </Button>
        <Button
          variant="ghost"
          disabled={!settings}
          onClick={() => {
            if (!settings) return
            fillForm(settings)
            toast('已恢复为当前已保存的设置')
          }}
        >
          放弃修改
        </Button>
        {/* 安装令牌是全局的（所有客户端都用它认领这台面板），
            只在设置页给入口 —— 挂在隧道页只会让人以为它跟某条隧道有关。 */}
        {section === 'global' && (
          <Button
            variant="ghost"
            size="sm"
            onClick={async () => {
              const r = await copyWithHint(settings?.install_token ?? '', null, '已复制安装令牌')
              toast(r.msg, r.kind)
            }}
          >
            复制安装令牌
          </Button>
        )}
        {!settings && (
          <span className="text-xs text-dim">
            配置还在加载中，暂不能保存 —— 空白表单如果直接保存会把已有配置覆盖掉
          </span>
        )}
      </div>
      {section === 'global' && (
        <Hint>安装令牌用于客户端认领到这台面板；更换后旧客户端仍保持已授权，不影响现有隧道。</Hint>
      )}
    </>
  )
}
