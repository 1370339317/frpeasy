# frpeasy

把 frp 配置白痴化的单二进制面板。浏览器里点几下，把内网服务映射到公网 ——
不用手写 TOML，不用记端口，不用碰命令行。

零外部依赖：不需要 Nginx / Docker / Redis，数据全在一个 SQLite 文件里。

- **面板**内置 frps，运行时拉官方 frp 二进制（默认 GitHub，可一键切国内镜像）。
- **客户端 Agent** 一条命令装上，自动探测那台机器在监听什么服务，保活 frpc、上报状态。
- **三层自更新**：面板自身、agent、frp 本体各自独立升级；客户端只从面板取二进制，
  永不直连 GitHub，所以内网机器也能升级。

---

## 装完以后：开哪几个端口，访问什么地址

这是最容易卡住的一步，先给结论。

| 端口 | 干什么用的 | 要放开吗 |
|---|---|---|
| **28080** | 你浏览器打开面板用的 | **必须** |
| **27900** | 客户端 agent 连服务器用的（frps 入口） | **必须** |
| **21000–26000** | 每条隧道从这里分走一个，这就是你服务的公网端口 | **必须**（不用全开，见下） |
| 27500 | frps 内部 dashboard，只监听 127.0.0.1 | 不用 |

然后浏览器打开：

```
http://你的服务器公网IP:28080
```

默认账号 `admin` / `admin` —— **登录后第一件事是改口令**，页面顶部会有红条提醒你。

> 云服务器要**两处**都放行：云厂商控制台的「安全组」+ 机器里的防火墙。
> 少放一处就是访问超时。
>
> 端口池 21000–26000 有上万个，**不用全开**。每建一条隧道，面板会明确告诉你
> 这条分到了哪个端口；左侧「端口 / 防火墙」页把它们列全，并给你可直接粘的
> ufw / firewalld / nftables / iptables 放行命令，还带外网回访检测确认真的通了。

---

## 安装（三种方式，任选其一）

### 方式一：一条命令（推荐）

Alpine / Debian / Ubuntu / 群晖都行：

```bash
curl -fsSL https://github.com/1370339317/frpeasy/releases/latest/download/deploy.sh | sudo bash
```

它会：拉清单 → 选本机平台的产物 → 下载 → 校验 sha256 → 预检二进制能否执行 →
停旧实例 → 装到 `/opt/frpeasy` → 注册 systemd 并启动 → 轮询 healthz 确认起来了。

**国内服务器拉不动 GitHub 时**，把地址换成镜像（整条命令连 `--base` 一起换，
否则脚本还是会去 GitHub 下二进制）：

```bash
MIRROR=https://ghfast.top/https://github.com/1370339317/frpeasy/releases/latest/download
curl -fsSL "$MIRROR/deploy.sh" | sudo bash -s -- --base "$MIRROR"
```

**Alpine 最小系统没有 bash**，先装：

```bash
apk add bash curl
```

**重复执行就是升级**，会装回原来的位置、沿用原来的端口和数据目录，配置一条不动。改端口：

```bash
curl -fsSL https://github.com/1370339317/frpeasy/releases/latest/download/deploy.sh | sudo PORT=9000 bash
```

### 方式二：绿色包（想自己控制进程）

在 [Releases](https://github.com/1370339317/frpeasy/releases) 下对应的 `tar.gz`，解压后：

```bash
tar xzf frpeasy-panel-*-linux-amd64.tar.gz
chmod +x frpeasy-panel
./frpeasy-panel --data ./data --listen :28080
```

包里还有 `frpeasy.service`（可直接用的 systemd unit）和 `README.txt`。

Windows 下 `frpeasy-panel-windows-amd64.exe` 双击就跑，配置落在旁边的 `data/`。

### 方式三：从源码构建

```bash
cd web && npm ci && npm run build && cd ..   # 前端产物不入库，首次必须构建
make build
./frpeasy-panel --data ./data --listen :28080
```

---

## 用起来是什么流程

1. 浏览器登录面板（**改口令**）。
2. 左侧「客户端」页复制一条安装命令，到**要被暴露的那台机器**上执行。
   agent 会自己探测那台机器正在监听什么服务，把「哪台机器 → 哪个端口」替你填好。
3. 「新建隧道」选机器、选服务、填个名字，保存。
4. 面板告诉你分到了哪个公网端口，浏览器访问 `http://服务器IP:那个端口` 就是你的服务。

客户端覆盖 Linux（amd64 / arm64 / armv7 / 386）、Windows（amd64 / arm64）、
Android（arm64-v8a / armeabi-v7a）。

agent 进程**本身不需要 root 运行**；装到系统目录时会自动用 `sudo`（没有 sudo 就装不动
默认路径，可以用 `--data` 指到可写目录）。装完注册自启、保活 frpc、周期上报状态。
各平台的具体命令和自启方式见 [QUICKSTART 第三节](QUICKSTART.md#三客户端平台与安装升级)。

## 升级

面板自己、客户端 agent、frp 本体，三层各自独立升级。**都不用去服务器敲命令。** 升级只换二进制，**配置全部保留**（口令、隧道、客户端、公网地址、frp 版本都还在）——状态全在 `data` 目录的 SQLite 里，升级不碰它。

- **面板**：面板「设置 → 版本与更新」点「检查更新」→ 发现新版本 → 点「更新到 x.y.z」。
  它会自己下载、校验、替换二进制、重启，页面断开几秒后自动重连并回报结果；
  **新版本起不来会自动回滚到旧的**，不会把你晾在起不来的版本上。
- **客户端**：面板「客户端」页点更新即可（也能开自动跟随，客户端自己升级）。
- **frp**：设置页锁版本，面板负责拉取。

前提是「更新源」已配置。用一键脚本装的话它已经自动写好了；
手动装的可以在同一个页面填 `https://github.com/1370339317/frpeasy/releases/latest/download`。

客户端**永不直连 GitHub**，只从面板取二进制 —— 面板能上网，内网机器就不用。

> 用一键脚本重跑一次也能升级（幂等），且会**沿用原来的安装目录 / 端口 / 数据目录**——不用你记得当初装在哪。

## 卸载

```bash
# 删服务、程序、数据库 —— 像没装过一样
curl -fsSL https://github.com/1370339317/frpeasy/releases/latest/download/deploy.sh | sudo bash -s -- --uninstall

# 想保留数据库和隧道配置（重装后能接上）
curl -fsSL .../deploy.sh | sudo bash -s -- --uninstall --keep-data
```

- 会先**列出**要删的服务、程序目录、数据目录，要求输入 `yes` 才动手；
  非交互场景（CI、批量脚本）加 `--yes`。
- 装到别处也能卸：它从 systemd unit 的 `ExecStart` **反查真实安装路径**，
  不依赖你记着当初用了哪个 `--prefix`。
- 会一并清掉 frps/frpc 二进制、`.prev` 备份、以及面板被强杀时残留的 frps 子进程
  （否则重装会撞 27900 端口，而且报错完全指不到它）。
- 拒绝在 `/`、`/opt`、`/usr` 这类过浅路径上执行，防手滑。
- 防火墙/安全组的放行规则**不会**自动撤销（删别人的规则风险太大），需要你自己去清。

**客户端卸载**在面板「客户端」页，两种方式：

- **在线就点一下**：那台机器上的卡片点「卸载」，面板通过心跳把指令发下去
  （15 秒内），客户端自己停 frpc、摘掉开机自启、删程序和配置，然后离线。
  离线时指令会存着，等它下次上线立即执行。
- **复制命令去执行**：同一页有「卸载命令」，按平台分好，粘到那台机器上跑即可
  （`--uninstall`，**不需要令牌**）。适合客户端已经失联、或面板换了地址的情况。

两者都会删干净：进程、自启（systemd / OpenRC / 计划任务）、二进制、数据目录。
卡片上的「移除」只是删面板记录 —— 那台机器的 agent 还在跑，下次心跳会重新注册回来。

---

## 文档

| 文件 | 内容 |
|---|---|
| [QUICKSTART.md](QUICKSTART.md) | 本地体验、实机部署细节、客户端各平台命令、端口页说明、已知坑 |
| [docs/CONVENTIONS.md](docs/CONVENTIONS.md) | **改代码前必读**：踩过坑之后定下来的硬约束（自更新、端口解析、TOML 语法、发布流程） |

测试脚本：

```bash
go test ./...             # 单元测试
bash it.sh                # 59 条 API 断言
bash e2e.sh               # 11 条真实穿透断言（真拉 frps + frpc）
bash selfupdate-e2e.sh    # 19 条面板自更新断言（含崩溃回滚）
bash agent-update-e2e.sh  # 17 条客户端自更新断言
```

四个脚本各自用独立端口与数据目录，可反复跑、互不污染。
