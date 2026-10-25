# frpeasy

把 frp 配置白痴化的单二进制面板 —— 浏览器里点几下就能把内网服务映射到公网，
不用手写 TOML，不用记端口，不用碰命令行。

## 它是什么

一个 Go 二进制内嵌 React 面板，零外部依赖（不需要 Redis / Nginx / Docker）：

- **面板**内置 frps，运行时按需拉官方 frp 二进制（默认 GitHub，可一键切国内镜像）。
  在你点「新建隧道」之后，frps 才真正对外提供端口。
- **客户端 Agent** 一条命令装上，自动探测本机正在监听的服务，
  把「哪台机器 → 哪个端口」替你填好；装完保活 frpc、周期上报状态。
- **三层版本自更新**：面板自身、客户端 agent、frp 本体各自独立升级。
  客户端**永不直连 GitHub**，只从面板取二进制 —— 内网无外网也能升级。
- 数据全在一个 SQLite 文件里，备份就是拷一个文件。

支持 Linux（amd64 / arm64 / armv7 / 386）、Windows（amd64 / arm64）、
Android（arm64-v8a / armeabi-v7a，复用 linux 静态产物）。

## 快速开始

完整步骤见 [QUICKSTART.md](QUICKSTART.md)（本地体验 → 实机部署 → 客户端安装与升级）。
最短路径：

```bash
cd web && npm ci && npm run build && cd ..   # 前端产物不入库，首次必须构建
make build                                    # 编译面板与客户端，并把本机 agent 放进 bin/
./frpeasy-panel --data ./data --listen :8080
```

浏览器打开 <http://127.0.0.1:8080>，用 `admin` / `admin` 登录，
**登录后第一件事是改口令**（页面顶部会红条提醒）。

## 目录结构

```
cmd/
  panel/          面板入口（含 --handoff 交接模式，用于自我更新）
  agent/          客户端入口（--version / --selftest / --reexec-wait）
  genmanifest/    生成自更新清单 manifest.json 与 sha256
internal/
  panel/          面板：HTTP 接口、SQLite 仓储、frps 管理、自更新
    scripts/      下发给客户端的一键安装脚本（install.sh / install.ps1）
  agent/          客户端：注册、心跳、服务探测、frpc 守护、自更新
  shared/         面板与客户端共用：数据模型、协议常量、版本比较、清单、镜像
  webui/          go:embed 前端产物（dist 由 Vite 生成，不入库）
web/              前端子项目：React 19 + TypeScript + Tailwind v4 + Vite 7
tools/            开发辅助脚本（按函数切分源文件、粗粒度死代码检查）
```

## 测试

```bash
go test ./...                # 单元测试（含跨平台编译守卫）
bash it.sh                   # 59 条 API 断言
bash e2e.sh                  # 11 条真实穿透断言（真拉 frps + frpc）
bash selfupdate-e2e.sh       # 19 条面板自更新断言（含崩溃回滚）
bash agent-update-e2e.sh     # 17 条客户端自更新断言
```

四个脚本都会自主启动独立端口与数据目录，可反复执行不互相污染。

## 改代码前必须知道的约定

这些不是风格偏好，是踩过坑之后定下来的硬约束：

1. **面板是唯一的版本源。** 客户端只从 `/api/agent/bin/{os}/{arch}` 取二进制，
   不直连 GitHub。所以「面板自带哪一版」才是权威，清单里的 agent 资产只用于提示。
2. **二进制查找顺序固定**：面板同级 → 同级 `bin/` → `<data>/bin`。
   改这里会导致「装的时候能下、升级时 404」。
3. **面板对外地址一律走 `resolvePanelBase(r)` / `resolveFRPSAddr(r, port)`**，
   禁止直接读 `public_addr`。该值默认为空，直接用会生成 `<你的服务器IP>` 这种
   复制到终端就报错的占位符。
4. **frp ≥ 0.52 的 TOML 必须是新版顶层驼峰键**（`bindPort` / `serverAddr` /
   `[[proxies]]`），不能沿用旧 INI（`[common]` / `server_addr`）。有测试守着。
5. **前端产物不入库，但必须 embed。** `web/src` → Vite → `internal/webui/dist` →
   `go:embed`。`vite.config.ts` 里的 `base: './'` 不能改成 `/`（面板可能挂在反代子路径下）。
6. **`internal/panel/scripts/*.sh|*.ps1` 的换行符必须是 LF**，由 `.gitattributes`
   钉死。它们经 HTTP 原样下发：CRLF 会让 `curl | sh` 报 bad interpreter，
   而 `install.ps1` 还必须保持纯 ASCII（PowerShell 5.1 在中文系统按 GBK 解码会破坏引号）。
7. **面板自我替换不能自己重启自己。** 旧进程先把自己复制成 handoff 副本，
   由副本等旧 PID 退出、换文件、拉起、再比对版本号确认成功，失败则回滚 `.prev`。
   健康检查必须比对 `version` 字段，只判 HTTP 200 会把「旧进程还没退干净」当成功。
