import { useCallback, useEffect, useRef, useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { Badge, Button, Card, Field, Hint, Input, cx } from '../components/ui'
import type {
  CdnConfig,
  CdnConfigPatch,
  CdnDomain,
  CdnDomainCreateResult,
  CdnDomainsResult,
  CdnProvisionResult,
  CdnVerifyResult,
  CdnAuditEntry,
  CdnZone,
  CdnZoneProbe,
  LinkQualityApplied,
  LinkQualityResponse,
} from '../types'

/**
 * CDN 加速接入（腾讯云 EdgeOne）—— 四步向导。
 *
 * 解决的问题：frpc → frps 这段跨境裸 TCP 一旦丢包，吞吐会塌到 KB/s 量级；
 * 把这段换成 CDN 边缘回源之后，客户端只需要一条到就近节点的 wss 长连接。
 *
 * 为什么是向导而不是一张表单：这条链路的四件事是有严格顺序的（验密钥 →
 * 选站点 → 选域名 → 建加速域名并开 WS），而且前一步的输出就是后一步的输入
 * （ZoneId、ZoneName、CNAME）。平铺表单逼用户自己记顺序、手工从表格里抄
 * ZoneId、手工打全域名，错一步就得到一条看不懂的云端报错。
 *
 * 状态机只有两个变量：
 *   step     —— 当前显示哪一步；
 *   unlocked —— 已经解锁到第几步（最多到哪）。
 * 第 1..unlocked-1 步视为「已完成」，可以点回去重做；> unlocked 的置灰；
 * 每一步的完成动作只做两件事：把结果 POST 到 /api/cdn/config，然后
 * unlock(下一步) + setStep(下一步)。配置本身只写库，所以任何一步都可以
 * 安全地重复进入 —— 回读的 cfg 才是唯一事实来源。
 */

type Step = 1 | 2 | 3 | 4

const ALL_STEPS: Step[] = [1, 2, 3, 4]

const STEP_TITLE: Record<Step, string> = {
  1: '连接腾讯云',
  2: '选站点',
  3: '选域名',
  4: '接入',
}

const STEP_NO: Record<Step, string> = { 1: '①', 2: '②', 3: '③', 4: '④' }

/** 与 internal/shared.DefaultFRPSBindPort 保持一致；settings 没加载完时兜底。 */
const DEFAULT_FRPS_PORT = 27900

const POLL_INTERVAL_MS = 2000
const POLL_TIMEOUT_MS = 60_000

type Busy = 'connect' | 'zone' | 'domain' | 'create' | 'provision' | 'toggle' | 'ws' | null

type LoadState = 'idle' | 'loading' | 'ready' | 'error'

/** 后端错误原始文本 + 可能夹在里面的诊断字段（4xx 时只给纯文本）。 */
interface BackendErr {
  text: string
  hint: string
  probes: CdnZoneProbe[]
}

interface PollState {
  running: boolean
  verdict: '' | 'ok' | 'timeout'
  applied: LinkQualityApplied[]
  err: string
}

const IDLE_POLL: PollState = { running: false, verdict: '', applied: [], err: '' }

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/**
 * 把接口错误拆成「原文 + 可选诊断」。
 *
 * api() 对非 JSON 响应体会把纯文本塞进 message，所以多数情况直接用 text；
 * 个别路径回的是 JSON，顺手取走 hint / probes，别让它们埋在原文里。
 */
function readErr(e: unknown): BackendErr {
  const text = errText(e)
  const out: BackendErr = { text, hint: '', probes: [] }
  const s = text.trim()
  if (!s.startsWith('{')) return out
  try {
    const v: unknown = JSON.parse(s)
    if (typeof v !== 'object' || v === null) return out
    const rec = v as Record<string, unknown>
    if (typeof rec.error === 'string' && rec.error !== '') out.text = rec.error
    if (typeof rec.hint === 'string') out.hint = rec.hint
    if (Array.isArray(rec.probes)) {
      out.probes = rec.probes.flatMap((p): CdnZoneProbe[] => {
        if (typeof p !== 'object' || p === null) return []
        const r = p as Record<string, unknown>
        return [{ action: String(r.action ?? ''), ok: r.ok === true, detail: String(r.detail ?? '') }]
      })
    }
  } catch {
    // 不是 JSON：message 本身就是后端纯文本
  }
  return out
}

function ProbeList({ probes }: { probes: CdnZoneProbe[] }) {
  return (
    <div className="mt-2">
      <div className="mb-1 text-[11.5px] font-[650]">权限探针</div>
      <div className="grid gap-1">
        {probes.map((pr, i) => (
          <div key={`${pr.action}-${i}`} className="flex min-w-0 items-baseline gap-1.5">
            <span className={pr.ok ? 'text-ok-fg' : 'text-bad-fg'}>{pr.ok ? '✓' : '✗'}</span>
            <span className="shrink-0 font-mono text-[11.5px]">{pr.action}</span>
            <span className="min-w-0 break-all text-[11px] opacity-80" title={pr.detail}>
              {pr.ok ? '允许' : pr.detail || '被拒'}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

function ErrBlock({ title, err }: { title: string; err: BackendErr }) {
  return (
    <div className="mt-3 rounded-lg border border-bad-fg/40 bg-bad-dim px-3.5 py-2.5 text-[12px] leading-[1.65] text-bad-fg">
      <div className="mb-0.5 font-[650]">{title}</div>
      <div className="break-all" title={err.text}>
        {err.text}
      </div>
      {err.hint && (
        <div className="mt-1.5 break-all opacity-90" title={err.hint}>
          {err.hint}
        </div>
      )}
      {err.probes.length > 0 && <ProbeList probes={err.probes} />}
    </div>
  )
}

export function CdnCard() {
  const { toast, settings } = useApp()

  // 回源端口跟着 frps 的实际 bindPort 走，别让提示里的数字和真实配置脱节。
  const parsedBind = Number.parseInt(settings?.frps_bind_port ?? '', 10)
  const originPort = Number.isFinite(parsedBind) && parsedBind > 0 ? parsedBind : DEFAULT_FRPS_PORT

  const [cfg, setCfg] = useState<CdnConfig | null>(null)
  const [loadErr, setLoadErr] = useState('')
  const [step, setStep] = useState<Step>(1)
  const [unlocked, setUnlocked] = useState<Step>(1)
  const [busy, setBusy] = useState<Busy>(null)

  // 第 1 步
  const [secretId, setSecretId] = useState('')
  const [secretKey, setSecretKey] = useState('')
  const [connectErr, setConnectErr] = useState<BackendErr | null>(null)
  const [connectedCount, setConnectedCount] = useState<number | null>(null)

  // 第 2 步
  const [verify, setVerify] = useState<CdnVerifyResult | null>(null)
  const [zones, setZones] = useState<CdnZone[]>([])
  const [zonesState, setZonesState] = useState<LoadState>('idle')
  const [zonesErr, setZonesErr] = useState<BackendErr | null>(null)
  const [zone, setZone] = useState<CdnZone | null>(null)
  const [zoneErr, setZoneErr] = useState('')

  // 第 3 步
  const [domains, setDomains] = useState<CdnDomain[]>([])
  const [domainsState, setDomainsState] = useState<LoadState>('idle')
  const [domainsErr, setDomainsErr] = useState<BackendErr | null>(null)
  const [creating, setCreating] = useState(false)
  const [prefix, setPrefix] = useState('')
  const [domainErr, setDomainErr] = useState<BackendErr | null>(null)

  // 第 4 步
  const [prov, setProv] = useState<CdnProvisionResult | null>(null)
  const [provErr, setProvErr] = useState<BackendErr | null>(null)
  const [toggleErr, setToggleErr] = useState('')
  const [poll, setPoll] = useState<PollState>(IDLE_POLL)

  const booted = useRef(false)
  const pollGen = useRef(0)
  const pollTimer = useRef<number | null>(null)

  const clearPoll = useCallback(() => {
    if (pollTimer.current !== null) {
      window.clearTimeout(pollTimer.current)
      pollTimer.current = null
    }
  }, [])

  const stopPolling = useCallback(() => {
    pollGen.current += 1
    clearPoll()
  }, [clearPoll])

  /**
   * 轮询 /api/linkquality，直到所有客户端都上报 match=true 或超时。
   *
   * 用「代号 + setTimeout 递归」而不是 setInterval：每次请求返回后再排下一次，
   * 慢请求不会堆叠；任何一次 start/stop 都会让旧代号失效，卸载时清定时器即可，
   * 不会有回调落到已卸载的组件上。
   */
  const startPolling = useCallback(() => {
    pollGen.current += 1
    const gen = pollGen.current
    clearPoll()
    const deadline = Date.now() + POLL_TIMEOUT_MS
    setPoll({ running: true, verdict: '', applied: [], err: '' })

    const tick = async () => {
      if (pollGen.current !== gen) return
      try {
        const r = await api<LinkQualityResponse>('GET', '/api/linkquality')
        if (pollGen.current !== gen) return
        const applied = r.applied ?? []
        // applied 为空（没有客户端）不能算「已切换」——宁可等到超时后说清楚。
        if (applied.length > 0 && applied.every((a) => a.match)) {
          setPoll({ running: false, verdict: 'ok', applied, err: '' })
          return
        }
        if (Date.now() >= deadline) {
          setPoll({ running: false, verdict: 'timeout', applied, err: '' })
          return
        }
        setPoll({ running: true, verdict: '', applied, err: '' })
      } catch (e) {
        if (pollGen.current !== gen) return
        if (Date.now() >= deadline) {
          setPoll({ running: false, verdict: 'timeout', applied: [], err: errText(e) })
          return
        }
      }
      pollTimer.current = window.setTimeout(() => void tick(), POLL_INTERVAL_MS)
    }
    void tick()
  }, [clearPoll])

  const reload = useCallback(async (): Promise<CdnConfig | null> => {
    try {
      const c = await api<CdnConfig>('GET', '/api/cdn/config')
      setCfg(c)
      setLoadErr('')
      // 只在首次读配置时决定落在哪一步：之后的回读（每一步保存后）不能把
      // 用户从当前步骤硬拽走。
      if (!booted.current) {
        booted.current = true
        const hasCred = c.cdn_tencent_secret_id !== '' || c.cdn_tencent_secret_key !== ''
        const entry: Step = hasCred ? (c.cdn_domain ? 4 : 2) : 1
        setStep(entry)
        setUnlocked(entry)
        // 之前已经开过加速：直接开始确认客户端到底切没切，省得用户再点一遍。
        if (entry === 4 && c.desired_protocol === 'wss') startPolling()
      }
      return c
    } catch (e) {
      setLoadErr(errText(e))
      return null
    }
  }, [startPolling])

  const loadZones = useCallback(async () => {
    setZonesState('loading')
    setZonesErr(null)
    try {
      const r = await api<CdnVerifyResult>('POST', '/api/cdn/verify')
      setVerify(r)
      setZones(r.zones ?? [])
      setZonesState('ready')
    } catch (e) {
      setVerify(null)
      setZones([])
      setZonesErr(readErr(e))
      setZonesState('error')
    }
  }, [])

  const loadDomains = useCallback(async (zoneID: string) => {
    if (!zoneID) return
    setDomainsState('loading')
    setDomainsErr(null)
    try {
      const r = await api<CdnDomainsResult>('POST', '/api/cdn/domains', { zone_id: zoneID })
      const list = r.domains ?? []
      setDomains(list)
      setDomainsState('ready')
      // 站点下一条域名都没有时不摆空列表，直接给「填前缀」的表单。
      if (list.length === 0) setCreating(true)
    } catch (e) {
      setDomains([])
      setDomainsErr(readErr(e))
      setDomainsState('error')
    }
  }, [])

  const [audit, setAudit] = useState<CdnAuditEntry[] | null>(null)
  // 追溯用：把面板对腾讯云/阿里云的每次调用都列出来，写操作高亮。
  const loadAudit = useCallback(async () => {
    try {
      const r = await api<{ entries: CdnAuditEntry[] | null }>('GET', '/api/cdn/audit?limit=40')
      setAudit(r.entries ?? [])
    } catch {
      setAudit([])
    }
  }, [])

  useEffect(() => {
    void reload()
    void loadAudit()
  }, [reload, loadAudit])

  // 卸载清理：让在飞的轮询代号失效并清掉待触发的定时器。
  useEffect(() => () => stopPolling(), [stopPolling])

  useEffect(() => {
    if (step !== 2 || zonesState !== 'idle') return
    void loadZones()
  }, [step, zonesState, loadZones])

  useEffect(() => {
    if (step !== 3 || domainsState !== 'idle') return
    const id = zone?.ZoneId || cfg?.cdn_tencent_zone_id || ''
    if (!id) return
    void loadDomains(id)
  }, [step, domainsState, zone, cfg, loadDomains])

  const unlock = (s: Step) => setUnlocked((u) => (u < s ? s : u))

  /** 第 1 步：先保存密钥，再验证。验证的是「已保存」的密钥，顺序不能反。 */
  const connect = async () => {
    if (busy !== null) return
    setBusy('connect')
    setConnectErr(null)
    setConnectedCount(null)
    // 重新连接可能换了账号/密钥：上一条密钥看到的站点列表不再可信。
    setZones([])
    setVerify(null)
    setZonesErr(null)
    setZonesState('idle')
    try {
      const patch: CdnConfigPatch = {}
      const id = secretId.trim()
      const key = secretKey.trim()
      // 留空 = 不改；恰好等于掩码也视为没改 —— 回传掩码会把库里的明文覆盖掉。
      if (id !== '' && id !== cfg?.cdn_tencent_secret_id) patch.cdn_tencent_secret_id = id
      if (key !== '' && key !== cfg?.cdn_tencent_secret_key) patch.cdn_tencent_secret_key = key
      if (Object.keys(patch).length > 0) {
        await api<CdnConfig>('POST', '/api/cdn/config', patch)
        await reload()
      }
      const r = await api<CdnVerifyResult>('POST', '/api/cdn/verify')
      const list = r.zones ?? []
      setVerify(r)
      setZones(list)
      setZonesState('ready')
      setZonesErr(null)
      setConnectedCount(list.length)
      setSecretId('')
      setSecretKey('')
      unlock(2)
      setStep(2)
      toast(`已连接腾讯云，可见 ${list.length} 个站点`, 'ok')
    } catch (e) {
      setConnectErr(readErr(e))
      toast('连接腾讯云失败', 'bad')
    } finally {
      setBusy(null)
    }
  }

  const chooseZone = async (z: CdnZone) => {
    if (busy !== null) return
    setBusy('zone')
    setZoneErr('')
    try {
      // 换了站点意味着旧域名属于另一个站点：不清掉的话，后面接入会拿着
      // A 站点的域名去 B 站点建加速域名。域名清空后后端不会切 wss，安全。
      const changed = z.ZoneId !== (cfg?.cdn_tencent_zone_id ?? '')
      await api<CdnConfig>('POST', '/api/cdn/config', {
        cdn_tencent_zone_id: z.ZoneId,
        ...(changed ? { cdn_domain: '' } : {}),
      })
      await reload()
      setZone(z)
      // 换了站点，域名列表必须重来：旧列表属于另一个站点。
      setDomains([])
      setDomainsErr(null)
      setDomainsState('idle')
      setCreating(false)
      setPrefix('')
      setDomainErr(null)
      if (changed) {
        setProv(null)
        setProvErr(null)
        setPoll(IDLE_POLL)
        stopPolling()
        setUnlocked(3)
      } else {
        unlock(3)
      }
      setStep(3)
    } catch (e) {
      const msg = errText(e)
      setZoneErr(msg)
      toast(msg, 'bad')
    } finally {
      setBusy(null)
    }
  }

  const saveDomain = async (domain: string) => {
    await api<CdnConfig>('POST', '/api/cdn/config', { cdn_domain: domain })
    await reload()
    setProv(null)
    setProvErr(null)
    unlock(4)
    setStep(4)
  }

  const chooseDomain = async (d: CdnDomain) => {
    if (busy !== null) return
    setBusy('domain')
    setDomainErr(null)
    try {
      await saveDomain(d.DomainName)
      toast(`已选 ${d.DomainName}`, 'ok')
    } catch (e) {
      const be = readErr(e)
      setDomainErr(be)
      toast(be.text, 'bad')
    } finally {
      setBusy(null)
    }
  }

  const prefixClean = prefix.trim().replace(/^\.+|\.+$/g, '')
  const zoneName = zone?.ZoneName ?? ''
  const fullDomain = prefixClean !== '' && zoneName !== '' ? `${prefixClean}.${zoneName}` : ''
  const zoneID = zone?.ZoneId || cfg?.cdn_tencent_zone_id || ''

  const createDomain = async () => {
    if (busy !== null) return
    if (!zone || fullDomain === '') return
    setBusy('create')
    setDomainErr(null)
    try {
      // 只提交前缀拼出来的完整域名：主域名取自步骤 2 的 ZoneName，避免手打全名打错。
      const r = await api<CdnDomainCreateResult>('POST', '/api/cdn/domain-create', {
        zone_id: zone.ZoneId,
        domain: fullDomain,
      })
      const chosen = r.domain || fullDomain
      await saveDomain(chosen)
      toast(r.existed ? `${chosen} 已存在，直接复用` : `已创建 ${chosen}`, 'ok')
    } catch (e) {
      setDomainErr(readErr(e))
      toast('创建加速域名失败', 'bad')
    } finally {
      setBusy(null)
    }
  }

  // 显式开一次站点 WebSocket。冷却机制在不在都允许（force），由用户主动承担一次全站重部署。
  const [wsNote, setWsNote] = useState<{ ok: boolean; text: string } | null>(null)
  const forceWs = async () => {
    if (busy !== null) return
    setBusy('ws')
    setWsNote(null)
    try {
      const r = await api<{ ok: boolean; enabled: boolean; note: string }>(
        'POST',
        '/api/cdn/ws-enable',
        { force: true },
      )
      setWsNote({ ok: r.enabled, text: r.note })
      await reload()
      await loadAudit()
    } catch (e) {
      const be = readErr(e)
      setWsNote({ ok: false, text: be.text })
    } finally {
      setBusy(null)
    }
  }

  const runProvision = async () => {
    if (busy !== null) return
    setBusy('provision')
    setProvErr(null)
    setProv(null)
    try {
      // 业务失败仍是 HTTP 200：结果先落 state，再按 ok 决定提示语气。
      const r = await api<CdnProvisionResult>('POST', '/api/cdn/provision')
      setProv(r)
      await reload()
      toast(r.ok ? '接入完成' : '接入没有走完，看下面的步骤', r.ok ? 'ok' : 'bad')
    } catch (e) {
      setProvErr(readErr(e))
      toast('接入请求失败', 'bad')
    } finally {
      setBusy(null)
    }
  }

  const toggleCdn = async (on: boolean) => {
    if (busy !== null) return
    setBusy('toggle')
    setToggleErr('')
    try {
      await api<CdnConfig>('POST', '/api/cdn/config', { cdn_enabled: on ? '1' : '0' })
      await reload()
      if (on) {
        startPolling()
        toast('已启用 CDN 加速，等客户端切到 wss', 'ok')
      } else {
        stopPolling()
        setPoll(IDLE_POLL)
        toast('已关闭 CDN 加速，客户端会退回直连 TCP', 'ok')
      }
    } catch (e) {
      const msg = errText(e)
      setToggleErr(msg)
      toast(msg, 'bad')
    } finally {
      setBusy(null)
    }
  }

  const isWss = cfg?.desired_protocol === 'wss'
  const protocolLabel = !cfg?.desired_protocol
    ? '直连 TCP'
    : cfg.desired_protocol === 'wss'
      ? '经 CDN（wss）'
      : cfg.desired_protocol
  const provisionSteps = prov?.steps ?? []
  // 已经接好（本次跑通）或早已启用过：才把按钮换成开关。
  const showToggle = prov?.ok === true || cfg?.cdn_enabled === '1'

  return (
    <Card title="CDN 加速接入" desc="腾讯云 EdgeOne · 换掉 frpc → frps 这段跨境链路">
      <div className="mb-3.5 text-[12.5px] leading-[1.7] text-muted">
        四步走完就能用：连接腾讯云 → 选站点 → 选域名 → 接入。站点 ID、域名全名、
        CNAME 都由面板从云上取，不用手工复制。
      </div>

      {/* 常驻的生效状态：这是「客户端真正在用」的值，不是表单里想改成的样子。 */}
      <div className="mb-3.5 rounded-lg border border-border2 bg-chip px-3.5 py-2.5 text-[12px] leading-[1.7]">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
          <span className="text-dim">客户端传输</span>
          <Badge tone={isWss ? 'ok' : 'gray'}>{protocolLabel}</Badge>
          <span className="text-dim">连接地址</span>
          <span
            className="min-w-0 font-mono break-all text-fg"
            title={cfg?.connect_addr || undefined}
          >
            {cfg?.connect_addr || '—'}
          </span>
        </div>
        {loadErr && (
          <div className="mt-1.5 text-[11.5px] leading-[1.6] text-bad-fg">
            读取配置失败：
            <span className="break-all" title={loadErr}>
              {loadErr}
            </span>
          </div>
        )}
      </div>

      {/* 步骤指示：已完成（< unlocked）可点回退，未解锁置灰。 */}
      <div className="mb-3.5 flex flex-wrap items-center gap-1.5">
        {ALL_STEPS.map((s) => {
          const done = s < unlocked
          const active = s === step
          const locked = s > unlocked
          return (
            <button
              key={s}
              type="button"
              disabled={locked || busy !== null}
              title={locked ? '先完成上一步' : STEP_TITLE[s]}
              onClick={() => setStep(s)}
              className={cx(
                'inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-[12px] transition disabled:cursor-not-allowed',
                active && 'border-accent bg-accent-dim font-[650] text-accent-fg',
                !active && done && 'cursor-pointer border-ok-fg/40 bg-ok-dim text-ok-fg',
                !active && locked && 'border-border2 bg-chip text-dim opacity-60',
                !active &&
                  !done &&
                  !locked &&
                  'cursor-pointer border-border2 bg-chip text-muted hover:border-accent',
              )}
            >
              <span>{done ? '✓' : STEP_NO[s]}</span>
              <span>{STEP_TITLE[s]}</span>
            </button>
          )
        })}
      </div>

      {/* ---------------- 第 1 步：连接腾讯云 ---------------- */}
      {step === 1 && (
        <div>
          <div className="grid grid-cols-2 gap-x-3.5 max-[820px]:grid-cols-1">
            <Field label="SecretId">
              <Input
                type="password"
                autoComplete="off"
                value={secretId}
                placeholder={cfg?.cdn_tencent_secret_id ? '已配置，留空不修改' : 'AKID 开头'}
                onChange={(e) => setSecretId(e.target.value)}
              />
            </Field>
            <Field label="SecretKey">
              <Input
                type="password"
                autoComplete="new-password"
                value={secretKey}
                placeholder={cfg?.cdn_tencent_secret_key ? '已配置，留空不修改' : '腾讯云 API 密钥'}
                onChange={(e) => setSecretKey(e.target.value)}
              />
            </Field>
          </div>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <Button disabled={busy !== null} onClick={() => void connect()}>
              {busy === 'connect' ? '连接中…' : '连接腾讯云'}
            </Button>
            {connectedCount !== null && (
              <span className="text-[12px] font-[650] text-ok-fg">
                ✓ 已连接，可见 {connectedCount} 个站点
              </span>
            )}
          </div>
          <Hint>
            密钥只用来调腾讯云 EdgeOne 接口，保存在面板本地；已配置过就留空。 建议给这个
            CAM 子账号绑 QcloudTEOFullAccess（资源范围选「全部资源」）。
          </Hint>
          {connectErr && <ErrBlock title="连接失败（下面是腾讯云原文）" err={connectErr} />}
        </div>
      )}

      {/* ---------------- 第 2 步：选站点 ---------------- */}
      {step === 2 && (
        <div>
          {zonesState === 'loading' && (
            <div className="py-2 text-[12.5px] text-dim">正在读取站点列表…</div>
          )}

          {zonesErr && (
            <ErrBlock title="读取站点列表失败（下面是腾讯云原文）" err={zonesErr} />
          )}
          {zonesErr && (
            <div className="mt-2 flex flex-wrap gap-2.5">
              <Button variant="ghost" size="sm" onClick={() => void loadZones()} disabled={busy !== null}>
                重新检测
              </Button>
              <Button variant="ghost" size="sm" onClick={() => setStep(1)} disabled={busy !== null}>
                回第 1 步改密钥
              </Button>
            </div>
          )}

          {zonesState === 'ready' && zones.length === 0 && (
            <div className="rounded-lg border border-warn-fg/40 bg-warn-dim px-3.5 py-2.5 text-[12px] leading-[1.7] text-warn-fg">
              <div className="mb-1 font-[650]">密钥可用，但没有拿到站点列表</div>
              <div className="break-all" title={verify?.hint || undefined}>
                {verify?.hint || '腾讯云返回了空列表，但没有给出更多诊断信息。'}
              </div>
              {verify?.endpoint && (
                <div className="mt-1.5 break-all text-[11.5px] opacity-80">
                  端点 {verify.endpoint} · TotalCount={verify.total ?? 0}
                </div>
              )}
              {verify?.probes && verify.probes.length > 0 && <ProbeList probes={verify.probes} />}
              <Button
                variant="ghost"
                size="sm"
                className="mt-2"
                onClick={() => void loadZones()}
                disabled={busy !== null}
              >
                重新检测
              </Button>
            </div>
          )}

          {zonesState === 'ready' && zones.length > 0 && (
            <>
              <div className="mb-2 flex flex-wrap items-baseline gap-2 text-[12px]">
                <span className="font-[650] text-ok-fg">
                  ✓ 已连接，可见 {zones.length} 个站点
                </span>
                <span className="text-dim">点一行选中，会自动保存并进入下一步</span>
              </div>
              <div className="grid gap-1.5">
                {zones.map((z) => {
                  const picked = z.ZoneId === cfg?.cdn_tencent_zone_id
                  return (
                    <button
                      key={z.ZoneId}
                      type="button"
                      disabled={busy !== null}
                      onClick={() => void chooseZone(z)}
                      className={cx(
                        'flex w-full items-center gap-2.5 rounded-[10px] border px-3 py-2.5 text-left transition disabled:cursor-wait',
                        picked
                          ? 'border-accent bg-accent-dim'
                          : 'border-border2 bg-chip hover:border-accent',
                      )}
                    >
                      <span className="min-w-0 flex-1">
                        <span className="flex flex-wrap items-center gap-2">
                          <b className="min-w-0 truncate text-[13px] text-fg">
                            {z.ZoneName || '（未命名站点）'}
                          </b>
                          <Badge tone={z.Status === 'online' ? 'ok' : 'gray'}>
                            {z.Status || '—'}
                          </Badge>
                          {picked && <span className="text-[11px] text-accent-fg">当前选中</span>}
                        </span>
                        <span
                          className="mt-0.5 block truncate font-mono text-[11.5px] text-dim"
                          title={z.ZoneId}
                        >
                          {z.ZoneId} · {z.Area || '—'}
                        </span>
                      </span>
                      <span className="shrink-0 text-dim">›</span>
                    </button>
                  )
                })}
              </div>
            </>
          )}

          {zoneErr && (
            <div className="mt-3 rounded-lg border border-bad-fg/40 bg-bad-dim px-3.5 py-2.5 text-[12px] leading-[1.65] text-bad-fg">
              保存站点失败：
              <span className="break-all" title={zoneErr}>
                {zoneErr}
              </span>
            </div>
          )}
        </div>
      )}

      {/* ---------------- 第 3 步：选域名 ---------------- */}
      {step === 3 && (
        <div>
          <div className="mb-2 flex flex-wrap items-baseline gap-x-2 gap-y-1 text-[12px]">
            <span className="text-dim">站点</span>
            <span className="min-w-0 break-all font-mono text-fg" title={zoneID}>
              {zoneName || zoneID || '—'}
            </span>
          </div>

          {domainsState === 'loading' && (
            <div className="py-2 text-[12.5px] text-dim">正在读取这个站点下的加速域名…</div>
          )}

          {domainsErr && (
            <ErrBlock title="读取加速域名失败（下面是腾讯云原文）" err={domainsErr} />
          )}
          {domainsErr && (
            <div className="mt-2">
              <Button
                variant="ghost"
                size="sm"
                onClick={() => void loadDomains(zoneID)}
                disabled={busy !== null || zoneID === ''}
              >
                重新检测
              </Button>
            </div>
          )}

          {domainsState === 'ready' && !creating && domains.length > 0 && (
            <>
              <div className="mb-1.5 flex flex-wrap items-center gap-2">
                <Button size="sm" onClick={() => setCreating(true)} disabled={busy !== null}>
                  新建域名
                </Button>
                <span className="text-[11.5px] text-dim">
                  多数情况直接新建；下面这些是站点里已有的，点一行可复用（不会改动它们）
                </span>
              </div>
              <div className="grid gap-1.5">
                {domains.map((d) => {
                  // HTTP / HTTPS 回源只会填其中一个，另一个是 0；0 视为「不知道」，
                  // 不要拿它出来吓人。
                  const domPort = d.HttpOriginPort || d.HttpsOriginPort || 0
                  const portMismatch =
                    domPort !== 0 && domPort !== originPort && domPort !== 443
                  return (
                    <button
                      key={d.DomainName}
                      type="button"
                      disabled={busy !== null}
                      onClick={() => void chooseDomain(d)}
                      className={cx(
                        'w-full rounded-[10px] border px-3 py-2.5 text-left transition disabled:cursor-wait',
                        d.DomainName === cfg?.cdn_domain
                          ? 'border-accent bg-accent-dim'
                          : 'border-border2 bg-chip hover:border-accent',
                      )}
                    >
                      <span className="flex flex-wrap items-center gap-2">
                        <b className="min-w-0 break-all text-[13px] text-fg">{d.DomainName}</b>
                        <Badge tone={d.DomainStatus === 'online' ? 'ok' : 'gray'}>
                          {d.DomainStatus || '—'}
                        </Badge>
                        {d.IdentificationStatus && (
                          <Badge tone="gray">{d.IdentificationStatus}</Badge>
                        )}
                      </span>
                      <span
                        className="mt-0.5 block break-all font-mono text-[11.5px] text-dim"
                        title={d.Cname}
                      >
                        CNAME {d.Cname || '—'}
                      </span>
                      {portMismatch && (
                        <span
                          className="mt-1 block break-all text-[11.5px] leading-[1.6] text-warn-fg"
                          title={`回源端口=${domPort}，frps bindPort=${originPort}`}
                        >
                          回源端口是 {domPort}，不是 frps 的 {originPort}
                          （也不是 443），这条域名接进来可能连不上 frps。
                        </span>
                      )}
                    </button>
                  )
                })}
              </div>
            </>
          )}

          {domainsState === 'ready' && creating && (
            <div className="rounded-lg border border-border2 bg-chip px-3.5 py-3">
              {zoneName === '' ? (
                <div className="text-[12px] leading-[1.7] text-warn-fg">
                  还没选中站点，无法确定主域名。
                  <Button
                    variant="ghost"
                    size="sm"
                    className="ml-2"
                    onClick={() => setStep(2)}
                    disabled={busy !== null}
                  >
                    回第 2 步选站点
                  </Button>
                </div>
              ) : (
                <>
                  <Field
                    label="域名前缀"
                    hint="主域名取自步骤 2 选中的站点，只填前缀就行"
                    className="mb-2.5"
                  >
                    <div className="flex items-center gap-1.5">
                      <Input
                        className="max-w-[200px]"
                        value={prefix}
                        placeholder="frp"
                        autoComplete="off"
                        onChange={(e) => setPrefix(e.target.value)}
                      />
                      <span className="shrink-0 break-all font-mono text-[12.5px] text-muted">
                        .{zoneName}
                      </span>
                    </div>
                  </Field>
                  <div className="mb-2.5 rounded-lg border border-border2 bg-card px-3 py-2 text-[12.5px]">
                    <span className="text-dim">将创建</span>
                    <b className="ml-2 break-all font-mono text-fg">{fullDomain || '—'}</b>
                  </div>
                  <Hint>
                    回源会自动指向本机 frps（端口 {originPort}），不用你填。
                  </Hint>
                  <div className="mt-2.5 flex flex-wrap gap-2.5">
                    <Button
                      disabled={busy !== null || fullDomain === ''}
                      onClick={() => void createDomain()}
                    >
                      {busy === 'create' ? '创建中…' : '创建并继续'}
                    </Button>
                    {domains.length > 0 && (
                      <Button
                        variant="ghost"
                        disabled={busy !== null}
                        onClick={() => {
                          setCreating(false)
                          setDomainErr(null)
                        }}
                      >
                        取消
                      </Button>
                    )}
                  </div>
                </>
              )}
            </div>
          )}

          {domainErr && <ErrBlock title="域名这一步失败了（下面是云厂商原文）" err={domainErr} />}

          {domainsState === 'ready' && domains.length > 0 && (
            <Hint>
              站点的 DNS 在腾讯 DNSPod 时，EdgeOne 添加域名会自动建好解析； 列表里的 CNAME
              只是给你核对用的，不需要再手工去别处加一遍。
            </Hint>
          )}
        </div>
      )}

      {/* ---------------- 第 4 步：接入 ---------------- */}
      {step === 4 && (
        <div>
          <div className="mb-2.5 flex flex-wrap items-baseline gap-x-3 gap-y-1 text-[12px]">
            <span className="text-dim">加速域名</span>
            <span className="min-w-0 break-all font-mono text-fg" title={cfg?.cdn_domain || undefined}>
              {cfg?.cdn_domain || '—'}
            </span>
          </div>

          {!showToggle && (
            <div className="flex flex-wrap items-center gap-2.5">
              <Button disabled={busy !== null} onClick={() => void runProvision()}>
                {busy === 'provision' ? '接入中…' : '开始接入'}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy !== null}
                onClick={() => void forceWs()}
              >
                {busy === 'ws' ? '开启中…' : '开启/重开 WebSocket'}
              </Button>
              <span className="text-[11.5px] text-dim">
                幂等：重复点不会产生重复的域名或解析
              </span>
            </div>
          )}

          {!showToggle && (
            <div className="mt-2 text-[11.5px] leading-[1.6] text-warn-fg">
              说明：WebSocket 是<strong>站点级</strong>设置。若该站点当前没开，接入时会写一次，
              腾讯云会把该站点下<strong>所有</strong>加速域名重新部署一遍；已开启则跳过、不产生任何变更。
            </div>
          )}

          {wsNote && (
            <div
              className={cx(
                'mt-2 rounded-lg border px-3 py-2 text-[12px] leading-[1.65]',
                wsNote.ok ? 'border-ok-fg/40 bg-ok-dim text-ok-fg' : 'border-warn-fg/40 bg-warn-dim text-warn-fg',
              )}
            >
              <span className="break-all" title={wsNote.text}>
                {wsNote.text}
              </span>
            </div>
          )}

          {provErr && <ErrBlock title="接入请求失败（下面是后端原文）" err={provErr} />}

          {prov && (
            <div className="mt-2.5">
              <div
                className={cx(
                  'mb-2 flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[12.5px]',
                  prov.ok ? 'text-ok-fg' : 'text-bad-fg',
                )}
              >
                <b>{prov.ok ? '✓ 接入完成' : '✗ 接入没有走完'}</b>
                {prov.connect_addr && (
                  <span
                    className="min-w-0 break-all font-mono text-[11.5px] text-muted"
                    title={prov.connect_addr}
                  >
                    客户端应连接 {prov.connect_addr}
                  </span>
                )}
              </div>
              {provisionSteps.length > 0 && (
                <div className="grid gap-1.5">
                  {provisionSteps.map((s, i) => (
                    <div
                      key={`${s.step}-${i}`}
                      className={cx(
                        'rounded-lg border px-3 py-2 text-[12px] leading-[1.65]',
                        s.ok
                          ? 'border-ok-fg/30 bg-ok-dim text-muted'
                          : 'border-bad-fg/40 bg-bad-dim text-bad-fg',
                      )}
                    >
                      <div className="flex items-start gap-2">
                        <span
                          className={cx(
                            'shrink-0 font-mono font-bold',
                            s.ok ? 'text-ok-fg' : 'text-bad-fg',
                          )}
                        >
                          {s.ok ? '✓' : '✗'}
                        </span>
                        <b className="min-w-0 break-all text-fg">{s.step}</b>
                      </div>
                      {s.detail && (
                        <div
                          className={cx(
                            'mt-0.5 pl-[18px] break-all',
                            s.ok ? 'text-dim' : 'text-bad-fg',
                          )}
                          title={s.detail}
                        >
                          {s.detail}
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              )}
              {!prov.ok && (
                <Hint>
                  修好失败那一步后可以直接重试：已建好的域名与配置会被复用。
                </Hint>
              )}
            </div>
          )}

          {/* 成功后才出现的开关：打开它就是真的让所有客户端切 wss。 */}
          {showToggle && (
            <div className="mt-3 rounded-lg border border-border2 bg-chip px-3.5 py-3">
              <label className="flex cursor-pointer items-start gap-2.5">
                <input
                  type="checkbox"
                  className="mt-0.5 h-4 w-4 shrink-0 accent-[var(--accent)] disabled:cursor-not-allowed"
                  checked={cfg?.cdn_enabled === '1'}
                  disabled={busy === 'toggle'}
                  onChange={(e) => void toggleCdn(e.target.checked)}
                />
                <span className="min-w-0">
                  <b className="block text-[13px]">启用 CDN 加速</b>
                  <span className="mt-0.5 block text-[11.5px] leading-[1.6] text-dim">
                    打开后客户端最多 15 秒切到 wss，切换期间隧道会短暂中断。
                    关掉则退回直连 frps 的 TCP。
                  </span>
                </span>
              </label>
              {toggleErr && (
                <div className="mt-2 break-all text-[11.5px] text-bad-fg" title={toggleErr}>
                  {toggleErr}
                </div>
              )}

              {poll.running && (
                <div className="mt-2 text-[11.5px] leading-[1.6] text-muted">
                  正在等客户端确认切换（每 2 秒查一次，最多 60 秒）…
                </div>
              )}
              {poll.verdict === 'ok' && (
                <div className="mt-2 text-[12px] font-[650] text-ok-fg">✓ 客户端已切到 wss</div>
              )}
              {poll.verdict === 'timeout' && (
                <div className="mt-2 rounded-lg border border-warn-fg/40 bg-warn-dim px-3 py-2 text-[11.5px] leading-[1.7] text-warn-fg">
                  <div className="font-[650]">已下发，客户端未确认</div>
                  {poll.err && (
                    <div className="mt-1 break-all" title={poll.err}>
                      {poll.err}
                    </div>
                  )}
                  {poll.applied.length === 0 ? (
                    <div className="mt-1">
                      还没有客户端上报传输配置（旧版本 agent，或还没到下一个心跳）。
                    </div>
                  ) : (
                    <div className="mt-1 grid gap-1">
                      {poll.applied.map((a, i) => (
                        <div
                          key={`${a.agent_id || a.name}-${i}`}
                          className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
                        >
                          <span className="break-all font-[650] text-fg">
                            {a.name || a.agent_id || '未命名客户端'}
                          </span>
                          <span className="break-all font-mono">
                            {a.match ? '✓ ' : '✗ '}
                            {a.transport || '未上报'}
                          </span>
                          {a.note && (
                            <span className="break-all opacity-90" title={a.note}>
                              {a.note}
                            </span>
                          )}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )}
            </div>
          )}
        </div>
      )}
      {audit && audit.length > 0 && (
        <div className="mt-3 border-t border-border2 pt-3">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <b className="text-[12.5px] text-fg">最近调用记录</b>
            <span className="text-[11.5px] text-dim">
              面板对腾讯云发起的每一次请求；标记「写」的会改变云上资源
            </span>
            <span className="flex-1" />
            <Button variant="ghost" size="sm" onClick={() => void loadAudit()}>
              刷新
            </Button>
          </div>
          <div className="grid gap-1">
            {audit.slice().reverse().map((e, i) => (
              <div
                key={`${e.at}-${i}`}
                className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 rounded-[8px] bg-chip px-2 py-1.5 text-[11.5px]"
              >
                <span className="shrink-0 font-mono text-dim">
                  {new Date(e.at).toLocaleTimeString('zh-CN', { hour12: false })}
                </span>
                <span className={cx('shrink-0 font-mono', e.ok ? 'text-fg' : 'text-bad-fg')}>
                  {e.action}
                </span>
                {e.write && <Badge tone="warn">写</Badge>}
                <span className={cx('shrink-0', e.ok ? 'text-ok-fg' : 'text-bad-fg')}>
                  {e.ok ? '✓' : '✗'}
                </span>
                <span className="shrink-0 text-dim">{e.ms}ms</span>
                {e.detail && (
                  <span className="min-w-0 flex-1 truncate text-bad-fg" title={e.detail}>
                    {e.detail}
                  </span>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
    </Card>
  )
}
