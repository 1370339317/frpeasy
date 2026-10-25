import { useState } from 'react'
import { useApp, type CommandKind } from '../store'
import { api } from '../lib/api'
import { copyText, fmtAgo } from '../lib/format'
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

const TABS: { key: CommandKind; label: string }[] = [
  { key: 'linux', label: 'Linux' },
  { key: 'win', label: 'Windows' },
  { key: 'android', label: 'Android · adb' },
  { key: 'androidRoot', label: 'Android · root' },
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
      手机/车机连上 adb 后执行（无需 root）。安装到{' '}
      <span className="font-mono">/data/local/tmp/frpeasy-agent</span>
      ，Android 没有 systemd，靠自带看护脚本保活。设备上通常没有 curl，命令里已带 toybox wget 兜底。
    </>
  ),
  androidRoot: (
    <>
      设备已 root（su 可用）时用这条：会额外把启动脚本挂到{' '}
      <span className="font-mono">/data/adb/service.d</span>（Magisk），重启后自动拉起。
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

export function Clients() {
  const { agents, install, uninstall, installBase, panelPort, accessHost, loadAll, toast } = useApp()
  const [kind, setKind] = useState<CommandKind>('linux')

  const cmd = install[kind] || '加载中…'
  const unCmd = uninstall[kind] || '加载中…'

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

  async function remove(id: string, name: string) {
    if (
      !confirm(
        `仅移除面板里的记录「${name}」？\n\n这只删面板这一条记录，并把它的隧道解绑为草稿；` +
          `那台机器上的 agent 仍在运行、仍会继续连面板（下次心跳会重新注册回来）。\n\n` +
          `要真正卸载那台机器，请用上面的「卸载命令」或卡片上的「卸载」。`,
      )
    )
      return
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
        <Pre>{cmd}</Pre>
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
          {kind === 'android' || kind === 'androidRoot' ? (
            <>
              {' '}
              Android 卸载同样<b>不会</b>注册或删除任何自启，只是把{' '}
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
            return (
              <div key={a.id} className="rounded-xl border border-border bg-card px-4.5 py-4">
                <div className="flex items-center gap-[9px] text-[14.5px] font-[650]">
                  <Led tone={a.online ? 'ok' : 'bad'} />
                  {a.name}
                  <span className="ml-auto">
                    <Badge tone={a.online ? 'ok' : 'gray'}>{a.online ? '在线' : '离线'}</Badge>
                  </span>
                  <div className="ml-2 flex items-center gap-1.5">
                    {/* 云端卸载：真正删那台机器上的东西，务必与「移除记录」区分开 */}
                    <Button
                      variant="danger"
                      size="sm"
                      onClick={() => void uninstallAgent(a.id, a.name, a.online)}
                    >
                      卸载
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => void remove(a.id, a.name)}>
                      移除
                    </Button>
                  </div>
                </div>

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
