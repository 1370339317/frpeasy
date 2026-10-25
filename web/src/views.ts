/** 视图清单：导航、标题、hash 路由都用这一份，避免三处各写一遍。 */

export type View = 'overview' | 'tunnels' | 'clients' | 'server' | 'ports' | 'files' | 'settings'

export const VIEW_ITEMS: { key: View; icon: string; label: string }[] = [
  { key: 'overview', icon: '▣', label: '概览' },
  { key: 'tunnels', icon: '⇄', label: '隧道' },
  { key: 'clients', icon: '▤', label: '客户端' },
  { key: 'server', icon: '◉', label: '服务端 frps' },
  { key: 'ports', icon: '◈', label: '端口 / 防火墙' },
  { key: 'files', icon: '▦', label: '文件' },
  { key: 'settings', icon: '⚙', label: '设置' },
]

export const VIEW_TITLES = Object.fromEntries(
  VIEW_ITEMS.map((v) => [v.key, v.label]),
) as Record<View, string>

export function isView(v: string): v is View {
  return VIEW_ITEMS.some((it) => it.key === v)
}
