import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import { fmtBytes } from '../lib/format'
import { Badge, Button, Card, Field, Hint, Input } from '../components/ui'
import { useApp } from '../store'
import type { PanelUpdateStart, VersionResponse } from '../types'

/**
 * 版本与更新。
 *
 * 三层版本各管各的，界面上必须分开摆，因为它们更新的代价完全不同：
 *   - 面板：会重启，所有会话断开、frps 短暂中断（秒级），所以要点确认；
 *   - 客户端：客户端自己下载替换并重启，隧道中断 1-3 秒，可以自动；
 *   - frp：要重启 frps，所有隧道全断，只提示不自动做。
 *
 * 混成一个"一键更新"是很诱人的，但那等于让用户在一次点击里承担三种未知后果。
 */

const STAGE_TEXT: Record<string, string> = {
  downloading: '下载中',
  swapping: '正在替换并重启',
  starting: '启动新版本',
  'health-check': '健康检查中',
  succeeded: '更新成功',
  'rolled-back': '已回滚到旧版本',
  failed: '更新失败',
}

const STAGE_TONE: Record<string, 'ok' | 'bad' | 'warn' | 'info'> = {
  succeeded: 'ok',
  'rolled-back': 'warn',
  failed: 'bad',
  downloading: 'info',
  swapping: 'info',
  starting: 'info',
  'health-check': 'info',
}

export function VersionCard() {
  const { settings, loadSettings, toast } = useApp()
  const [v, setV] = useState<VersionResponse | null>(null)
  const [busy, setBusy] = useState<'' | 'check' | 'update'>('')
  const [src, setSrc] = useState('')
  const [savingSrc, setSavingSrc] = useState(false)
  // updating: 已经点了更新，正在等面板重启
  const [updating, setUpdating] = useState(false)
  const timer = useRef<number | null>(null)

  const load = useCallback(
    async (force = false) => {
      try {
        const r = await api<VersionResponse>(
          force ? 'POST' : 'GET',
          force ? '/api/version/check' : '/api/version',
        )
        setV(r)
        return r
      } catch (e) {
        // 更新期间面板会短暂不可用，这不是错误，交给轮询处理
        if (!updating) toast((e as Error).message, 'bad')
        return null
      }
    },
    [toast, updating],
  )

  useEffect(() => {
    void load(false)
    // 版本与文件状态会自己变：后台把缓存刷成新版本、客户端陆续升级完成、
    // 上游又发了新版。原来只在挂载时取一次，用户停在这一页等升级时
    // 看不到任何变化，只能手动刷新。load(false) 走已缓存的清单，不出网，很轻。
    const h = window.setInterval(() => {
      if (document.visibilityState === 'visible') void load(false)
    }, 10000)
    return () => window.clearInterval(h)
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // 只在第一次拿到设置时回填。
  //
  // 依赖写成 [settings] 是不够的：store 每次 loadSettings 都会放一个新对象进来，
  // 依赖每次都变，于是用户正在输入更新源地址时会被冲回库里的旧值 ——
  // 和设置页那个坑一模一样（那次导致「填了没生效 / 配置像是丢了」）。
  const srcHydrated = useRef(false)
  useEffect(() => {
    if (!settings || srcHydrated.current) return
    srcHydrated.current = true
    setSrc(settings.update_base ?? '')
  }, [settings])

  // 更新期间轮询：先探 healthz（不需要登录，面板一活就能应答），
  // 恢复后再拉一次版本，确认到底成功了还是回滚了。
  useEffect(() => {
    if (!updating) return
    let alive = 0
    const tick = async () => {
      try {
        const r = await fetch('./api/healthz', { cache: 'no-store' })
        if (r.ok) alive++
      } catch {
        alive = 0
      }
      if (alive >= 2) {
        const r = await load(false)
        if (r && r.last_update) {
          const stage = r.last_update.stage
          if (stage === 'succeeded' || stage === 'rolled-back' || stage === 'failed') {
            setUpdating(false)
            if (stage === 'succeeded') {
              toast(`面板已更新到 ${r.last_update.to}，正在重新加载页面…`, 'ok')
              // 必须重载页面。
              //
              // 面板换了二进制不会让已打开的标签页换代码：内存里跑的还是更新前
              // 那份 JS，只有重新拉 index.html 才会换上新的。不刷新就会出现
              // 「提示更新成功，但左侧没有新菜单、点了没反应」—— 用户会以为
              // 功能没做。这里等一秒让提示出来再刷，hash 路由会保留。
              window.setTimeout(() => window.location.reload(), 1200)
              return
            }
            if (stage === 'rolled-back') toast('新版本未通过健康检查，已自动回滚', 'bad')
            else toast(`更新失败：${r.last_update.error}`, 'bad')
          }
        }
      } else {
        // 面板还没回来，只刷新状态文字（state 是落盘的，读不到就沿用旧值）
        void load(false)
      }
      timer.current = window.setTimeout(tick, 2000)
    }
    timer.current = window.setTimeout(tick, 1200)
    return () => {
      if (timer.current) window.clearTimeout(timer.current)
    }
  }, [updating, load, toast])

  async function check() {
    if (busy) return
    setBusy('check')
    try {
      const r = await load(true)
      if (r) {
        if (r.update_error) toast(`检查失败：${r.update_error}`, 'bad')
        else if (!r.update_source) toast('尚未配置更新源，无法检查面板自身更新', 'ok')
        else if (r.panel.update_available) toast(`发现新版本 ${r.panel.latest}`, 'ok')
        else toast('面板已是最新版本')
      }
    } finally {
      setBusy('')
    }
  }

  async function doUpdate() {
    if (busy || updating) return
    const to = v?.panel.latest || ''
    if (
      !window.confirm(
        `将面板更新到 ${to}。\n\n` +
          '过程中面板会重启：当前页面会断开，几秒后自动重连；frps 与所有隧道会短暂中断。\n' +
          `如果新版本启动失败，会自动回滚到 ${v?.panel.current}。\n\n继续？`,
      )
    )
      return
    setBusy('update')
    try {
      const r = await api<PanelUpdateStart>('POST', '/api/panel/update')
      if (r.started) {
        setUpdating(true)
        toast(r.message, 'ok')
      }
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setBusy('')
    }
  }

  async function saveSource() {
    if (savingSrc) return
    setSavingSrc(true)
    try {
      await api('POST', '/api/settings', { update_base: src.trim() })
      srcHydrated.current = false // 允许回填，显示服务端规范化后的值
      await loadSettings()
      const r = await load(true)
      if (r) toast(r.update_error ? `更新源已保存，但拉取失败：${r.update_error}` : '更新源已保存', r.update_error ? 'bad' : 'ok')
    } catch (e) {
      toast((e as Error).message, 'bad')
    } finally {
      setSavingSrc(false)
    }
  }

  async function toggleAutoUpdate(next: boolean) {
    try {
      await api('POST', '/api/settings', { agent_auto_update: next ? '1' : '0' })
      await loadSettings()
      await load(false)
      toast(next ? '客户端会自动跟随面板自带版本升级' : '已停止自动升级客户端', next ? 'ok' : 'bad')
    } catch (e) {
      toast((e as Error).message, 'bad')
    }
  }

  const p = v?.panel
  const a = v?.agent
  const f = v?.frp
  const last = v?.last_update
  const canUpdate = !!p?.update_available && !!p?.can_update && !!p?.exe_writable

  return (
    <Card
      title="版本与更新"
      desc="面板 / 客户端 / frp 三层各自独立"
      actions={
        <Button variant="ghost" size="sm" onClick={check} disabled={busy !== '' || updating}>
          {busy === 'check' ? '检查中…' : '检查更新'}
        </Button>
      }
    >
      <div className="grid gap-3 sm:grid-cols-3">
        {/* 面板 */}
        <VersionBox
          name="面板"
          current={p?.current || '—'}
          sub={p?.build_time && p.build_time !== 'unknown' ? `构建 ${shortTime(p.build_time)}` : '构建时间未注入'}
          badge={
            updating ? (
              <Badge tone={STAGE_TONE[last?.stage || ''] || 'info'}>
                {STAGE_TEXT[last?.stage || ''] || '更新中'}
              </Badge>
            ) : p?.update_available ? (
              <Badge tone="info">有新版本 {p.latest}</Badge>
            ) : p?.latest ? (
              <Badge tone="ok">已是最新</Badge>
            ) : (
              <Badge tone="gray">{v?.update_source ? '未检查' : '未配置更新源'}</Badge>
            )
          }
        />

        {/* 客户端 */}
        <VersionBox
          name="客户端 agent"
          // 显示「客户端实收版本」，而不是「面板本机平台缓存的那一版」。
          // 后者在服务器上通常是 linux/amd64 的一份旧缓存，与在线客户端的
          // 版本毫无关系 —— 拿它当主数字会出现「卡片写 0.7.16、而客户端
          // 0.7.24、落后 0 台」的自相矛盾。
          current={a?.client_version || a?.hosted || '—'}
          sub={
            (a && a.total > 0
              ? `${a.total} 台在线记录，${a.outdated} 台版本落后` +
                (a.legacy > 0 ? `，${a.legacy} 台旧版（不支持自动升级）` : '') +
                (a.missing > 0 ? `，${a.missing} 台缺对应平台产物` : '')
              : '还没有客户端接入') +
            // 本机平台缓存与实收版本不一致时，单独说一句，否则「缓存落后」
            // 那条提示会和一个对不上的数字一起出现，看起来像 bug。
            (a?.cache_behind && a?.hosted && a.hosted !== (a.client_version || '')
              ? `；面板本机缓存 ${a.hosted}（会自动重取）`
              : '')
          }
          badge={
            a && a.missing > 0 ? (
              // 缺产物是运维问题（漏编某个平台），必须比"落后"更显眼：
              // 落后的会自动升，缺产物的永远升不了。
              <Badge tone="warn">缺 {a.missing} 台平台产物</Badge>
            ) : a?.error ? (
              // 只有「既无本机产物、又取不到更新源清单」才会走到这里：
              // 此时客户端确实装不上，属于必须修的运维问题。
              <Badge tone="warn">无法下发 agent</Badge>
            ) : a && a.legacy > 0 ? (
              // 老客户端不认识心跳里的 agent_update，点多少次都不会升 ——
              // 这里必须说"需重装"，否则用户会等一个永不到来的升级。
              <Badge tone="warn">{a.legacy} 台旧版待重装</Badge>
            ) : a && a.outdated > 0 && a.auto_update ? (
              <Badge tone="info">将自动升级 {a.outdated} 台</Badge>
            ) : a?.auto_update ? (
              <Badge tone="ok">自动跟随</Badge>
            ) : (
              <Badge tone="gray">已关闭自动升级</Badge>
            )
          }
        />

        {/* frp */}
        <VersionBox
          name="服务端 frp"
          current={f?.current || '未安装'}
          sub={f?.pinned ? `已锁定 ${f.target}` : f?.target === 'latest' ? '自动：首次装定后不再变' : `目标 ${f?.target || '—'}`}
          badge={
            f?.update_available ? (
              <Badge tone="info">上游有 {f.latest}</Badge>
            ) : f?.latest ? (
              <Badge tone="ok">已是最新</Badge>
            ) : (
              <Badge tone="gray">未知</Badge>
            )
          }
        />
      </div>

      {/* 上次更新结果：面板重启后页面会重连，用户回来第一件事就是想知道"成没成" */}
      {last && (
        <div className="mt-3 flex flex-wrap items-center gap-2 rounded-lg border border-border2 bg-input px-3 py-2 text-xs">
          <Badge tone={STAGE_TONE[last.stage] || 'gray'}>{STAGE_TEXT[last.stage] || last.stage}</Badge>
          <span className="text-dim">{last.at ? shortTime(last.at) : ''}</span>
          {last.to && (
            <span className="font-mono text-[11.5px] text-muted">
              {last.from || '?'} → {last.to}
            </span>
          )}
          <span className="text-dim">{last.detail}</span>
          {last.error && <span className="text-bad-fg">{last.error}</span>}
        </div>
      )}

      {/* 面板更新动作 */}
      <div className="mt-3 flex flex-wrap items-center gap-2.5">
        <Button onClick={doUpdate} disabled={!canUpdate || busy !== '' || updating}>
          {updating ? '更新中…' : p?.update_available ? `更新到 ${p.latest}` : '当前已是最新'}
        </Button>
        {p?.update_available && !p?.exe_writable && (
          <span className="text-xs text-bad-fg">
            面板所在目录不可写，无法自更新：{p.exe_path}
          </span>
        )}
        {p?.error && <span className="text-xs text-warn-fg">{p.error}</span>}
      </div>

      {/* 更新源 */}
      <div className="mt-4 border-t border-border2 pt-3.5">
        <Field
          label="更新源（面板自身的新版本从这里取）"
          hint={
            <>
              填一个静态目录，里面放 <code className="font-mono">manifest.json</code> 和各平台二进制。
              用 GitHub Releases 的话填 <code className="font-mono">https://github.com/&lt;owner&gt;/&lt;repo&gt;/releases/latest/download</code>
              —— 这个地址永远指向最新 release，不需要 API、不会被限流。
              客户端不走这里：它只从本面板下载，所以内网也能用。
            </>
          }
        >
          <div className="flex flex-wrap gap-2">
            <Input
              className="min-w-[280px] flex-1"
              value={src}
              onChange={(e) => setSrc(e.target.value)}
              placeholder="留空 = 不检查面板自身更新"
            />
            <Button variant="ghost" onClick={saveSource} disabled={savingSrc}>
              {savingSrc ? '保存中…' : '保存并检查'}
            </Button>
          </div>
        </Field>

        {v?.update_error && (
          <Hint className="text-bad-fg">拉取清单失败：{v.update_error}</Hint>
        )}
        {v?.checked_at && !v.update_error && v.update_source && (
          <Hint>上次检查：{shortTime(v.checked_at)}</Hint>
        )}

        <label className="mt-3 flex cursor-pointer items-center gap-2 text-[13px]">
          <input
            type="checkbox"
            className="size-4 accent-[var(--accent)]"
            checked={!!a?.auto_update}
            onChange={(e) => toggleAutoUpdate(e.target.checked)}
          />
          <span>客户端自动跟随面板自带的 agent 版本升级</span>
        </label>
        <Hint>
          客户端只从本面板下载（<code className="font-mono">/api/agent/bin/…</code>），
          下载后会校验 sha256 并试运行确认版本；失败按包退避 30 分钟，不会反复重试。
          {a?.error ? (
            <span className="text-warn-fg">{a.error}</span>
          ) : a?.hosted ? (
            <>
              当前可下发：<b>{a.hosted}</b>
              {a.hosted_size > 0 ? `（${fmtBytes(a.hosted_size)}）` : ''}
              {a.hosted_note ? `，${a.hosted_note}` : ''}。
            </>
          ) : null}
        </Hint>
        {/* 缓存落后时「客户端升不上去」但 outdated=0，界面上一切正常 ——
            必须单独说一句，否则用户只能自己对比两个版本号猜。 */}
        {a?.cache_behind && (
          <Hint>
            <span className="text-warn-fg">
              面板缓存还是 {a.hosted}，清单已到 {a.latest}。
            </span>{' '}
            有客户端心跳时会自动换成新版本，下一轮心跳再下发升级；不想等就去
            <a
              className="cursor-pointer font-semibold text-accent-fg underline"
              onClick={() => (window.location.hash = '#/files')}
            >
              文件与更新
            </a>
            页点「预存」立刻拉。
          </Hint>
        )}
      </div>
    </Card>
  )
}

function VersionBox({
  name,
  current,
  sub,
  badge,
}: {
  name: string
  current: string
  sub: string
  badge: React.ReactNode
}) {
  return (
    <div className="rounded-lg border border-border2 bg-input px-3.5 py-3">
      <div className="flex items-center gap-2">
        <span className="text-xs text-muted">{name}</span>
        <div className="flex-1" />
        {badge}
      </div>
      <div className="mt-1 font-mono text-[17px] font-semibold break-all">{current}</div>
      <div className="mt-0.5 text-[11.5px] text-dim">{sub}</div>
    </div>
  )
}

/** 把 RFC3339 压成 "10-25 13:20"（面板上不需要秒和时区）。 */
function shortTime(s: string): string {
  const d = new Date(s)
  if (Number.isNaN(d.getTime())) return s
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}
