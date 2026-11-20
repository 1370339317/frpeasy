import { useCallback, useEffect, useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { Badge, Button, Card, Field, Hint, Input, Select, Table, Td, cx } from '../components/ui'
import type {
  CdnConfig,
  CdnConfigKey,
  CdnConfigPatch,
  CdnProvisionResult,
  CdnVerifyResult,
  CdnZone,
} from '../types'

/**
 * CDN 加速接入（腾讯云 EdgeOne）。
 *
 * 解决的问题：frpc → frps 这段跨境裸 TCP 一旦丢包，吞吐会塌到 KB/s 量级；
 * 把这段换成 CDN 边缘回源之后，客户端只需要一条到就近节点的 wss 长连接。
 * 代价是加速层只认 HTTP / WebSocket，所以客户端必须从 TCP 改成 wss
 * —— 这也是为什么「启用 CDN」不是纯服务器侧开关，它会改变所有客户端的连接方式。
 *
 * 三个按钮对应三种完全不同的语义，界面上必须区分清楚：
 *   保存配置  — 只写库，不发任何云厂商请求；
 *   验证密钥  — 只读调用（列站点），验证的是**已保存**的密钥；
 *   一键接入  — 建加速域名 / 开 WebSocket / 加 CNAME，幂等但会真的动云端资源。
 * 所以前两个按钮旁必须提示「先保存再验证」，否则用户改完密钥直接点验证，
 * 验的是旧密钥，会得到一条莫名其妙的报错。
 */

type Busy = 'save' | 'verify' | 'provision' | null

/** 四个密钥字段：掩码回传 = 未修改，绝不能原样 POST 回去。 */
type SecretKey =
  | 'cdn_tencent_secret_id'
  | 'cdn_tencent_secret_key'
  | 'cdn_aliyun_ak_id'
  | 'cdn_aliyun_ak_secret'

const SECRET_KEYS: SecretKey[] = [
  'cdn_tencent_secret_id',
  'cdn_tencent_secret_key',
  'cdn_aliyun_ak_id',
  'cdn_aliyun_ak_secret',
]

/** 表单是「全部 13 个可写键」的字符串映射（后端就是这么存的）。 */
type CdnForm = Record<CdnConfigKey, string>

const EMPTY_FORM: CdnForm = {
  cdn_enabled: '0',
  cdn_tencent_secret_id: '',
  cdn_tencent_secret_key: '',
  cdn_tencent_zone_id: '',
  cdn_tencent_region: 'ap-guangzhou',
  cdn_domain: '',
  cdn_connect_port: '443',
  cdn_origin_scheme: 'HTTP',
  cdn_ws_timeout: '300',
  cdn_aliyun_ak_id: '',
  cdn_aliyun_ak_secret: '',
  cdn_aliyun_root_domain: '',
  cdn_dns_rr: '',
}

/** 接口视图 → 表单值。回填与新填都走这一处，避免两条路径慢慢漂移。 */
function formOf(c: CdnConfig): CdnForm {
  return {
    cdn_enabled: c.cdn_enabled === '1' ? '1' : '0',
    cdn_tencent_secret_id: c.cdn_tencent_secret_id ?? '',
    cdn_tencent_secret_key: c.cdn_tencent_secret_key ?? '',
    cdn_tencent_zone_id: c.cdn_tencent_zone_id ?? '',
    cdn_tencent_region: c.cdn_tencent_region ?? '',
    cdn_domain: c.cdn_domain ?? '',
    cdn_connect_port: c.cdn_connect_port ?? '',
    cdn_origin_scheme: c.cdn_origin_scheme ?? 'HTTP',
    cdn_ws_timeout: c.cdn_ws_timeout ?? '',
    cdn_aliyun_ak_id: c.cdn_aliyun_ak_id ?? '',
    cdn_aliyun_ak_secret: c.cdn_aliyun_ak_secret ?? '',
    cdn_aliyun_root_domain: c.cdn_aliyun_root_domain ?? '',
    cdn_dns_rr: c.cdn_dns_rr ?? '',
  }
}

function secretOf(f: CdnForm): Record<SecretKey, string> {
  return {
    cdn_tencent_secret_id: f.cdn_tencent_secret_id,
    cdn_tencent_secret_key: f.cdn_tencent_secret_key,
    cdn_aliyun_ak_id: f.cdn_aliyun_ak_id,
    cdn_aliyun_ak_secret: f.cdn_aliyun_ak_secret,
  }
}

/**
 * 组装 POST 体。
 *
 * 非密钥字段一律提交（空串 = 清空，语义明确）；密钥字段只在「用户真的输入了
 * 新值」时提交 —— 留空是「不改」，而等于回填掩码则说明用户压根没动过，
 * 回传它会把库里的明文覆盖成掩码字符串本身。
 */
function buildPatch(form: CdnForm, masked: Record<SecretKey, string>): CdnConfigPatch {
  const patch: CdnConfigPatch = {
    cdn_enabled: form.cdn_enabled === '1' ? '1' : '0',
    cdn_tencent_zone_id: form.cdn_tencent_zone_id.trim(),
    cdn_tencent_region: form.cdn_tencent_region.trim() || 'ap-guangzhou',
    cdn_domain: form.cdn_domain.trim(),
    cdn_connect_port: form.cdn_connect_port.trim() || '443',
    cdn_origin_scheme: form.cdn_origin_scheme === 'HTTPS' ? 'HTTPS' : 'HTTP',
    cdn_ws_timeout: form.cdn_ws_timeout.trim() || '300',
    cdn_aliyun_root_domain: form.cdn_aliyun_root_domain.trim(),
    cdn_dns_rr: form.cdn_dns_rr.trim(),
  }
  for (const k of SECRET_KEYS) {
    const v = form[k].trim()
    if (v !== '' && v !== masked[k]) patch[k] = v
  }
  return patch
}

export function CdnCard() {
  const { toast } = useApp()
  const [form, setForm] = useState<CdnForm>(EMPTY_FORM)
  const [masked, setMasked] = useState<Record<SecretKey, string>>({
    cdn_tencent_secret_id: '',
    cdn_tencent_secret_key: '',
    cdn_aliyun_ak_id: '',
    cdn_aliyun_ak_secret: '',
  })
  // 当前生效值（desired_protocol / connect_addr）只看接口，不看表单：
  // 表单是「想改成的样子」，这两项是「客户端真正在用的样子」。
  const [cfg, setCfg] = useState<CdnConfig | null>(null)
  const [busy, setBusy] = useState<Busy>(null)
  const [loadErr, setLoadErr] = useState('')
  const [saveErr, setSaveErr] = useState('')
  const [verifyErr, setVerifyErr] = useState('')
  const [zones, setZones] = useState<CdnZone[] | null>(null)
  // 空列表时腾讯云给不出"为什么"，这两项是后端补的诊断信息。
  const [verifyMeta, setVerifyMeta] = useState<CdnVerifyResult | null>(null)
  const [provErr, setProvErr] = useState('')
  const [prov, setProv] = useState<CdnProvisionResult | null>(null)

  const applyConfig = useCallback((c: CdnConfig) => {
    const f = formOf(c)
    setCfg(c)
    setForm(f)
    setMasked(secretOf(f))
  }, [])

  const load = useCallback(async () => {
    try {
      const c = await api<CdnConfig>('GET', '/api/cdn/config')
      applyConfig(c)
      setLoadErr('')
    } catch (e) {
      setLoadErr((e as Error).message)
    }
  }, [applyConfig])

  useEffect(() => {
    void load()
  }, [load])

  const set = <K extends CdnConfigKey>(k: K, v: string) =>
    setForm((prev) => ({ ...prev, [k]: v }))

  const isWss = (cfg?.desired_protocol ?? '') === 'wss'
  const protocolLabel = !cfg?.desired_protocol
    ? '直连 TCP'
    : cfg.desired_protocol === 'wss'
      ? '经 CDN（wss）'
      : cfg.desired_protocol

  const save = async () => {
    if (busy) return
    setBusy('save')
    setSaveErr('')
    try {
      await api<CdnConfig>('POST', '/api/cdn/config', buildPatch(form, masked))
      // 保存后重新 GET 一次：库里可能对空值做了补默认（端口/区域/超时），
      // 只有回读才能保证界面上显示的就是「现在真正生效的配置」。
      await load()
      toast('CDN 配置已保存', 'ok')
    } catch (e) {
      const msg = (e as Error).message
      setSaveErr(msg)
      toast(msg, 'bad')
    } finally {
      setBusy(null)
    }
  }

  const verify = async () => {
    if (busy) return
    setBusy('verify')
    setVerifyErr('')
    setZones(null)
    try {
      const r = await api<CdnVerifyResult>('POST', '/api/cdn/verify')
      setZones(r.zones ?? [])
      setVerifyMeta(r)
    } catch (e) {
      // 后端把腾讯云原文（纯文本）透传出来，整段保留才能定位是签名、区域还是时钟问题。
      const msg = (e as Error).message
      setVerifyErr(msg)
    } finally {
      setBusy(null)
    }
  }

  const provision = async () => {
    if (busy) return
    setBusy('provision')
    setProvErr('')
    setProv(null)
    try {
      // 业务失败仍是 200：结果永远先落在 state 里，再按 ok 决定提示语气。
      const r = await api<CdnProvisionResult>('POST', '/api/cdn/provision')
      setProv(r)
      toast(r.ok ? '一键接入完成' : '接入没有走完，看下面的步骤', r.ok ? 'ok' : 'bad')
      try {
        await load()
      } catch {
        // 回读失败不影响上面的步骤结果，load 自己会记 loadErr
      }
    } catch (e) {
      const msg = (e as Error).message
      setProvErr(msg)
      toast(msg, 'bad')
    } finally {
      setBusy(null)
    }
  }

  /** 密钥框的 placeholder：已配置的不能提示「粘贴密钥」，而要提示留空不改。 */
  const secretPlaceholder = (k: SecretKey, fallback: string) =>
    masked[k] ? '已配置，留空不修改' : fallback

  const steps = prov?.steps ?? []

  return (
    <Card title="CDN 加速接入" desc="腾讯云 EdgeOne · 换掉 frpc → frps 这段跨境链路">
      <div className="mb-3.5 text-[12.5px] leading-[1.7] text-muted">
        用腾讯云 EdgeOne 把 frpc → frps 这段跨境链路换成 CDN
        加速；需要已备案域名，且加速层只认 HTTP / WebSocket，所以客户端会改用
        <b className="text-fg"> wss </b>连接。
      </div>

      {/* 常驻的生效状态：保存只是写库，真正改客户端行为的是这里的两个派生值。 */}
      <div className="mb-3.5 rounded-lg border border-border2 bg-chip px-3.5 py-2.5 text-[12px] leading-[1.7]">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5">
          <span className="text-dim">客户端传输</span>
          <Badge tone={isWss ? 'ok' : 'gray'}>{protocolLabel}</Badge>
          <span className="text-dim">连接地址</span>
          <span
            className="min-w-0 font-mono break-all text-fg"
            title={cfg?.connect_addr || undefined}
          >
            {cfg?.connect_addr || '—（用原 frps 地址）'}
          </span>
        </div>
        {isWss && (
          <div className="mt-1.5 text-[11.5px] leading-[1.6] text-warn-fg">
            客户端最多 15 秒内切换，切换期间隧道会短暂中断；wss 走 CDN
            边缘节点，切完请到「隧道」页测一次速确认。
          </div>
        )}
        {loadErr && (
          <div className="mt-1.5 text-[11.5px] leading-[1.6] text-bad-fg">
            读取配置失败：
            <span className="break-all" title={loadErr}>
              {loadErr}
            </span>
          </div>
        )}
      </div>

      {/* 启用开关 */}
      <label className="mb-3.5 flex cursor-pointer items-start gap-2.5">
        <input
          type="checkbox"
          className="mt-0.5 h-4 w-4 shrink-0 accent-[var(--accent)]"
          checked={form.cdn_enabled === '1'}
          onChange={(e) => set('cdn_enabled', e.target.checked ? '1' : '0')}
        />
        <span className="min-w-0">
          <b className="block text-[13.5px]">启用 CDN 加速</b>
          <span className="mt-0.5 block text-xs leading-[1.6] text-dim">
            开关打开且加速域名填好后才会真正切到 wss；
            只开开关不填域名时，客户端仍按原样直连 TCP —— 避免改个设置把隧道全断掉。
          </span>
        </span>
      </label>

      <div className="mb-2 text-[12.5px] font-[650] text-fg">腾讯云 EdgeOne</div>
      <div className="grid grid-cols-2 gap-x-3.5 max-[820px]:grid-cols-1">
        <Field label="SecretId">
          <Input
            type="password"
            autoComplete="off"
            value={form.cdn_tencent_secret_id}
            placeholder={secretPlaceholder('cdn_tencent_secret_id', 'AKID 开头')}
            onChange={(e) => set('cdn_tencent_secret_id', e.target.value)}
          />
        </Field>
        <Field label="SecretKey">
          <Input
            type="password"
            autoComplete="new-password"
            value={form.cdn_tencent_secret_key}
            placeholder={secretPlaceholder('cdn_tencent_secret_key', '腾讯云 API 密钥')}
            onChange={(e) => set('cdn_tencent_secret_key', e.target.value)}
          />
        </Field>
        <Field label="ZoneId" hint="「验证密钥」成功后从下面的站点列表里挑一个填进来">
          <Input
            value={form.cdn_tencent_zone_id}
            placeholder="zone-xxxx"
            onChange={(e) => set('cdn_tencent_zone_id', e.target.value)}
          />
        </Field>
        <Field label="Region">
          <Input
            value={form.cdn_tencent_region}
            placeholder="ap-guangzhou"
            onChange={(e) => set('cdn_tencent_region', e.target.value)}
          />
        </Field>
        <Field label="加速域名" hint="已备案、且准备用 CNAME 指到 EdgeOne 的域名">
          <Input
            value={form.cdn_domain}
            placeholder="frp.example.com"
            onChange={(e) => set('cdn_domain', e.target.value)}
          />
        </Field>
        <Field label="连接端口">
          <Input
            type="number"
            min={1}
            max={65535}
            value={form.cdn_connect_port}
            placeholder="443"
            onChange={(e) => set('cdn_connect_port', e.target.value)}
          />
        </Field>
        <Field label="回源协议" hint="CDN 回源到 frps 时用 HTTP 还是 HTTPS">
          <Select
            value={form.cdn_origin_scheme === 'HTTPS' ? 'HTTPS' : 'HTTP'}
            onChange={(e) => set('cdn_origin_scheme', e.target.value)}
          >
            <option value="HTTP">HTTP</option>
            <option value="HTTPS">HTTPS</option>
          </Select>
        </Field>
        <Field label="WebSocket 空闲超时（秒）">
          <Input
            type="number"
            min={1}
            value={form.cdn_ws_timeout}
            placeholder="300"
            onChange={(e) => set('cdn_ws_timeout', e.target.value)}
          />
        </Field>
      </div>

      <div className="mt-1 mb-2 text-[12.5px] font-[650] text-fg">
        阿里云 DNS<span className="ml-2 text-[11.5px] font-normal text-dim">可选，用于自动加 CNAME</span>
      </div>
      <div className="grid grid-cols-2 gap-x-3.5 max-[820px]:grid-cols-1">
        <Field label="AccessKey ID">
          <Input
            type="password"
            autoComplete="off"
            value={form.cdn_aliyun_ak_id}
            placeholder={secretPlaceholder('cdn_aliyun_ak_id', 'LTAI 开头')}
            onChange={(e) => set('cdn_aliyun_ak_id', e.target.value)}
          />
        </Field>
        <Field label="AccessKey Secret">
          <Input
            type="password"
            autoComplete="new-password"
            value={form.cdn_aliyun_ak_secret}
            placeholder={secretPlaceholder('cdn_aliyun_ak_secret', '阿里云 AccessKey Secret')}
            onChange={(e) => set('cdn_aliyun_ak_secret', e.target.value)}
          />
        </Field>
        <Field label="主域名">
          <Input
            value={form.cdn_aliyun_root_domain}
            placeholder="example.com"
            onChange={(e) => set('cdn_aliyun_root_domain', e.target.value)}
          />
        </Field>
        <Field label="记录前缀">
          <Input
            value={form.cdn_dns_rr}
            placeholder="frp（留空则从加速域名推导）"
            onChange={(e) => set('cdn_dns_rr', e.target.value)}
          />
        </Field>
      </div>

      <div className="mt-1 flex flex-wrap items-center gap-2.5">
        <Button disabled={busy !== null} onClick={() => void save()}>
          {busy === 'save' ? '保存中…' : '保存配置'}
        </Button>
        <Button variant="ghost" disabled={busy !== null} onClick={() => void verify()}>
          {busy === 'verify' ? '验证中…' : '验证密钥'}
        </Button>
        <Button variant="ghost" disabled={busy !== null} onClick={() => void provision()}>
          {busy === 'provision' ? '接入中…' : '一键接入'}
        </Button>
      </div>
      <Hint>
        验证与接入用的都是<b className="text-fg">已保存</b>的密钥和域名 —— 改完先点「保存配置」。
        一键接入依次做：校验密钥 → 查/建加速域名 → 开 WebSocket → 加 CNAME 解析，每一步幂等，可重复点。
      </Hint>

      {saveErr && (
        <div className="mt-3 rounded-lg border border-bad-fg/40 bg-bad-dim px-3.5 py-2.5 text-[12px] leading-[1.65] text-bad-fg">
          保存失败：
          <span className="break-all" title={saveErr}>
            {saveErr}
          </span>
        </div>
      )}

      {/* ---- 验证密钥结果 ---- */}
      {verifyErr && (
        <div className="mt-3 rounded-lg border border-bad-fg/40 bg-bad-dim px-3.5 py-2.5 text-[12px] leading-[1.65] text-bad-fg">
          <div className="mb-0.5 font-[650]">验证失败（下面是腾讯云原文）</div>
          <div className="break-all" title={verifyErr}>
            {verifyErr}
          </div>
        </div>
      )}

      {zones !== null && !verifyErr && (
        <div className="mt-3">
          <div className="mb-1.5 flex flex-wrap items-baseline gap-2">
            <b className="text-[12.5px] text-ok-fg">密钥可用</b>
            <span className="text-[11.5px] text-dim">
              {zones.length > 0
                ? `共 ${zones.length} 个站点：把上面的 ZoneId 填进表单再保存`
                : '密钥可用，但没有拿到站点列表'}
            </span>
          </div>
          {zones.length === 0 && (
            <div className="rounded-lg border border-warn-fg/40 bg-warn-dim px-3.5 py-2.5 text-[12px] leading-[1.7] text-warn-fg">
              <div className="mb-1 font-[650]">为什么列表是空的</div>
              <div className="break-all" title={verifyMeta?.hint || undefined}>
                {verifyMeta?.hint || '腾讯云返回了空列表，但没有给出更多诊断信息。'}
              </div>
              {verifyMeta?.endpoint && (
                <div className="mt-1.5 text-[11.5px] opacity-80 break-all">
                  实际端点：{verifyMeta.endpoint}；返回 TotalCount={verifyMeta.total ?? 0}
                </div>
              )}
              {verifyMeta?.probes && verifyMeta.probes.length > 0 && (
                <div className="mt-2">
                  <div className="mb-1 text-[11.5px] font-[650]">权限探针</div>
                  <div className="grid gap-1">
                    {verifyMeta.probes.map((pr) => (
                      <div key={pr.action} className="flex min-w-0 items-baseline gap-1.5">
                        <span className={pr.ok ? 'text-ok-fg' : 'text-bad-fg'}>
                          {pr.ok ? '✓' : '✗'}
                        </span>
                        <span className="font-mono text-[11.5px] shrink-0">{pr.action}</span>
                        <span
                          className="min-w-0 truncate text-[11px] opacity-80"
                          title={pr.detail}
                        >
                          {pr.ok ? '允许' : '被拒'}
                        </span>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          )}
          {zones.length > 0 && (
            <Table head={['ZoneId', '站点名称', '状态', '区域']}>
              {zones.map((z) => (
                <tr key={z.ZoneId}>
                  <Td className="max-w-[220px]">
                    <span className="font-mono text-[11.5px] break-all" title={z.ZoneId}>
                      {z.ZoneId}
                    </span>
                  </Td>
                  <Td>
                    <div className="max-w-[200px] truncate" title={z.ZoneName}>
                      {z.ZoneName || '—'}
                    </div>
                  </Td>
                  <Td>
                    <Badge tone={z.Status === 'online' ? 'ok' : 'gray'}>{z.Status || '—'}</Badge>
                  </Td>
                  <Td className="text-[11.5px] text-dim">{z.Area || '—'}</Td>
                </tr>
              ))}
            </Table>
          )}
        </div>
      )}

      {/* ---- 一键接入结果 ---- */}
      {provErr && (
        <div className="mt-3 rounded-lg border border-bad-fg/40 bg-bad-dim px-3.5 py-2.5 text-[12px] leading-[1.65] text-bad-fg">
          <div className="mb-0.5 font-[650]">一键接入请求失败</div>
          <div className="break-all" title={provErr}>
            {provErr}
          </div>
        </div>
      )}

      {prov && (
        <div className="mt-3">
          {/* 总结果 + 每一步全部保留：失败时把前面成功的步骤也留着，
              才能看出「卡在哪一步」，只留最后一条错误反而看不出上下文。 */}
          <div
            className={cx(
              'mb-2 flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[12.5px]',
              prov.ok ? 'text-ok-fg' : 'text-bad-fg',
            )}
          >
            <b>{prov.ok ? '✓ 一键接入完成' : '✗ 一键接入没有走完'}</b>
            {prov.connect_addr && (
              <span
                className="min-w-0 font-mono text-[11.5px] text-muted break-all"
                title={prov.connect_addr}
              >
                客户端应连接 {prov.connect_addr}
              </span>
            )}
          </div>
          <div className="grid gap-1.5">
            {steps.map((s, i) => (
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
                  <b className="min-w-0 text-fg break-all">{s.step}</b>
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
          {!prov.ok && (
            <div className="mt-1.5 text-[11.5px] leading-[1.6] text-dim">
              修好失败那一步之后可以直接重试：已建好的域名与解析会被复用，不会产生重复资源。
            </div>
          )}
        </div>
      )}
    </Card>
  )
}
