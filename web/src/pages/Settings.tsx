import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { useApp } from '../store'
import { api, qs } from '../lib/api'
import { copyText } from '../lib/format'
import {
  Button,
  Card,
  Chip,
  Field,
  Hint,
  Input,
  cx,
} from '../components/ui'
import type { MirrorTestResult, Settings as SettingsShape, SettingsBody, SettingsSaveResult } from '../types'

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
  probe: Record<string, MirrorTestResult>
}) {
  const list = parseSources(value)
  const set = (next: string[]) => onChange(next.join(','))
  const move = (i: number, d: number) => {
    const next = [...list]
    const j = i + d
    if (j < 0 || j >= next.length) return
    ;[next[i], next[j]] = [next[j], next[i]]
    set(next)
  }
  const remove = (i: number) => set(list.filter((_, k) => k !== i))
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
                <span className={cx('ml-auto shrink-0 text-[11.5px]', r.ok ? 'text-ok-fg' : 'text-bad-fg')}>
                  {r.ok ? `${r.elapsed_ms}ms · ${Math.round((r.speed_kbps ?? 0))}KB/s` : '不通'}
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
            const v = custom.trim().replace(/\/+$/, '')
            if (!/^https?:\/\//.test(v)) return
            set([...list, v])
            setCustom('')
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

export function SettingsPage() {
  const {
    settings,
    net,
    versions,
    theme,
    setTheme,
    loadSettings,
    loadMe,
    loadTunnels,
    loadNet,
    toast,
  } = useApp()

  // 表单是受控的：只在 settings 真的刷新时回填，避免每次渲染把用户输入冲掉
  const [publicAddr, setPublicAddr] = useState('')
  // 两个源列表（有序、逗号分隔）。以前的单值 mirror_base 已被它们取代。
  const [serverSources, setServerSources] = useState('github')
  const [clientSources, setClientSources] = useState('panel')
  // 逐项测速结果，key 是源 token。面板测得到自己的网络，测不到客户端的，
  // 所以只有服务端列表能测。
  const [probe, setProbe] = useState<Record<string, MirrorTestResult>>({})
  const [pStart, setPStart] = useState('')
  const [pEnd, setPEnd] = useState('')
  const [bindPort, setBindPort] = useState('')
  const [dashPort, setDashPort] = useState('')
  const [token, setToken] = useState('')
  const [pass, setPass] = useState('')
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)

  // 表单只在「第一次拿到设置」时回填一次。
  //
  // 早期写法是 useEffect(..., [settings]) —— 而 store 每次 loadSettings 都会
  // 放一个**新的对象**进来，于是依赖每次都变、表单被反复重置。用户正在输入
  // 公网地址时，只要期间发生过一次设置刷新（切页面回来、保存别的项），
  // 输入框就被冲回库里的旧值；如果那次保存发生在这之后，提交上去的就是空。
  // 用户看到的现象就是「填了没生效 / 更新后配置消失」。
  //
  // hydrated 标记保证：除非保存成功后再显式重新回填，否则不动用户的输入。
  const hydrated = useRef(false)

  // fillForm 把「库里的值」灌进表单。抽出来是为了让「放弃修改」也能复用：
  // 那个按钮原来只是重新 loadSettings，而 loadSettings 会因为 hydrated 已置位
  // 不再回填，于是点了没反应。
  const fillForm = useCallback((s: SettingsShape) => {
    setPublicAddr(s.public_addr ?? '')
    setServerSources(s.server_sources || 'github')
    setClientSources(s.client_sources || 'panel')
    setPStart(s.port_range_start ?? '')
    setPEnd(s.port_range_end ?? '')
    setBindPort(s.frps_bind_port ?? '')
    setDashPort(s.dashboard_port ?? '')
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
      const trim = (v: string) => v.trim()
      const body: SettingsBody = {}

      if (trim(publicAddr) !== trim(settings.public_addr ?? '')) {
        body.public_addr = trim(publicAddr)
        // 清空公网地址是破坏性操作（安装命令和客户端连 frps 的地址都失去基准），
        // 必须显式带上 clear 标记，服务端才允许写入空值。
        if (!body.public_addr) body.clear_public_addr = '1'
      }
      if (serverSources !== (settings.server_sources || 'github')) body.server_sources = serverSources
      if (clientSources !== (settings.client_sources || 'panel')) body.client_sources = clientSources
      if (trim(pStart) !== trim(settings.port_range_start ?? '')) body.port_range_start = trim(pStart)
      if (trim(pEnd) !== trim(settings.port_range_end ?? '')) body.port_range_end = trim(pEnd)
      if (trim(bindPort) !== trim(settings.frps_bind_port ?? '')) body.frps_bind_port = trim(bindPort)
      if (trim(dashPort) !== trim(settings.dashboard_port ?? '')) body.dashboard_port = trim(dashPort)
      // 令牌输入框拿到的是脱敏串，只有用户重新输入时才提交
      if (token.trim()) body.frps_token = token.trim()
      if (pass) body.new_password = pass

      if (Object.keys(body).length === 0) {
        toast('没有改动，无需保存')
        return
      }

      const r = await api<SettingsSaveResult>('POST', '/api/settings', body)
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
      toast((e as Error).message, 'bad')
    } finally {
      setSaving(false)
    }
  }, [
    saving,
    settings,
    publicAddr,
    serverSources,
    clientSources,
    pStart,
    pEnd,
    bindPort,
    dashPort,
    token,
    pass,
    loadSettings,
    loadMe,
    loadTunnels,
    toast,
  ])

  // 按源测速。面板只能测自己这台机器的网络，所以只对服务端列表提供。
  async function probeSource(tok: string) {
    try {
      const q = qs({ mirror: tok === 'github' ? '' : tok, version: versions?.target_version || 'latest' })
      const r = await api<MirrorTestResult>('GET', `/api/system/mirror-test?${q}`)
      setProbe((p) => ({ ...p, [tok]: r }))
    } catch (e) {
      setProbe((p) => ({ ...p, [tok]: { ok: false, error: (e as Error).message } }))
    }
  }

  async function probeAll() {
    if (testing) return
    setTesting(true)
    try {
      // 顺序测，不并发：并发会让每个源的测速结果互相抢带宽，排出来的顺序反而不可信。
      for (const tok of parseSources(serverSources)) {
        await probeSource(tok)
      }
    } finally {
      setTesting(false)
    }
  }

  const nicChips = [
    ...(net?.public_ip
      ? [{ ip: net.public_ip, name: '公网出口 IP · 推荐', tone: 'info' as const }]
      : []),
    ...(net?.nics ?? []).map((n) => ({ ip: n.ip, name: n.name, tone: 'gray' as const })),
  ]

  return (
    <>
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

      <Card
        title="二进制下载"
        desc="两个有序列表：面板自己取、客户端取。从左往右试，第一个能用的就停"
      >
        <div className="mb-2 text-[13px] font-semibold text-fg">
          ① 面板自己取 frps / frpc / 自己的更新
        </div>
        <SourceListEditor
          value={serverSources}
          onChange={setServerSources}
          allowPanel={false}
          probe={probe}
        />
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Button variant="ghost" size="sm" disabled={testing} onClick={() => void probeAll()}>
            {testing ? '测速中…' : '逐个测速'}
          </Button>
          <span className="text-xs text-dim">
            每项实下 512KB 测速，按结果调整顺序比凭感觉猜可靠
          </span>
        </div>
        <Hint>
          这是<b>面板这台机器</b>自己出网取二进制时的顺序（frps、frpc 缓存、agent 缓存、面板自更新）。
          某项失败才试下一项，失败原因记进日志。
          <br />
          没有 panel 选项：面板不能给自己中继。
        </Hint>

        <div className="mb-2 mt-6 text-[13px] font-semibold text-fg">② 客户端取 agent / frpc</div>
        <SourceListEditor
          value={clientSources}
          onChange={setClientSources}
          allowPanel
          probe={probe}
        />
        <Hint>
          这是<b>客户端那台机器</b>从哪儿取。两种通道：
          <b>面板中继</b>（客户端只连面板，由面板转发；任何环境都能用，但占面板出口带宽）
          和<b>镜像前缀</b>（客户端经该前缀自己取 GitHub 上的文件，不占面板带宽，要求它能出网）。
          最后一项可以是「GitHub 直连」——没配任何前缀时它最慢，先测一下再排。
          <br />
          这里没有测速：面板测不到客户端那台机器的网络。
          <br />
          改完下一轮心跳就生效，<b>不用重装客户端</b>。安装那一步固定经面板（装的时候 agent
          还没跑起来、这份顺序还没下发，而且装不上比走慢一点的通道严重得多）。
        </Hint>
      </Card>

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

      <Card title="高级" desc="一般不用动">
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
        <div className="mt-3.5 grid grid-cols-2 gap-3.5 max-[820px]:grid-cols-1">
          <Field label="frps 状态端口（dashboard，仅 127.0.0.1）" className="mb-0">
            <Input type="number" value={dashPort} onChange={(e) => setDashPort(e.target.value)} />
          </Field>
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
        </div>
      </Card>

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
      {/* 版本与更新、以及各种二进制文件，都搬到了「这台服务器 → 文件与更新」。
          两处分开时，用户看到「有新版」却要去另一页看文件是否就绪，很别扭。
          这里只留一个入口，别在这里再挂一份（挂了就会出现两个更新按钮，
          状态还各自轮询、互相打架）。 */}
      <Hint>
        面板与客户端的版本、以及 frps / frpc / agent 各平台的二进制文件，都在左侧{' '}
        <a className="cursor-pointer font-semibold text-accent-fg underline" onClick={() => (window.location.hash = '#/files')}>
          这台服务器 → 文件与更新
        </a>
        。
      </Hint>
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
        <Button
          variant="ghost"
          size="sm"
          onClick={async () =>
            toast((await copyText(settings?.install_token ?? '')) ? '已复制安装令牌' : '复制失败', 'ok')
          }
        >
          复制安装令牌
        </Button>
        {!settings && (
          <span className="text-xs text-dim">
            配置还在加载中，暂不能保存 —— 空白表单如果直接保存会把已有配置覆盖掉
          </span>
        )}
      </div>
      <Hint>安装令牌用于客户端认领到这台面板；更换后旧客户端仍保持已授权，不影响现有隧道。</Hint>
    </>
  )
}
