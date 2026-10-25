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
  hosted: string
  hosted_sha256: string
  hosted_size: number
  latest: string
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

export interface TunnelsResponse {
  tunnels: Tunnel[]
  agents: Agent[]
  frps: FrpsStatus
  install_command: string
  install_command_windows: string
  install_command_android: string
  install_command_android_root: string
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

export interface SettingsBody {
  public_addr: string
  mirror_base: string
  port_range_start: string
  port_range_end: string
  frps_bind_port: string
  dashboard_port: string
  frps_token?: string
  new_password?: string
  update_base?: string
  agent_auto_update?: string
}

export interface SettingsSaveResult {
  ok?: boolean
  public_addr_changed?: boolean
  affected_tunnels?: number
  client_pickup_second?: number
  frps_restarting?: boolean
  frps_bind_port?: number
}
