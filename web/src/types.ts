/**
 * 与 Go 侧 JSON 严格对应的类型。
 *
 * 对应关系（改动任何一边都要同步这里）：
 *   internal/shared/model.go     Tunnel / Agent / DiscoveredService
 *   internal/panel/frps.go       Status() 的 map 字段
 *   internal/panel/ports.go      PortNeed / PortCheck / FirewallInfo / FWCommand
 *   internal/panel/probe.go      DialOutcome / ExtProbeResult
 *   internal/panel/tunnels.go    listTunnelsHandler / settingsHandler
 *   internal/panel/linkquality.go  LinkQuality / LinkConn（ss -tin 采样）
 *   internal/panel/advice.go     Advice（检查点 → 建议）
 *
 * 注意：Go 的零值 time.Time 序列化成 "0001-01-01T00:00:00Z"，所以时间字段
 * 一律按字符串处理，由 format.fmtAgo 判断有效性。
 */

export interface Me {
  user: string
  needs_password_change: boolean
  public_addr_configured: boolean
  panel_version: string
  panel_built: string
  ui_source: 'embed' | 'dir'
}

export interface DiscoveredService {
  host: string
  port: number
  addr: string
  name: string
  /** 3=Web 优先 2=常见服务 1=其它监听端口 */
  confidence: number
}

export interface Agent {
  id: string
  name: string
  os: string
  arch: string
  ips: string[] | null
  frp_version: string
  agent_version: string
  last_seen: string
  online: boolean
  created_at: string
  services: DiscoveredService[] | null
  /** 客户端自测的「本机 → 面板」下行速率（kbps）；0 = 还没测过 */
  down_kbps: number
  /**
   * 客户端自测的上行速率（kbps）；0 = 还没测过。
   *
   * 必须与下行分开看：客户端绝大多数在家庭宽带上，上下行严重不对等。
   * 实测同一台机器下行 9.5MB/s、上行在 3.9KB/s~841KB/s 之间跳 ——
   * 合成一个"吞吐"会让人把瓶颈判到错误的方向。
   */
  up_kbps: number
  /** 上次测速时间（RFC3339），零值表示没测过 */
  speed_at: string
  /** 卸载指令已排队，还没被心跳领走 */
  uninstall_requested: boolean
  /**
   * 卸载指令被领走的时刻。有值就说明「已经让它卸载了」：
   * 配合 online 判断结果 —— 仍在线=没生效，已离线=卸载成功。
   */
  uninstall_sent_at?: string | null
  /** 这次卸载是在那台机器上执行的（跑了卸载命令），而不是面板云端下发 */
  uninstall_by_local: boolean
  /** 客户端上报的「实际从哪儿拿到二进制」：panel / git（空 = 旧客户端没上报） */
  bin_source: string
  /** 最近一次取二进制失败的明细（回退原因），空 = 首选就成功 */
  bin_note: string
}

export type ProxyState = 'online' | 'waiting_server' | 'not_deployed' | 'offline'

export interface Tunnel {
  id: number
  name: string
  local_addr: string
  public_port: number
  protocol: string
  agent_id: string
  created_at: string
  online: boolean
  last_seen: string
  traffic_in: number
  traffic_out: number
  /** 瞬时速率（字节/秒），面板对累计流量做差分得到 */
  rate_in: number
  rate_out: number
  /** 面板综合判定：只有 online 才是 frps 侧真的建起来了 */
  proxy_state: ProxyState
  /** 由面板实时推导，不落库；草稿隧道为空 */
  access_url: string
}

export interface FrpsStatus {
  running: boolean
  starting: boolean
  port_alive: boolean
  bind_port: number
  binary_path: string
  binary_ready: boolean
  current_version: string
  target_version: string
  dashboard_ok: boolean
  dashboard_port: number | string
  dashboard_error: string
  /** 最近一次启动失败的原因，成功启动后会清空 */
  last_error?: string
  last_error_at?: string
}

/** settings 是键值 map，后端直接把整张表返回（令牌已脱敏） */
export interface Settings {
  public_addr: string
  mirror_base: string
  port_range_start: string
  port_range_end: string
  frps_bind_port: string
  dashboard_port: string
  frp_version: string
  frps_token: string
  install_token: string
  update_base: string
  agent_auto_update: string
  /** 面板自己取二进制时的源顺序（有序、逗号分隔）：github / https://前缀,… */
  server_sources: string
  /** 客户端取二进制时的源顺序：panel / github / https://前缀,… */
  client_sources: string
  /** 下载策略：auto = 下载前临时测速排优；manual = 按上面的顺序 */
  source_mode: string
  /** 隧道是否压缩传输内容（'1' / '0'） */
  tunnel_compression: string
  /**
   * 隧道是否启用 frp 的 TCP 多路复用（yamux）。'1' / '0'。
   * **两端必须一致**：面板同时写进 frps.toml 和客户端配置，所以只能在面板改。
   */
  tunnel_tcp_mux: string
  /** 预建连接池大小，仅在多路复用关闭时生效 */
  tunnel_pool_count: string
  /** 是否让客户端定期自测（'1'/'0'） */
  speedtest_enabled: string
  /** 自测间隔（分钟），5~1440 */
  speedtest_interval_min: string
  [key: string]: string
}

/* ---------------- 版本与更新 ---------------- */

/** 上次自更新的落盘状态（面板重启后 UI 靠它说明结果）。 */
export interface UpdateState {
  stage: string
  from: string
  to: string
  at: string
  detail: string
  error: string
}

export interface VersionPanel {
  current: string
  build_time: string
  latest: string
  update_available: boolean
  notes: string
  can_update: boolean
  exe_path: string
  exe_writable: boolean
  error: string
}

export interface VersionAgent {
  // hosted 面板现在能给客户端提供的 agent 版本。本机平台没有内置产物时
  // 依次退到「已接入客户端拿到的那份」→「清单登记版本（按需拉取）」，
  // 所以它正常都该有值；hosted_note 说明这个号是从哪来的。
  hosted: string
  hosted_sha256: string
  hosted_size: number
  hosted_note: string
  // client_version 已接入的客户端**实际会收到**的那一版（取第一台作代表）。
  // 与 hosted 的区别：hosted 是面板自己那台机器上缓存的那份（服务器上通常是
  // linux/amd64 的旧缓存），两者不同时界面优先显示这个。
  client_version: string
  latest: string
  // cache_behind 面板手上的可下发文件比清单旧。这时客户端升不上去，
  // 而 outdated 会是 0（客户端和缓存版本一致），界面看起来一切正常。
  cache_behind: boolean
  auto_update: boolean
  outdated: number
  legacy: number
  // missing 面板缺该平台产物、无法升级的客户端数
  missing: number
  total: number
  min_supported: string
  error: string
}

export interface VersionFRP {
  current: string
  target: string
  latest: string
  pinned: boolean
  update_available: boolean
}

export interface VersionResponse {
  panel: VersionPanel
  agent: VersionAgent
  frp: VersionFRP
  update_source: string
  update_error: string
  checked_at: string
  last_update: UpdateState | null
}

export interface PanelUpdateStart {
  started: boolean
  from: string
  to: string
  message: string
}

export type AccessHostSource = 'settings' | 'request' | 'fallback'

/** 一个源的实测画像。字段含义见 internal/shared/probe.go。 */
export interface ProbeResult {
  token: string
  label: string
  url: string
  /** 至少一轮成功并拿到数据 */
  ok: boolean
  /** 没有可测的地址（客户端列表里的「面板中继」）：显示"不用测"，不是"不可用" */
  skipped?: boolean
  /** 测了几轮 / 其中几轮失败 —— 失败过就不是稳定链路 */
  attempts: number
  failures: number
  /** 首字节延迟（毫秒） */
  ttfb_ms: number
  /** 中位速度（KB/s）。用中位数而非最大值：峰值会被"刚好赶上一次快"骗到 */
  speed_kbps: number
  /** 速度抖动 (max-min)/max，0 表示两轮几乎一致 */
  jitter: number
  /** 0-100 综合分，仅用于排序与展示 */
  score: number
  error: string
}

/** 最近一次自动排优的结果。 */
export interface RankInfo {
  at: string
  side: string
  order: string[]
  summary: string
  results: ProbeResult[] | null
  /** 超过缓存时长：能看，但不该拿它判断"现在该排什么" */
  stale: boolean
}

/** 一条排优 / 下载记录（诊断用）。 */
export interface SourceEvent {
  id: number
  at: string
  side: string
  kind: string
  agent_id: string
  agent_name: string
  what: string
  source: string
  ok: boolean
  elapsed_ms: number
  bytes: number
  detail: string
  error: string
}

export interface SourceEventsResponse {
  events: SourceEvent[]
  mode: string
  last_rank: RankInfo | null
}

export interface TunnelsResponse {
  tunnels: Tunnel[]
  agents: Agent[]
  frps: FrpsStatus
  install_command: string
  install_command_windows: string
  install_command_android: string
  /** 卸载命令：在那台机器上直接执行即可（与云端下发卸载互补） */
  uninstall_command: string
  uninstall_command_windows: string
  uninstall_command_android: string
  access_host: string
  access_host_source: AccessHostSource
  frps_bind_port: number
  /** 安装命令实际用的基址 */
  install_base: string
  panel_listen_port: string
  /** 下载策略与最近一次自动排优（设置页据此显示"上次测出来什么顺序"） */
  source_mode: string
  last_rank: RankInfo | null
}

export interface AgentsResponse {
  agents: Agent[]
}

export interface VersionsResponse {
  current_frps_version: string
  target_version: string
  available_versions: string[]
  latest_version: string
  pinned: boolean
  update_available: boolean
}

export interface Nic {
  name: string
  ip: string
}

export interface NetworkResponse {
  nics: Nic[] | null
  public_ip: string
}

export interface MirrorTestResult {
  ok: boolean
  url?: string
  bytes?: number
  elapsed_ms?: number
  speed_kbps?: number
  error?: string
}

/* ---------- 端口 / 防火墙 ---------- */

export type PortPurpose = 'panel' | 'frps' | 'tunnel' | 'dashboard'

export interface PortNeed {
  port: number
  proto: string
  purpose: PortPurpose
  label: string
  tunnel_id?: number
  /** 是否必须对外放行（dashboard 这类只绑 127.0.0.1 的为 false） */
  required: boolean
  listening: boolean
  note?: string
}

export interface FirewallInfo {
  type: 'ufw' | 'firewalld' | 'nftables' | 'iptables' | 'none' | 'unsupported'
  active: boolean
  detail: string
  checked_at: string
}

export interface FWCommand {
  kind: string
  name: string
  cmd: string
  note: string
  recommended: boolean
}

export interface PortsResponse {
  access_host: string
  access_host_source: AccessHostSource
  access_host_kind: string
  probe_host_ok: boolean
  probe_host_note: string
  panel_port: number
  panel_listen: string
  port_range: [number, number]
  items: PortNeed[]
  required_ports: number[]
  firewall: FirewallInfo
  commands: FWCommand[]
  frps_running: boolean
}

export interface DialOutcome {
  ok: boolean
  error?: string
  /** ok | refused | timeout | dns | other | skipped */
  kind: string
  millis: number
}

export interface ExtProbeNode {
  node: string
  ok: boolean
  error?: string
  time_ms?: number
}

export interface ExtProbeResult {
  /** open | closed | rate_limited | error */
  state: string
  detail: string
  nodes?: ExtProbeNode[]
  link?: string
  millis: number
}

export type PortCheckState = 'open' | 'blocked' | 'no_service' | 'unknown' | 'rate_limited'

export interface PortCheck {
  port: number
  proto: string
  purpose: PortPurpose
  label: string
  state: PortCheckState
  /** external | self | none */
  evidence: string
  listening: boolean
  self: DialOutcome
  external?: ExtProbeResult
  detail: string
}

export interface PortCheckResponse {
  ok: boolean
  host: string
  external: boolean
  results: PortCheck[]
  checked_at: string
}

/* ---------- 请求体 ---------- */

export interface TunnelBody {
  name: string
  local_addr: string
  protocol: string
  agent_id: string
  manual_port: number
  /** 编辑时显式解绑（agent_id 传空仍会保留原绑定，必须用这个字段） */
  clear_agent?: boolean
}

/**
 * 设置保存请求体。
 *
 * **所有字段都是可选的，而且只该提交「真正改动过」的字段。**
 * 早期实现每次都把整份表单原样回传，后果是：表单里任何一个字段状态不对
 * （例如设置还没加载完就被渲染成空），保存时会把它在库里的值覆盖成空。
 * 用户看到的就是「配置莫名其妙消失了」——公网地址被清空就是这么来的。
 *
 * 服务端对「非空变空」还有一道护栏（见 clear_public_addr）。
 */
export interface SettingsBody {
  public_addr?: string
  /** 把它置 '1' 才允许把已有的公网地址清空；不带这个字段时空值会被忽略 */
  clear_public_addr?: string
  mirror_base?: string
  port_range_start?: string
  port_range_end?: string
  frps_bind_port?: string
  dashboard_port?: string
  frps_token?: string
  new_password?: string
  update_base?: string
  agent_auto_update?: string
  server_sources?: string
  client_sources?: string
  source_mode?: string
  tunnel_compression?: string
  tunnel_tcp_mux?: string
  tunnel_pool_count?: string
  speedtest_enabled?: string
  speedtest_interval_min?: string
}

/** 系统调优现状（设置 → 隧道传输）。congestion 只在 Linux 上有意义。 */
export interface TuningCongestion {
  supported: boolean
  /** 当前拥塞控制算法，如 cubic / bbr */
  cc: string
  bbr_available: boolean
  recommended: boolean
  error: string
}

export interface TunnelTransportState {
  tcp_mux: boolean
  pool_count: number
  compression: boolean
}

/** 客户端自测速率的调度（面板统一下发）。 */
export interface SpeedTestConfig {
  enabled: boolean
  interval_min: number
  /** 每次测速拉取的字节数，前端据此算流量成本 */
  bytes: number
}

export interface TuningResponse {
  congestion: TuningCongestion
  transport: TunnelTransportState
  min_agent_version: string
  max_pool_count: number
  speedtest: SpeedTestConfig
}

/** 隧道级测速结果（从服务器经 frps → frpc → 客户端服务）。 */
export interface TunnelProbeResult {
  tunnel_id: number
  /** 首字节耗时（毫秒）——最接近"纯延迟"的数字。注意不是 TCP 握手：那一段是本地回环，无意义 */
  ttfb_ms: number
  /** 第一个请求的完整耗时（毫秒）—— 打开这个服务一次要等多久，体感数字 */
  first_ms: number
  first_bytes: number
  /** 窗口内反复请求的平均吞吐（字节/秒）；非 HTTP 服务为 0 */
  speed: number
  bytes: number
  ms: number
  /** 窗口内发了几次请求。1 表示响应太大只测到一次 */
  requests: number
  /** 探测用了几个来回才拿到响应。>1 = 第一次抽到了失效连接（隧道"能用但会卡"） */
  attempts: number
  /** 目标是否是 HTTP 服务；false 表示只测了连通性与延迟 */
  http: boolean
  ok: boolean
  /** 面向上层的一句结论（需要提醒时非空） */
  note: string
  /** 失败原因。ok=false 时必定非空 */
  err: string
  /** 排障建议：下一步该查什么 */
  hint: string
}

/** 链路自检的单个目标结果。 */
export interface DiagnoseItem {
  key: string
  name: string
  /** 首次访问速率（字节/秒）：**含**握手与慢启动，就是第一次打开的体感 */
  speed: number
  /** 同一连接上第二次请求的速率（字节/秒）：高多少就是"复用 + 缓存"省下的 */
  speed_reuse: number
  bytes: number
  ms: number
  /** 建连成本：DNS + TCP + TLS 握手（毫秒）—— "第一次为什么慢"就在这里 */
  warm_ms: number
  /** 首次阶段（8MB）耗时（毫秒） */
  cold_ms: number
  /** 复用阶段（4MB）耗时（毫秒） */
  reuse_ms: number
  /** 两个阶段都远超物理上限（疑似命中本地/边缘缓存），不能当带宽依据 */
  suspect: boolean
  ok: boolean
  error: string
}

export interface DiagnoseResponse {
  items: DiagnoseItem[]
  advice: string
}

export interface SettingsSaveResult {
  ok?: boolean
  public_addr_changed?: boolean
  affected_tunnels?: number
  client_pickup_second?: number
  frps_restarting?: boolean
  frps_bind_port?: number
}

/** 「文件」页：面板托管的二进制（frps / frpc / agent）现状。 */

export type FileKind = 'frps' | 'frpc' | 'agent'

export interface HostedFile {
  kind: FileKind
  os: string
  arch: string
  name: string
  path: string
  present: boolean
  version: string
  size: number
  /** builtin = 随发布包内置；cache = 运行时拉取的缓存 */
  source: string
  note: string
  can_delete: boolean
}

export interface FilesResponse {
  items: HostedFile[]
  bin_dir: string
  data_dir: string
  used_bytes: number
  prefetch_all: boolean
  frp_version: string
  update_base: string
  mirror_base: string
}

/**
 * 客户端回传的日志快照（面板只留最近一次）。
 *
 * 面板连不上 NAT 后面的客户端，所以是"面板请求 → 客户端下一次心跳时回传"。
 * pending=true 表示这次请求还没被答复（客户端离线，或请求刚发出）。
 */
export interface AgentLogView {
  request_id: string
  requested_at: string | null
  content: string
  fetched_at: string | null
  /** 该内容对应哪一次请求：界面据此判断"我要的这份到了没有" */
  content_req: string
  pending: boolean
}

export interface AgentLogResponse {
  logs: AgentLogView
  online: boolean
  agent: string
  version: string
  last_see: string
}

/* ---------- 链路体检（内核 TCP 指标） ---------- */

/** 一条 frpc↔frps 连接的 TCP 内核指标快照。 */
export interface LinkQualityConn {
  local: string
  peer: string
  /** 拥塞控制算法：bbr / cubic / reno */
  cc: string
  /** 内核当前平滑 RTT（毫秒） */
  rtt_ms: number
  rtt_var_ms: number
  /** 只有 BBR 会报；0 表示这项拿不到 */
  min_rtt_ms: number
  cwnd: number
  sent: number
  retrans: number
  lost: number
  retrans_pct: number
  delivery_bps: number
}

export type LinkQualityVerdict = 'healthy' | 'jittery' | 'lossy' | 'idle'

/** 一次采样 + 聚合的结果。supported=false 时 reason 说明为什么拿不到。 */
export interface LinkQuality {
  supported: boolean
  reason: string
  /** RFC3339；Go 零值由 fmtAgo 显示为「从未」 */
  sampled_at: string
  bind_port: number
  /** Go 空切片序列化成 null */
  conns: LinkQualityConn[] | null
  rtt_p50: number
  rtt_p95: number
  rtt_max: number
  rtt_min: number
  min_rtt: number
  retrans_pct: number
  total_sent: number
  total_retrans: number
  verdict: LinkQualityVerdict
}

export type LinkQualitySeverity = 'critical' | 'warn' | 'info'

/**
 * 一条「检查点 → 建议」。
 *
 * patch 为空（后端 omitempty）表示只提示、不可一键应用；
 * 非空时键只可能是 tunnel_tcp_mux / tunnel_pool_count / tunnel_compression。
 */
export interface LinkQualityAdvice {
  id: string
  severity: LinkQualitySeverity
  title: string
  detail: string
  evidence: string[] | null
  patch?: Record<string, string> | null
}

/** 一个客户端「期望值 vs 实际生效值」的比对结果。 */
export interface LinkQualityApplied {
  agent_id: string
  name: string
  /** 客户端上报的传输配置摘要；空 = 旧版本或还没心跳 */
  transport: string
  match: boolean
  note?: string
}

export interface LinkQualityResponse {
  quality: LinkQuality | null
  advice: LinkQualityAdvice[] | null
  /** 面板设置算出的期望摘要，如 mux=1,pool=0,comp=1 */
  desired: string
  applied: LinkQualityApplied[] | null
}

export interface LinkQualityApplyResult {
  ok: boolean
  desired: string
  /** 本次真正写入的键值 */
  applied: Record<string, string> | null
  /** 客户端最快多久拉取新配置（秒） */
  confirm_within_sec: number
  warnings: string[] | null
}

/* ---------- CDN 加速接入（腾讯云 EdgeOne） ---------- */

/**
 * GET /api/cdn/config 的扁平键值视图。
 *
 * 四个密钥字段只回传掩码（shared.NewTokenMask），永远拿不到明文；
 * POST 时留空表示「不修改」，所以前端也不能把掩码原样回传 —— 那会把库里的
 * 明文覆盖成一串星号。
 */
export interface CdnConfig {
  /** '1' 启用 / '0' 关闭 */
  cdn_enabled: string
  /** 掩码；'' = 未配置 */
  cdn_tencent_secret_id: string
  cdn_tencent_secret_key: string
  cdn_tencent_zone_id: string
  cdn_tencent_region: string
  /** cn | intl —— EdgeOne 两套互不相通的账号体系，空值由后端补 'cn' */
  cdn_tencent_endpoint: string
  cdn_domain: string
  cdn_connect_port: string
  /** HTTP / HTTPS */
  cdn_origin_scheme: string
  cdn_ws_timeout: string
  cdn_aliyun_ak_id: string
  cdn_aliyun_ak_secret: string
  cdn_aliyun_root_domain: string
  /** 留空则由后端从加速域名推导 */
  cdn_dns_rr: string
  /** '' = 直连 TCP；'wss' = 经 CDN */
  desired_protocol: string
  /** '' = 用原 frps 地址；否则是「域名:端口」 */
  connect_addr: string
}

/** POST /api/cdn/config 允许的键（desired_protocol / connect_addr 是只读派生值）。 */
export type CdnConfigKey = Exclude<keyof CdnConfig, 'desired_protocol' | 'connect_addr'>

/** POST 请求体：只带用户真正改过的字段。 */
export type CdnConfigPatch = Partial<Record<CdnConfigKey, string>>

/** EdgeOne 站点。字段名跟腾讯云返回的 JSON 一致（大写开头）。 */
export interface CdnZone {
  ZoneId: string
  ZoneName: string
  Status: string
  Area: string
}

/** POST /api/cdn/verify：失败时后端返回 4xx/5xx + 纯文本，不会走到这里。 */
export interface CdnZoneProbe {
  action: string
  ok: boolean
  detail: string
}

export interface CdnVerifyResult {
  ok: boolean
  zones: CdnZone[] | null
  /** 腾讯云返回的总条数；与 zones.length 不一致说明分页/字段有问题 */
  total?: number
  /** 实际调用的 API 端点（国内站 / 国际站） */
  endpoint?: string
  /** 站点为空时返回的原始响应片段，用于判断"真没站点"还是"权限被收窄" */
  raw?: string
  /** 空列表时的排查指引：优先指向缺失的 CAM 权限 */
  hint?: string
  /** 只读权限探针：被拒的动作说明子账号策略被收窄 */
  probes?: CdnZoneProbe[] | null
}

/** 一键接入里的一步。失败时 ok=false，detail 是云厂商原文。 */
export interface CdnProvisionStep {
  step: string
  ok: boolean
  detail: string
}

/**
 * POST /api/cdn/provision：业务失败仍是 HTTP 200，靠 ok / steps[].ok 判断，
 * 所以调用方必须按 steps 逐条展示，不能只看顶层 ok。
 */
export interface CdnProvisionResult {
  ok: boolean
  /** 成功后客户端应连接的「域名:端口」 */
  connect_addr?: string
  steps: CdnProvisionStep[] | null
}

/** 站点下的一条加速域名（字段名跟腾讯云 JSON 一致）。 */
export interface CdnDomain {
  DomainName: string
  /** EdgeOne 分配的 CNAME，需要（手工或自动）指到 DNS */
  Cname: string
  DomainStatus: string
  IdentificationStatus: string
  /** 回源端口：应该是 frps 的 bindPort，否则这条域名连不上 */
  HttpOriginPort: number
  HttpsOriginPort: number
}

/** POST /api/cdn/domains */
export interface CdnDomainsResult {
  ok: boolean
  domains: CdnDomain[] | null
}

/** POST /api/cdn/domain-create：existed=true 表示复用已有域名，没有新建。 */
export interface CdnDomainCreateResult {
  ok: boolean
  existed: boolean
  cname: string
  domain: string
}

/* ---------- 一键把 tcp 隧道暴露成域名并接入 CDN ---------- */

/** POST /api/cdn/expose 里的一步。失败时 ok=false，detail 是云厂商原文。 */
export interface CdnExposeStep {
  step: string
  ok: boolean
  detail: string
}

/**
 * 把某条 tcp 隧道背后的本地服务**另建一条 http 隧道**并接到 EdgeOne。
 *
 * 原 tcp 隧道保留不动，所以两条入口并存 —— 界面必须把这一点说清楚，
 * 否则用户会以为「接入 CDN」把原来的端口入口改掉了。
 */
export interface CdnExposeResult {
  url?: string
  domain?: string
  /** EdgeOne 分配的 CNAME */
  cname?: string
  /** 新建（或复用）的 http 隧道 id */
  tunnel_id?: number
  /** 后端一定会回传（Go 侧非 omitempty），但空数组也可能序列化成 null */
  steps: CdnExposeStep[] | null
  warnings?: string[] | null
  /** 需要放行的 vhost 端口（安全组入方向 TCP） */
  need_firewall_port?: number
}

/**
 * POST /api/cdn/expose 的响应。
 *
 * 业务失败仍是 HTTP 200 且 ok=false，所以调用方必须逐条看 steps[].ok，
 * 不能只信顶层 ok；参数/前置条件不满足时才会是 4xx 纯文本（走 ApiError）。
 */
export interface CdnExposeResponse {
  ok: boolean
  result?: CdnExposeResult | null
}

/** 一次云厂商调用的审计记录（GET /api/cdn/audit）。 */
export interface CdnAuditEntry {
  at: string
  service: string
  action: string
  ok: boolean
  ms: number
  detail?: string
  request_id?: string
  /** true = 会改变云上资源的动作 */
  write: boolean
}
