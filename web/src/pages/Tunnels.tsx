import { SettingsPage } from './Settings'
import { useEffect, useState } from 'react'
import { useApp, type TunnelDraft } from '../store'
import { api } from '../lib/api'
import { copyWithHint, fmtBytes } from '../lib/format'
import { Badge, Button, Card, Empty, Table, Td, Led } from '../components/ui'
import type { Tunnel, TunnelProbeResult } from '../types'
import { TunnelModal } from './TunnelModal'

function StateBadge({ t }: { t: Tunnel }) {
  switch (t.proxy_state) {
    case 'online':
      return (
        <Badge tone="ok">
          <Led tone="ok" />
          在线
        </Badge>
      )
    case 'waiting_server':
      return <Badge tone="gray" title="frps 没运行，隧道不可能连通">等待服务端</Badge>
    case 'not_deployed':
      return <Badge tone="gray" title="未绑定机器，不会下发给任何客户端">未部署</Badge>
    default:
      return (
        <Badge tone="bad" title="客户端未连上 frps，或 frps 拒绝了这条代理">
          <Led tone="bad" />
          离线
        </Badge>
      )
  }
}

export function Tunnels() {
  const { tunnels, agentMap, frps, accessHost, accessSource, loadTunnels, tunnelDraft, setTunnelDraft, toast } = useApp()
  const [editing, setEditing] = useState<Tunnel | null>(null)
  const [modalOpen, setModalOpen] = useState(false)
  // 从「客户端」页带过来的建隧道草稿（哪台机器 + 哪个服务）。
  // 只在交接那一刻消费一次，消费后清空 —— 否则轮询重渲染会把弹窗反复打开。
  const [prefill, setPrefill] = useState<TunnelDraft | null>(null)
  // 每条隧道的实测结果（按 id 记，互不影响）。'loading' 表示正在测。
  //
  // 结果只留在本页内存里、不进库：它是一个"此刻这条链路能跑多快"的快照，
  // 存下来反而会让人分不清"这是刚测的还是上周的"。
  const [probes, setProbes] = useState<Record<number, TunnelProbeResult | 'loading'>>({})

  /**
   * 测一条隧道的实际吞吐。
   *
   * 请求从**服务器**发出，走 127.0.0.1:公网端口 → frps → frpc → 客户端本地服务，
   * 所以数据真的过了一遍隧道，不是本机回环。
   *
   * 这是判断「关闭 TCP 多路复用」有没有用的唯一实测手段：关 mux 的理论收益是
   * 突破 yamux 的 256KB 单流窗口，但实际能提到多少只能这么量。
   */
  async function runProbe(t: Tunnel) {
    setProbes((p) => ({ ...p, [t.id]: 'loading' }))
    try {
      const r = await api<TunnelProbeResult>('POST', `/api/tunnels/${t.id}/speedtest`, {})
      setProbes((p) => ({ ...p, [t.id]: r }))
      if (!r.ok) toast(r.err || '测速没有结果', 'bad')
      else if (r.note) toast(r.note)
    } catch (e) {
      setProbes((p) => {
        const next = { ...p }
        delete next[t.id]
        return next
      })
      toast((e as Error).message, 'bad')
    }
  }

  useEffect(() => {
    if (!tunnelDraft) return
    setEditing(null)
    setPrefill(tunnelDraft)
    setModalOpen(true)
    setTunnelDraft(null)
  }, [tunnelDraft, setTunnelDraft])

  /** 这条隧道的本地地址对应客户端上报的哪个服务（认不出返回空串）。 */
  function svcNameOf(t: Tunnel): string {
    const ag = t.agent_id ? agentMap[t.agent_id] : null
    const hit = (ag?.services ?? []).find((s) => s.addr === t.local_addr)
    return hit?.name ?? ''
  }

  const readOnly = frps?.running && frps.port_alive && frps.dashboard_ok
  const srcText =
    accessSource === 'settings'
      ? '设置里固定填写的公网地址'
      : accessSource === 'request'
        ? '从你当前访问面板的地址推断（未固定，换网络可能变）'
        : '本机回环，仅本机可用'

  function openNew() {
    setEditing(null)
    setModalOpen(true)
  }

  function openEdit(t: Tunnel) {
    setEditing(t)
    setModalOpen(true)
  }

  async function del(t: Tunnel) {
    if (!confirm(`确认删除隧道「${t.name}」？删除后对应的公网端口会释放。`)) return
    try {
      await api('DELETE', `/api/tunnels/${t.id}`)
      toast('已删除')
      await loadTunnels()
    } catch (e) {
      toast((e as Error).message, 'bad')
    }
  }

  return (
    <>
      <Card
        title="隧道"
        desc="一条隧道 = 把某台机器的一个本地端口映射到公网端口"
        actions={
          <Button size="sm" onClick={openNew}>
            + 新建隧道
          </Button>
        }
      >
        {tunnels.length === 0 ? (
          <Empty>还没有隧道。点右上角「+ 新建隧道」，选机器 → 选服务 → 起名，三步搞定。</Empty>
        ) : (
          <Table
            head={['名称', '客户端', '本地服务', '公网访问', '状态', '流量 ↑上传 / ↓下载', '']}
          >
            {tunnels.map((t) => {
              const ag = t.agent_id ? agentMap[t.agent_id] : undefined
              const url = t.access_url
              return (
                <tr key={t.id}>
                  <Td>
                    {/* 名字是这一页唯一「用户能随便写多长」的列：不截断会把它撑宽，
                        把状态和操作按钮整个挤出屏幕（实测 200 字符的名字 → 名称列
                        1473px、整表 2210px，而视口只有 1440px）。服务端已限 64 字符，
                        这里再兜一层 —— 列宽由界面决定，不由数据决定。 */}
                    <b className="block max-w-[15rem] truncate" title={t.name}>
                      {t.name}
                    </b>
                  </Td>
                  <Td>
                    {ag ? (
                      <>
                        {ag.name}{' '}
                        <span className="text-[11.5px] text-dim">
                          {ag.os}/{ag.arch}
                        </span>
                      </>
                    ) : (
                      <Badge tone="gray">未绑定</Badge>
                    )}
                  </Td>
                  <Td>
                    {/* 只显示 host:port 时，用户看不出这条隧道实际穿透的是什么服务
                        （127.0.0.1:8788 到底是网盘、面板还是别的）。客户端上报过服务名
                        就一起显示，认不出来才退回纯地址。 */}
                    {svcNameOf(t) ? (
                      <span className="block">
                        <b className="block max-w-[12rem] truncate font-medium" title={svcNameOf(t)}>
                          {svcNameOf(t)}
                        </b>
                        <span className="block font-mono text-[11.5px] text-dim">
                          {t.local_addr}
                        </span>
                      </span>
                    ) : (
                      <span className="font-mono">{t.local_addr}</span>
                    )}
                  </Td>
                  <Td>
                    {/* 以后端下发的 access_url 为准（已按当前公网地址推导）；
                        草稿隧道没下发配置，地址是假的，不能给用户复制 */}
                    {url ? (
                      <span className="flex items-center gap-1.5">
                        <span className="font-mono">{url}</span>
                        <Button
                          variant="ghost"
                          size="sm"
                          // whitespace-nowrap：这一列宽度随「公网访问」内容浮动，
                          // 挤到极限时「复制」会被压成上下两行（"复/制"）。
                          className="touch-copy whitespace-nowrap px-2 py-0.5"
                          onClick={async () => {
                            const r = await copyWithHint(url, null, '已复制访问地址')
                            toast(r.msg, r.kind)
                          }}
                        >
                          复制
                        </Button>
                      </span>
                    ) : (
                      <span className="text-dim">
                        {t.agent_id ? '等待分配端口' : '部署后可访问'}
                      </span>
                    )}
                  </Td>
                  <Td>
                    <StateBadge t={t} />
                  </Td>
                  <Td className="font-mono text-dim">
                    <span className="block whitespace-nowrap">
                      {fmtBytes(t.traffic_in)} / {fmtBytes(t.traffic_out)}
                    </span>
                    {/* 只有累计值看不出"是不是刚变慢"。速率由面板对相邻两次采样
                        做差分得到，空闲时为 0 就不显示，避免一片 0 B/s 干扰视线。 */}
                    {(t.rate_in > 0 || t.rate_out > 0) && (
                      <span
                        className="block whitespace-nowrap text-[11px] text-accent-fg"
                        title="frps 统计的实时速率：↑ 是用户上传（进隧道），↓ 是用户下载（出隧道）"
                      >
                        ↑{fmtBytes(t.rate_in)}/s ↓{fmtBytes(t.rate_out)}/s
                      </span>
                    )}
                    {/* 实测吞吐。与上面两行的区别：上面是"用户此刻在用多少"
                        （没人用就是 0），这里是"主动压一下，这条链路最多能跑多少"。
                        两者都不为 0 但不相等很正常 —— 前者受用户操作影响。 */}
                    {(() => {
                      const pr = probes[t.id]
                      if (pr === 'loading') {
                        return (
                          <span className="block whitespace-nowrap text-[11px] text-dim">
                            测速中…
                          </span>
                        )
                      }
                      if (!pr) return null
                      if (!pr.ok) {
                        return (
                          <span
                            className="block max-w-[13rem] truncate text-[11px] text-bad-fg"
                            title={pr.err}
                          >
                            测速失败
                          </span>
                        )
                      }
                      return (
                        <span className="block whitespace-nowrap text-[11px] text-ok-fg">
                          实测 {pr.speed > 0 ? `${fmtBytes(pr.speed)}/s` : '—'}
                          <span className="ml-1 text-dim">RTT {pr.rtt_ms}ms</span>
                        </span>
                      )
                    })()}
                  </Td>
                  <Td align="right" className="whitespace-nowrap">
                    <Button variant="ghost" size="sm" onClick={() => openEdit(t)}>
                      编辑
                    </Button>{' '}
                    {/* 用 primary 而不是 ghost：三个 ghost 按钮并排时，"测速"在
                        视觉上跟"编辑 / 删除"完全同级，用户扫一眼根本注意不到。
                        它是这一页唯一能给出"这条隧道到底多快"的动作，值得突出。 */}
                    <Button
                      size="sm"
                      className="whitespace-nowrap px-2 py-0.5"
                      disabled={probes[t.id] === 'loading' || !t.agent_id}
                      // 测不了的情况要说原因，不然用户只看到按钮灰着
                      title={
                        !t.agent_id
                          ? '未绑定机器，没有可测的目标'
                          : '从本服务器压一次数据，走 frps → frpc → 客户端本地服务，测这条隧道的实际吞吐（约 1~8 秒）'
                      }
                      onClick={() => void runProbe(t)}
                    >
                      {probes[t.id] === 'loading' ? '测速中' : '测速'}
                    </Button>{' '}
                    <Button variant="danger" size="sm" onClick={() => void del(t)}>
                      删除
                    </Button>
                  </Td>
                </tr>
              )
            })}
          </Table>
        )}

        <div className="mt-1.5 text-xs leading-[1.5] text-dim">
          {readOnly
            ? `状态来自 frps 自身（dashboard 直读），「在线」= frps 上这条代理确实建立了。公网入口 ${accessHost} —— ${srcText}。`
            : `提示：状态以 frps 侧为准，frps 没运行时一律显示「等待服务端」，不会再误报在线。公网入口 ${accessHost} —— ${srcText}。`}
          {/* 流量列的说明必须两个分支都有：这一列始终存在，用户对三点数字的困惑
              与 frps 是否运行无关。之前只写在 readOnly 分支里，正常运行时反而看不到。 */}
          {' '}流量三行：「累计」是 frps 统计的今日总量；「↑↓ 每秒」是最近 5 秒用户实际在
          用多少（没人访问就是 0）；「实测」是点「测速」后主动压出来的链路吞吐上限。
          上传 = 用户 → 服务，下载 = 服务 → 用户。
          右侧「测速」量的是<b className="text-fg">服务器 → frps → frpc → 客户端本地服务</b>
          这一整段的吞吐，也就是用户走这条隧道时的真实上限；它不是"你这台电脑到服务器"的速度。
        </div>
      </Card>

      {/* 隧道传输参数（压缩 / tcpMux / 连接池）原本在「设置」里。
          它只影响隧道行为，用户在隧道页才会问"为什么慢"，所以搬到这里。 */}
      <SettingsPage section="tunnel" />

      <TunnelModal
        open={modalOpen}
        tunnel={editing}
        prefill={prefill}
        onClose={() => {
          setModalOpen(false)
          // 关掉就把草稿丢掉，下次从客户端点进来才是新的一份
          setPrefill(null)
        }}
        onSaved={() => {
          setModalOpen(false)
          void loadTunnels()
        }}
      />
    </>
  )
}
