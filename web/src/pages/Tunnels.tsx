import { useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { copyText, fmtBytes } from '../lib/format'
import { Badge, Button, Card, Empty, Table, Td, Led } from '../components/ui'
import type { Tunnel } from '../types'
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
  const { tunnels, agentMap, frps, accessHost, accessSource, loadTunnels, toast } = useApp()
  const [editing, setEditing] = useState<Tunnel | null>(null)
  const [modalOpen, setModalOpen] = useState(false)

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
            head={['名称', '客户端', '本地服务', '公网访问', '状态', '今日流量 入/出', '']}
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
                    <span className="font-mono">{t.local_addr}</span>
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
                          className="px-2 py-0.5"
                          onClick={async () =>
                            toast(
                              (await copyText(url)) ? '已复制访问地址' : '复制失败',
                              'ok',
                            )
                          }
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
                    {fmtBytes(t.traffic_in)} / {fmtBytes(t.traffic_out)}
                  </Td>
                  <Td align="right" className="whitespace-nowrap">
                    <Button variant="ghost" size="sm" onClick={() => openEdit(t)}>
                      编辑
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
            ? `状态来自 frps 自身（dashboard 直读），「在线」= frps 上这条代理确实建立了。流量为 frps 统计的今日累计值。公网入口 ${accessHost} —— ${srcText}。`
            : `提示：状态以 frps 侧为准，frps 没运行时一律显示「等待服务端」，不会再误报在线。公网入口 ${accessHost} —— ${srcText}。`}
        </div>
      </Card>

      <TunnelModal
        open={modalOpen}
        tunnel={editing}
        onClose={() => setModalOpen(false)}
        onSaved={() => {
          setModalOpen(false)
          void loadTunnels()
        }}
      />
    </>
  )
}
