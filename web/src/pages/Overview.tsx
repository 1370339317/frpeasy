import { useApp } from '../store'
import { copyText } from '../lib/format'
import { Card, CopyRow, Led, Stats, Stat } from '../components/ui'

export function Overview() {
  const { frps, agents, tunnels, versions, accessHost, settings, accessSource, toast } = useApp()

  const frpsOn = !!frps?.running && !!frps.port_alive
  const frpsTone = frps?.starting ? 'warn' : frpsOn ? 'ok' : 'bad'
  const agentsOn = agents.filter((a) => a.online).length
  const tunnelsOn = tunnels.filter((t) => t.proxy_state === 'online').length

  // 后端已按「设置 → 请求 Host → 127.0.0.1」推导好并剥掉端口，直接采信。
  // 不再从 public_addr 原始值自己拼，否则带面板端口时会拼出 host:28080:21001。
  const host =
    accessHost || (settings?.public_addr ?? '').replace(/^https?:\/\//i, '').replace(/[:/].*$/, '')

  const srcText =
    accessSource === 'settings'
      ? '来自「设置 → 公网地址」（已固定）'
      : accessSource === 'request'
        ? '从你当前访问面板的地址推断，未固定 —— 换网络/换端口访问面板时它会变，建议到「设置」里显式填写'
        : '本机回环，只能本机访问；放到公网服务器后请到「设置」填写公网地址'

  return (
    <>
      <Stats>
        <Stat
          label="服务端 frps"
          led={<Led tone={frpsTone} />}
          value={frps?.starting ? '拉取中' : frpsOn ? '运行中' : frps?.running ? '已启动' : '已停止'}
          sub={`frps 端口 ${frps?.bind_port ?? '—'}${frps?.port_alive ? ' · 端口可连' : ''}`}
        />
        <Stat
          label="客户端"
          value={`${agentsOn} / ${agents.length}`}
          sub={agents.length ? `${agentsOn} 台在线` : '还没有客户端，去「客户端」页装一个'}
        />
        <Stat
          label="隧道"
          value={`${tunnelsOn} / ${tunnels.length}`}
          sub={tunnels.length ? `${tunnelsOn} 条在线` : '还没有隧道'}
        />
        <Stat
          label="frp 版本"
          value={frps?.current_version || '未安装'}
          sub={versions?.pinned ? `锁定 ${frps?.target_version ?? ''}` : '跟随最新'}
        />
      </Stats>

      <Card title="隧道公网主机" desc="所有隧道共用它，端口各隧道独立">
        <CopyRow
          value={host}
          onCopy={async () => {
            toast((await copyText(host)) ? '已复制' : '复制失败，请手动选中', host ? 'ok' : 'bad')
          }}
        />
        <div className="mt-1.5 text-xs leading-[1.5] text-dim">
          {srcText}。每条隧道在「隧道」页有完整的{' '}
          <span className="font-mono">主机:公网端口</span> 访问地址。
        </div>
      </Card>

      <Card title="三步跑通" desc="第一次使用按这个顺序来">
        <div className="grid gap-3 [grid-template-columns:repeat(auto-fit,minmax(240px,1fr))]">
          {[
            {
              t: '启动服务端 frps',
              d: '到「服务端 frps」页点启动，首次会自动拉取 frps 二进制（国内建议先配镜像）。',
            },
            {
              t: '在被穿透的机器装客户端',
              d: '到「客户端」页复制一键安装命令，在那台机器上执行；装完自动出现并上报本机服务。',
            },
            {
              t: '建隧道',
              d: '到「隧道」页新建：选机器 → 点它探测到的服务 → 起名。之后改映射、换端口都只在面板上点，客户端不用再动。',
            },
          ].map((s, i) => (
            <div key={s.t} className="rounded-[10px] border border-border bg-card2 px-4 py-3.5">
              <div className="mb-2 inline-flex h-[22px] w-[22px] items-center justify-center rounded-full bg-accent-dim text-[12px] font-bold text-accent-fg">
                {i + 1}
              </div>
              <div className="text-[13.5px] font-[650]">{s.t}</div>
              <div className="mt-1 text-xs leading-[1.5] text-muted">{s.d}</div>
            </div>
          ))}
        </div>
      </Card>
    </>
  )
}
