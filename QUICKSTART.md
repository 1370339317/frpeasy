# frpeasy 上手说明

只想尽快装上用起来 → 直接看[第二节](#二装到服务器的三种方式速查)。
第一次接触、想先摸清楚它怎么跑 → 从[第一节](#一本地体验10-分钟推荐先做这个)的本地体验开始,
零成本,不需要公网 IP 和域名。

**部署后要开哪些端口、浏览器访问什么地址,见
[README 的端口表](README.md#装完以后开哪几个端口访问什么地址)。**

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

## 二、装到服务器的三种方式（速查）

详细命令和注意事项见 [README 的安装一节](README.md#安装三种方式任选其一)，这里只列差异。

| 方式 | 一句话 | 更新源 |
|---|---|---|
| **A 一键脚本** | `curl -fsSL .../deploy.sh \| sudo bash` | 自动写进 systemd 环境变量 |
| **B 绿色包** | 下 `tar.gz`，解压跑二进制 | 要自己带 `FRPEASY_UPDATE_BASE` |
| **C 源码** | 自己编 → [第六节](#六自己编译然后放上去开发者路线)，用 `scripts/frpeasy.service` 装服务 | 模板里已填好 |

A 和 B 的完整命令在 [README 的安装一节](README.md#安装三种方式任选其一)，这里只补差异部分。

一键脚本的可调项：

```bash
# 换端口
curl -fsSL .../deploy.sh | sudo PORT=9000 bash

# 换目录 + 换端口（先落盘再执行，参数写法更顺）
curl -fsSLO .../deploy.sh && sudo bash deploy.sh --port 9000 --prefix /srv/frpeasy

# 不注册 systemd，只放二进制
curl -fsSL .../deploy.sh | sudo NO_SERVICE=1 bash
```

**重复执行就是升级**：二进制换掉，`data` 目录（含数据库）原样保留，
旧二进制备份成 `frpeasy-panel.prev`，新版起不来可以立刻换回去。

### 装完之后：更新源是怎么自动配好的

方式 A 会把仓库的 Release 地址写进 unit 的 `Environment=FRPEASY_UPDATE_BASE=...`。
面板启动时读取它 —— **仅当数据库里该值为空时生效**，所以你在设置页手改过之后，
重启不会把你的改动冲掉。

裸跑（方式 B）时自己带上即可：

```bash
FRPEASY_UPDATE_BASE='https://github.com/1370339317/frpeasy/releases/latest/download' \
  ./frpeasy-panel --data ./data --listen :8080
```

没配也能用，只是「检查更新」会显示「未配置更新源」—— 那不是故障，只是没告诉它去哪找。

---

## 三、客户端平台与安装/升级

面板「客户端」页有四个页签，复制哪条都行，脚本都是**幂等**的。

| 平台 | 命令形式 | 装完的自启行为 | 安装位置 |
|---|---|---|---|
| Linux | `curl -sSL <面板>/install \| sh -s -- --server ... --token ...` | systemd / OpenRC 注册开机自启；两者都没有则挂看护循环（开机自启需手动） | `/usr/local/bin/frpeasy-agent`，数据 `/var/lib/frpeasy-agent` |
| Windows | `powershell -ExecutionPolicy Bypass -Command "iwr .../install.ps1 ..."` | 计划任务（SYSTEM 身份，开机即启） | `%ProgramData%\frpeasy-agent` |
| Android（adb，免 root） | `adb shell "curl ... \|\| toybox wget ...; sh /data/local/tmp/frpeasy-install.sh ..."` | **只启动一次，不注册自启、不挂看护** | `/data/local/tmp/frpeasy-agent` |
| Android（root） | 同上，外面套 `su -c '...'` 或加 `--elevate` | **同上，只启动一次** | 同上 |

**Android 有意不注册任何自启。** 原因是不猜 `su` 的实现：Magisk / KernelSU / APatch /
adbsu 的自启目录各不相同，系统版本差异又大，写错位置轻则静默无效，重则在系统升级后
留下一份失效脚本 —— 猜错的代价远大于收益。所以 Android 上安装器只做「正常启动一次」，
之后启停完全归你：

```bash
sh <数据目录>/run.sh start|stop|restart|status|log
```

（旧版本可能留下过自启文件，安装器会主动清掉 `$MAGISK_BOOT` 和
`~/.termux/boot/frpeasy-agent.sh` 这两个位置的残留。）

Android 上的 agent 是**linux 静态二进制**（Go 没有 `GOOS=android` 的可执行构建；
arm64-v8a → `linux/arm64`，armeabi-v7a → `linux/arm`，frp 官方也是这么发的）。
面板侧 `/api/agent/bin/android/{arch}` 会自动映射。

### 重跑就是升级 / 修复

同一条安装命令重复执行不会装出两份，也不会把机器搞成"半个客户端"：

1. 先把新二进制下到临时文件，跑 `--version` 校验（能跑才算数：截断、404 页面、
   架构不对都会被拦下）；
2. 校验通过后才停旧服务、清掉陈旧 unit / pidfile / 残留进程；
3. 原子替换，重新注册自启（Android 除外）并启动；
4. 等进程真的起来才算成功，否则如实报错。

下载失败时**旧安装原样保留、继续运行**。Windows 额外多做一步：替换前把旧 exe
备份成 `.bak`，新版本 20 秒内起不来就自动回滚到旧版本。

卸载：`sh frpeasy-install.sh --uninstall`（Linux/Android）、`.\install.ps1 -Uninstall`（Windows）。

---

## 四、端口 / 防火墙页

**要开哪些端口、默认值是多少,先看
[README 的端口表](README.md#装完以后开哪几个端口访问什么地址)。** 这一页是帮你去开它们的。

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

> 注意云服务器是**两层**防火墙：控制台的「安全组」和机器里的 ufw/iptables。
> 只开一层照样访问超时,这是最常见的「明明放行了还是连不上」。

---

## 五、已知坑（会踩的）

1. **忘记改默认口令**：面板暴露公网 + `admin/admin`，等于把内网送人。页面有红条提醒，别忽略。
2. **端口没放开**：隧道建好了但访问超时，99% 是安全组/防火墙没开那个端口（面板端口、
   frps 的 7000、隧道分到的公网端口，三个里少一个都连不通）。见
   [README 端口表](README.md#装完以后开哪几个端口访问什么地址)。
3. **没填公网地址又用内网 IP 访问面板**：面板会用你访问的地址（如 192.168.x.x）生成安装命令，客户端在公网侧连不上。公网部署请显式填公网地址。
4. **国内拉 frps/frpc 慢**：面板设置里配镜像，别硬等。
5. **流量数字看着不对**：显示的是 **今日累计**（来自 frps dashboard），不是历史总量，
   每天归零。dashboard 不可用时该项回退为 0，同时隧道在线状态改用端口探测判定。
6. **改了前端却没重新构建**（最容易踩）：前端是独立子项目（`web/`），产物由 Vite 构建到 `internal/webui/dist`，再由 `go:embed` 在**编译期**打进二进制。少跑一步就会打开旧页面，看起来像"新功能没做"。
   - 正确姿势：根目录 `make build`（先建前端再编 Go），或手动 `cd web && npm run build && cd .. && go build ./cmd/panel`；
   - 换掉旧进程再启动新二进制（`taskkill /F /IM frpeasy-panel.exe`，或先 Ctrl+C）；
   - 看侧栏左下角的 `版本 · 构建时间`，时间没变就是没换上新二进制；
   - 改样式时用 `make web-dev`（Vite HMR，改完浏览器自动刷新，接口代理到 8080），完全不用编译 Go；
   - `index.html` 已强制 `no-store`，`/assets/` 下带 hash 的文件长缓存 —— 升级必生效，重复访问不重复下载。

---

## 六、自己编译然后放上去（开发者路线）

> 只是想装到服务器上,用一键脚本就够了,看 [README](README.md#安装三种方式任选其一)。
> 这一节是「我要改代码 / 我要自己编」的路径。

### 前置（少一条就跑不通）

| 项 | 说明 |
|---|---|
| 公网 IP | 云服务器需要公网 IP,且不是 NAT 后面 |
| 端口 | 要放开哪几个,见 [README 的端口表](README.md#装完以后开哪几个端口访问什么地址) —— 简言之 8080(面板)、7000(agent 接入)、隧道分到的公网端口 |
| Alpine 版本 | 任意。产物静态链接(无 PT_INTERP),musl 直接跑 |
| 拉取通路 | 服务器要能拉到 GitHub;拉不动就在面板设置里填镜像前缀(如 `https://ghfast.top/`) |

### 构建

```bash
# 前端只需构建一次（与目标平台无关，纯静态资源）
cd web && npm ci && npm run build && cd ..

# 再按目标平台交叉编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/frpeasy-panel-linux-amd64 ./cmd/panel
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/frpeasy-agent-linux-amd64  ./cmd/agent
```

（arm64 同理换 `GOARCH=arm64`。根目录 `make build-alpine` 会把所有平台一次编完。）

### 部署

```bash
scp bin/frpeasy-panel-linux-amd64 root@你的服务器:/opt/frpeasy/frpeasy-panel
```

然后在服务器上装成服务。**直接渲染仓库里的 unit 模板,不要手写** ——
模板里的 `Restart=on-failure` 和 `KillMode=process` 是面板自更新的前提,
手写成 `Restart=always` 会让「点更新」永远回滚(原因见
[docs/CONVENTIONS.md](docs/CONVENTIONS.md#b2-systemd-托管时交接器必须用-systemctl-启停不能自己-spawn) 的 B2):

```bash
# 在仓库根目录执行，把 unit 渲染好推到服务器
sed -e 's|@@PREFIX@@|/opt/frpeasy|g' \
    -e 's|@@LISTEN@@|:8080|g' \
    -e 's|@@UPDATE_BASE@@|https://github.com/1370339317/frpeasy/releases/latest/download|g' \
    scripts/frpeasy.service | ssh root@你的服务器 'cat > /etc/systemd/system/frpeasy.service && systemctl daemon-reload && systemctl enable --now frpeasy'
```

浏览器打开 `http://你的服务器IP:8080`，`admin`/`admin` 登录 → **先改口令** → 设置里填：

- **公网地址**：你的服务器公网 IP（或域名）。填了它，安装命令和 agent 连 frps 都用它。
- **镜像前缀**：国内建议 `https://ghfast.top/`（实测约 483KB/s，直连 GitHub 约 20KB/s）

手动部署还要多做一步:把 agent 二进制放到**面板可执行文件同级目录**,命名
`frpeasy-agent-linux-amd64`,否则客户端跑一键命令会 404:

```bash
scp bin/frpeasy-agent-linux-amd64 root@你的服务器:/opt/frpeasy/
```

之后就是在面板里建隧道 → 复制「Linux 一键命令」→ 到要被暴露的那台机器上执行。

---

## 七、项目结构（前端是独立子项目）

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

## 八、自检脚本

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

---

## 九、发布新版本（维护者看）

更新源就是一个**放 `manifest.json` 加各平台二进制的静态目录**，不需要 API、
不需要 token、不会被限流。GitHub Release、自己 nginx、对象存储都行。

### 打 tag 就自动发（已配好 GitHub Actions）

```bash
git tag v0.5.1 && git push origin v0.5.1
```

`.github/workflows/release.yml` 会自动：构建前端 → 跑单元测试 →
交叉编译 10 个产物 → 生成清单 → 打绿色包 → 算 SHA256SUMS →
建 Release 并上传全部资产。用的是仓库内置 token，不用配 secret。

**别手工往 Release 里拖文件**，漏传一个 `manifest.json` 就会让所有客户端
和面板都升不上去，而且没有任何报错。

### 想本地打一遍

```bash
make dist VERSION=0.5.1 NOTES="这次改了什么"
```

交叉编译全部平台到 `bin/` → 生成 `bin/manifest.json`（含每个文件的 sha256）→
打绿色包到 `bin/bundles/` → **立刻校验清单与文件是否严格一致**。
任何一步不过都会非 0 退出。

### 更新源填什么

```
https://github.com/1370339317/frpeasy/releases/latest/download
```

`latest` 永远指向最新 Release，以后发版都不用改这个地址。
自建静态目录就填目录本身，不要填到文件名。

### 发布前的两条硬规矩

1. **改过内容就必须改版本号。** 同版本号配不同 sha256，会让已装这个版本的
   客户端判定「已最新」却实际文件不同——它按版本号短路，根本不会去看哈希。
2. **打完包之后不要再动 `bin/`。** `make build`（本地开发）会把本机 agent
   覆盖进 `bin/` 且不更新清单。上传前养成习惯：

```bash
make manifest VERSION=0.5.1 && make check-manifest
```

`check-manifest` 会逐个核对 sha256 与大小，并检查目录里有没有清单未登记的产物。
它报错时**不要**跳过：客户端拿到哈希不符的包会抛错拒装，现象是「升级一直失败」，
而面板侧看起来一切正常（清单版本比客户端新，于是不停下发）。

### 客户端自更新的自举限制

装着的版本若早于「支持自更新的那一版」，它不认识心跳里的 `agent_update` 字段，
**只能靠重跑一次安装命令**进入自动更新通道。面板「客户端」页把这种显示为
「旧版（未上报版本）」，不是故障。

### CI 会跑什么

每次推 main：构建前端 → `go test` → `make dist` → `tests/deploy-smoke.sh`
（在 ubuntu runner 上用一个本地静态目录冒充 Release，真跑一遍一键部署：
安装、启动、healthz、重复执行幂等、篡改产物必须被拒）。

`it.sh` / `e2e.sh` 还没进 CI —— 它们里面有给 Windows 开发机写的进程清理，
在 Linux 上未实测过，不想让 CI 因为无关原因变红。
