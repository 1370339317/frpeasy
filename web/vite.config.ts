import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// 构建产物直接落到 Go 侧的嵌入目录，避免多一次拷贝步骤。
// internal/webui/embed.go 用 //go:embed all:dist 把它打进二进制。
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // 用相对路径引用资源：面板可能挂在反向代理的子路径下（/frpeasy/），
  // 绝对路径 /assets/... 会 404，而 ./assets/... 在哪一层都能取到。
  base: './',
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    // 产物要小：frpeasy 的整体卖点是单文件二进制，前端不该拖后腿
    target: 'es2022',
    cssCodeSplit: false,
    reportCompressedSize: false,
  },
  server: {
    port: 5173,
    // 开发期直连后端：npm run dev 时先起 panel(:28080)，前端走 HMR，不用重新编译 Go
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:28080',
        changeOrigin: true,
      },
    },
  },
})
