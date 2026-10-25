import { useCallback, useEffect, useMemo, useState } from 'react'
import { useApp, useFrpsPolling } from '../store'
import { api } from '../lib/api'
import { Badge, Button, Card, Field, Hint, Input, Led, Select, Stats, Stat, Alert } from '../components/ui'
import type { VersionsResponse } from '../types'

const sleep = (ms: number) => new Promise((r) => window.setTimeout(r, ms))

export function Frps() {
  const { frps, versions, loadFrps, loadVersions, loadTunnels, toast } = useApp()
  const [log, setLog] = useState('')
  const [manualVer, setManualVer] = useState('')
  const [selVer, setSelVer] = useState('latest')
  const [busy, setBusy] = useState(false)

  const frpsOn = !!frps?.running && !!frps.port_alive
  const tone = frps?.starting ? 'warn' : frpsOn ? 'ok' : 'bad'

  // 版本下拉：在线列表 + 当前锁定值（锁定值可能不在列表里，比如网络拉不到时）
  const versionOptions = useMemo(() => {
    const list = [...(versions?.available_versions ?? [])]
    const t = versions?.target_version
    if (t && t !== 'latest' && !list.includes(t)) list.unshift(t)
    return list
  }, [versions])

  useEffect(() => {
    if (versions?.target_version) setSelVer(versions.target_version)
  }, [versions?.target_version])

  const loadLog = useCallback(async () => {
    try {
      const d = await api<{ log: string }>('GET', '/api/frps/log?bytes=8192')
      setLog(d.log || '（暂无日志）')
    } catch {
      /* 日志拉不到不影响主流程 */
    }
  }, [])

  useEffect(() => {
    void loadLog()
    void loadVersions()
  }, [loadLog, loadVersions])

  // 启动/换版本时 frps 要先下载再起进程，状态会连续变化，必须盯着
  useFrpsPolling(true, () => void loadLog())

  /** 启动后盯一段时间，直到已绑定的隧道全部回到在线。只拉一次往往还是 offline。 */
  const waitReconnect = useCallback(
    async (limit = 14) => {
      const bound = (await loadTunnels()).filter((t) => t.agent_id)
      if (!bound.length) return true
      for (let i = 0; i < limit; i++) {
        await sleep(1500)
        const now = (await loadTunnels()).filter((t) => t.agent_id)
        if (now.length && now.every((t) => t.proxy_state === 'online')) return true
      }
      return false
    },
    [loadTunnels],
  )

  async function act(action: 'start' | 'stop' | 'restart') {
    if (busy) return
    if (action === 'restart' && !confirm('重启 frps 会短暂中断所有隧道；若版本有变还会重新下载二进制。确认继续？'))
      return
    if (action === 'stop' && !confirm('停止 frps 后，所有隧道会立刻断开（客户端会保持在线并自动重连）。确认停止？'))
      return

    setBusy(true)
    const verb = { start: '启动', stop: '停止', restart: '重启' }[action]
    try {
      toast(`正在${verb} frps…`)
      await api('POST', `/api/frps?action=${action}`)
      await Promise.all([loadFrps(), loadLog(), loadTunnels()])

      if (action === 'stop') {
        toast('frps 已停止，隧道已断开；重新启动后客户端会在数秒内自动重连')
        return
      }
      toast(`frps 已${verb}，等待客户端重连…`)
      const ok = await waitReconnect()
      toast(
        ok ? '隧道已全部重新连接' : 'frps 已启动，但隧道尚未全部恢复，请看状态或日志',
        ok ? 'ok' : 'bad',
      )
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setBusy(false)
    }
  }

  async function applyVersion(force?: string) {
    const v = force || manualVer.trim() || selVer
    if (!v) {
      toast('请选择或填写版本', 'bad')
      return
    }
    if (!confirm(`切换到 ${v} 会重新拉取 frps 二进制（可能需要几分钟），期间隧道会中断。确认继续？`))
      return
    setBusy(true)
    try {
      toast('正在保存并切换版本…')
      await api('POST', '/api/settings', { frp_version: v })
      await api('POST', '/api/frps?action=restart')
      await Promise.all([loadFrps(), loadVersions(), loadLog(), loadTunnels()])
      setManualVer('')
      toast(`已切换到 ${v}，二进制下载/启动中`)
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setBusy(false)
    }
  }

  const v = versions as VersionsResponse | null
  const cur = v?.current_frps_version || '未安装'

  return (
    <>
      <Card
        title="运行状态"
        actions={
          <Badge tone={tone}>
            <Led tone={tone} />
            {frps?.starting
              ? '拉取/启动中'
              : frpsOn
                ? '运行中'
                : frps?.running
                  ? '已启动但端口未通'
                  : '已停止'}
          </Badge>
        }
      >
        <Stats>
          <Stat
            label="进程"
            value={frps?.starting ? '拉取中' : frpsOn ? '运行中' : frps?.running ? '已启动' : '已停止'}
            sub={frps?.port_alive ? '端口探活通过' : frps?.running ? '端口探活失败' : '进程未运行'}
          />
          <Stat
            label="frps 端口"
            value={frps?.bind_port ?? '—'}
            sub={frps?.port_alive ? '可连接' : '未监听'}
          />
		  <Stat
			label="已装版本"
			value={frps?.current_version || '未安装'}
			sub={
			  frps?.target_version && frps.target_version !== 'latest'
				? `锁定 ${frps.target_version}`
				: '自动：首次装定后不再自动变'
			}
		  />
          <Stat
            label="状态来源"
            value={frpsOn && frps?.dashboard_ok ? 'dashboard' : frpsOn ? '端口探活' : '—'}
            sub={
              frpsOn && frps?.dashboard_ok
                ? `127.0.0.1:${frps.dashboard_port} · 权威状态`
                : frpsOn
                  ? `dashboard 不可用${frps?.dashboard_error ? `：${frps.dashboard_error}` : ''}`
                  : 'frps 未运行'
            }
          />
        </Stats>

        <div className="mt-4 flex flex-wrap items-center gap-2.5">
          <Button size="sm" onClick={() => void act('start')} disabled={busy}>
            启动
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void act('stop')} disabled={busy}>
            停止
          </Button>
          <Button variant="ghost" size="sm" onClick={() => void act('restart')} disabled={busy}>
            重启
          </Button>
          <span className="text-xs text-dim">
            首次启动 / 换版本会自动下载 frps 二进制，可能要等一会儿。
          </span>
        </div>

        <Hint>
          在线判定以 frps 自身的代理状态为准（通过只监听 127.0.0.1 的 dashboard 读取），不是靠客户端猜；dashboard
          不可用时才回退到「公网端口探活」。
        </Hint>
        <Hint>
          停 / 启 frps 只会影响服务端：客户端上的 frpc 会保持运行并自动重连，启动后通常在 1～3
          秒内把所有隧道接回来（面板会盯着状态直到恢复）。
        </Hint>
      </Card>

      <Card title="版本管理" desc="同时决定服务端 frps 与客户端 frpc 的版本">
        <div className="flex flex-wrap items-end gap-3.5">
          <Field label="目标版本" className="mb-0 min-w-[220px] flex-1">
            <Select value={selVer} onChange={(e) => setSelVer(e.target.value)}>
              {versionOptions.length === 0 && <option value="latest">latest</option>}
              {versionOptions.map((x) => (
                <option key={x} value={x}>
                  {x}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="手填版本" className="mb-0 w-[140px]">
            <Input
              value={manualVer}
              onChange={(e) => setManualVer(e.target.value)}
              placeholder="如 v0.61.1"
            />
          </Field>
          <Button onClick={() => void applyVersion()} disabled={busy}>
            应用版本
          </Button>
        </div>

        <Hint>
          策略：
          {v?.pinned ? `锁定 ${v.target_version}` : '跟随最新（装好后不会自动升级，只有你点了才换）'}。
          当前已装：{cur}。
          {versionOptions.length === 0 && ' 未能拉到在线版本列表（网络或镜像不可用），可在「手填版本」里直接写。'}
        </Hint>

        {v?.update_available && (
          <Alert tone="info">
            有新版本 <b>{v.latest_version}</b>（当前 {cur}）。
            <Button size="sm" className="ml-2" onClick={() => void applyVersion(v.latest_version)}>
              升级到 {v.latest_version}
            </Button>
          </Alert>
        )}
      </Card>

      <Card
        title="frps 日志"
        desc="末尾若干行，用于排障"
        actions={
          <Button variant="ghost" size="sm" onClick={() => void loadLog()}>
            刷新
          </Button>
        }
      >
        <div className="max-h-[300px] overflow-auto rounded-[10px] border border-border bg-logbg px-3.5 py-3 font-mono text-xs leading-[1.5] break-all whitespace-pre-wrap text-logfg">
          {log || '—'}
        </div>
      </Card>
    </>
  )
}
