package panel

import _ "embed"

// installScript 客户端安装脚本（POSIX sh）。Linux / Android / Termux 共用。
//
// 正文放在 scripts/install.sh，用 go:embed 原样嵌入，而不是写成 Go 的 raw string：
//   - 编辑器能按 shell 语法高亮与跳转，不再是"一整块带反引号的字符串"；
//   - 可以脱离 Go 直接 sh -n 校验（install_test.go 校验的仍是这份字节）；
//   - 改脚本的 diff 只落在脚本文件上，不与 Go 代码混在一起。
//
// 注意 scripts/*.sh 与 *.ps1 的换行符由 .gitattributes 钉死为 LF：
// 这份字节会经 HTTP 原样下发给 `curl ... | sh` 客户端，CRLF 会让 sh 报
// "bad interpreter: no such file or directory"。
//
// 设计约束（都是踩过坑的）：
//  1. **可重复执行**：重复跑不是"再装一次"，而是"收敛到目标状态"——
//     停掉旧实例、清掉残留 unit 与 pidfile、换上本次的二进制、重新注册自启、再确认存活。
//     用户装失败/装错/出新版本时，原地重跑一条命令就该得到正确结果。
//  2. **失败不破坏现有安装**：下载 → 校验（能跑 `--version`）全部通过之后，才去停旧进程。
//     下载断了、拿到的是 404 页面、架构不对（arm64 机器装了 amd64），旧版本继续跑，
//     脚本以非 0 退出并说明原因，而不是留下一个半死的客户端。
//  3. **不用 set -e**：这是修复脚本，误杀自身于中途反而更难收拾；每步显式判定。
//  4. Android 上没有 systemd，也**不注册任何自启**：root 方案（Magisk / KernelSU /
//     APatch / adbsu）自启目录各不相同，系统版本差异又大，猜错要么静默无效、要么
//     在系统升级后残留失效脚本。按用户要求，装完只正常启动一次，之后启停完全由
//     用户用 run.sh 控制。
//
//go:embed scripts/install.sh
var installScript string

// installScriptPS1 客户端安装脚本（Windows PowerShell 5.1+）。
//
// 正文必须是纯 ASCII：Windows PowerShell 5.1 读取不带 BOM 的 .ps1 时按系统
// ANSI 代码页（简体中文机器为 GBK）解码，UTF-8 中文提示会被解成乱码，其中
// 某些字节序列还会吞掉引号，导致"表达式或语句中包含意外的标记"这类语法错误。
// 而下发路径是 `irm -OutFile`，原样落字节、不带 BOM，无法依赖 BOM 自救。
// 所以脚本内部只用英文提示，让字节在任何代码页下都一致（install_test.go 守着）。
//
//go:embed scripts/install.ps1
var installScriptPS1 string
