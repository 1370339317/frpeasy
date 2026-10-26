import { useEffect, useState } from 'react'
import { useApp, type CommandKind } from '../store'
import { api } from '../lib/api'
import { copyText, fmtAgo } from '../lib/format'
import type { Agent } from '../types'
import {
  Alert,
  Button,
  Card,
  Chip,
  Led,
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

/** 二进制来源。与 shared/binsource.go 的 BinSource* 常量一一对应。 */
type BinSource = 'auto' | 'git'

/** 把设置里的来源链说成人话，用在安装区与客户端卡片上。 */
function chainLabel(chain: string | undefined): string {
  switch (chain) {
    case 'git':
      return '仅 GitHub'
    case 'panel,git':
      return '面板中继 → GitHub'
    case 'git,panel':
      return 'GitHub → 面板中继'
    default:
      return '仅面板中继'
  }
}

/**
 * 来源切换。
 *
 * 只有两个选项，因为实际上只有两条路：经面板转发，或这台机器自己去上游取。
 * 没有"自动"——自动需要在两条都慢的时候赌一把，而用户比程序更清楚这台机器的处境
 * （内网？出网正常？）。选完写进命令，一条命令就定死了走哪条。
 */
function SourcePicker({
  value,
  onChange,
  gitAvailable,
  gitWarn,
}: {
  value: BinSource
  onChange: (v: BinSource) => void
  gitAvailable: boolean
  /** 非空表示「能用但不推荐」，悬停时优先展示这句话 */
  gitWarn: string
}) {
  const items: { key: BinSource; label: string; hint: string; disabled: boolean }[] = [
    {
      key: 'auto',
      label: '自动',
      // 自动 = 跟随面板设置里的顺序。把当前顺序直接写在提示里，
      // 否则"自动"对用户来说等于黑箱。
      hint: '跟随面板设置里的取二进制顺序，改设置下一轮心跳即生效',
      disabled: false,
    },
    {
      key: 'git',
      label: 'GitHub',
      hint: !gitAvailable
        ? '还没配更新源，暂不可用'
        : gitWarn || '钉死：客户端自己去 GitHub Release 取，失败不回退',
      disabled: !gitAvailable,
    },
  ]
  return (
    <div className="inline-flex flex-wrap gap-[3px] rounded-[9px] border border-border bg-chip p-[3px]">
      {items.map((it) => (
        <button
          key={it.key}
          type="button"
          title={it.hint}
          disabled={it.disabled}
          onClick={() => onChange(it.key)}
          className={cx(
            'cursor-pointer rounded-[7px] border-none px-[13px] py-1 text-[12.5px] transition',
            value === it.key ? 'bg-card2 font-semibold text-fg' : 'bg-transparent text-dim',
            it.disabled && 'cursor-not-allowed opacity-45',
          )}
        >
          {it.label}
        </button>
      ))}
    </div>
  )
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
    installGit,
    gitBase,
    gitAvailable,
    gitNote,
    gitWarn,
    settings,
    loadAll,
    loadAgents,
    toast,
  } = useApp()
  const [kind, setKind] = useState<CommandKind>('linux')

  // 二进制来源。
  //   自动（默认）= 跟随面板设置里的顺序，改设置下一轮心跳就生效；
  //   Git        = 钉死在更新源，失败也不回退。
  //
  // 只换"从哪儿取文件"，版本仍由面板决定。选择存在本地，下次进来还是它。
  const [source, setSource] = useState<BinSource>(() => {
    try {
      return localStorage.getItem('frpeasy.binSource') === 'git' ? 'git' : 'auto'
    } catch {
      return 'auto'
    }
  })
  useEffect(() => {
    try {
      localStorage.setItem('frpeasy.binSource', source)
    } catch {
      /* 隐私模式下 localStorage 可能不可写，不影响功能 */
    }
  }, [source])
  // 面板没配更新源时根本没有第二个地方可取二进制，强制回到面板源，
  // 否则会显示一条地址为空、执行必然失败的 Git 命令。
  useEffect(() => {
    if (!gitAvailable && source === 'git') setSource('auto')
  }, [gitAvailable, source])

  const usingGit = source === 'git' && gitAvailable
  const cmd = (usingGit ? installGit[kind] : install[kind]) || '加载中…'
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
    const msg = online
      ? `让「${name}」自己卸载 frpeasy agent？\n\n` +
        `它会在下一次心跳（约 15 秒内）停掉 frpc、摘掉开机自启、删除程序和配置目录，然后从面板上离线。\n\n` +
        `这台机器以后要重新执行安装命令才能再接入。若它当前不是以 root / 管理员运行，会卸载失败并继续在线。`
      : `「${name}」当前离线。\n\n卸载指令会先存下来，等它下次上线心跳时立即执行。确定下发吗？`
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
        `要真正卸载那台机器，请用上面的「卸载命令」或卡片上的「卸载」。`
    if (!confirm(msg)) return
    try {
      await api('DELETE', `/api/agents/${encodeURIComponent(id)}`)
      toast('已移除客户端记录')
      await loadAll()
    } catch (e) {
      toast(`移除失败：${(e as Error).message}`, 'bad')
    }
  }

  return (
    <>
      <Card
        title="安装客户端"
        desc="在需要被穿透的那台机器上执行一次；重跑即升级 / 修复"
        actions={<Tabs value={kind} onChange={setKind} items={TABS} />}
      >
        <div className="mb-2 flex flex-wrap items-center gap-2">
          <span className="text-xs text-dim">客户端从哪儿取</span>
          <SourcePicker
            value={source}
            onChange={setSource}
            gitAvailable={gitAvailable}
            gitWarn={gitWarn}
          />
          <span className="text-xs text-dim">
            {usingGit ? (
              <>
                钉死 GitHub：地址直指 <span className="font-mono">{gitBase || '更新源'}</span>
                （已套用镜像前缀），不经过面板转发、失败也不回退
              </>
            ) : (
              <>
                跟随面板设置（当前：<b>{chainLabel(settings?.bin_source_chain)}</b>），
                改设置后下一轮心跳生效，不用重装
              </>
            )}
          </span>
        </div>
        <Pre>{cmd}</Pre>
        {usingGit && gitWarn ? (
          // 选了 Git 但这套配置下它并不快（最常见的是没配镜像前缀，等于直连 GitHub）。
          // 必须在这里说，否则用户会以为"换了条路应该更快"，实际更卡却不知道为什么。
          <div className="mt-2 text-xs leading-[1.6] text-warn-fg">{gitWarn}</div>
        ) : usingGit ? (
          <div className="mt-2 text-xs leading-[1.6] text-dim">
            选了 GitHub：这台机器必须能出网访问上面的地址。agent 与 frpc 都从那里取，
            版本仍由面板决定；<b>这个选择是钉死的，失败也不会自动改走面板</b>。
            内网机器拉不动时，换回「自动」重新复制一条命令重装即可。
          </div>
        ) : !gitAvailable && gitNote ? (
          <div className="mt-2 text-xs leading-[1.6] text-dim">{gitNote}</div>
        ) : null}
        <div className="mt-2 text-xs leading-[1.6] text-dim">{HINTS[kind]}</div>
        <PortWarn base={installBase} port={panelPort} accessHost={accessHost} />
        <div className="mt-3 flex flex-wrap items-center gap-3">
          <Button
            size="sm"
            onClick={async () =>
              toast((await copyText(cmd)) ? '已复制安装命令' : '复制失败，请手动选中', 'ok')
            }
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
        <Pre>{unCmd}</Pre>
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
            onClick={async () =>
              toast((await copyText(unCmd)) ? '已复制卸载命令' : '复制失败，请手动选中', 'ok')
            }
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
                <div className="flex items-center gap-[9px] text-[14.5px] font-[650]">
                  <Led tone={st.led} />
                  {a.name}
                  <span className="ml-auto">
                    <Badge tone={st.tone}>{st.label}</Badge>
                  </span>
                  <div className="ml-2 flex items-center gap-1.5">
                    {/* 云端卸载：真正删那台机器上的东西，务必与「移除记录」区分开。
                        已经确认卸载掉的（机器上没 agent 了）不再给这个按钮：
                        点了只会把指令存起来等一台不会再上线的机器。
                        正在卸载途中、以及指令已排队时同样不给 —— 前者会让清理脚本重跑，
                        后者本来就有指令在队列里，只留「撤销卸载」一个出口。 */}
                    {!st.gone && st.label !== '正在卸载' && !a.uninstall_requested && (
                      <Button
                        variant="danger"
                        size="sm"
                        onClick={() => void uninstallAgent(a.id, a.name, a.online)}
                      >
                        {st.label === '卸载未生效' ? '重新卸载' : '卸载'}
                      </Button>
                    )}
                    {/* 排队中的指令可以撤销：离线点错了否则只能等它开机把自己删掉 */}
                    {a.uninstall_requested && (
                      <Button size="sm" onClick={() => void cancelUninstall(a.id, a.name)}>
                        撤销卸载
                      </Button>
                    )}
                    <Button
                      variant={st.gone ? 'primary' : 'ghost'}
                      size="sm"
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
                        {a.bin_source === 'git' ? 'GitHub' : '面板中继'}
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
    </>
  )
}
