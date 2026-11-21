import { useEffect, useRef, useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { copyWithHint } from '../lib/format'
import { Alert, Button, Field, Hint, Input, Modal, Section } from '../components/ui'
import type { CdnConfig, CdnExposeResponse, CdnExposeResult, CdnVerifyResult, Tunnel } from '../types'

/**
 * 把一条 tcp 隧道背后的本地服务「一键暴露成域名并接入 CDN」。
 *
 * 后端做的是**另建一条 http 隧道**（原 tcp 隧道原样保留），所以这个弹窗
 * 有两件事必须说清楚：
 *   1. 拼域名用的是「已选站点的 ZoneName」，不是 cdn_domain —— 后者是 CDN
 *      自己那条加速域名（如 ecsfrp.dvvb.cn），拼上去会得到一个不存在的子域；
 *   2. 成功后是两条入口并存，旧端口仍然可用，确认新入口没问题再删旧的。
 *
 * 站点主域名不缓存在本地：它由「CDN 卡片 → ② 选站点」写进配置，换了站点
 * 之后必须重新问云端要，否则会拿旧站点的域名去建加速域名。
 */

type ZoneState = 'loading' | 'ready' | 'missing' | 'error'

interface Props {
  open: boolean
  /** 要暴露的那条 tcp 隧道；open=false 时可能是 null */
  tunnel: Tunnel | null
  onClose: () => void
}

/** 前缀只允许 DNS label 的字符集：小写字母、数字、内部连字符。 */
const PREFIX_RE = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

export function CdnExposeModal({ open, tunnel, onClose }: Props) {
  const { toast } = useApp()

  // 打开弹窗那一刻的隧道：effect 只依赖 [open]，不跟着全局轮询重跑，
  // 所以它不能在 effect 里直接读 props（那会让闭包永远是首次渲染的值）。
  const seedRef = useRef(tunnel)
  seedRef.current = tunnel

  const [prefix, setPrefix] = useState('')
  const [zoneName, setZoneName] = useState('')
  const [zoneState, setZoneState] = useState<ZoneState>('loading')
  const [zoneErr, setZoneErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<CdnExposeResult | null>(null)
  const [ok, setOk] = useState(false)
  const [submitErr, setSubmitErr] = useState('')

  // 复制失败时要把这段文本选中让用户长按（移动端唯一的兜底路径）。
  const urlRef = useRef<HTMLDivElement | null>(null)

  // 多次「重新读取 / 重新提交」可能并发，只有最后一次的结果能落地。
  const zoneGen = useRef(0)

  const label = prefix.trim()
  const labelValid = PREFIX_RE.test(label)
  const fullDomain = labelValid && zoneName !== '' ? `${label}.${zoneName}` : ''

  /**
   * 读「已选站点」的主域名。
   *
   * 必须先 GET config 拿 zone_id，再 POST verify 拿 zones —— verify 只是在
   * 云端列一遍站点，面板并没有把 ZoneName 落库。找不到就说明用户还没在
   * CDN 卡片里选站点（或换密钥后站点列表变了），这时不能自己猜域名。
   */
  async function loadZone() {
    const gen = ++zoneGen.current
    setZoneState('loading')
    setZoneErr('')
    setZoneName('')
    try {
      const cfg = await api<CdnConfig>('GET', '/api/cdn/config')
      if (zoneGen.current !== gen) return
      const zoneID = (cfg.cdn_tencent_zone_id ?? '').trim()
      if (zoneID === '') {
        setZoneState('missing')
        setZoneErr('还没有在 CDN 卡片里选择站点')
        return
      }
      const v = await api<CdnVerifyResult>('POST', '/api/cdn/verify')
      if (zoneGen.current !== gen) return
      const hit = (v.zones ?? []).find((z) => z.ZoneId === zoneID)
      if (!hit || !hit.ZoneName) {
        setZoneState('missing')
        setZoneErr('已选站点不在当前密钥可见的列表里（可能换了账号或密钥）')
        return
      }
      setZoneName(hit.ZoneName)
      setZoneState('ready')
    } catch (e) {
      if (zoneGen.current !== gen) return
      setZoneState('error')
      setZoneErr(errText(e))
    }
  }

  // 每次打开都从干净状态开始：上一次的结果、前缀与错误不能留在这一次里。
  // 依赖只留 [open] —— onClose / tunnel 都是内联或轮询来的新引用，进依赖
  // 会让这个 effect 在用户打字时反复重跑（输入被清空、结果被抹掉）。
  useEffect(() => {
    if (!open) {
      // 关掉时让在飞的读取失效，避免它回来把下一次打开的状态改掉。
      zoneGen.current++
      return
    }
    setPrefix('')
    setBusy(false)
    setResult(null)
    setOk(false)
    setSubmitErr('')
    void loadZone()
    return () => {
      zoneGen.current++
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  async function submit() {
    if (busy) return
    const t = seedRef.current
    if (!t) return
    if (!labelValid) {
      toast('前缀只能用小写字母、数字和连字符，且不能以连字符开头或结尾', 'bad')
      return
    }
    if (zoneName === '') {
      toast('还没拿到站点主域名，先去 CDN 卡片选站点', 'bad')
      return
    }
    setBusy(true)
    setSubmitErr('')
    try {
      const r = await api<CdnExposeResponse>('POST', '/api/cdn/expose', {
        tunnel_id: t.id,
        prefix: label,
      })
      // 业务失败也是 HTTP 200：只有 steps[].ok 才是事实。
      setResult(r.result ?? null)
      setOk(r.ok)
      if (r.ok) toast('已接入 CDN')
      else toast('接入未完全成功，看下面的步骤', 'bad')
    } catch (e) {
      // 4xx 是前置条件不满足（没选站点、前缀非法…），原文就直接给用户看。
      const msg = errText(e)
      setSubmitErr(msg)
      toast(msg, 'bad')
    } finally {
      setBusy(false)
    }
  }

  async function copyURL() {
    if (!result?.url) return
    const r = await copyWithHint(result.url, urlRef.current, '已复制域名入口')
    toast(r.msg, r.kind)
  }

  const steps = result?.steps ?? []
  const warnings = result?.warnings ?? []
  const fwPort = result?.need_firewall_port ?? 0

  return (
    <Modal
      open={open}
      title={`接入 CDN · ${tunnel?.name ?? '隧道'}`}
      onClose={onClose}
      footer={
        result ? (
          <>
            {!ok && (
              <Button variant="ghost" onClick={() => void submit()} disabled={busy}>
                {busy ? '重试中…' : '重试'}
              </Button>
            )}
            {!ok && (
              <Button
                variant="ghost"
                onClick={() => {
                  setResult(null)
                  setSubmitErr('')
                }}
              >
                改前缀
              </Button>
            )}
            <Button onClick={onClose}>关闭</Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={busy}>
              取消
            </Button>
            <Button
              onClick={() => void submit()}
              disabled={busy || zoneState !== 'ready' || !labelValid}
            >
              {busy ? '接入中…' : '开始接入'}
            </Button>
          </>
        )
      }
    >
      {tunnel && (
        <div className="mb-3.5 text-xs leading-[1.5] text-dim">
          把 <span className="font-mono text-fg">{tunnel.local_addr}</span>（tcp 隧道
          「{tunnel.name}」）再暴露成一个 https 域名。面板会新建一条 http
          隧道并完成加速域名、DNS 与证书，原 tcp 隧道不动。
        </div>
      )}

      {submitErr !== '' && (
        <Alert tone="bad">
          <span className="break-all" title={submitErr}>
            {submitErr}
          </span>
        </Alert>
      )}

      {result === null ? (
        <>
          <Section
            no={1}
            title="站点主域名"
            desc="域名会建在 CDN 卡片里已选的站点下，前缀拼在它的主域名前面。"
          >
            {zoneState === 'loading' && (
              <div className="text-[12.5px] text-dim">正在读取已选站点…</div>
            )}
            {zoneState === 'ready' && (
              <div className="text-[13px]">
                已选站点 <b className="break-all">{zoneName}</b>
                <Hint>
                  新域名会是 <span className="font-mono">前缀.{zoneName}</span>
                </Hint>
              </div>
            )}
            {(zoneState === 'missing' || zoneState === 'error') && (
              <Alert tone="warn">
                <div className="break-all" title={zoneErr}>
                  拿不到站点主域名：{zoneErr}
                </div>
                <div className="mt-1">
                  请先到「隧道页底部 CDN 卡片 → 步骤② 选站点」，选好后再回来。
                </div>
                <Button
                  variant="ghost"
                  size="sm"
                  className="mt-2"
                  onClick={() => void loadZone()}
                >
                  重新读取
                </Button>
              </Alert>
            )}
          </Section>

          <Section no={2} title="子域名前缀" desc="例如 wb，最终域名就是 wb.站点主域名。">
            <Field className="mb-0" label="前缀">
              <Input
                value={prefix}
                maxLength={32}
                disabled={zoneState !== 'ready' || busy}
                placeholder="wb"
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => {
                  // 直接按后端规则收窄：大写转小写，非法字符不进输入框。
                  // 与其让用户提交后吃一条云端报错，不如当场就只允许合法字符。
                  setPrefix(e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, ''))
                  // 上一次提交的 4xx 原文不再适用于新前缀，别让它留在上面误导。
                  setSubmitErr('')
                }}
              />
            </Field>
            <Hint>
              {prefix !== '' && !labelValid
                ? '前缀只能用小写字母、数字和连字符，且不能以连字符开头或结尾。'
                : '前缀只能用小写字母、数字和连字符。'}
            </Hint>
            <div className="mt-2 rounded-[10px] border border-border bg-pre px-3 py-2 text-[13.5px]">
              <span className="text-dim">https://</span>
              <span
                className="break-all font-mono font-[650] text-accent-fg"
                title={fullDomain}
              >
                {fullDomain || `<前缀>.${zoneName || '站点主域名'}`}
              </span>
            </div>
          </Section>
        </>
      ) : (
        <>
          <Section
            no={1}
            title="执行步骤"
            desc={ok ? '每一步都成功了。' : '有步骤失败，下面标 ✗ 的就是卡住的地方。'}
          >
            {steps.length === 0 ? (
              <div className="text-[12.5px] text-dim">后端没有返回步骤明细。</div>
            ) : (
              <div className="grid gap-1.5">
                {steps.map((s, i) => (
                  <div key={`${s.step}-${i}`} className="flex items-start gap-2 text-[12.5px]">
                    <span className={s.ok ? 'text-ok-fg' : 'text-bad-fg'}>
                      {s.ok ? '✓' : '✗'}
                    </span>
                    <span className="min-w-0">
                      <b className="font-[650]">{s.step}</b>
                      {s.detail !== '' && (
                        <span
                          className="ml-1.5 break-all text-muted"
                          title={s.detail}
                        >
                          {s.detail}
                        </span>
                      )}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </Section>

          {result.url !== undefined && result.url !== '' && (
            <>
              <div className="mb-3.5 rounded-[11px] border border-border bg-card px-4 py-3.5">
                <div className="text-xs text-dim">新入口（可复制）</div>
                <div className="mt-1.5 flex items-start gap-2.5">
                  <div
                    ref={urlRef}
                    className="min-w-0 flex-1 break-all font-mono text-[16px] font-[650] text-accent-fg"
                    title={result.url}
                  >
                    {result.url}
                  </div>
                  <Button variant="ghost" size="sm" className="shrink-0" onClick={() => void copyURL()}>
                    复制
                  </Button>
                </div>
                {result.cname !== undefined && result.cname !== '' && (
                  <div className="mt-1 break-all text-[11.5px] text-dim" title={result.cname}>
                    CNAME {result.cname}
                  </div>
                )}
                {result.tunnel_id !== undefined && result.tunnel_id > 0 && (
                  <div className="mt-1 text-[11.5px] text-dim">
                    http 隧道 #{result.tunnel_id}
                  </div>
                )}
              </div>
              {ok && (
                <Alert tone="info">
                  原 tcp 隧道仍然可用，两条入口并存；确认新域名没问题后再删除旧的。
                </Alert>
              )}
            </>
          )}

          {(warnings.length > 0 || fwPort > 0) && (
            <div className="mb-3.5 rounded-[10px] border border-alert-bad-bd bg-bad-dim px-3.5 py-2.5 text-[12.5px] text-bad-fg">
              {warnings.map((w, i) => (
                <div key={i} className="break-all" title={w}>
                  {w}
                </div>
              ))}
              {fwPort > 0 && (
                <div className="mt-1">
                  安全组需放行 TCP {fwPort} 入方向，否则 CDN 回源打不进来。
                </div>
              )}
            </div>
          )}
        </>
      )}
    </Modal>
  )
}
