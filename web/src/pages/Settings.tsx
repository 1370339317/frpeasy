import { useCallback, useEffect, useRef, useState } from 'react'
import { useApp } from '../store'
import { api, qs } from '../lib/api'
import { copyText, fmtBytes } from '../lib/format'
import {
  Button,
  Card,
  Chip,
  Field,
  Hint,
  Input,
} from '../components/ui'
import type { MirrorTestResult, Settings as SettingsShape, SettingsBody, SettingsSaveResult } from '../types'

// 客户端取二进制的来源链。合法组合只有这四种，所以只给预设，不做自由输入 ——
// 手打逗号串的结果通常是拼写错误，而错误的表现是"客户端一直拉取失败"。
// 名字必须自己解释自己。原来叫「仅面板 / 面板 → Git / 仅 Git」，
// 用户第一眼会以为 Git 是上面镜像预设里的某一项 —— 那是两码事：
// 上面填的是"代理前缀"，这里选的是"谁来下载"。所以一律带上"中继 / GitHub"。
const CHAIN_PRESETS: { label: string; value: string; note: string }[] = [
  { label: '仅面板中继', value: 'panel', note: '默认；客户端只连面板' },
  { label: '面板中继 → GitHub', value: 'panel,git', note: '面板取不到再自己取' },
  { label: 'GitHub → 面板中继', value: 'git,panel', note: '优先自己取，省面板带宽' },
  { label: '仅 GitHub', value: 'git', note: '客户端必须能出网' },
]

const MIRROR_PRESETS: { label: string; value: string; note?: string }[] = [
  { label: 'GitHub 官方', value: '' },
  { label: 'ghfast', value: 'https://ghfast.top/', note: '国内快' },
  { label: 'ghproxy', value: 'https://ghproxy.com/' },
  { label: 'gh-proxy', value: 'https://gh-proxy.com/' },
]

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
  const [mirror, setMirror] = useState('')
  const [pStart, setPStart] = useState('')
  const [pEnd, setPEnd] = useState('')
  const [bindPort, setBindPort] = useState('')
  // 客户端取二进制的来源链。这里只给几个预设，不做自由编辑 ——
  // 合法组合只有四种，让用户手打逗号串只会打出拼写错误。
  const [chain, setChain] = useState('panel')
  const [dashPort, setDashPort] = useState('')
  const [token, setToken] = useState('')
  const [pass, setPass] = useState('')
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<MirrorTestResult | null>(null)

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
    setMirror(s.mirror_base ?? '')
    setPStart(s.port_range_start ?? '')
    setPEnd(s.port_range_end ?? '')
    setBindPort(s.frps_bind_port ?? '')
    setChain(s.bin_source_chain || 'panel')
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
      if (trim(mirror) !== trim(settings.mirror_base ?? '')) body.mirror_base = trim(mirror)
      if (trim(pStart) !== trim(settings.port_range_start ?? '')) body.port_range_start = trim(pStart)
      if (trim(pEnd) !== trim(settings.port_range_end ?? '')) body.port_range_end = trim(pEnd)
      if (trim(bindPort) !== trim(settings.frps_bind_port ?? '')) body.frps_bind_port = trim(bindPort)
      if (trim(dashPort) !== trim(settings.dashboard_port ?? '')) body.dashboard_port = trim(dashPort)
      if (chain !== (settings.bin_source_chain || 'panel')) body.bin_source_chain = chain
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
    mirror,
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

  async function testMirror() {
    if (testing) return
    setTesting(true)
    setTestResult(null)
    try {
      const q = qs({ mirror: mirror.trim(), version: versions?.target_version || 'latest' })
      const r = await api<MirrorTestResult>('GET', `/api/system/mirror-test?${q}`)
      setTestResult(r)
    } catch (e) {
      setTestResult({ ok: false, error: (e as Error).message })
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

      <Card title="二进制下载" desc="分两项：面板自己取（frps / frpc）、客户端取（agent / frpc）">
        <div className="mb-2 text-[13px] font-semibold text-fg">
          ① 面板自己取 frps / frpc
        </div>
        <Field label="镜像前缀（留空 = 直连 github.com）">
          <Input
            value={mirror}
            onChange={(e) => setMirror(e.target.value)}
            placeholder="如 https://ghfast.top/"
          />
        </Field>
        <div className="flex flex-wrap items-center gap-2.5">
          <div className="flex flex-wrap gap-2">
            {MIRROR_PRESETS.map((p) => (
              <Chip
                key={p.value}
                selected={mirror === p.value}
                sub={p.note}
                onClick={() => {
                  setMirror(p.value)
                  toast(p.value ? '已选择镜像' : '已切回 GitHub 官方')
                }}
              >
                {p.label}
              </Chip>
            ))}
          </div>
          <Button variant="ghost" size="sm" onClick={() => void testMirror()} disabled={testing}>
            {testing ? '测试中…' : '测试当前源'}
          </Button>
        </div>
        <Hint>
          这是<b>面板这台机器</b>去 github.com 取 frps / frpc 时用的地址代理，
          按「前缀 + GitHub 原始链接」拼接。测试会实际解析版本资产并下载 512KB 测速。
          <br />
          它同时也会拼进下面「GitHub」那条通道的地址里 —— 两处用的是同一个前缀。
        </Hint>

        <div className="mb-2 mt-5 text-[13px] font-semibold text-fg">
          ② 客户端取 agent / frpc
        </div>
        <Field label="来源顺序">
          <div className="flex flex-wrap gap-2">
            {CHAIN_PRESETS.map((p) => (
              <Chip
                key={p.value}
                selected={chain === p.value}
                sub={p.note}
                onClick={() => setChain(p.value)}
              >
                {p.label}
              </Chip>
            ))}
          </div>
        </Field>
        <Hint>
          这里决定的是<b>谁来下载</b>，只有两条通道：
          <br />
          · <b>面板中继</b>：客户端只连面板，由面板把文件转发过去。任何环境都能用，
          但占面板的出口带宽（装一台要发 6.5MB 的 agent，之后还有 20MB 的 frpc）。
          <br />
          · <b>GitHub</b>：客户端自己去 GitHub Release 取，<b>自动套用上面填的镜像前缀</b>。
          不占面板带宽，但这台机器必须能出网。
          <br />
          选两项时按顺序尝试，前一个失败才试后一个；失败原因会写进客户端卡片。
          改完下一轮心跳就生效，<b>不需要重装客户端</b>（安装命令里显式选过 GitHub 的机器除外，
          那些是钉死的，不参与回退）。
          <br />
          注意：<b>没填镜像前缀时「GitHub」这条路就是直连 github.com</b>，
          国内实测比经面板还慢，先配好前缀再用。
        </Hint>

        {testing && (
          <div className="mt-3 rounded-[10px] border border-border bg-card2 px-3.5 py-3 text-[12.5px]">
            正在解析版本资产并下载 512KB 测速，最多 45 秒…
          </div>
        )}
        {testResult && !testing && (
          <div
            className={
              testResult.ok
                ? 'mt-3 rounded-[10px] border border-ok bg-card2 px-3.5 py-3 text-[12.5px] leading-[1.6] text-ok-fg'
                : 'mt-3 rounded-[10px] border border-bad bg-card2 px-3.5 py-3 text-[12.5px] leading-[1.6] text-bad-fg'
            }
          >
            {testResult.ok ? (
              <>
                <b>可用</b> · 下载 {fmtBytes(testResult.bytes)} 用了 {testResult.elapsed_ms} ms · 约{' '}
                {Math.round(testResult.speed_kbps ?? 0)} KB/s
              </>
            ) : (
              <>
                <b>不可用</b> · {testResult.error || '未知错误'}
              </>
            )}
            {testResult.url && (
              <div className="mt-1.5 font-mono text-xs break-all text-dim">{testResult.url}</div>
            )}
          </div>
        )}
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
