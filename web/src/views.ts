/** 视图清单：导航、标题、hash 路由都用这一份，避免三处各写一遍。 */

export type View =
  | 'overview'
  | 'tunnels'
  | 'clients'
  | 'diagnose'
  | 'server'
  | 'ports'
  | 'files'
  | 'settings'

export interface ViewItem {
  key: View
  icon: string
  label: string
}

/**
 * 侧边栏按「你在这页到底在干什么」分成三组。
 *
 * 分组不是为了好看，是因为全部平铺之后有两个具体问题：
 *  - 「frps 服务端」「端口与防火墙」「文件」回答的都是**这台服务器**的情况，
 *    却和「隧道」「客户端」混在一列，找东西得从头读一遍；
 *  - 版本相关的入口原来散在「设置 → 版本与更新」和「文件」两处，用户根本
 *    不知道该去哪看、去哪点更新（真实反馈：「功能感觉有点混乱」）。
 *
 * 现在每条视线只解决一件事：**同一类东西必须落在同一组里**。新增页面前
 * 先想清楚它属于 组建隧道 / 这台服务器 / 系统 哪一组，不要平铺到最上层。
 */
export const VIEW_GROUPS: { key: string; title: string; items: ViewItem[] }[] = [
  {
    key: 'tunnel',
    title: '组建隧道',
    items: [
      { key: 'overview', icon: '▣', label: '概览' },
      { key: 'tunnels', icon: '⇄', label: '隧道' },
      { key: 'clients', icon: '▤', label: '客户端' },
      // 体检和隧道是同一件事的两面：「怎么建」在隧道页，「为什么慢」在这页。
      // 放在同一组，用户问"隧道卡"时不用去「系统」里翻。
      { key: 'diagnose', icon: '◎', label: '链路体检' },
    ],
  },
  {
    key: 'server',
    title: '这台服务器',
    items: [
      { key: 'server', icon: '◉', label: 'frps 服务端' },
      { key: 'ports', icon: '◈', label: '端口与防火墙' },
      // 二进制（frps / frpc / agent）和它们的版本、以及面板自身的更新是同一
      // 件事：「这台机器上有哪些文件、都是什么版本」。放一起才不会两头找。
      { key: 'files', icon: '▦', label: '文件与更新' },
    ],
  },
  {
    key: 'system',
    title: '系统',
    items: [{ key: 'settings', icon: '⚙', label: '设置' }],
  },
]

/** 扁平列表：顶栏标题、isView、hash 路由都用它，顺序以分组定义为准。 */
export const VIEW_ITEMS: ViewItem[] = VIEW_GROUPS.flatMap((g) => g.items)

export const VIEW_TITLES = Object.fromEntries(
  VIEW_ITEMS.map((v) => [v.key, v.label]),
) as Record<View, string>

export function isView(v: string): v is View {
  return VIEW_ITEMS.some((it) => it.key === v)
}
