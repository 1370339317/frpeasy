import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useApp } from '../store'
import { api } from '../lib/api'
import { fmtBytes } from '../lib/format'
import { Button, Card, Field, Hint, Select, cx } from '../components/ui'
import type { TunnelProbeResult } from '../types'

/**
 * 链路体检。
 *
 * 这一页存在的唯一理由：用户说「隧道很卡」，但「卡」有好几种完全不同的原因，
 * 而且处置方式互不相同。不拆开量，就只能靠猜。
 *
 * 按**物理路径**拆成三段（这是用户自己提出、并且是对的模型）：
 *
 *   ③ 我 → 服务器
 *      浏览器直接打面板。这一段只跟你在哪、跨境出口怎么样有关，
 *      调任何 frp 参数都改变不了它。
 *
 *   ① 服务器 → 客户端（走隧道）
 *      面板从本机连 127.0.0.1:公网端口，数据经 frps → frpc → 客户端本地服务。
 *      起点在服务器，所以它**不包含** ③ —— 这正是旧版隧道测速被人误解的地方：
 *      数字好看不代表你访问快。
 *
 *   ② 我 → 隧道 → 客户端服务
 *      浏览器经面板转发进隧道，一次计时覆盖 ③+①。这是**用户真实体感**。
 *
 * 三段放在一起才有诊断价值：
 *   ③ 慢            → 是你的网络/跨境，换节点才有效
 *   ③ 快、① 慢      → 客户端那条链路或 frpc 侧的问题
 *   ③① 都快、② 慢很多 → 隧道转发本身的开销（连接池、tcpMux、并发）
 */

const DOWN_BYTES = 4 << 20
// ② 要额外经过一次面板转发，字节数再小一点，避免在慢链路上把用户挂太久。
const PROXY_BYTES = 2 << 20
// 上行单独用小样本：跨境上行经常只有几十 KB/s，4MB 永远传不完。
const UP_BYTES = 1 << 20
const SEG_TIMEOUT = 25000

type SegState = 'idle' | 'running' | 'done' | 'fail'

/** ③ 的结果。 */
interface BrowserSeg {
  state: SegState
  /** RTT 中位数（毫秒）。单看它会被"看起来正常"骗到，必须配合抖动一起看 */
  rtt: number
  /** 95 分位延迟（毫秒）—— 决定体感的是它，不是中位数 */
  rttP95: number
  /** 明显慢于中位数的请求次数 / 总次数。跨境丢包会直接体现为这个 */
  slowCount: number
  sampleCount: number
  down: number // 字节/秒
  up: number
  /** 上行单独失败的原因；为空表示测到了 */
  upErr: string
  downTtfb: number
  bytes: number
  err: string
}

/** ② 的结果。 */
interface ProxySeg {
  state: SegState
  speed: number
  ttfb: number
  ms: number
  bytes: number
  /** 时间到了还没传完：此时 speed 是"已完成部分"的速率，仍然有效 */
  incomplete: boolean
  err: string
}

const emptyBrowser: BrowserSeg = {
  state: 'idle',
  rtt: 0,
  rttP95: 0,
  slowCount: 0,
  sampleCount: 0,
  down: 0,
  up: 0,
  upErr: '',
  downTtfb: 0,
  bytes: 0,
  err: '',
}
const emptyProxy: ProxySeg = { state: 'idle', speed: 0, ttfb: 0, ms: 0, bytes: 0, incomplete: false, err: '' }

/**
 * 给单段测量套一个超时。
 *
 * 为什么需要：跨境慢链路上一段 8MB 的下载可能要几十秒，而用户看不到任何进展，
 * 只会以为页面卡死。超时后报"超过 N 秒没完成"，比无限转圈诚实。
 * 同时外层"停止"按钮仍能立刻中断（两个信号合流）。
 */
async function withSegTimeout<T>(
  outer: AbortSignal,
  fn: (signal: AbortSignal) => Promise<T>,
): Promise<T> {
  const ac = new AbortController()
  const onOuter = () => ac.abort()
  outer.addEventListener('abort', onOuter, { once: true })
  const timer = setTimeout(() => ac.abort(), SEG_TIMEOUT)
  try {
    return await fn(ac.signal)
  } finally {
    clearTimeout(timer)
    outer.removeEventListener('abort', onOuter)
  }
}

/** 读完整条响应流，边读边记首字节时间。返回吞吐与首字节。 */
async function downloadAndTime(url: string, signal: AbortSignal) {
  const t0 = performance.now()
  const res = await fetch(url, { cache: 'no-store', signal })
  if (!res.ok) {
    // 服务端把失败原因写在 body 里（比如"隧道没有返回任何数据"），
    // 直接丢掉会让用户只看到一个 502。
    const txt = await res.text().catch(() => '')
    throw new Error(txt.trim() || `HTTP ${res.status}`)
  }
  if (!res.body) throw new Error('浏览器不支持流式读取')
  const reader = res.body.getReader()
  let total = 0
  let ttfb = 0
  let incomplete = false
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      if (!ttfb && value && value.byteLength > 0) ttfb = performance.now() - t0
      total += value ? value.byteLength : 0
    }
  } catch (e) {
    // 读流中途被中断（超时或用户点停止）：**已经收到的字节仍然有意义**。
    // 之前这里直接抛出，用户看到的是 "BodyStreamBuffer was aborted" 这种
    // 纯技术错误，而"25 秒只传了 200KB"这个真正有用的结论被丢掉了。
    if (!signal.aborted) throw e
    incomplete = true
  }
  const ms = performance.now() - t0
  return { speed: ms > 0 ? total / (ms / 1000) : 0, ttfb, total, ms, incomplete }
}

/** 往服务器推一段不可压缩的数据，量上行。 */
async function uploadAndTime(url: string, bytes: number, signal: AbortSignal) {
  // 用随机数据：全 0 的 body 一旦被任何一层 gzip，测出来的就是压缩速度。
  const chunk = new Uint8Array(64 << 10)
  crypto.getRandomValues(chunk)
  const parts: BlobPart[] = []
  for (let sent = 0; sent < bytes; sent += chunk.length) parts.push(chunk)
  const body = new Blob(parts)

  const t0 = performance.now()
  const res = await fetch(url, { method: 'POST', body, cache: 'no-store', signal })
  const ms = performance.now() - t0
  if (!res.ok) throw new Error(`HTTP ${res.status}`)
  const j = (await res.json()) as { bytes: number }
  return { speed: ms > 0 ? j.bytes / (ms / 1000) : 0, total: j.bytes, ms }
}

/**
 * 量延迟分布。
 *
 * **只报中位数是错的**，这是 2026-10-29 实测的直接教训：
 * 30 次请求里 26 次是 164~212ms、4 次是 1211~7820ms。
 * 中位数 191ms 看起来"延迟良好"，但每 7 个请求就有 1 个卡几秒 ——
 * 网页由几十个请求组成，用户感受到的是"卡死"，不是"191ms"。
 *
 * 所以这里同时给出 p95 和慢请求占比：它们才是体感的直接指标。
 * 慢请求的成因是丢包导致的 TCP 重传超时（200→400→800→1600→3200ms 指数退避），
 * 与带宽无关，调任何 frp 参数都改变不了。
 */
async function measureRTT(signal: AbortSignal, times = 10) {
  await fetch('/api/linktest/ping', { cache: 'no-store', signal }) // 预热：建立连接，结果丢弃
  const samples: number[] = []
  for (let i = 0; i < times; i++) {
    const t0 = performance.now()
    await fetch('/api/linktest/ping', { cache: 'no-store', signal })
    samples.push(performance.now() - t0)
  }
  const sorted = [...samples].sort((a, b) => a - b)
  const at = (q: number) => sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * q))]
  const p50 = at(0.5)
  return {
    p50,
    p95: at(0.95),
    max: sorted[sorted.length - 1],
    // 超过中位数 2 倍就算"卡了"：正常抖动在 20% 以内
    slow: samples.filter((x) => x > p50 * 2).length,
    n: samples.length,
  }
}

type Verdict = { tone: 'ok' | 'warn' | 'bad'; title: string; detail: string }

/**
 * 把三段数字变成一句人话。
 *
 * 判断顺序按"先排除致命故障、再比较相对快慢"来 —— 不能让一个失败的测量
 * 参与比值运算，否则会得出"比 0 还慢"这种毫无意义的结论。
 */
function buildVerdict(
  s3: BrowserSeg,
  s1: TunnelProbeResult | null,
  s2: ProxySeg,
  muxOff: boolean,
): Verdict {
  if (s1 && !s1.ok) {
    return {
      tone: 'bad',
      title: '隧道是断的：服务器到客户端这一整段过不去',
      detail: `${s1.err}${s1.hint ? ' —— ' + s1.hint : ''}`,
    }
  }
  if (s3.state === 'fail') {
    return {
      tone: 'bad',
      title: '你这台机器到服务器就不通',
      detail: `浏览器侧测速失败：${s3.err}。这一段与隧道无关，先确认本机网络能正常打开面板。`,
    }
  }
  // 关闭 tcpMux 是实测里代价最大、也最容易被"理论推导"误导的一项配置。
  // 2026-10-29 对照：开启 6/6 次 7.2MB/s；关闭后 21~59KB/s 与 3MB/s 二值跳变，
  // 还出现 8 秒超时 0 字节 —— 差约 200 倍。
  // 只要它关着且测得慢，就必须第一个说出来，否则后面所有判断都会指向错误方向
  // （会被误读成"上行瓶颈"或"链路不稳定"）。
  if (muxOff && s1 && s1.ok && s1.http && s1.speed > 0 && s1.speed < 512*1024) {
    return {
      tone: 'bad',
      title: '你关闭了 TCP 多路复用（tcpMux），这大概率就是慢的主因',
      detail:
        `当前测得 ${fmtBytes(s1.speed)}/s，落在慢档。18 次对照的结果是：` +
        '开启时 18/18 次全部稳定在 7MB/s 左右；关闭后会在 3~6MB/s 与 20~60KB/s 两档之间跳，' +
        '约一半的请求落在慢档，还会随机超时。' +
        '原因：关闭后每个用户连接都要单独新建 work 连接、重新从 TCP 慢启动开始；' +
        '链路有丢包时（你这条实测 10%），新建连接第一轮就把窗口打崩，之后一直起不来。' +
        '去「隧道」页把「关闭 TCP 多路复用」取消勾选并保存，等约 15 秒恢复后再测一次 —— ' +
        '这是唯一一处"改一个开关就能拿回两个数量级"的地方。',
    }
  }
  // 抖动优先于一切"参数类"问题：
  // 中位数正常、p95 极高，是"看着不慢、实际卡死"的典型形态，
  // 而它跟带宽、连接池、tcpMux 全都无关 —— 不先说清楚，用户会一直调参数。
  if (s3.sampleCount > 0 && s3.slowCount / s3.sampleCount >= 0.1) {
    return {
      tone: 'bad',
      title: `你的链路在丢包：${s3.slowCount}/${s3.sampleCount} 次请求卡了数秒`,
      detail:
        `延迟中位只有 ${Math.round(s3.rtt)}ms，但 p95 到了 ${Math.round(s3.rttP95)}ms。` +
        '这是丢包导致 TCP 重传超时的特征（200→400→800→1600→3200ms 指数退避）。' +
        '网页由几十个请求组成，只要一部分卡住，整体观感就是"卡"。' +
        '调带宽、连接池、tcpMux 都改变不了它 —— 只能换线路（国内节点或中转）。',
    }
  }
  if (s1 && s1.attempts > 1) {
    return {
      tone: 'warn',
      title: '隧道能用，但会卡：连接池里有失效连接',
      detail:
        `第 ${s1.attempts} 次才探测成功，说明池里的旧连接被网络设备回收了 —— ` +
        '数据写进去没有回应，用户侧表现就是偶发卡住几十秒。' +
        '老客户端没有保活，升级客户端可解决；仍出现就把「连接池」调小或设 0。',
    }
  }
  // 上下行严重不对称 —— 这是本机宽带的典型问题，必须先说，否则用户会以为
  // 是隧道配置问题去反复调参。
  //
  // 判据：① 测的是"服务器从隧道里往外读"，数据由客户端送出，占用**客户端上行**；
  // ③ 下行测的是反方向。若上行不足下行的 1/5，说明这条宽带的上行是瓶颈。
  // 实测现场：下行 6.5MB/s、① 88KB/s，差 75 倍，而两边都稳定复现。
  if (s1 && s1.http && s1.speed > 0 && s3.down > 0 && s1.speed < s3.down * 0.2) {
    return {
      tone: 'warn',
      title: '瓶颈在你的上行带宽，不在隧道',
      detail:
        `下行有 ${fmtBytes(s3.down)}/s，但从客户端往外读只有 ${fmtBytes(s1.speed)}/s —— ` +
        '隧道返回数据走的就是这条上行。家用宽带的上行通常只有下行的几十分之一，' +
        '而所有"从你机器送出"的流量都要挤它。' +
        '这跟 tcpMux、连接池、压缩都无关，只能换宽带套餐或改走别的方式（比如在国内加中转）。',
    }
  }
  // 上行极慢：所有"从你这台机器往外送"的流量都会被它卡住。
  //
  // 这条必须单列，否则会被"三段都正常"掩盖掉 —— 实测就出现过
  // ① 6.7MB/s、② 875ms 都很好，但浏览器传 1MB 上服务器 12 秒都传不完。
  // 用 curl 对照确认过不是前端实现的问题：同一时刻 1MB 上传有 60 秒超时的，
  // 也有 1.25 秒跑完的（841KB/s），波动近 90 倍。
  if (s3.upErr && s3.down > 0 && s1 && s1.ok) {
    return {
      tone: 'warn',
      title: '你的上行方向极不稳定（下行是好的）',
      detail:
        `下行 ${fmtBytes(s3.down)}/s、走隧道 ${fmtBytes(s1.speed)}/s 都正常，` +
        '但从这台机器往服务器推数据传不完（' +
        (s3.upErr || '超时') +
        '）。用 curl 对照过，这不是测量方式的问题：同一时刻 1MB 上传有 60 秒超时的，' +
        '也有 1.2 秒跑完的，波动接近 90 倍。' +
        '影响面是「从你这边发出的所有大流量」：上传文件、提交表单、视频上行。' +
        '下行不受影响，所以下载和看网页感觉还行。这属于运营商上行链路质量，' +
        'frp 侧没有参数能修 —— 换宽带或加中转。',
    }
  }
  // 两次测量差好几倍 = 这条链路不可信，单次数字不能当结论。
  //
  // 这是 2026-10-29 踩到的坑：① 测出 56KB/s、② 同一时刻 1 秒传完 2MB（≈2MB/s），
  // 差 36 倍。当时我的结论逻辑拿单次采样做绝对值比较，直接判成"瓶颈在隧道转发"，
  // 那是错的 —— 实际是 ① 那次撞上了丢包重传，窗口被一次卡顿吃掉。
  // 所以比"谁慢"更重要的是先说清楚：**这次测量本身可信吗**。
  if (s1 && s1.http && s1.speed > 0 && s2.state === 'done' && s2.speed > 0) {
    const hi = Math.max(s1.speed, s2.speed)
    const lo = Math.min(s1.speed, s2.speed)
    if (hi / lo >= 3) {
      return {
        tone: 'warn',
        title: `这条链路很不稳定：两次测量差了 ${Math.round(hi / lo)} 倍`,
        detail:
          `① 测出 ${fmtBytes(s1.speed)}/s、② 测出 ${fmtBytes(s2.speed)}/s。` +
          '同一时刻、同一条隧道差这么多，说明链路上有严重排队或丢包 —— ' +
          '某一次刚好撞上 TCP 重传退避，窗口被吃掉大半。' +
          '这种情况下单次数字不能当结论：连点几次「开始体检」看波动范围。' +
          '（稳定链路上 ② 应该只比 ① 略低，因为它多绕一次面板转发。）',
      }
    }
  }
  // 相对比较：② 的理论上限是 min(③ 下行, ① 吞吐) —— 两段串联取慢的那个。
  if (s1 && s1.http && s3.down > 0 && s2.state === 'done' && s2.speed > 0) {
    const ceiling = Math.min(s3.down, s1.speed)
    if (ceiling > 0 && s2.speed < ceiling * 0.25) {
      return {
        tone: 'warn',
        title: `② 比 ① 慢 ${Math.round(ceiling / s2.speed)} 倍：卡在「面板转发」这一环`,
        detail:
          `① 直接从面板读隧道有 ${fmtBytes(s1.speed)}/s，② 经面板转给浏览器只有 ${fmtBytes(s2.speed)}/s。` +
          '② 比 ① 多经过一次面板中转，数字天然会低一些，但差到这个程度说明这一环有瓶颈。' +
          '需要分清两种可能：一是 frp 隧道参数（关掉 tcpMux、把连接池设 0 后再测一次即可对照）；' +
          '二是面板转发本身（它是串行读写的，在高延迟链路上会有额外损失）。' +
          '先做前一个对照，因为它是你能控制的。',
      }
    }
  }
  if (s1 && s1.http && s1.first_ms > 500) {
    return {
      tone: 'warn',
      title: '带宽够，但每个请求都要等一次跨境往返',
      detail:
        `单次请求 ${s1.first_ms}ms，其中首字节就占了 ${s1.ttfb_ms}ms。` +
        '网页由几十个小请求组成，每个都付一次这个延迟，所以感觉"一格一格加载"。' +
        '这不是带宽问题，参数调不回来 —— 换国内节点或加中转才有效。',
    }
  }
  if (s3.rtt > 0 && s3.rtt > 150) {
    return {
      tone: 'warn',
      title: `延迟偏高：你这台机器到服务器 ${Math.round(s3.rtt)}ms`,
      detail:
        '跨境物理距离决定的，任何软件层参数都换不回这段。' +
        '要改善体感只有两条路：把服务器换到离你近的地方，或在国内加一台中转。',
    }
  }
  if (s2.state === 'fail') {
    return {
      tone: 'warn',
      title: '隧道侧测速失败，但服务器到客户端是通的',
      detail: `${s2.err}。多半是这条隧道的目标服务不是 HTTP，或它对这个请求没有响应。`,
    }
  }
  return {
    tone: 'ok',
    title: '三段都正常',
    detail:
      '你这台机器到服务器、服务器到客户端、以及走隧道的完整路径都测通了。' +
      '如果仍然觉得慢，多半是使用方式的问题（比如一次传大文件 vs 频繁小请求），而不是链路能力。',
  }
}

export function DiagnosePage() {
  const { tunnels, agents, settings, toast } = useApp()

  const [tunnelId, setTunnelId] = useState('')
  const [running, setRunning] = useState(false)
  const [step, setStep] = useState('')
  const [s3, setS3] = useState<BrowserSeg>(emptyBrowser)
  const [s1, setS1] = useState<TunnelProbeResult | null>(null)
  const [s2, setS2] = useState<ProxySeg>(emptyProxy)
  const abortRef = useRef<AbortController | null>(null)

  // 默认选中隧道：优先用从隧道页带过来的那条（用户点的是哪一行，就看哪一条），
  // 否则退到第一条已绑定机器的隧道（没绑定机器的隧道没法测）。
  useEffect(() => {
    // effect-deps-ok: 这里**必须**跟着 tunnels 的重算走。
    // 首屏渲染时 tunnels 还是空数组（store 的首次拉取还没回来），
    // 只在 [tunnelId] 上触发的话会永远选不中 —— 因为 tunnels 到达时
    // tunnelId 并没有变化，effect 不会重跑。它不写任何表单、不覆盖
    // 用户输入，且 tunnelId 一旦有值就立刻 return，不会自激。
    if (tunnelId) return
    const pre = sessionStorage.getItem('diag_tunnel')
    if (pre) {
      // 用后立刻清掉：否则下次从菜单直接进来，会莫名其妙锁在上次那条。
      sessionStorage.removeItem('diag_tunnel')
      setTunnelId(pre)
      return
    }
    const usable = (tunnels ?? []).filter((t) => t.agent_id)
    if (usable.length > 0) setTunnelId(String(usable[0].id))
  }, [tunnels, tunnelId])

  // 切页面时中断还在跑的测量，避免请求挂在那里继续吃带宽。
  useEffect(() => {
    return () => {
      abortRef.current?.abort()
    }
  }, [])

  const tunnel = useMemo(
    () => (tunnels ?? []).find((t) => String(t.id) === tunnelId) ?? null,
    [tunnels, tunnelId],
  )
  const agent = useMemo(
    () => (agents ?? []).find((a) => a.id === tunnel?.agent_id) ?? null,
    [agents, tunnel],
  )

  const run = useCallback(async () => {
    if (!tunnel || running) return
    abortRef.current?.abort()
    const ac = new AbortController()
    abortRef.current = ac
    setRunning(true)
    setS1(null)
    setS2(emptyProxy)

    // ---- ③ 我 → 服务器 ----
    setStep('正在测「你 → 服务器」：延迟、下行、上行…')
    setS3({ ...emptyBrowser, state: 'running' })
    const next3: BrowserSeg = { ...emptyBrowser, state: 'running' }
    try {
      await withSegTimeout(ac.signal, async (signal) => {
        const rtt = await measureRTT(signal)
        next3.rtt = rtt.p50
        next3.rttP95 = rtt.p95
        next3.slowCount = rtt.slow
        next3.sampleCount = rtt.n
        const down = await downloadAndTime(`/api/linktest/down?bytes=${DOWN_BYTES}`, signal)
        next3.down = down.speed
        next3.downTtfb = down.ttfb
        next3.bytes = down.total
        // 上行单独容错，绝不允许它连累下行结果。
        // 2026-10-29 实测这台机器上行只有 16.7KB/s（跨境丢包导致），
        // 一次 1MB 上传要一分多钟 —— 如果不隔离，整段超时，
        // 用户连"下行 1.9MB/s 是好的"这个关键信息都拿不到。
        const upAC = new AbortController()
        const upTimer = setTimeout(() => upAC.abort(), 12000)
        const onAbort = () => upAC.abort()
        signal.addEventListener('abort', onAbort, { once: true })
        try {
          const up = await uploadAndTime('/api/linktest/up', UP_BYTES, upAC.signal)
          next3.up = up.speed
        } catch (e) {
          next3.upErr =
            (e as Error).name === 'AbortError'
              ? `上行太慢：${fmtBytes(UP_BYTES)} 在 12 秒内没传完`
              : (e as Error).message
        } finally {
          clearTimeout(upTimer)
          signal.removeEventListener('abort', onAbort)
        }
      })
      next3.state = 'done'
    } catch (e) {
      if (ac.signal.aborted) return
      next3.state = 'fail'
      next3.err = (e as Error).message
    }
    setS3(next3)
    if (ac.signal.aborted) return

    // ---- ① 服务器 → 客户端（走隧道）----
    setStep('正在测「服务器 → 客户端」：这条隧道的实际吞吐…')
    try {
      const r = await api<TunnelProbeResult>('POST', `/api/tunnels/${tunnel.id}/speedtest`, {})
      setS1(r)
    } catch (e) {
      if (ac.signal.aborted) return
      setS1({
        tunnel_id: tunnel.id,
        ttfb_ms: 0,
        first_ms: 0,
        first_bytes: 0,
        speed: 0,
        bytes: 0,
        ms: 0,
        requests: 0,
        attempts: 1,
        http: false,
        ok: false,
        note: '',
        err: (e as Error).message,
        hint: '',
      })
    }
    if (ac.signal.aborted) return

    // ---- ② 我 → 隧道 → 服务 ----
    setStep('正在测「你 → 隧道 → 你的服务」：完整路径（这一段才是体感）…')
    setS2({ ...emptyProxy, state: 'running' })
    const next2: ProxySeg = { ...emptyProxy, state: 'running' }
    try {
      const r = await withSegTimeout(ac.signal, (signal) =>
        downloadAndTime(`/api/tunnels/${tunnel.id}/proxy-down?bytes=${PROXY_BYTES}`, signal),
      )
      next2.speed = r.speed
      next2.ttfb = r.ttfb
      next2.ms = r.ms
      next2.bytes = r.total
      // 服务端 budget 到点会正常结束响应（EOF），前端感知不到 abort，
      // 所以「没传满目标字节」本身就是被截断的证据。
      next2.incomplete = r.incomplete || r.total < PROXY_BYTES
      next2.state = 'done'
    } catch (e) {
      if (ac.signal.aborted) return
      next2.state = 'fail'
      next2.err = (e as Error).message
    }
    setS2(next2)
    setStep('')
    setRunning(false)
  }, [tunnel, running])

  const stop = () => {
    abortRef.current?.abort()
    setRunning(false)
    setStep('')
    toast('已停止体检')
  }

  const done = s3.state === 'done' || s3.state === 'fail'
  // 关闭 tcpMux 时结论要优先指向它 —— 这是实测代价最大的一项配置。
  const muxOff = settings?.tunnel_tcp_mux === '0'
  const verdict = done ? buildVerdict(s3, s1, s2, muxOff) : null

  return (
    <>
      <Card
        title="链路体检"
        desc="把你说的三段分开量：哪一段慢，处置方式完全不同"
        actions={
          running ? (
            <Button variant="ghost" onClick={stop}>
              停止
            </Button>
          ) : (
            <Button disabled={!tunnel} onClick={() => void run()}>
              开始体检
            </Button>
          )
        }
      >
        <div className="flex flex-wrap items-end gap-3">
          <Field label="体检哪条隧道" className="mb-0 min-w-[220px] flex-1">
            <Select value={tunnelId} onChange={(e) => setTunnelId(e.target.value)}>
              <option value="">选择一条隧道…</option>
              {(tunnels ?? []).map((t) => (
                <option key={t.id} value={String(t.id)}>
                  {t.name} · 公网 {t.public_port}
                  {t.agent_id ? '' : '（未绑定机器，测不了）'}
                </option>
              ))}
            </Select>
          </Field>
          {tunnel && (
            <div className="pb-2 text-[12px] leading-[1.7] text-dim">
              本地服务 <span className="font-mono text-fg">{tunnel.local_addr}</span>
              <br />
              承载客户端{' '}
              <span className="text-fg">{agent?.name ?? '未知'}</span>
              {agent && (
                <span className={agent.online ? 'text-ok-fg' : 'text-bad-fg'}>
                  {agent.online ? ' · 在线' : ' · 离线'}
                </span>
              )}
            </div>
          )}
        </div>

        {step && (
          <div className="mt-3 flex items-center gap-2 rounded-lg border border-border2 bg-chip px-3 py-2 text-[12px] text-dim">
            <span className="inline-block h-2 w-2 animate-pulse rounded-full bg-accent-fg" />
            {step}
          </div>
        )}

        <div className="mt-4 grid grid-cols-3 gap-3 max-[980px]:grid-cols-1">
          <SegmentCard
            no="③"
            name="你 → 服务器"
            what="浏览器直接打面板。只跟你在哪、跨境出口有关"
            tone={segTone(s3.state)}
          >
            {s3.state === 'idle' && <Placeholder />}
            {s3.state === 'running' && <Placeholder text="测量中…" />}
            {s3.state === 'fail' && <FailLine err={s3.err} />}
            {s3.state === 'done' && (
              <Lines
                rows={[
                  ['延迟中位', `${Math.round(s3.rtt)}ms`, toneOfMs(s3.rtt)],
                  ['抖动 p95', `${Math.round(s3.rttP95)}ms`, toneOfJitter(s3.rttP95, s3.rtt)],
                  [
                    '卡住的请求',
                    `${s3.slowCount}/${s3.sampleCount} 次`,
                    s3.slowCount === 0 ? 'ok' : s3.slowCount / Math.max(1, s3.sampleCount) >= 0.1 ? 'bad' : 'warn',
                  ],
                  ['下行', `${fmtBytes(s3.down)}/s`, toneOfSpeed(s3.down)],
                  [
                    '上行',
                    s3.upErr ? '测不完（极慢）' : `${fmtBytes(s3.up)}/s`,
                    s3.upErr ? 'bad' : toneOfSpeed(s3.up),
                  ],
                ]}
              />
            )}
          </SegmentCard>

          <SegmentCard
            no="①"
            name="服务器 → 客户端"
            what="从服务器压进隧道，测这条隧道本身的能力"
            tone={s1 ? (s1.ok ? 'ok' : 'bad') : 'idle'}
          >
            {!s1 && <Placeholder />}
            {s1 && !s1.ok && <FailLine err={s1.err} hint={s1.hint} />}
            {s1 && s1.ok && !s1.http && (
              <Lines rows={[['说明', s1.note || '非 HTTP，无法测吞吐', 'dim']]} />
            )}
            {s1 && s1.ok && s1.http && (
              <Lines
                rows={[
                  ['吞吐', `${fmtBytes(s1.speed)}/s`, toneOfSpeed(s1.speed)],
                  ['单次请求', `${s1.first_ms}ms`, toneOfMs(s1.first_ms)],
                  ['首字节', `${s1.ttfb_ms}ms`, toneOfMs(s1.ttfb_ms)],
                ]}
              />
            )}
          </SegmentCard>

          <SegmentCard
            no="②"
            name="你 → 隧道 → 服务"
            what="完整路径（③+①）。这一段才是你实际感受到的"
            tone={segTone(s2.state)}
          >
            {s2.state === 'idle' && <Placeholder />}
            {s2.state === 'running' && <Placeholder text="测量中…" />}
            {s2.state === 'fail' && <FailLine err={s2.err} />}
            {s2.state === 'done' && (
              <>
                <Lines
                  rows={[
                    ['吞吐', `${fmtBytes(s2.speed)}/s`, toneOfSpeed(s2.speed)],
                    ['首字节', `${Math.round(s2.ttfb)}ms`, toneOfMs(s2.ttfb)],
                    ['本次耗时', `${Math.round(s2.ms)}ms`, 'dim'],
                  ]}
                />
                {s2.incomplete && (
                  <div className="mt-1.5 text-[11px] leading-[1.6] text-warn-fg">
                    时间到只传完 {fmtBytes(s2.bytes)}（目标 {fmtBytes(PROXY_BYTES)}）。
                    上面的吞吐按已传完的部分算，仍然可比。
                  </div>
                )}
              </>
            )}
          </SegmentCard>
        </div>

        {verdict && (
          <div
            className={cx(
              'mt-3.5 rounded-lg border px-3.5 py-3 text-[12.5px] leading-[1.75]',
              verdict.tone === 'ok'
                ? 'border-ok-fg/35 bg-ok-dim text-ok-fg'
                : verdict.tone === 'warn'
                  ? 'border-warn-fg/40 bg-warn-dim text-warn-fg'
                  : 'border-bad-fg/40 bg-bad-dim text-bad-fg',
            )}
          >
            <b>{verdict.title}</b>
            <div className="mt-1 opacity-90">{verdict.detail}</div>
          </div>
        )}

        {/* mux 关着就始终提醒一句，不管这次测快还是测慢。
            原因：它是"二值分布"——关掉之后有时也能跑到 2.9MB/s，
            单次测量碰巧撞上快的那一档就不会触发结论里的那条判断。
            但这个开关本身是隐患：慢的那一档是 20~60KB/s 且会随机超时，
            用户下次访问就可能撞上。 */}
        {muxOff && (
          <div className="mt-2.5 rounded-lg border border-warn-fg/40 bg-warn-dim px-3.5 py-2.5 text-[12px] leading-[1.7] text-warn-fg">
            <b>提醒：你的「关闭 TCP 多路复用」是开着的。</b>
            {' '}这次可能测得不慢，但关闭状态下速度是二值分布 ——
            实测同一时刻会落在 7.2MB/s 或 20~60KB/s 两档之一，并随机出现 8 秒超时。
            建议在「隧道」页取消勾选，这是唯一一处"改一个开关就能稳掉两个数量级风险"的地方。
          </div>
        )}
      </Card>

      <Card title="怎么读这三个数" desc="每段的含义与调优手段完全不同，别混在一起看">
        <div className="grid gap-2.5 text-[12.5px] leading-[1.8] text-dim">
          <p>
            <b className="text-fg">③ 你 → 服务器</b>
            {' '}
            由浏览器实测。这一段只取决于你的位置与跨境出口。它慢的话，调 frp 的任何参数都没有用 ——
            换节点、加国内中转才有意义。
          </p>
          <p>
            <b className="text-fg">① 服务器 → 客户端</b>
            {' '}
            起点的服务器，所以它<b className="text-warn-fg">不包含</b> ③。
            数字好看不代表你访问快 —— 这是最容易误解的地方。它反映的是客户端所在那条上行/下行链路
            与 frpc 的配置（tcpMux、连接池、压缩）。
          </p>
          <p>
            <b className="text-fg">② 你 → 隧道 → 服务</b>
            {' '}
            经面板同源转发，一次计时覆盖 ③+①。它比 min(③, ①) 低很多时，问题出在「转发」这一环 ——
            要么是 frp 的传输参数（连接池、mux），要么是面板代理本身的串行读写开销。
            两者要分开：先关掉 tcpMux 再测一次做对照，那是你能控制的变量。
          </p>
          <p>
            <b className="text-fg">关于「单次请求」</b>
            {' '}
            带宽说的是传大文件的能力，而网页是由几十个小请求组成的。真正决定"卡不卡"的是
            <b className="text-fg"> 单次请求耗时</b>，其中大部分是跨境往返（RTT）。
            带宽再高，也换不回这段物理距离。
          </p>
        </div>
        <Hint>
          体检结果不落库：它只是"此刻这条链路的状态"。存下来反而会让人分不清这是刚测的还是上周的。
        </Hint>
      </Card>
    </>
  )
}

function segTone(s: SegState): 'ok' | 'bad' | 'warn' | 'idle' {
  if (s === 'done') return 'ok'
  if (s === 'fail') return 'bad'
  if (s === 'running') return 'warn'
  return 'idle'
}

function toneOfMs(ms: number): string {
  if (ms <= 0) return 'dim'
  if (ms > 500) return 'bad'
  if (ms > 250) return 'warn'
  return 'ok'
}

/** 抖动着色：p95 相对中位数的倍数才是关键，绝对值意义不大（跨境本来就近 200ms）。 */
function toneOfJitter(p95: number, p50: number): string {
  if (p95 <= 0 || p50 <= 0) return 'dim'
  const ratio = p95 / p50
  if (ratio > 3) return 'bad'
  if (ratio > 1.6) return 'warn'
  return 'ok'
}

function toneOfSpeed(bps: number): string {
  if (bps <= 0) return 'dim'
  if (bps < 300 * 1024) return 'bad'
  if (bps < 1 << 20) return 'warn'
  return 'ok'
}

function SegmentCard({
  no,
  name,
  what,
  tone,
  children,
}: {
  no: string
  name: string
  what: string
  tone: 'ok' | 'bad' | 'warn' | 'idle'
  children: React.ReactNode
}) {
  return (
    <div
      className={cx(
        'rounded-lg border px-3 py-2.5',
        tone === 'ok'
          ? 'border-ok-fg/35 bg-ok-dim/40'
          : tone === 'bad'
            ? 'border-bad-fg/40 bg-bad-dim/40'
            : tone === 'warn'
              ? 'border-warn-fg/35 bg-warn-dim/30'
              : 'border-border2 bg-chip',
      )}
    >
      <div className="flex items-baseline gap-1.5">
        <span className="font-mono text-[11px] text-dim">{no}</span>
        <b className="text-[12.5px]">{name}</b>
      </div>
      <div className="mb-2 mt-0.5 text-[11px] leading-[1.6] text-dim">{what}</div>
      {children}
    </div>
  )
}

function Lines({ rows }: { rows: [string, string, string][] }) {
  return (
    <div className="grid gap-0.5">
      {rows.map(([k, v, tone]) => (
        <div key={k} className="flex items-baseline justify-between gap-2 text-[12px]">
          <span className="text-dim">{k}</span>
          <span
            className={cx(
              'font-mono',
              tone === 'ok'
                ? 'text-ok-fg'
                : tone === 'bad'
                  ? 'text-bad-fg'
                  : tone === 'warn'
                    ? 'text-warn-fg'
                    : 'text-fg',
            )}
          >
            {v}
          </span>
        </div>
      ))}
    </div>
  )
}

function Placeholder({ text = '未测' }: { text?: string }) {
  return <div className="text-[12px] text-dim">{text}</div>
}

function FailLine({ err, hint }: { err: string; hint?: string }) {
  return (
    <div className="text-[11.5px] leading-[1.65] text-bad-fg">
      <b>失败</b>：{err}
      {hint && <div className="mt-1 text-dim">{hint}</div>}
    </div>
  )
}
