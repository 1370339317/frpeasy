import { useEffect, useMemo, useRef, useState } from 'react'
import { useApp, type TunnelDraft } from '../store'
import { api } from '../lib/api'
import { fmtAgo } from '../lib/format'
import {
  Button,
  Chip,
  Field,
  Hint,
  Input,
  Led,
  Modal,
  Section,
  cx,
} from '../components/ui'
import type { Tunnel, TunnelBody } from '../types'

/**
 * 新建 / 编辑隧道。
 *
 * 三个字段的顺序就是用户的心智顺序：哪台机器 → 它的哪个服务 → 叫什么名字。
 * 「先不部署」是一个显式选项（保存为草稿），因为 frp 的 proxy 名与 remote_port
 * 在单个 frps 上必须唯一，未绑定机器的隧道不能广播给所有 agent。
 */
export function TunnelModal({
  open,
  tunnel,
  prefill,
  onClose,
  onSaved,
}: {
  open: boolean
  tunnel: Tunnel | null
  /** 从「客户端」页点某个服务带过来的草稿：机器 + 服务地址 + 建议的名字 */
  prefill?: TunnelDraft | null
  onClose: () => void
  onSaved: () => void
}) {
  const { agents, agentMap, tunnels, toast } = useApp()
  const [name, setName] = useState('')
  const [local, setLocal] = useState('')
  const [port, setPort] = useState('')
  const [agentID, setAgentID] = useState('')
  const [busy, setBusy] = useState(false)

  // 回填只在「这一次打开」发生，绝不跟着轮询重跑。
  //
  // 原来的依赖是 [open, tunnel, agents]，而 agents 来自 store：全局 5 秒轮询
  // 每次都会放进一个**新数组**（`r.agents ?? []`），于是 effect 每 5 秒重跑一次，
  // 把用户正在填的名字、地址、端口全部重置成空 —— 体感就是"输入框自己清空了"。
  //
  // 正确模型：表单的初值只由"打开弹窗的那一瞬间"决定。
  // args 放进 ref 只是为了在打开时读到最新值，不参与依赖比较。
  const seedRef = useRef({ tunnel, agents, prefill })
  seedRef.current = { tunnel, agents, prefill }
  const seededRef = useRef(false)

  useEffect(() => {
    if (!open) {
      // 关掉后允许下一次打开重新回填
      seededRef.current = false
      return
    }
    if (seededRef.current) return
    seededRef.current = true
    const { tunnel: t, agents: ags, prefill: pf } = seedRef.current
    // 预填优先于"编辑回填"：从客户端页点进来时 t 一定是 null，
    // 但把它写在前面能保证以后两种入口都用同一套取值顺序。
    setName(pf?.name ?? t?.name ?? '')
    setLocal(pf?.localAddr ?? t?.local_addr ?? '')
    // 编辑时把当前公网端口填出来。原来这里永远是空的，用户想改端口
    // 得先去列表里看一眼现在是多少，否则不知道该填什么。
    // 清空后再保存仍是"保留原端口"（后端只在 manual_port>0 时才改），不会误换。
    setPort(t ? String(t.public_port || '') : '')
    // 「只有一台机器就默认选中它」同样只在打开时判断一次：
    // 跟着轮询走的话，用户主动点了「先不部署」也会被改回去。
    setAgentID(pf?.agentID || t?.agent_id || (ags.length === 1 ? ags[0].id : ''))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  const services = useMemo(() => (agentID ? (agentMap[agentID]?.services ?? []) : []), [agentID, agentMap])

  async function submit() {
    if (busy) return
    const n = name.trim()
    const l = local.trim()
    const p = Number.parseInt(port, 10) || 0
    if (!n) {
      toast('请给隧道起个名字', 'bad')
      return
    }
    if (!agentID && !l) {
      toast('请先选一台机器并挑一个服务，或手填本地服务地址', 'bad')
      return
    }

    const body: TunnelBody = {
      name: n,
      local_addr: l,
      protocol: 'tcp',
      agent_id: agentID,
      manual_port: p,
    }
    // agent_id 传空串后端会保留原绑定，解除绑定必须显式声明
    if (tunnel && !agentID) body.clear_agent = true

    setBusy(true)
    try {
      if (tunnel) await api('PUT', `/api/tunnels/${tunnel.id}`, body)
      else await api('POST', '/api/tunnels', body)
      toast(tunnel ? '已保存修改' : '隧道已创建')
      onSaved()
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      title={tunnel ? `编辑隧道 · ${tunnel.name}` : '新建隧道'}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            取消
          </Button>
          <Button onClick={() => void submit()} disabled={busy}>
            {busy ? '保存中…' : '保存'}
          </Button>
        </>
      }
    >
      <Section no={1} title="选哪台机器" desc="隧道由这台机器上的客户端承载。列表里带 ● 的是在线机器。">
        {agents.length === 0 ? (
          <div className="text-[12.5px] text-dim">
            还没有客户端。先到「客户端」页复制安装命令，在那台机器上执行一次。
          </div>
        ) : (
          <div className="grid gap-2.5 [grid-template-columns:repeat(auto-fill,minmax(210px,1fr))]">
            {agents.map((a) => (
              <div
                key={a.id}
                onClick={() => setAgentID(a.id)}
                className={cx(
                  'cursor-pointer rounded-[10px] border px-3.5 py-3 transition',
                  a.id === agentID
                    ? 'border-accent bg-accent-dim'
                    : 'border-border2 bg-chip hover:border-border3',
                )}
              >
                <div className="flex items-center gap-[7px] text-[13.5px] font-[650]">
                  <Led tone={a.online ? 'ok' : 'bad'} />
                  {a.name}
                </div>
                <div className="mt-1 text-[11.5px] text-muted">
                  {a.os}/{a.arch} ·{' '}
                  {a.online ? '在线' : `离线 ${fmtAgo(a.last_seen)}`} ·{' '}
                  {(a.services ?? []).length} 个服务
                </div>
              </div>
            ))}
            <div
              onClick={() => setAgentID('')}
              className={cx(
                'cursor-pointer rounded-[10px] border px-3.5 py-3 transition',
                agentID === ''
                  ? 'border-accent bg-accent-dim'
                  : 'border-border2 bg-chip hover:border-border3',
              )}
            >
              <div className="text-[13.5px] font-[650]">先不部署</div>
              <div className="mt-1 text-[11.5px] text-muted">保存为草稿，不下发给任何机器</div>
            </div>
          </div>
        )}
      </Section>

      <Section no={2} title="穿透它的哪个服务" desc="下面是这台机器上报的本机监听服务，点一下就选中；没想要的就手填。">
        <div className="flex flex-wrap gap-2">
          {!agentID ? (
            <span className="text-xs text-dim">先在上面选一台机器。</span>
          ) : services.length === 0 ? (
            <span className="text-xs text-dim">
              这台机器还没上报服务（可能离线）。可在下面手填 host:port。
            </span>
          ) : (
            services.map((s) => (
              <Chip
                key={s.addr}
                selected={s.addr === local}
                sub={s.addr}
                onClick={() => setLocal(s.addr)}
              >
                {s.name}
              </Chip>
            ))
          )}
        </div>
        <Field className="mt-3 mb-0" label="本地服务地址（host:port）">
          <Input
            value={local}
            onChange={(e) => setLocal(e.target.value)}
            placeholder="如 127.0.0.1:80 或 192.168.1.10:3389"
          />
        </Field>
      </Section>

      <Section no={3} title="取个名字">
        <Input
          value={name}
          maxLength={64}
          onChange={(e) => setName(e.target.value)}
          placeholder="如 家里群晖 / 办公室远程桌面"
        />
        <Hint>最多 64 个字符（名字会被截断显示在列表里，悬停可看全名）。</Hint>
        <Field
          className="mt-3 mb-0"
          label={
            tunnel
              ? `公网端口（当前 ${tunnel.public_port}；改成别的数字，清空则保持不变）`
              : '公网端口（留空自动分配）'
          }
        >
          <Input
            type="number"
            value={port}
            onChange={(e) => setPort(e.target.value)}
            placeholder="自动分配"
          />
        </Field>
        <Hint>
          当前共 {tunnels.length} 条隧道
          {tunnel ? `，正在编辑「${tunnel.name}」` : ''}。留空由面板从端口池里挑一个没被占用的。
        </Hint>
      </Section>
    </Modal>
  )
}
