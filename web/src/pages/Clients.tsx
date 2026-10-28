import { useEffect, useRef, useState } from 'react'
import { useApp, type CommandKind } from '../store'
import { api } from '../lib/api'
import { copyWithHint, fmtAgo, fmtBytes } from '../lib/format'
import type { Agent, AgentLogResponse } from '../types'
import {
  Alert,
  Button,
  Card,
  Chip,
  Led,
  Modal,
  Pre,
  Tabs,
  Badge,
  cx,
} from '../components/ui'

// 卸载宽限期。
//
// 客户端收到指令后：先停 frpc，再起一个独立的清理进程删文件，agent 本身还要
// 多存活 8 秒（否则清理进程会被 systemd 的 cgroup 回收一起杀掉）。
// 面板侧这时还认它在线（离线判定走的是「卸载中」的短 TTL，20 秒），
// 所以宽限期只需要覆盖「清理脚本执行 + 一次离线判定」这一段。
//
// 早期取值 60 秒：配合常规 60 秒离线 TTL，用户要等 75 秒才看到结果，
// 期间界面一直停在「正在卸载」，看起来就是卡住了（真机反馈过）。
const UNINSTALL_GRACE_MS = 30_000

/** 距卸载指令被心跳领走过了多久（毫秒）。解析不出来按「已超时」处理。 */
function sinceUninstallSent(a: Agent): number {
  const raw = a.uninstall_sent_at
  if (!raw) return Number.POSITIVE_INFINITY
  const t = Date.parse(raw)
  if (Number.isNaN(t)) return Number.POSITIVE_INFINITY
  return Date.now() - t
}

/** 这台机器是否正处在「已下发卸载、还在等它退干净」的窗口里。 */
function isUninstalling(a: Agent): boolean {
  return !!a.uninstall_sent_at && a.online && sinceUninstallSent(a) < UNINSTALL_GRACE_MS
}

/** 把客户端上报的源 token 说成人话。空串 = 旧版 agent 没上报。 */
function binSourceLabel(tok: string): string {
  if (tok === 'panel') return '面板中继'
  if (tok === 'github') return 'GitHub 直连'
  try {
    return new URL(tok).host
  } catch {
    return tok
  }
}

const TABS: { key: CommandKind; label: string }[] = [
  { key: 'linux', label: 'Linux' },
  { key: 'win', label: 'Windows' },
  { key: 'android', label: 'Android' },
]

const HINTS: Record<CommandKind, React.ReactNode> = {
  linux: (
    <>
      需要 root / 可写 <span className="font-mono">/usr/local/bin</span>；有 systemd
      就注册服务，有 OpenRC（Alpine）就注册 init.d，都没有则用自带看护脚本后台跑。
      重跑同一条命令即升级或修复：先下载校验，通过后才停旧进程替换。
    </>
  ),
  win: (
    <>
      必须在<b>以管理员身份运行</b>的 PowerShell 里执行。脚本纯 ASCII，中文系统不会乱码。重跑即升级：新二进制先用{' '}
      <span className="font-mono">--version</span> 验证通过才替换旧文件，起不来会自动回滚到旧版本。卸载：
      <span className="font-mono">.\install.ps1 -Uninstall</span>
    </>
  ),
  android: (
    <>
      手机/车机连上 adb 后执行，<b>不需要 root</b>。只把二进制放到{' '}
      <span className="font-mono">/data/local/tmp/frpeasy-agent</span> 并启动一次 ——
      <b>不注册任何开机自启、不挂看护循环</b>。设备和 root 方案的差别太大（Magisk / KernelSU /
      APatch / adb su 各自的自启目录都不同），写错位置轻则静默无效，重则系统升级后
      留下一份失效脚本，所以这里一律不碰。启停由你控制：
      <span className="font-mono">sh run.sh start|stop|restart|status</span>。
      探测本机服务与转发端口都不需要 root；确实要绑低端口时，自己在命令末尾加{' '}
      <span className="font-mono">--elevate</span>。
    </>
  ),
}

/**
 * 公网地址没写端口、而面板又不在 80/443 时，安装命令里的 URL 会指向 80 端口，
 * 客户端必然装不上。这种配置错误必须当场说清，而不是等用户拿着命令反复试。
 */
function PortWarn({ base, port, accessHost }: { base: string; port: string; accessHost: string }) {
  if (!base || !port) return null
  const m = base.match(/^[a-z]+:\/\/(\[[^\]]+\]|[^/:]+)(?::(\d+))?/i)
  if (!m || m[2]) return null
  if (port === '80' || port === '443') return null
  return (
    <Alert tone="warn">
      ⚠ 安装命令里的面板地址是 <span className="font-mono">{base}</span>，<b>没有带端口</b>，而面板实际监听在{' '}
      <b>{port}</b>。除非前面有反向代理把 80/443 转过来，否则客户端连不上 —— 到「设置 →
      公网地址」改成{' '}
      <span className="font-mono">
        {accessHost || '主机'}:{port}
      </span>
      。
    </Alert>
  )
}

/**
 * 客户端卡片的状态。
 *
 * 只看 online 不够：点了卸载之后，客户端要先停 frpc、删文件、退进程才会消失，
 * 这中间画面上只会显示「在线」，用户根本分不清是「正在卸载」还是「点了没反应」。
 * 而卸载失败（多因权限不足）时它会一直在线，那就更看不出来了。
 *
 * 三个字段的组合含义见 shared.Agent 的注释：
 *   requested 未消费   → 指令排队中
 *   已消费 + 仍在心跳  → 卸载没生效
 *   已消费 + 已离线    → 卸载成功，记录是残留
 */
function agentState(a: Agent): {
  label: string
  tone: 'ok' | 'gray' | 'warn' | 'bad'
  led: 'ok' | 'bad' | 'warn'
  note?: React.ReactNode
  /** 机器上已经没有 agent 了（只剩面板记录） */
  gone: boolean
} {
  const sent = !!a.uninstall_sent_at
  if (sent && a.online && sinceUninstallSent(a) < UNINSTALL_GRACE_MS) {
    const waited = Math.max(0, Math.round(sinceUninstallSent(a) / 1000))
    return {
      label: '正在卸载',
      tone: 'warn',
      led: 'warn',
      gone: false,
      note: (
        <>
          客户端已收到卸载指令，正在停服务、摘开机自启、删文件。
          <b>这期间它还会心跳一两轮，显示「在线」是正常的</b>，不是没生效。
          <br />
          已等待 <b>{waited} 秒</b>（一般 30 秒内退干净，超过才会判「卸载未生效」）。
          页面切到后台时浏览器会限制定时器，回来点一下刷新即可。
        </>
      ),
    }
  }
  if (sent && a.online) {
    return {
      label: '卸载未生效',
      tone: 'bad',
      led: 'bad',
      gone: false,
      note: (
        <>
          指令已送达，但一分钟后它仍在心跳 —— 说明清理没跑完，或者跑完又被拉了起来。
          最常见的原因是<b>自启没摘掉</b>（进程退出后计划任务 / systemd 又把它拉回来），
          其次是 exe 或数据目录被别的进程占用。
          <br />
          到那台机器上再执行一次上面的卸载命令最稳。想知道它卡在哪一步：
          Windows 看 <code className="font-mono">C:\ProgramData\frpeasy-uninstall.log</code>，
          Linux 看 <code className="font-mono">/tmp/frpeasy-uninstall.log</code> 与{' '}
          <code className="font-mono">journalctl -u frpeasy-agent</code>。
        </>
      ),
    }
  }
  if (sent) {
    return {
      label: '已卸载',
      tone: 'gray',
      led: 'bad',
      gone: true,
      note: (
        <>
          {a.uninstall_by_local ? (
            <>
              那台机器<b>在本机执行了卸载</b>（跑的是卸载命令），临退出前通知了面板
              （最后心跳 {fmtAgo(a.last_seen)}）。
            </>
          ) : (
            <>
              面板下发的卸载已完成，那台机器上的 agent 已经退出（最后心跳 {fmtAgo(a.last_seen)}）。
            </>
          )}{' '}
          面板这条记录只是残留，不影响隧道之外的任何东西，点「移除」就能清掉。
        </>
      ),
    }
  }
  if (a.uninstall_requested) {
    return {
      label: '等待卸载',
      tone: 'warn',
      led: 'warn',
      gone: false,
      note: a.online ? (
        <>卸载指令已排队，等它下一次心跳（约 15 秒内）领取后执行。</>
      ) : (
        <>
          卸载指令已排队，但它<b>当前离线</b> —— 指令存在面板里，等它下次上线心跳时<b>立即执行</b>。
          它可能几天后才开机，想反悔的话现在还能撤销。
        </>
      ),
    }
  }
  return { label: a.online ? '在线' : '离线', tone: a.online ? 'ok' : 'gray', led: a.online ? 'ok' : 'bad', gone: false }
}

export function Clients() {
  const {
    agents,
    install,
    uninstall,
    installBase,
    panelPort,
    accessHost,
    loadAll,
    loadAgents,
    toast,
  } = useApp()
  const [kind, setKind] = useState<CommandKind>('linux')
  // 命令块的引用：复制失败时用它把命令原文选中，让用户长按复制。
  // 手机浏览器（尤其国产内核 / 非 HTTPS）经常禁掉 execCommand，
  // 这条"人工路径"是最后也最可靠的一条。
  const installRef = useRef<HTMLPreElement | null>(null)
  const uninstallRef = useRef<HTMLPreElement | null>(null)
  const logPreRef = useRef<HTMLPreElement | null>(null)

  // 客户端日志弹窗。面板连不上 NAT 后面的机器，所以是「请求 → 等它下一次心跳回传」，
  // 中间隔着最长一个心跳周期：界面必须把"还在等"和"到了"分清楚，否则用户只会看到转圈。
  //
  // 超时只有一个来源：LOG_WAIT_MS。文案里的秒数由它算出来，不再手写数字 ——
  // 之前文案写"约 15 秒"、判定是 25 秒、实际显示 27~30 秒，三个数字互相打架。
  const LOG_WAIT_MS = 20000
  const LOG_WAIT_SEC = Math.round(LOG_WAIT_MS / 1000)
  const [logFor, setLogFor] = useState<Agent | null>(null)
  const [logReq, setLogReq] = useState('')
  const [log, setLog] = useState<{
    content: string
    fetchedAt: string | null
    /** 本次请求还没被答复，正在等 */
    waiting: boolean
    /** 这份内容是不是「我这次请求」的结果。false 且 content 非空 = 只剩上一次的旧快照 */
    answered: boolean
    /** 客户端当前离线：请求已排队，等它上线后的第一次心跳才执行（此时不该转圈） */
    offline: boolean
    error: string
    elapsed: number
  }>({
    content: '',
    fetchedAt: null,
    waiting: false,
    answered: false,
    offline: false,
    error: '',
    elapsed: 0,
  })

  /** 读一次面板上留存的那份快照（不触发新请求）。 */
  async function loadLogSnapshot(id: string) {
    try {
      const r = await api<AgentLogResponse>('GET', `/api/agents/${encodeURIComponent(id)}/logs`)
      setLog((v) => ({
        ...v,
        content: r.logs.content || v.content,
        fetchedAt: r.logs.fetched_at ?? v.fetchedAt,
      }))
    } catch {
      /* 快照读不到不影响主流程（用户还能看到"没有内容"） */
    }
  }

  /** 发起一次日志请求（也用于「重新拉取」）。 */
  async function openLog(a: Agent) {
    setLogFor(a)
    setLogReq('')
    // 先按列表里的在线状态给一个初值，随后以面板返回的 online 为准
    // （列表数据可能是几秒前的，点了之后它刚好上下线都有可能）。
    setLog({
      content: '',
      fetchedAt: null,
      waiting: a.online,
      answered: false,
      offline: !a.online,
      error: '',
      elapsed: 0,
    })
    try {
      const r = await api<{ request_id: string; online: boolean; message: string }>(
        'POST',
        `/api/agents/${encodeURIComponent(a.id)}/logs`,
      )
      if (!r.online) {
        // 它不在线：请求会排在面板里、等它上线后执行。此时**不能转圈等** ——
        // 等多久都不会有结果，用户只会以为坏了。
        setLog((v) => ({ ...v, waiting: false, offline: true }))
        setLogReq('')
        void loadLogSnapshot(a.id)
        return
      }
      setLogReq(r.request_id)
    } catch (e) {
      // 版本过低 / 客户端不存在都会走到这里：直接把面板给的原因摊在弹窗里，
      // 比只弹一句 toast 有用得多（它带着该去哪儿看日志文件）。
      setLog((v) => ({ ...v, waiting: false, offline: false, error: (e as Error).message }))
    }
  }

  // 等回传：每 2 秒看一次快照，拿到"这次请求的回包"就停。
  // 超过 LOG_WAIT_MS 就停下并说明原因 —— 不能无限转圈。
  useEffect(() => {
    if (!logFor || !logReq) return
    const id = logFor.id
    const started = Date.now()
    let stopped = false
    const h = window.setInterval(() => {
      void (async () => {
        try {
          const r = await api<AgentLogResponse>('GET', `/api/agents/${encodeURIComponent(id)}/logs`)
          if (stopped) return
          const done = r.logs.content_req === logReq
          setLog((v) => ({
            ...v,
            content: r.logs.content || v.content,
            fetchedAt: r.logs.fetched_at ?? v.fetchedAt,
            waiting: !done,
            answered: done,
            elapsed: Math.round((Date.now() - started) / 1000),
          }))
          if (done) window.clearInterval(h)
        } catch {
          /* 面板暂时不可达：下一轮再试 */
        }
        if (!stopped && Date.now() - started > LOG_WAIT_MS) {
          window.clearInterval(h)
          setLog((v) => ({
            ...v,
            waiting: false,
            elapsed: Math.round((Date.now() - started) / 1000),
          }))
        }
      })()
    }, 2000)
    return () => {
      stopped = true
      window.clearInterval(h)
    }
  }, [logFor, logReq])

  // 安装命令只有一个版本：源顺序由面板设置决定、每轮下发，
  // 装完改设置就生效 —— 所以命令里不再有"从哪儿取"这个选项。
  const cmd = install[kind] || '加载中…'
  const unCmd = uninstall[kind] || '加载中…'
  // 常规刷新交给 store 里的全局轮询（5 秒）；这里只处理一种例外：
  // 「正在卸载」那几秒状态在快速变化，5 秒粒度会显得卡住，加密到 3 秒。
  const uninstalling = agents.some(isUninstalling)
  useEffect(() => {
    if (!uninstalling) return
    const h = window.setInterval(() => void loadAgents(), 3000)
    return () => window.clearInterval(h)
  }, [uninstalling, loadAgents])

  /** 云端下发卸载：标记随该客户端下一次心跳下发，由它自己清理并退出。 */
  async function uninstallAgent(id: string, name: string, online: boolean) {
    // 第一行必须是结论：这是唯一会动到"别人那台机器"的操作，
    // 手机上原生确认框只显示几行字，用户往下滚之前看到的就是第一行。
    const msg = online
      ? `⚠ 彻底卸载「${name}」上的 frpeasy agent？\n\n` +
        `它会在下一次心跳（约 15 秒内）停掉 frpc、摘掉开机自启、删除程序和配置目录，然后从面板上离线。\n\n` +
        `这台机器以后要重新执行安装命令才能再接入。若它当前不是以 root / 管理员运行，会卸载失败并继续在线。`
      : `⚠「${name}」当前离线。\n\n卸载指令会先存下来，等它下次上线心跳时立即执行。确定下发吗？`
    if (!confirm(msg)) return
    try {
      const r = await api<{ message: string }>('POST', `/api/agents/${encodeURIComponent(id)}/uninstall`)
      toast(r?.message || '卸载指令已下发', 'ok')
      await loadAll()
    } catch (e) {
      toast(`下发失败：${(e as Error).message}`, 'bad')
    }
  }

  /** 撤销还没被领走的卸载指令：离线时点错了的唯一反悔途径。 */
  async function cancelUninstall(id: string, name: string) {
    if (!confirm(`撤销对「${name}」的卸载指令？\n\n它还没被那台机器领走，撤销后不会再执行。`)) return
    try {
      const r = await api<{ message: string }>('DELETE', `/api/agents/${encodeURIComponent(id)}/uninstall`)
      toast(r?.message || '已撤销卸载指令', 'ok')
      await loadAll()
    } catch (e) {
      toast(`撤销失败：${(e as Error).message}`, 'bad')
    }
  }

  async function remove(id: string, name: string, gone: boolean) {
    const msg = gone
      ? `清掉残留记录「${name}」？\n\n` +
        `那台机器上已经没有 frpeasy 的 agent 了，这条记录只是面板留下的历史。\n\n` +
        `如果还有隧道绑着它，那些隧道会变成「未部署」草稿（配置不删）。`
      : `仅移除面板里的记录「${name}」？\n\n这只删面板这一条记录，并把它的隧道解绑为草稿；` +
        `那台机器上的 agent 仍在运行、仍会继续连面板（下次心跳会重新注册回来）。\n\n` +
        `⚠ 解绑后的隧道不会再自动绑回这台机器 —— 它的新身份是一个新的 id。\n\n` +
        `要真正卸载那台机器，请用上面的「卸载命令」或卡片上的「卸载」。`
    if (!confirm(msg)) return
    try {
      await api('DELETE', `/api/agents/${encodeURIComponent(id)}`)
      toast('已移除客户端记录')
      await loadAll()
    } catch (e) {
      // 在线时后端会拒绝并给出"该怎么办"的完整说明，
      // 这里原样摊出来 —— 换成一句"移除失败"等于把最有用的信息丢掉。
      toast(`移除失败：${(e as Error).message}`, 'bad')
      await loadAgents()
    }
  }

  return (
    <>
      <Card
        title="安装客户端"
        desc="在需要被穿透的那台机器上执行一次；重跑即升级 / 修复"
        actions={<Tabs value={kind} onChange={setKind} items={TABS} />}
      >
        {/* 命令块要能被"点一下全选"：手机上复制按钮可能被内核禁掉，
            这是用户自己长按复制的入口，所以命令必须可选中、且一次选全。 */}
        <Pre ref={installRef} selectAll>
          {cmd}
        </Pre>
        <div className="mt-2 text-xs leading-[1.6] text-dim">
          安装这一步<b>固定经面板</b>取 agent（装的时候 agent 还没跑起来，源顺序也还没下发；
          而且装不上比走慢一点的通道严重得多）。装好之后，frpc 与自动更新按
          <b>「设置 → 二进制下载」</b>里的客户端顺序走；改设置下一轮心跳生效，不用重装。
          当前生效的源写在下面客户端卡片的「二进制来源」一栏。
        </div>
        <div className="mt-2 text-xs leading-[1.6] text-dim">{HINTS[kind]}</div>
        <PortWarn base={installBase} port={panelPort} accessHost={accessHost} />
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <Button
            size="sm"
            className="touch-copy"
            onClick={async () => {
              const r = await copyWithHint(cmd, installRef.current, '已复制安装命令')
              toast(r.msg, r.kind)
            }}
          >
            复制命令
          </Button>
          <span className="text-xs text-dim">
            装完之后，映射哪些服务、用哪个公网端口，全部在这块面板上点；客户端只负责拉配置和保活 frpc，不用再碰。
          </span>
        </div>
      </Card>

      <Card
        title="卸载客户端"
        desc="客户端在线时更省事：点下面卡片上的「卸载」，面板会通知它自己清理；离线或用不了面板时，复制这条命令到那台机器执行"
        actions={<Tabs value={kind} onChange={setKind} items={TABS} />}
      >
        <Pre ref={uninstallRef} selectAll>
          {unCmd}
        </Pre>
        <div className="mt-2 text-xs leading-[1.6] text-dim">
          卸载命令<b>不需要令牌</b>：它只做三件事 —— 停掉进程、摘掉开机自启（systemd / OpenRC /
          计划任务）、删掉二进制和数据目录。跑完那台机器上不留 frpeasy 的任何东西。
          {kind === 'android' ? (
            <>
              {' '}
              Android 上没有自启可摘（本来就没注册过），卸载只是把{' '}
              <span className="font-mono">/data/local/tmp/frpeasy-agent</span> 整个删掉。
            </>
          ) : null}
        </div>
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <Button
            size="sm"
            className="touch-copy"
            onClick={async () => {
              const r = await copyWithHint(unCmd, uninstallRef.current, '已复制卸载命令')
              toast(r.msg, r.kind)
            }}
          >
            复制卸载命令
          </Button>
          <span className="text-xs text-dim">
            卸载只影响这一台机器；它绑定的隧道会变成草稿（不删配置），重新装回客户端即可继续使用。
          </span>
        </div>
      </Card>

      <div className="grid gap-3.5 [grid-template-columns:repeat(auto-fill,minmax(290px,1fr))]">
        {agents.length === 0 ? (
          <div className="rounded-xl border border-border bg-card px-4.5 py-4">
            <div className="text-[14.5px] font-[650]">还没有客户端</div>
            <div className="mt-2 text-xs text-muted">
              复制上面的安装命令，在需要被穿透的那台机器上执行一次，装好会自动出现在这里。
            </div>
          </div>
        ) : (
          agents.map((a) => {
            const svcs = a.services ?? []
            const shown = svcs.slice(0, 8)
            const rest = svcs.length - shown.length
            const st = agentState(a)
            return (
              <div key={a.id} className="rounded-xl border border-border bg-card px-4.5 py-4">
                {/* 窄屏把操作按钮换到单独一行：原来名称、状态徽章和「卸载」挤在同一行，
                    393px 手机上一排小按钮彼此只隔 6px，误触「卸载」的代价是整个客户端被删掉。 */}
                <div className="flex items-center gap-[9px] text-[14.5px] font-[650] max-[820px]:flex-wrap">
                  <Led tone={st.led} />
                  {a.name}
                  <span className="ml-auto">
                    <Badge tone={st.tone}>{st.label}</Badge>
                  </span>
                  <div className="ml-2 flex items-center gap-1.5 max-[820px]:ml-0 max-[820px]:w-full max-[820px]:justify-end max-[820px]:gap-2 max-[820px]:pt-1">
                    {/* 云端卸载：真正删那台机器上的东西，务必与「移除记录」区分开。
                        已经确认卸载掉的（机器上没 agent 了）不再给这个按钮：
                        点了只会把指令存起来等一台不会再上线的机器。
                        正在卸载途中、以及指令已排队时同样不给 —— 前者会让清理脚本重跑，
                        后者本来就有指令在队列里，只留「撤销卸载」一个出口。 */}
                    {!st.gone && st.label !== '正在卸载' && !a.uninstall_requested && (
                      <Button
                        variant="danger"
                        size="sm"
                        className="touch-copy"
                        onClick={() => void uninstallAgent(a.id, a.name, a.online)}
                      >
                        {st.label === '卸载未生效' ? '重新卸载' : '卸载'}
                      </Button>
                    )}
                    {/* 排队中的指令可以撤销：离线点错了否则只能等它开机把自己删掉 */}
                    {a.uninstall_requested && (
                      <Button
                        size="sm"
                        className="touch-copy"
                        onClick={() => void cancelUninstall(a.id, a.name)}
                      >
                        撤销卸载
                      </Button>
                    )}
                    {/* 日志：诊断用。面板读不到客户端本地文件，只能请它回传 —— 
                        客户端离线时请求会留着，等它上线第一次心跳就回传。 */}
                    <Button
                      variant="ghost"
                      size="sm"
                      className="touch-copy"
                      onClick={() => void openLog(a)}
                    >
                      日志
                    </Button>
                    <Button
                      variant={st.gone ? 'primary' : 'ghost'}
                      size="sm"
                      className="touch-copy"
                      onClick={() => void remove(a.id, a.name, st.gone)}
                    >
                      {st.gone ? '清掉记录' : '移除'}
                    </Button>
                  </div>
                </div>

                {st.note && (
                  <div
                    className={cx(
                      'mt-2 rounded-lg border px-3 py-2 text-[12.5px] leading-[1.65]',
                      st.label === '卸载未生效'
                        ? 'border-bad/40 bg-bad/10 text-fg'
                        : st.label === '已卸载'
                          ? 'border-border bg-card2 text-muted'
                          : 'border-warn/40 bg-warn/10 text-fg',
                    )}
                  >
                    {st.note}
                  </div>
                )}

                <div className="mt-2 grid grid-cols-[auto_1fr] gap-x-2.5 gap-y-1 text-xs text-muted">
                  <b className="font-medium text-dim">系统</b>
                  <span>
                    {a.os} / {a.arch}
                  </span>
                  <b className="font-medium text-dim">agent 版本</b>
                  <span className="font-mono">
                    {a.agent_version ? (
                      a.agent_version
                    ) : (
                      <span className="text-warn">
                        旧版（未上报版本）
                        <span className="ml-1 font-sans text-dim">
                          —— 该客户端不支持自动更新，需重跑一次安装命令换成新版
                        </span>
                      </span>
                    )}
                  </span>
                  <b className="font-medium text-dim">frpc 版本</b>
                  <span className="font-mono">{a.frp_version || '—'}</span>
                  <b className="font-medium text-dim">二进制来源</b>
                  <span>
                    {a.bin_source ? (
                      <>
                        {binSourceLabel(a.bin_source)}
                        {a.bin_note ? (
                          <span
                            className="ml-1 text-warn-fg"
                            title={a.bin_note}
                          >
                            （已回退，悬停看原因）
                          </span>
                        ) : null}
                      </>
                    ) : (
                      <span className="text-dim">—（旧版 agent 未上报）</span>
                    )}
                  </span>
                  <b className="font-medium text-dim">内网 IP</b>
                  <span className="font-mono">{(a.ips ?? []).join(', ') || '—'}</span>
                  <b className="font-medium text-dim">最后心跳</b>
                  <span>{fmtAgo(a.last_seen)}</span>
                </div>

                <div className="mt-3">
                  <label className="mb-1 block text-[12.5px] text-muted">
                    本机探测到的服务（{svcs.length}）{rest > 0 ? '，仅显示前 8 个' : ''}
                  </label>
                  <div className="flex flex-wrap gap-2">
                    {shown.length === 0 ? (
                      <span className={cx('text-xs text-dim')}>
                        未上报本机服务（客户端离线，或该机器上确实没有监听端口）
                      </span>
                    ) : (
                      shown.map((s) => (
                        <Chip key={s.addr} static sub={s.addr}>
                          {s.name}
                        </Chip>
                      ))
                    )}
                    {rest > 0 && (
                      <Chip static sub="建隧道时可全部选">
                        还有 {rest} 个
                      </Chip>
                    )}
                  </div>
                </div>
              </div>
            )
          })
        )}
      </div>

      {/* 日志弹窗：面板侧只留最近一次快照，所以"重新拉取"＝再请它传一次。 */}
      <Modal
        open={!!logFor}
        wide
        title={`客户端日志${logFor ? ` · ${logFor.name}` : ''}`}
        onClose={() => {
          setLogFor(null)
          setLogReq('')
        }}
        footer={
          <>
            <Button
              variant="ghost"
              size="sm"
              disabled={!log.content}
              onClick={async () => {
                const r = await copyWithHint(log.content, logPreRef.current, '已复制日志')
                toast(r.msg, r.kind)
              }}
            >
              复制
            </Button>
            <div className="flex-1" />
            <Button size="sm" variant="ghost" onClick={() => logFor && void openLog(logFor)}>
              重新拉取
            </Button>
            <Button
              size="sm"
              onClick={() => {
                setLogFor(null)
                setLogReq('')
              }}
            >
              关闭
            </Button>
          </>
        }
      >
        {log.error ? (
          <Alert tone="bad">
            <div className="whitespace-pre-wrap">{log.error}</div>
          </Alert>
        ) : (
          <>
            <div className="mb-3 flex flex-wrap items-center gap-2 text-xs text-dim">
              {log.waiting ? (
                <>
                  <span className="inline-block h-2 w-2 animate-pulse rounded-full bg-accent" />
                  正在请客户端回传…（最长约 {LOG_WAIT_SEC} 秒，已等 {log.elapsed}s）
                </>
              ) : log.offline ? (
                // 离线是与"等待"互斥的独立状态：请求已经排在面板里，
                // 等它上线才会执行。这里不能转圈，否则用户会以为面板卡住了。
                <span className="text-warn-fg">
                  这台客户端<b>当前离线</b>，请求已存进面板 —— 它下次上线的心跳会立刻执行。
                  {log.content ? (
                    <>
                      {' '}
                      下面显示的是<b>上一次回传（{fmtAgo(log.fetchedAt)}）</b>的内容，
                      <b>不是</b>本次请求的结果。
                    </>
                  ) : (
                    <> 现在没有可显示的历史内容。</>
                  )}
                </span>
              ) : log.answered ? (
                <>
                  已于 {fmtAgo(log.fetchedAt)} 回传 · {fmtBytes(new Blob([log.content]).size)}
                  <span className="text-dim">
                    · 面板只保留最近一次，内容来自那台机器上的 agent.log
                  </span>
                </>
              ) : (
                <span className="text-warn-fg">
                  等了 {log.elapsed} 秒还没收到回传：客户端可能刚离线，或它的 agent
                  版本低于支持回传日志的版本。可以直接到那台机器上看 agent.log。
                  {log.content && (
                    <>
                      {' '}
                      下面显示的是<b>上一次回传（{fmtAgo(log.fetchedAt)}）</b>的内容，
                      <b>不是</b>本次请求的结果。
                    </>
                  )}
                </span>
              )}
            </div>
            <Pre ref={logPreRef} selectAll className="max-h-[58vh] overflow-y-auto">
              {log.content || '（还没有内容）'}
            </Pre>
          </>
        )}
      </Modal>
    </>
  )
}
