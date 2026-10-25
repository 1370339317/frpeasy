# frpeasy 上手说明

两条路：**先在本地体验**（零成本，验证交互和穿透是否通），再**装到实机**。

---

## 一、本地体验（10 分钟，推荐先做这个）

本地跑的好处：不需要公网 IP、不需要开防火墙、不需要域名。全程在 127.0.0.1 上，
能把「登录 → 建隧道 → 装 agent → 真实穿透 → 看到页面」这条主链路完整走一遍。

### 前置

- Go 1.26+（编译用，见 go.mod）
- 官方 frp 二进制**不用手动下载**，面板会在运行时自动拉（建议先在设置里填镜像）

### 步骤

```bash
# 0) 先构建前端（React 子项目，产物会被 go:embed 打进二进制）
#    前端产物不入库，所以首次一定要跑这一步，否则二进制里没有页面
cd web && npm ci && npm run build && cd ..

# 1) 编译两个二进制
go build -o frpeasy-panel.exe ./cmd/panel
go build -o frpeasy-agent.exe  ./cmd/agent

# 2) 起面板（数据落在 ./data）
./frpeasy-panel.exe --data ./data --listen :8080
```

（根目录 `make build` 等价于上面三步。没装 Node 也可以用 `--ui-dir` 指向已有的
`internal/webui/dist`，或者接受页面提示「前端未构建」—— 此时接口是全好的。）

浏览器打开 <http://127.0.0.1:8080>，用 `admin` / `admin` 登录。

**登录后第一件事：去「设置」改口令。** 页面顶部会有一条红色告警提醒你。

接着：

1. 起一个本地服务当作要暴露的目标（随便什么都行）：
   ```bash
   python -m http.server 8000
   ```
2. 面板里点「新建隧道」，客户端选空（本地还没有 agent），本地服务地址手填 `127.0.0.1:8000`。
3. 本地体验可以直接在**本机**装 agent：
   ```bash
   ./frpeasy-agent.exe --server http://127.0.0.1:8080 --token <面板给出的 install_token> --data ./agent-data
   ```
   令牌在面板「设置」页或安装命令里都能拿到。
4. 回到面板，等 frps 状态变成运行中（首次会从 GitHub/镜像拉 frps，几十秒到几分钟）。
5. 浏览器访问 `http://127.0.0.1:<面板分配的公网端口>`，应该能看到你那个 8000 端口的目录列表。

**本地体验时「公网地址」可以留空** —— 面板会自动用你当前访问它的地址（127.0.0.1:8080）。

### 本地体验验证不了什么

- 真实公网可达性 / 防火墙 / 云厂商安全组（要靠「端口 / 防火墙」页的外部回访）
- 跨机器组网（agent 在另一台机器上）
- Android 分支：脚本逻辑、二进制映射、安装命令生成都验证过，但**没有在真机
  Android 上跑过**（本仓库开发环境没有 Android 设备）

---

## 二、装到实机（Alpine + 公网服务器）

### 前置清单（少一条就跑不通）

| 项 | 说明 |
|---|---|
| 公网 IP | 云服务器需要有公网 IP，且不是 NAT 后面 |
| 安全组/防火墙 | 放行 **面板端口**（如 8080）、**frps 绑定端口**（默认 7000）、以及每条隧道分到的**公网端口** —— TCP。具体是哪几个、开没开，看面板左侧「端口 / 防火墙」页，它会列全并给出可直接复制的放行命令 |
| Alpine 版本 | 任意。产物是静态链接（无 PT_INTERP），musl 直接跑 |
| 拉取通路 | 服务器要能拉到 GitHub；拉不动就在面板设置里填镜像前缀（如 `https://ghfast.top/`） |

### 构建

```bash
# 前端只需构建一次（与目标平台无关，纯静态资源）
cd web && npm ci && npm run build && cd ..

# 再按目标平台交叉编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o bin/frpeasy-panel-linux-amd64 ./cmd/panel
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o bin/frpeasy-agent-linux-amd64  ./cmd/agent
```

（arm64 同理换 `GOARCH=arm64`。根目录 `make build-alpine` 会把所有平台一次编完。）

### 部署

```bash
# 面板放上去
scp bin/frpeasy-panel-linux-amd64 root@你的服务器:/usr/local/bin/frpeasy-panel
ssh root@你的服务器
chmod +x /usr/local/bin/frpeasy-panel
mkdir -p /var/lib/frpeasy

# 想用一条 systemd 管着就跑这个
cat > /etc/systemd/system/frpeasy-panel.service <<'EOF'
[Unit]
Description=frpeasy panel
After=network.target

[Service]
ExecStart=/usr/local/bin/frpeasy-panel --data /var/lib/frpeasy --listen :8080
Restart=always

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now frpeasy-panel
```

浏览器打开 `http://你的服务器IP:8080`，`admin`/`admin` 登录 → **先改口令** → 设置里填：

- **公网地址**：你的服务器公网 IP（或域名）。填了它，安装命令和 agent 连 frps 都用它。
- **镜像前缀**：国内建议 `https://ghfast.top/`（实测约 483KB/s，直连 GitHub 约 20KB/s）

然后把 agent 二进制放到**面板可执行文件同级目录**，命名 `frpeasy-agent-linux-amd64`，
否则客户端跑一键命令会 404：

```bash
cp bin/frpeasy-agent-linux-amd64 /usr/local/bin/frpeasy-agent-linux-amd64
```

之后就是在面板里建隧道 → 复制「Linux 一键命令」→ 到要被暴露的那台机器上执行。

---

## 三、客户端平台与安装/升级

面板「客户端」页有四个页签，复制哪条都行，脚本都是**幂等**的。

| 平台 | 命令形式 | 自启方式 | 安装位置 |
|---|---|---|---|
| Linux | `curl -sSL <面板>/install \| sh -s -- --server ... --token ...` | systemd / OpenRC；都没有则用自带看护脚本 | `/usr/local/bin/frpeasy-agent`，数据 `/var/lib/frpeasy-agent` |
| Windows | `powershell -ExecutionPolicy Bypass -Command "iwr .../install.ps1 ..."` | 计划任务（SYSTEM 身份，开机即启） | `%ProgramData%\frpeasy-agent` |
| Android（adb，免 root） | `adb shell "curl ... \|\| toybox wget ...; sh /data/local/tmp/frpeasy-install.sh ..."` | 看护脚本 `run.sh supervise` | `/data/local/tmp/frpeasy-agent` |
| Android（root） | 同上，外面套 `su -c '...'` | 额外挂 `/data/adb/service.d`（Magisk 开机自启） | 同上 |

Android 说明：Go 没有 `GOOS=android` 的可执行构建，Android 上的 agent 就是
**linux 静态二进制**（arm64-v8a → `linux/arm64`，armeabi-v7a → `linux/arm`），
frp 官方对 Android 也是这么发的。面板侧 `/api/agent/bin/android/{arch}` 会自动映射。

### 重跑就是升级 / 修复

同一条安装命令重复执行不会装出两份，也不会把机器搞成"半个客户端"：

1. 先把新二进制下到临时文件，跑 `--version` 校验（能跑才算数：截断、404 页面、
   架构不对都会被拦下）；
2. 校验通过后才停旧服务、清掉陈旧 unit / pidfile / 残留进程；
3. 原子替换，重新注册自启并启动；
4. 等进程真的起来才算成功，否则如实报错。

下载失败时**旧安装原样保留、继续运行**。Windows 额外多做一步：替换前把旧 exe
备份成 `.bak`，新版本 20 秒内起不来就自动回滚到旧版本。

卸载：`sh frpeasy-install.sh --uninstall`（Linux/Android）、`.\install.ps1 -Uninstall`（Windows）。

---

## 四、端口 / 防火墙页

左侧「端口 / 防火墙」是给云服务器部署用的：

- **清单**：面板端口、frps 端口、每个已部署隧道映射出来的公网端口，以及只监听
  127.0.0.1 因而不用放行的 dashboard 端口。草稿隧道（没绑机器/没分到端口）不会列进去。
- **放行命令**：自动识别 ufw / firewalld / nftables / iptables，把匹配的那套排最前，
  逐端口放行（也可选整段端口池的省事方案）。
- **检测**分两级：
  - 「检测本机」——面板从公网地址回访自己，验证主机防火墙 + 服务是否在监听；
  - 「外部回访检测」——借公开的 `check-host.net` 境外节点从真正的外网连回来，
    同时验证云安全组。它按来源 IP 限流，被限流时会明确标为「探测不可用」，
    **不会**误报成端口关闭。

---

## 五、已知坑（会踩的）

1. **忘记改默认口令**：面板暴露公网 + `admin/admin`，等于把内网送人。页面有红条提醒，别忽略。
2. **端口池没放行**：隧道建好了但访问超时，99% 是安全组/防火墙没开那个端口。
3. **没填公网地址又用内网 IP 访问面板**：面板会用你访问的地址（如 192.168.x.x）生成安装命令，客户端在公网侧连不上。公网部署请显式填公网地址。
4. **国内拉 frps/frpc 慢**：面板设置里配镜像，别硬等。
5. **流量统计暂时恒为 0**：还没接 frpc 的 admin API，这一项是已知未实现。
6. **改了前端却没重新构建**（最容易踩）：前端是独立子项目（`web/`），产物由 Vite 构建到 `internal/webui/dist`，再由 `go:embed` 在**编译期**打进二进制。少跑一步就会打开旧页面，看起来像"新功能没做"。
   - 正确姿势：根目录 `make build`（先建前端再编 Go），或手动 `cd web && npm run build && cd .. && go build ./cmd/panel`；
   - 换掉旧进程再启动新二进制（`taskkill /F /IM frpeasy-panel.exe`，或先 Ctrl+C）；
   - 看侧栏左下角的 `版本 · 构建时间`，时间没变就是没换上新二进制；
   - 改样式时用 `make web-dev`（Vite HMR，改完浏览器自动刷新，接口代理到 8080），完全不用编译 Go；
   - `index.html` 已强制 `no-store`，`/assets/` 下带 hash 的文件长缓存 —— 升级必生效，重复访问不重复下载。

---

## 六、项目结构（前端是独立子项目）

```
frpeasy/
  cmd/panel/            panel 入口（Web 面板 + 内置 frps 管理）
  cmd/agent/            agent 入口（客户端常驻，保活 frpc）
  internal/panel/       面板后端：路由、SQLite、frps 管理、端口探测、安装脚本
  internal/agent/       agent 逻辑：注册心跳、探测本机服务、frpc 生命周期
  internal/shared/      panel 与 agent 共用的模型与工具
  internal/webui/       只做一件事：把前端产物嵌入二进制
    dist/               ← Vite 构建输出（不入库，靠 .keep 保证可编译）
  web/                  前端子项目（React 19 + TypeScript + Tailwind v4 + Vite）
    src/pages/          六个视图：概览/隧道/客户端/服务端/端口防火墙/设置
    src/components/     布局、登录、UI 原子组件（Button/Card/Table/Modal…）
    src/lib/            api 封装、格式化、主题
    src/store.tsx       全局数据层（替代旧的 S 大对象 + 手写 renderXxx）
    src/types.ts        与 Go 结构体严格对应的类型
```

前后端的唯一契约是 JSON 接口，类型集中在 `web/src/types.ts`，每个类型都注明了对应哪个 Go 文件。
前端构建产物约 **300KB（gzip 后 ~90KB）**，对单文件二进制影响很小。

**前端日常开发**（不用每次编 Go）：

```bash
# 终端 1：起后端
make build-go && ./frpeasy-panel --data ./data

# 终端 2：起前端（HMR，/api 自动代理到 8080）
make web-dev
```

**只改后端**：`make build-go`。**改前端并要验证最终形态**：`make build`。

---

## 七、自检脚本

仓库里几个脚本可直接跑，不需要额外参数：

```bash
bash it.sh                   # 59 条 API 断言：登录/建隧道/自动探测/编辑/解绑/配置隔离/二进制下发/Android 别名/端口清单与检测
bash e2e.sh                  # 11 条真实穿透断言：真拉 frps+frpc，实际访问公网端口取回页面
bash selfupdate-e2e.sh       # 19 条面板自更新断言：正常升级 / 启动即崩必回滚 / 预检版本不符拒绝
bash agent-update-e2e.sh     # 17 条客户端自更新断言：下载校验替换交接 / 不降级 / 旧进程退出 / 身份不重复
```

`e2e.sh` 可用 `MIRROR=https://ghfast.top/ bash e2e.sh` 指定镜像。
四个脚本都会自主启动独立端口和数据目录，可反复跑。

注意：所有脚本开头都会**用当前源码重新构建**被测二进制到独立文件名，
避免跑到根目录里过期的 exe 上得出错误结论。
