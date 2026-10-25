import { useCallback, useEffect, useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { fmtBytes } from '../lib/format'
import { Alert, Badge, Button, Card, Empty, Hint, Table, Td, type Tone } from '../components/ui'
import type { FileKind, FilesResponse, HostedFile } from '../types'

const KIND_LABEL: Record<FileKind, string> = {
  frps: '服务端 frp',
  frpc: '客户端 frpc',
  agent: '客户端 agent',
}

const KIND_TONE: Record<FileKind, Tone> = {
  frps: 'info',
  frpc: 'gray',
  agent: 'gray',
}

/**
 * 文件：面板到底存了哪些二进制、占多少、要不要提前备好。
 *
 * 存在的理由有三个，都是实际踩出来的：
 *  1. 面板托管的文件默认按需拉取（谁先用到谁触发），于是「哪份已就绪、
 *     哪份还没有」在界面上完全看不出来 —— 客户端装失败时无从判断；
 *  2. 内网机器连不上 GitHub，需要有人**提前**把对应平台的文件备到面板上；
 *  3. 缓存会一直占着面板磁盘（frpc + agent 六平台约 90MB），得能看见、能删。
 */
export function Files() {
  const { toast } = useApp()
  const [data, setData] = useState<FilesResponse | null>(null)
  const [busy, setBusy] = useState('')
  const [err, setErr] = useState('')

  const load = useCallback(async () => {
    try {
      setData(await api<FilesResponse>('GET', '/api/files'))
      setErr('')
    } catch (e) {
      setErr((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const keyOf = (f: { kind: string; os: string; arch: string }) => `${f.kind}/${f.os}/${f.arch}`

  async function fetchOne(f: HostedFile) {
    const k = keyOf(f)
    setBusy(k)
    try {
      const r = await api<{ ok: boolean; size?: number }>('POST', '/api/files/fetch', {
        kind: f.kind,
        os: f.os,
        arch: f.arch,
      })
      toast(`${KIND_LABEL[f.kind]} ${f.os}/${f.arch} 已就绪${r.size ? `（${fmtBytes(r.size)}）` : ''}`)
      await load()
    } catch (e) {
      // 下载失败的原因千差万别（镜像不通、上游没有该架构、磁盘满），
      // 原样带出来比"操作失败"有用得多
      toast(`预存失败：${(e as Error).message}`, 'bad')
    } finally {
      setBusy('')
    }
  }

  async function remove(f: HostedFile) {
    if (
      !confirm(
        `从面板删除 ${f.name}（${fmtBytes(f.size)}）？\n\n` +
          `不会影响已经装好的客户端，但该平台下次安装/取用时会重新下载。\n` +
          (f.kind === 'frps' ? '服务端 frp 删除后，下次启动会重新拉取。\n' : ''),
      )
    )
      return
    const k = keyOf(f)
    setBusy(k)
    try {
      await api('DELETE', `/api/files?kind=${f.kind}&os=${f.os}&arch=${f.arch}`)
      toast('已删除，腾出 ' + fmtBytes(f.size))
      await load()
    } catch (e) {
      toast(`删除失败：${(e as Error).message}`, 'bad')
    } finally {
      setBusy('')
    }
  }

  async function togglePrefetch(on: boolean) {
    try {
      await api('POST', '/api/settings', { prefetch_all: on ? '1' : '0' })
      toast(on ? '已开启全平台预存，后台开始下载' : '已切回按需拉取')
      await load()
    } catch (e) {
      toast(`保存失败：${(e as Error).message}`, 'bad')
    }
  }

  const items = data?.items ?? []
  const missing = items.filter((i) => !i.present).length

  return (
    <div className="grid gap-3.5">
      {err && <Alert tone="bad">{err}</Alert>}

      <Card
        title="面板托管的文件"
        actions={
          <span className="text-xs text-dim">
            合计 {fmtBytes(data?.used_bytes ?? 0)}
            {missing > 0 ? ` · ${missing} 项未就绪` : ' · 全部就绪'}
          </span>
        }
      >
        <div className="grid gap-3 sm:grid-cols-3">
          <div>
            <div className="text-xs text-dim">服务端 frp 版本</div>
            <div className="mt-0.5 font-mono text-[13.5px]">{data?.frp_version || '—'}</div>
          </div>
          <div>
            <div className="text-xs text-dim">缓存目录</div>
            <div className="mt-0.5 font-mono text-[12.5px] break-all">{data?.bin_dir || '—'}</div>
          </div>
          <div>
            <div className="text-xs text-dim">更新源</div>
            <div className="mt-0.5 font-mono text-[12.5px] break-all">
              {data?.update_base || '未配置（面板/agent 无法自动拉取）'}
            </div>
          </div>
        </div>

        <label className="mt-4 flex cursor-pointer items-start gap-2 text-[13px]">
          <input
            type="checkbox"
            className="mt-0.5 size-4 accent-[var(--accent)]"
            checked={!!data?.prefetch_all}
            onChange={(e) => void togglePrefetch(e.target.checked)}
          />
          <span>
            全平台预存
            <span className="ml-1.5 text-dim">
              （开着就在检查更新后把 frpc + agent 的六个平台全部备好，约 90MB；关着是谁先用到谁拉）
            </span>
          </span>
        </label>

        <Hint>
          客户端只从面板取文件（<code className="font-mono">/api/frpc/bin/…</code>、
          <code className="font-mono">/api/agent/bin/…</code>），取不到才回退直连 GitHub —— 所以内网机器
          只要连得上面板就能装。想给内网准备客户端，先把对应平台「预存」好即可。
        </Hint>
      </Card>

      <Card title="明细">
        {items.length === 0 ? (
          <Empty>读取中…</Empty>
        ) : (
          <Table head={['类型', '平台', '版本', '大小', '状态', '操作']}>
            {items.map((f) => {
              const k = keyOf(f)
              const loading = busy === k
              return (
                <tr key={k}>
                  <Td>
                    <Badge tone={KIND_TONE[f.kind]}>{KIND_LABEL[f.kind]}</Badge>
                  </Td>
                  <Td className="font-mono text-[12.5px]">
                    {f.os}/{f.arch}
                  </Td>
                  <Td className="font-mono text-[12.5px]">{f.version || '—'}</Td>
                  <Td className="text-[12.5px]">{f.present ? fmtBytes(f.size) : '—'}</Td>
                  <Td>
                    {f.present ? (
                      <Badge tone={f.source === 'builtin' ? 'info' : 'ok'}>
                        {f.source === 'builtin' ? '内置' : '已缓存'}
                      </Badge>
                    ) : (
                      <Badge tone="gray">未下载</Badge>
                    )}
                  </Td>
                  <Td align="right">
                    <div className="flex justify-end gap-2">
                      <Button
                        size="sm"
                        variant={f.present ? 'ghost' : 'primary'}
                        disabled={loading}
                        onClick={() => void fetchOne(f)}
                        title={f.present ? '重新拉取到当前版本' : '现在就从更新源拉下来'}
                      >
                        {loading ? '处理中…' : f.present ? '刷新' : '预存'}
                      </Button>
                      <Button
                        size="sm"
                        variant="danger"
                        disabled={loading || !f.can_delete}
                        onClick={() => void remove(f)}
                        title={f.can_delete ? '删除本地缓存' : f.note}
                      >
                        删除
                      </Button>
                    </div>
                  </Td>
                </tr>
              )
            })}
          </Table>
        )}
      </Card>

      <Hint>
        版本号取自缓存文件的元数据（面板自己下载时写入的版本戳），不是猜的。
        「内置」表示它随面板发布包一起部署，删除它没有意义 —— 界面会拦住。
        服务端 frp 正在运行时也不能删：删了会立刻被重新拉取，白白中断隧道。
      </Hint>
    </div>
  )
}
