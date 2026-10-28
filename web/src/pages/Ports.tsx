import { SettingsPage } from './Settings'
import { useCallback, useEffect, useRef, useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { copyWithHint } from '../lib/format'
import {
  Alert,
  Badge,
  Button,
  Card,
  Hint,
  Pre,
  Stats,
  Stat,
  Table,
  Td,
  type Tone,
} from '../components/ui'
import type { PortCheck, PortCheckResponse, PortsResponse } from '../types'

/** 可达性徽标。没检测过不能标「可达」——那会让人以为公网已经通了。 */
function ReachBadge({ c }: { c?: PortCheck }) {
  if (!c) return <span className="text-xs text-dim">未检测</span>
  const kind = c.evidence === 'external' ? '外部节点已连上' : c.evidence === 'self' ? '本机回访通过' : ''
  // 「本机回访通过」不等于「公网可达」：本机连自己的公网 IP 会走本机路由，
  // 云安全组把整个端口段封死时它照样能通。所以这两种证据必须用不同的词，
  // 否则用户看到绿色「可达」，实际从外网连不上。
  if (c.state === 'open' && c.evidence === 'self') {
    return (
      <Badge tone="warn" title={`${kind}（不代表已从外网验证）`}>
        仅本机可达
      </Badge>
    )
  }
  const map: Record<string, [Tone, string]> = {
    open: ['ok', '可达'],
    blocked: ['bad', '被拦截'],
    no_service: ['gray', '无服务'],
    unknown: ['warn', '无法判定'],
    rate_limited: ['warn', '探测限流'],
  }
  const [tone, label] = map[c.state] ?? ['gray', c.state]
  return (
    <Badge tone={tone} title={kind}>
      {label}
    </Badge>
  )
}

export function Ports() {
  const { me, toast } = useApp()
  const [ports, setPorts] = useState<PortsResponse | null>(null)
  const [checks, setChecks] = useState<Record<number, PortCheck>>({})
  const [busy, setBusy] = useState<'local' | 'ext' | null>(null)
  // 端口集合变了（新增/删除隧道、改端口）就丢掉旧结论，
  // 否则表里会出现"上一轮对这些端口"的判断，比不显示更误导。
  const snapshot = useRef<string>('')

  const loadPorts = useCallback(async () => {
    try {
      const d = await api<PortsResponse>('GET', '/api/ports')
      const cur = (d.required_ports ?? []).join(',')
      if (snapshot.current !== '' && snapshot.current !== cur) setChecks({})
      snapshot.current = cur
      setPorts(d)
    } catch (e) {
      toast((e as Error).message, 'bad')
    }
  }, [toast])

  useEffect(() => {
    void loadPorts()
    // 端口清单会随「建隧道 / 删隧道 / 改端口设置」变化。停在页面上不动时
    // 原来永远是进页面那一刻的快照，别人在另一处建了隧道这里也看不到。
    // 10 秒足够（清单变化不频繁），且只在页面可见时跑。
    const h = window.setInterval(() => {
      if (document.visibilityState === 'visible') void loadPorts()
    }, 10000)
    return () => window.clearInterval(h)
  }, [loadPorts])

  const itemList = ports?.items ?? []
  const requiredItems = itemList.filter((x) => x.required)
  const done = requiredItems.filter((x) => checks[x.port])
  const okCount = done.filter((x) => checks[x.port]?.state === 'open').length
  const listenCount = requiredItems.filter((x) => x.listening).length

  async function runCheck(external: boolean) {
    if (!ports || busy) return
    const list = ports.required_ports ?? []
    if (!list.length) {
      toast('还没有需要放行的端口', 'bad')
      return
    }
    if (external && !ports.probe_host_ok) {
      toast(ports.probe_host_note || '当前公网地址不适合做外部回访', 'bad')
      return
    }
    setBusy(external ? 'ext' : 'local')
    if (external) {
      toast(`外部回访要逐个端口交给第三方节点，约 ${Math.max(6, list.length * 7)} 秒，请稍候`)
    }
    try {
      const r = await api<PortCheckResponse>('POST', '/api/ports/check', { ports: list, external })
      const next: Record<number, PortCheck> = {}
      for (const x of r.results ?? []) next[x.port] = x
      setChecks(next)

      const res = r.results ?? []
      const bad = res.filter((x) => x.state === 'blocked')
      const nos = res.filter((x) => x.state === 'no_service')
      const lim = res.filter((x) => x.external?.state === 'rate_limited')
      // 限流的提示必须能同时出现：它说的是"这次外部回访没做成"，
      // 与"有几个端口被拦"是两件事。以前用 if/else 串起来，
      // 只要有端口被拦，限流这条就永远显示不出来 —— 用户会以为
      // 表里的结论真的是从外网验证过的。
      if (external && lim.length) {
        toast(
          `外部回访没做成（第三方探测服务限流），下面只有本机回访的结论：${lim
            .map((x) => x.port)
            .join(', ')}。稍后再测一次。`,
          'bad',
        )
      } else if (bad.length) {
        toast(`${bad.length} 个端口被防火墙挡住：${bad.map((x) => x.port).join(', ')}`, 'bad')
      } else if (nos.length) {
        toast(`${nos.length} 个端口本机没在监听，先启动服务再测`, 'bad')
      } else {
        toast(
          external ? '检测完成：端口已可从外部访问' : '检测完成：本机回访全部通过（不代表公网可达）',
        )
      }
    } catch (e) {
      toast(`检测失败：${(e as Error).message}`, 'bad')
    } finally {
      setBusy(null)
    }
  }

  async function copyAll() {
    if (!ports) return
    const list = requiredItems
    if (!list.length) {
      toast('还没有需要放行的端口', 'bad')
      return
    }
    const lines = [`# ${ports.access_host || ''} 需要放行的端口`]
    for (const x of list) lines.push(`${x.port}/tcp  ${x.label}${x.note ? `   # ${x.note}` : ''}`)
    const r = await copyWithHint(lines.join('\n'), null, '端口清单已复制')
    toast(r.msg, r.kind)
  }

  const fw = ports?.firewall
  const srcText =
    { settings: '来自设置', request: '从访问地址推断', fallback: '本机回环' }[
      ports?.access_host_source ?? 'fallback'
    ] ?? ''

  const notes: { tone: Tone; text: React.ReactNode }[] = []
  if (ports && !ports.probe_host_ok) notes.push({ tone: 'info', text: ports.probe_host_note })
  if (ports && !ports.frps_running)
    notes.push({
      tone: 'warn',
      text: 'frps 当前未运行：frps 端口和所有隧道端口都不会在监听，此时检测「无服务」是正常的。到「服务端 frps」页启动后再测。',
    })
  const blocked = requiredItems.filter((x) => checks[x.port]?.state === 'blocked')
  if (blocked.length)
    notes.push({
      tone: 'bad',
      text: (
        <>
          <b>{blocked.length} 个端口被挡住</b>：{blocked.map((x) => x.port).join(', ')} ——
          本机在监听，但公网连不回来。先按下面的命令放行主机防火墙，再到云控制台放行安全组。
        </>
      ),
    })
  const notListening = requiredItems.filter((x) => !x.listening)
  if (notListening.length && ports?.frps_running)
    notes.push({
      tone: 'warn',
      text: (
        <>
          这些端口本机没有进程监听，现在放行也没用：{notListening.map((x) => x.port).join(', ')}。
        </>
      ),
    })

  return (
    <>
      <Card
        title="需要放行的端口"
        desc="面板放到云服务器后，这些端口必须对公网开放"
        actions={
          <>
            <Button variant="ghost" size="sm" onClick={() => void runCheck(false)} disabled={!!busy}>
              {busy === 'local' ? '检测中…' : '检测本机'}
            </Button>
            <Button size="sm" onClick={() => void runCheck(true)} disabled={!!busy}>
              {busy === 'ext' ? '外部探测中…' : '外部回访检测'}
            </Button>
          </>
        }
      >
        <Stats>
          <Stat
            label="公网入口"
            value={<span className="text-base">{ports?.access_host || '—'}</span>}
            sub={`${srcText ? `${srcText} · ` : ''}${ports?.panel_listen ?? ''}`}
          />
          <Stat
            label="主机防火墙"
            value={<span className="text-base">{fw?.type === 'none' ? '未检测到' : (fw?.type ?? '—')}</span>}
            sub={`${fw?.active ? '已启用 · ' : ''}${fw?.detail ?? ''}`}
          />
          <Stat
            label="需放行端口"
            value={`${ports?.required_ports?.length ?? 0} 个`}
            sub="面板 + frps + 隧道"
          />
          <Stat
            label={done.length ? '可达（已检测）' : '本机监听'}
            value={`${done.length ? okCount : listenCount} / ${requiredItems.length}`}
            sub={done.length ? `已检测 ${done.length} 个` : '点上面按钮做可达性检测'}
          />
        </Stats>

        {notes.map((n, i) => (
          <Alert key={i} tone={n.tone}>
            {n.text}
          </Alert>
        ))}

        <Table head={['端口', '用途', '本机监听', '可达性', '说明']}>
          {itemList.map((it) => {
            const c = checks[it.port]
            return (
              <tr key={`${it.port}-${it.proto}`}>
                <Td className="font-mono align-top whitespace-nowrap">
                  <b>{it.port}</b>
                  <span className="text-dim">/{it.proto}</span>
                </Td>
                <Td className="min-w-[130px] align-top">
                  {it.label}
                  {!it.required && (
                    <>
                      {' '}
                      <Badge tone="gray">内部</Badge>
                    </>
                  )}
                </Td>
                <Td className="align-top">
                  {it.required ? (
                    it.listening ? (
                      <Badge tone="ok">在听</Badge>
                    ) : (
                      <Badge tone="gray">未监听</Badge>
                    )
                  ) : (
                    <span className="text-dim">—</span>
                  )}
                </Td>
                <Td className="align-top">
                  {it.required ? (
                    <ReachBadge c={c} />
                  ) : (
                    <span className="text-xs text-dim">无需放行</span>
                  )}
                </Td>
                <Td className="min-w-[250px] align-top text-[12.5px]">
                  {c?.detail ? (
                    <>
                      <span className="text-muted">{c.detail}</span>
                      {it.note && (
                        <>
                          <br />
                          <span className="text-[11.5px] text-dim">{it.note}</span>
                        </>
                      )}
                    </>
                  ) : (
                    it.note
                  )}
                </Td>
              </tr>
            )
          })}
        </Table>

        <Hint>
          「本机监听」是此刻现取的（连一下看有没有人应答）；「可达性」需要点上面的检测按钮。外部回访借用公开的
          check-host.net 境外节点，能同时验证主机防火墙与云安全组。
        </Hint>
      </Card>

      <Card
        title="放行命令"
        desc="按识别到的防火墙给最合适的一套，其余也能复制"
        actions={
          <Button variant="ghost" size="sm" onClick={() => void copyAll()}>
            复制端口清单
          </Button>
        }
      >
        {(ports?.commands ?? []).length === 0 ? (
          <div className="text-xs text-dim">当前没有需要放行的端口。</div>
        ) : (
          (ports?.commands ?? []).map((c, i) => (
            <div key={i} className="mb-3.5 rounded-[11px] border border-border bg-card px-4 py-3.5">
              <div className="mb-2.5 flex items-center gap-2 text-[13px] font-[650]">
                {c.name}
                {c.recommended && <Badge tone="ok">你的机器用这套</Badge>}
                <div className="flex-1" />
                <Button
                  variant="ghost"
                  size="sm"
                  className="touch-copy"
                  onClick={async () => {
                    const r = await copyWithHint(c.cmd, null, '已复制命令')
                    toast(r.msg, r.kind)
                  }}
                >
                  复制
                </Button>
              </div>
              {c.note && <div className="mb-2.5 text-xs text-dim">{c.note}</div>}
              <Pre>{c.cmd}</Pre>
            </div>
          ))
        )}
      </Card>

      <Card title="云服务器还差一步" desc="安全组不在操作系统里，面板读不到">
        <div className="text-xs leading-[1.6] text-dim">
          上面的命令只改<b className="text-fg">主机防火墙</b>（ufw / firewalld / iptables）。阿里云、腾讯云、华为云、AWS、Cloudflare
          等还有一层 <b className="text-fg">安全组 / 入方向规则</b>
          ，必须到云控制台手动放行同样的端口 —— 否则本机在监听、防火墙也放行了，公网照样连不上。「外部回访检测」通了，才说明这两层都已经放行。
        </div>
        <div className="mt-2 text-xs leading-[1.6] text-dim">
          外部回访借用公开的 <span className="font-mono">check-host.net</span>
          节点从境外发起连接，它按来源 IP 限流：短时间内连续检测可能返回「探测服务限流」。这属于探测源不可用，
          <b className="text-fg">不等于端口关闭</b>，隔一会儿再点即可。
        </div>
        {me?.ui_source === 'dir' && (
          <Hint>当前 UI 来自磁盘目录（--ui-dir），修改前端后刷新浏览器即生效，无需重编译。</Hint>
        )}
      </Card>
      {/* 端口池：只影响自动分配的公网端口范围，属于这一页。 */}
      <SettingsPage section="ports" />

    </>
  )
}
