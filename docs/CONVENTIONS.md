# 改代码前必须知道的硬约束

这些不是风格偏好,是踩过坑之后定下来的。每一条都对应一个「改错了会静默失败、
而且现象看起来完全像别的原因」的场景。改之前先读完,改完跑一遍对应测试。

---

## A. 版本与发布

### A1. 面板是唯一的版本源

客户端只从 `/api/agent/bin/{os}/{arch}` 取二进制,**永不直连 GitHub**。
所以「面板自带哪一版 agent」才是权威,`manifest.json` 里的 agent 资产只用于提示。

改这里会导致:内网机器明明能连面板,却因为尝试访问 GitHub 而升级失败。

### A2. 发布前必须 `make check-manifest`

清单里的 sha256 一旦与产物不符,客户端会校验失败拒装 —— 现象是
**「升级一直失败」而面板侧完全正常**(清单版本比客户端新,于是不停下发)。

`make build` 里的 `stage-local` 会把本机 agent 覆盖进 `bin/` 而同平台的旧清单
不更新,这是最常见的失配来源。上传前养成习惯:

```bash
make manifest VERSION=x.y.z && make check-manifest
```

所有 `go build` 都带 `-trimpath`,保证同一源码在不同目录构建出字节相同的产物。

### A3. 更新源的初始值来自 `FRPEASY_UPDATE_BASE`,但只在 DB 里该值为空时生效

`update_base` 为空串的语义是「不检查更新」,所以一键部署脚本靠这个环境变量
把面板接到 Release 上;**而用户在设置页手改过之后,DB 里有值,重启不再覆盖他**。

这条链断了不会报错,只会表现为「点检查更新什么都不发生」。
`deploy.sh` 写进 unit 的 `Environment=FRPEASY_UPDATE_BASE=...` 就是为了接上它。

---

## B. 自更新

### B1. 面板自我替换不能自己重启自己

旧进程先把自己复制成 handoff 副本,由副本等旧 PID 退出、换文件、拉起、
再比对 `version` 字段确认成功,失败则回滚 `.prev`。

**健康检查必须比对 `version` 字段**,只判 HTTP 200 会把「旧进程还没退干净」
当成成功 —— 于是报告升级成功,实际还是旧版本在跑。

### B2. systemd 托管时,交接器必须用 `systemctl` 启停,不能自己 spawn

配套的 unit 必须是 `Restart=on-failure` + `KillMode=process`,三处缺一不可:

- **`on-failure`**:面板交接时 exit 0 主动退出,systemd 认为正常退出不重启,
  给交接器一段干净的时间换文件。用 `always` 会和交接器抢端口,更坏的情况是
  旧版抢先占住端口,交接器把新版本的启动失败误判成「新版本有问题」而回滚 ——
  **升级永远升不上去,日志还会说是新版本的错**。
- **`KillMode=process`**:交接器是面板的子进程、同一 cgroup,默认策略会在服务
  停止时把它一起清掉 —— 正好在「面板已退出、交接器要换文件」这个最脆弱的时刻。
- **交接器自己 `spawn` 也不行**:起来的进程不在 unit 的 cgroup 里,
  `systemctl status/stop` 管不到,下次重启就两份进程抢端口。

`startTarget` / `stopTarget` 与三个测试守着这条。unit 模板只有一份:
`scripts/frpeasy.service`(`deploy.sh` 从更新源拉的就是它,绿色包里那份是同一文件的渲染结果)。

### B3. 二进制查找顺序固定:面板同级 → 同级 `bin/` → `<data>/bin`

改这里会导致「装的时候能下、升级时 404」。

---

## C. 网络与端口

### C1. 面板对外地址一律走 `resolvePanelBase(r)` / `resolveFRPSAddr(r, port)`

禁止直接读 `public_addr`。该值默认为空,直接用会生成 `<你的服务器IP>` 这种
复制到终端就报错的占位符,或者 `FRPSAddr` 是空串导致 frpc 连不上。

优先级:设置项 → 请求 Host(采信 `X-Forwarded-Proto` / `r.TLS`)→ `127.0.0.1:<listenPort>`。

### C2. frp ≥ 0.52 的 TOML 必须是新版顶层驼峰键

```
frps:  bindPort / auth.method / auth.token
frpc:  serverAddr / serverPort / auth.* / [[proxies]]
```

不能沿用旧 INI 的 `[common]` / `server_addr` / `remote_port`。有测试守着
(断言生成的配置里不出现这些旧键)。

### C3. 端口池默认 10000–20000,frps bindPort 默认 7000,dashboard 默认 7500

dashboard **只监听 127.0.0.1**,是面板读取真实代理状态的通道,不对外暴露,
也不需要放行。端口清单的唯一来源是 `ports.go`,文档里不要另抄一份。

---

## D. 前端与构建

### D1. 前端产物不入库,但必须 embed

链路:`web/src` → Vite → `internal/webui/dist` → `go:embed`。

`vite.config.ts` 里的 `base: './'` **不能改成 `/`** —— 面板可能挂在反代子路径下,
绝对路径会 404。有测试守着。

`internal/webui/dist/.keep` 必须入库:真实产物不入库,缺了它干净 clone 编不过
(`go:embed all:dist` 无匹配)。

缓存策略:`/assets/*` immutable 长缓存,`index.html` 强制 `no-store`
(否则升级后仍显示旧页)。

### D2. 类型双写

Go 结构体与 `web/src/types.ts` 必须逐字对齐,没有编译期约束。改一个就要改另一个。

---

## E. 下发的脚本

`internal/panel/scripts/*.sh|*.ps1` 的换行符**必须是 LF**,由 `.gitattributes` 钉死。

它们经 HTTP 原样下发给客户端:`CRLF` 会让 `curl | sh` 报 bad interpreter。
而 `install.ps1` 还必须保持**纯 ASCII** —— PowerShell 5.1 在中文系统按 GBK 解码,
会把中文注释里的字节吃进相邻引号,脚本直接语法错误。

可执行位也要注意:`deploy.sh` 在 Windows 上编辑后 git 里会变成 644,
`./deploy.sh` 就 permission denied(看起来像脚本坏了)。用
`git update-index --chmod=+x <file>` 单独提交。

---

## F. 日常开发命令

| 场景 | 命令 |
|---|---|
| 改前端,实时看效果 | `make web-dev`(HMR,不用编 Go) |
| 只改后端 | `make build-go` |
| 改前端并验证最终形态 | `make build` |
| 交叉编译全部平台 | `make build-alpine` |
| 打发布包 | `make dist VERSION=x.y.z` |
