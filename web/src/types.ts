/**
 * 与 Go 侧 JSON 严格对应的类型。
 *
 * 对应关系（改动任何一边都要同步这里）：
 *   internal/shared/model.go     Tunnel / Agent / DiscoveredService
 *   internal/panel/frps.go       Status() 的 map 字段
 *   internal/panel/ports.go      PortNeed / PortCheck / FirewallInfo / FWCommand
 *   internal/panel/probe.go      DialOutcome / ExtProbeResult
 *   internal/panel/tunnels.go    listTunnelsHandler / settingsHandler
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
