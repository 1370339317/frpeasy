import { useCallback, useEffect, useRef, useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { fmtAgo } from '../lib/format'
import { Badge, Button, Card, Table, Td, cx, type Tone } from '../components/ui'
import type {
  LinkQuality,
  LinkQualityAdvice,
  LinkQualityApplyResult,
  LinkQualityResponse,
  LinkQualityVerdict,
} from '../types'

/**
 * 链路体检（内核 TCP 指标）。
 *
 * 与上面那张「三段测速」卡的分工：
 *   卡片一靠**打流量**量出结果，回答"现在有多快"；
 *   这张卡靠 `ss -tin` 读**内核自己记的账**（rtt / 重传 / cwnd），
 *   零流量、零干扰，回答"为什么慢"——丢包和抖动只有内核知道。
 *
 * 应用建议后不自己宣布成功：面板只负责把设置写进库，真正的"生效"由客户端
 * 下一次心跳回报它写进 frpc.toml 的值。所以这里轮询 /api/linkquality，
 * 直到 applied[].match 全为 true 才显示「已生效」；等不到就如实说"未确认"。
 */

const CONFIRM_TIMEOUT_MS = 30000
const POLL_INTERVAL_MS = 2000

type ConfirmPhase = 'idle' | 'pending' | 'confirmed' | 'timeout'
type MetricTone = 'ok' | 'warn' | 'bad' | 'fg'

const VERDICT: Record<LinkQualityVerdict, { label: string; tone: Tone }> = {
  healthy: { label: '正常', tone: 'ok' },
  jittery: { label: '抖动', tone: 'warn' },
  lossy: { label: '丢包', tone: 'bad' },
  idle: { label: '无连接', tone: 'gray' },
}

const SEVERITY: Record<string, { label: string; tone: Tone; box: string }> = {
  critical: { label: '严重', tone: 'bad', box: 'border-bad-fg/40 bg-bad-dim/30' },
  warn: { label: '警告', tone: 'warn', box: 'border-warn-fg/35 bg-warn-dim/25' },
  info: { label: '提示', tone: 'gray', box: 'border-border2 bg-chip' },
}

const METRIC_TEXT: Record<MetricTone, string> = {
  ok: 'text-ok-fg',
  warn: 'text-warn-fg',
  bad: 'text-bad-fg',
  fg: 'text-fg',
}

/** RTT 统一 0 位小数 + ms；重传率统一 2 位 + %。 */
const fmtMs = (v: number) => `${Math.round(v)} ms`
const fmtPct = (v: number) => `${v.toFixed(2)}%`

function retransTone(pct: number): MetricTone {
  if (pct >= 2) return 'bad'
  if (pct >= 0.5) return 'warn'
  return 'ok'
}

/** 抖动着色看 p95/p50 的倍数：跨境链路 p50 160ms 是常态，绝对值意义不大。 */
function rttTone(p95: number, p50: number): MetricTone {
  if (p50 <= 0 || p95 <= 0) return 'fg'
  const ratio = p95 / p50
  if (ratio >= 3) return 'bad'
  if (ratio >= 1.6) return 'warn'
  return 'ok'
}

/** 空 patch（后端 omitempty 时为 undefined）不可一键应用。 */
function hasPatch(a: LinkQualityAdvice): boolean {
  return !!a.patch && Object.keys(a.patch).length > 0
}

function patchText(patch: Record<string, string>): string {
  return Object.entries(patch)
    .map(([k, v]) => `${k}=${v}`)
    .join(' · ')
}

export function LinkQualityCard() {
  const { toast } = useApp()
  const [data, setData] = useState<LinkQualityResponse | null>(null)
  const [loading, setLoading] = useState(false)
  const [err, setErr] = useState('')
  const [applying, setApplying] = useState<string | null>(null)
  const [phase, setPhase] = useState<ConfirmPhase>('idle')
  const [desired, setDesired] = useState('')
  const [warnings, setWarnings] = useState<string[]>([])

  // 确认阶段只用 setTimeout 递归，不引入全局 setInterval（那是 store 的职责）。
  // runId 用来作废上一轮：重新应用、手动刷新、组件卸载都会让它 +1，
  // 迟到的轮询结果直接丢弃，不会把状态写回已经过期的界面上。
  const timerRef = useRef<number | null>(null)
  const runIdRef = useRef(0)

  const stopPolling = useCallback(() => {
    runIdRef.current++
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current)
      timerRef.current = null
    }
  }, [])

  useEffect(() => {
    return () => stopPolling()
  }, [stopPolling])

  const load = useCallback(async () => {
    setLoading(true)
    setErr('')
    try {
      const r = await api<LinkQualityResponse>('GET', '/api/linkquality')
      setData(r)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [])

  // 进页面先采一次：GET 本身就是一次采样，不必让用户先点一下才看到数字。
  useEffect(() => {
    void load()
  }, [load])

  const beginConfirm = useCallback(
    (want: string) => {
      stopPolling()
      const run = runIdRef.current
      const startedAt = Date.now()
      setPhase('pending')
      setDesired(want)

      const tick = async () => {
        timerRef.current = null
        if (runIdRef.current !== run) return
        try {
          const r = await api<LinkQualityResponse>('GET', '/api/linkquality')
          if (runIdRef.current !== run) return
          setData(r)
          const list = r.applied ?? []
          // 全部客户端都已上报且与期望一致才算"已生效"。
          // 留一台未上报就显示"待确认"：离线/旧版本 agent 确实可能没吃着配置。
          if (list.length > 0 && list.every((a) => a.transport !== '' && a.match)) {
            setPhase('confirmed')
            return
          }
        } catch {
          // 单次轮询失败不终止：可能是面板重启中，下一次再试，总时限兜底。
        }
        if (runIdRef.current !== run) return
        if (Date.now() - startedAt >= CONFIRM_TIMEOUT_MS) {
          setPhase('timeout')
          return
        }
        timerRef.current = window.setTimeout(() => void tick(), POLL_INTERVAL_MS)
      }

      timerRef.current = window.setTimeout(() => void tick(), POLL_INTERVAL_MS)
    },
    [stopPolling],
  )

  const applyAdvice = async (a: LinkQualityAdvice) => {
    if (!hasPatch(a) || applying) return
    setApplying(a.id)
    setErr('')
    setWarnings([])
    try {
      const r = await api<LinkQualityApplyResult>('POST', '/api/linkquality/apply', {
        patch: a.patch,
      })
      setWarnings(r.warnings ?? [])
      // 后端返回的 desired 是写库后的真实期望值，比本地拼的字符串可靠。
      setData((prev) => (prev ? { ...prev, desired: r.desired } : prev))
      beginConfirm(r.desired)
      toast('已下发，等待客户端确认', 'ok')
    } catch (e) {
      const msg = (e as Error).message
      setErr(msg)
      toast(msg, 'bad')
    } finally {
      setApplying(null)
    }
  }

  const q: LinkQuality | null = data?.quality ?? null
  const advice = data?.advice ?? []
  const applied = data?.applied ?? []
  const desiredShown = desired || data?.desired || ''
  const conns = q?.conns ?? []
  const verdict = q ? (VERDICT[q.verdict] ?? { label: q.verdict || '未知', tone: 'gray' }) : null
  const idle = !q || !q.supported || conns.length === 0

  return (
    <Card
      title="链路体检（内核 TCP 指标）"
      desc="直接读内核的 rtt / 重传 / 拥塞窗口，零流量、不用压测"
      actions={
        <div className="flex flex-wrap items-center justify-end gap-2.5">
          <span className="text-[11.5px] text-dim">
            上次采样 {q ? fmtAgo(q.sampled_at) : '—'}
          </span>
          <Button variant="ghost" size="sm" disabled={loading} onClick={() => void load()}>
            {loading ? '采样中…' : '立即体检'}
          </Button>
        </div>
      }
    >
      {err && (
        <div className={cx('text-[12px] leading-[1.65] text-bad-fg', data && 'mb-2.5')}>
          {err}
        </div>
      )}

      {!data && loading && <div className="text-[12px] text-dim">正在读取内核指标…</div>}

      {q && !q.supported && (
        <div className="rounded-lg border border-border2 bg-chip px-3.5 py-2.5 text-[12px] leading-[1.7] text-muted">
          <b className="text-fg">拿不到内核 TCP 指标</b>：{q.reason || '当前系统不支持'}。
          仅 Linux 面板支持该采样，不影响隧道本身。
        </div>
      )}

      {q && q.supported && (
        <div className="grid gap-2.5 [grid-template-columns:repeat(auto-fit,minmax(150px,1fr))]">
          <Metric
            label="重传率"
            value={idle ? '—' : fmtPct(q.retrans_pct)}
            tone={idle ? 'fg' : retransTone(q.retrans_pct)}
          />
          <Metric
            label="RTT p50"
            value={idle ? '—' : fmtMs(q.rtt_p50)}
            tone={idle ? 'fg' : rttTone(q.rtt_p95, q.rtt_p50)}
          />
          <Metric
            label="RTT p95"
            value={idle ? '—' : fmtMs(q.rtt_p95)}
            tone={idle ? 'fg' : rttTone(q.rtt_p95, q.rtt_p50)}
          />
          <Metric
            label="最小 RTT"
            value={idle || q.min_rtt <= 0 ? '—' : fmtMs(q.min_rtt)}
            tone="fg"
          />
          <Metric label="连接数" value={idle ? '0' : String(conns.length)} tone="fg" />
          <div className="rounded-lg border border-border2 bg-chip px-3 py-2">
            <div className="text-[11px] text-dim">判定</div>
            <div className="mt-1.5">
              <Badge tone={verdict?.tone ?? 'gray'}>{verdict?.label ?? '未知'}</Badge>
            </div>
          </div>
        </div>
      )}

      {q && q.supported && idle && (
        <div className="mt-2 text-[11.5px] leading-[1.6] text-dim">
          采样窗口里没有活跃连接（客户端没连上来，或 frps 端口还没流量）。
        </div>
      )}

      {(phase !== 'idle' || warnings.length > 0) && (
        <div
          className={cx(
            'mt-3 rounded-lg border px-3.5 py-2.5 text-[12px] leading-[1.7]',
            phase === 'pending'
              ? 'border-accent bg-accent-dim text-accent-fg'
              : phase === 'confirmed'
                ? 'border-ok-fg/35 bg-ok-dim text-ok-fg'
                : phase === 'timeout'
                  ? 'border-warn-fg/40 bg-warn-dim text-warn-fg'
                  : 'border-border2 bg-chip text-muted',
          )}
        >
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <b>
              {phase === 'pending'
                ? '下发中：等待客户端确认…'
                : phase === 'confirmed'
                  ? '已生效'
                  : phase === 'timeout'
                    ? '已下发，但客户端未确认'
                    : '最近一次下发'}
            </b>
            <span className="font-mono text-[11.5px] break-all">期望 {desiredShown || '—'}</span>
            {phase === 'pending' && (
              <span className="text-[11px] opacity-80">每 2 秒刷新，最多等 30 秒</span>
            )}
          </div>
          {phase === 'timeout' && (
            <div className="mt-1">
              检查客户端是否在线：离线、旧版本 agent 不上报、或 agent
              写配置/重启 frpc 失败，都会停在这一步。
            </div>
          )}
          {warnings.length > 0 && (
            <ul className="mt-1.5 grid gap-1 text-bad-fg">
              {warnings.map((w, i) => (
                <li key={i} className="flex gap-1.5">
                  <span className="mt-[7px] inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-bad-fg" />
                  <span className="min-w-0 break-all">{w}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      <div className="mt-4">
        <div className="mb-1.5 flex flex-wrap items-baseline gap-2">
          <b className="text-[12.5px]">建议</b>
          <span className="text-[11px] text-dim">
            {advice.length > 0 ? `共 ${advice.length} 条` : '没有需要处理的项目'}
          </span>
        </div>
        {advice.length > 0 && (
          <div className="grid gap-2">
            {advice.map((a) => {
              const sev = SEVERITY[a.severity] ?? SEVERITY.info
              const evidence = a.evidence ?? []
              return (
                <div key={a.id} className={cx('rounded-lg border px-3.5 py-2.5', sev.box)}>
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge tone={sev.tone}>{sev.label}</Badge>
                    <b className="min-w-0 text-[12.5px] break-all">{a.title}</b>
                  </div>
                  {a.detail && (
                    <div className="mt-1 text-[12px] leading-[1.7] text-muted break-all">
                      {a.detail}
                    </div>
                  )}
                  {evidence.length > 0 && (
                    <ul className="mt-1.5 grid gap-0.5 text-[11.5px] leading-[1.6] text-dim">
                      {evidence.map((e, i) => (
                        <li key={i} className="flex gap-1.5">
                          <span className="mt-[7px] inline-block h-1 w-1 shrink-0 rounded-full bg-dim" />
                          <span className="min-w-0 break-all">{e}</span>
                        </li>
                      ))}
                    </ul>
                  )}
                  {hasPatch(a) && (
                    <div className="mt-2 flex flex-wrap items-center gap-2">
                      <span className="min-w-0 font-mono text-[11px] text-dim break-all">
                        {patchText(a.patch!)}
                      </span>
                      <div className="flex-1" />
                      <Button
                        size="sm"
                        disabled={applying !== null || phase === 'pending'}
                        onClick={() => void applyAdvice(a)}
                      >
                        {applying === a.id ? '应用中…' : '应用此建议'}
                      </Button>
                    </div>
                  )}
                </div>
              )
            })}
          </div>
        )}
      </div>

      <div className="mt-4">
        <div className="mb-1.5 flex flex-wrap items-baseline gap-2">
          <b className="text-[12.5px]">客户端生效情况</b>
          <span className="min-w-0 font-mono text-[11px] text-dim break-all">
            期望 {desiredShown || '—'}
          </span>
        </div>
        {applied.length === 0 ? (
          <div className="text-[12px] text-dim">还没有客户端接入。</div>
        ) : (
          <Table head={['客户端', '期望', '实际', '状态']}>
            {applied.map((a) => {
              const status = a.match
                ? { label: '已生效', tone: 'ok' as Tone }
                : a.transport === ''
                  ? { label: '未上报', tone: 'gray' as Tone }
                  : { label: '待确认', tone: 'warn' as Tone }
              return (
                <tr key={a.agent_id}>
                  <Td className="max-w-[180px] break-all">{a.name || a.agent_id}</Td>
                  <Td className="font-mono text-[11.5px] break-all">{desiredShown || '—'}</Td>
                  <Td className="font-mono text-[11.5px] break-all">{a.transport || '未上报'}</Td>
                  <Td>
                    <span title={a.note || undefined} className="inline-flex">
                      <Badge tone={status.tone}>{status.label}</Badge>
                    </span>
                  </Td>
                </tr>
              )
            })}
          </Table>
        )}
      </div>
    </Card>
  )
}

function Metric({ label, value, tone }: { label: string; value: string; tone: MetricTone }) {
  return (
    <div className="rounded-lg border border-border2 bg-chip px-3 py-2">
      <div className="text-[11px] text-dim">{label}</div>
      <div className={cx('mt-1 font-mono text-[15px] font-[650] break-all', METRIC_TEXT[tone])}>
        {value}
      </div>
    </div>
  )
}
