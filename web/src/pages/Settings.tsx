import { useCallback, useEffect, useState } from 'react'
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
import type { MirrorTestResult, SettingsBody, SettingsSaveResult } from '../types'

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
  const [dashPort, setDashPort] = useState('')
  const [token, setToken] = useState('')
  const [pass, setPass] = useState('')
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<MirrorTestResult | null>(null)

  useEffect(() => {
    if (!settings) return
    setPublicAddr(settings.public_addr ?? '')
    setMirror(settings.mirror_base ?? '')
    setPStart(settings.port_range_start ?? '')
    setPEnd(settings.port_range_end ?? '')
    setBindPort(settings.frps_bind_port ?? '')
    setDashPort(settings.dashboard_port ?? '')
    setToken('')
    setPass('')
  }, [settings])

  useEffect(() => {
    void loadNet()
  }, [loadNet])

  const save = useCallback(async () => {
    if (saving) return
    setSaving(true)
    try {
      const body: SettingsBody = {
        public_addr: publicAddr.trim(),
        mirror_base: mirror.trim(),
        port_range_start: pStart.trim(),
        port_range_end: pEnd.trim(),
        frps_bind_port: bindPort.trim(),
        dashboard_port: dashPort.trim(),
      }
      // 令牌输入框拿到的是脱敏串，只有用户重新输入时才提交
      if (token.trim()) body.frps_token = token.trim()
      if (pass) body.new_password = pass

      const r = await api<SettingsSaveResult>('POST', '/api/settings', body)
      setToken('')
      setPass('')
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

      <Card title="下载源" desc="拉取 frps / frpc 二进制用">
        <Field label="镜像前缀（留空 = GitHub 官方）">
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
          镜像按「前缀 + GitHub 原始链接」拼接。测试会实际解析版本资产并下载 512KB
          测速，结果比凭感觉猜可靠。
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
      <div className="flex flex-wrap gap-2.5">
        <Button onClick={() => void save()} disabled={saving}>
          {saving ? '保存中…' : '保存设置'}
        </Button>
        <Button variant="ghost" onClick={() => void loadSettings()}>
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
      </div>
      <Hint>安装令牌用于客户端认领到这台面板；更换后旧客户端仍保持已授权，不影响现有隧道。</Hint>
    </>
  )
}
