import { useEffect, useRef } from 'react'

/**
 * 让「页面跑着旧代码」这件事自己被发现、自己修好。
 *
 * 面板自更新换的是二进制，但**已经打开的标签页不会重新拉 index.html** ——
 * 内存里跑的还是更新前那份 JS。于是用户点了「更新到 x.y.z」、看到「更新成功」，
 * 界面上却找不到新功能（典型症状：「左侧怎么没有这个菜单」「点了没反应」），
 * 而且换台设备/换个浏览器又是对的，极难自查。只能靠用户自己想到按 Ctrl+F5。
 *
 * 判据用入口脚本名：Vite 产出的文件带内容哈希（index-D12qKqd1.js），
 * 内容一变名字就变，所以「我这边的名字」和「服务器 index.html 引用的名字」
 * 不一致，就等价于「我这个标签页过期了」。比版本号更准 —— 版本号对不上也可能
 * 只是后端改动，而文件名不同必然是前端产物换了。
 */
function ownAsset(): string {
  try {
    // import.meta.url 指向当前这份入口 chunk，例如 /assets/index-D12qKqd1.js
    const u = new URL(import.meta.url)
    return u.pathname.split('/').pop() || ''
  } catch {
    return ''
  }
}

const RELOADED_KEY = 'frpeasy:ui-reloaded-for'

export function useUiAutoRefresh(onStale?: () => void) {
  const cb = useRef(onStale)
  cb.current = onStale

  useEffect(() => {
    const own = ownAsset()
    // 开发态（vite dev）入口是 /src/main.tsx，没有内容哈希，不参与比较
    if (!/^index-[A-Za-z0-9_-]+\.js$/.test(own)) return

    let stop = false
    let reloaded = false

    const check = async () => {
      if (stop || reloaded) return
      try {
        const r = await fetch('./api/ui/build', { cache: 'no-store' })
        if (!r.ok) return
        const d = (await r.json()) as { asset?: string }
        const asset = d.asset || ''
        if (!asset || asset === own || stop) return

        // 同一个新产物只强制刷一次。万一服务端 index.html 被换成一份引用
        // 别的名字的产物（比如 --ui-dir 指到半成品目录），也不会来回刷。
        if (sessionStorage.getItem(RELOADED_KEY) === asset) return
        sessionStorage.setItem(RELOADED_KEY, asset)

        reloaded = true
        cb.current?.()
        window.setTimeout(() => window.location.reload(), 1500)
      } catch {
        /* 面板正在重启时请求会失败，下一轮再试 */
      }
    }

    // 面板更新完回来的那几秒最容易命中，所以先快后慢各查一次
    const first = window.setTimeout(() => void check(), 3000)
    const timer = window.setInterval(() => void check(), 15000)
    return () => {
      stop = true
      window.clearTimeout(first)
      window.clearInterval(timer)
    }
  }, [])
}
