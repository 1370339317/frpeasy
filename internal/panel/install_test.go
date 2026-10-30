package panel

import (
	"os/exec"
	"strings"
	"testing"
)

// 安装脚本最终要在设备上被 sh 执行，语法错一次就是全线装不上。
//
// 这条尤其值得守：installScript 是 Go 的 raw string，里面一旦出现反引号
// （比如注释里想引用命令），字符串会被提前截断，Go 编译报一个莫名其妙的
// 语法错误；而 shell 侧的语法错误（比如提权探测那段）Go 完全看不见。
// sh -n 只做解析不执行，能把这两类问题当场拦住。
func TestInstallScriptShellSyntax(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("PATH 上没有 sh，跳过语法检查")
	}
	cmd := exec.Command(shBin, "-n")
	cmd.Stdin = strings.NewReader(installScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("安装脚本 shell 语法错误: %v\n%s", err, out)
	}
}

// 回归：install.ps1 必须是纯 ASCII。
//
// 原始故障：脚本里写了中文提示，而 Windows PowerShell 5.1 读取不带 BOM 的 .ps1
// 时按系统 ANSI 代码页（简体中文为 GBK）解码。安装命令是
// `iwr ... -OutFile $env:TEMP\frpeasy-install.ps1; & ...`，落地的是原样 UTF-8
// 字节、没有 BOM，于是中文被解成乱码，并吞掉引号导致
// 「表达式或语句中包含意外的标记"）"」「字符串缺少终止符」等语法错误。
// 只要正文保持 ASCII，任何代码页解码结果都一样，彻底免疫。
func TestInstallScriptPS1IsASCII(t *testing.T) {
	for i := 0; i < len(installScriptPS1); i++ {
		if installScriptPS1[i] > 0x7F {
			line := 1 + strings.Count(installScriptPS1[:i], "\n")
			t.Fatalf("install.ps1 含非 ASCII 字节 0x%02X（第 %d 行附近），"+
				"PowerShell 5.1 在中文系统按 GBK 解码会破坏引号导致语法错误", installScriptPS1[i], line)
		}
	}
}

// 回归：bash 安装脚本不能有 \r（Windows 上编辑后混入 CRLF 会让 sh 报 bad interpreter）。
func TestInstallScriptNoCR(t *testing.T) {
	for name, s := range map[string]string{"install(bash)": installScript, "install.ps1": installScriptPS1} {
		if strings.ContainsRune(s, '\r') {
			t.Fatalf("%s 含 CR，打包/落盘时会被污染成 CRLF", name)
		}
	}
}

// 回归：Windows 上会被**执行**的临时文件必须带 .exe 后缀。
//
// 真机踩过：临时文件写成 "$exe.new"（展开成 frpeasy-agent.exe.new），
// Windows 不认为它是可执行文件，`& $tmp --version` 的校验必然失败，
// 报错却是 "cannot run (wrong architecture or corrupt download)" ——
// 把人的注意力全引到"下载坏了/平台不对"上，实际文件完好无损。
//
// 同理 internal/panel/panelupdate.go 里的 panel.new 也必须带 ExeSuffix()。
func TestInstallScriptPS1TempFileHasExeSuffix(t *testing.T) {
	if strings.Contains(installScriptPS1, `$tmp = "$exe.new"`) {
		t.Fatal("临时文件不能命名成 <exe>.new（扩展名变成 .new，Windows 无法执行，" +
			"会导致 --version 校验永远失败）")
	}
	if !strings.Contains(installScriptPS1, `frpeasy-agent.new.exe`) {
		t.Fatal("临时文件应带 .exe 后缀，形如 frpeasy-agent.new.exe")
	}
}

// 计划任务必须以 SYSTEM 身份注册：AtStartup 触发时用户往往尚未登录，
// 若用交互用户身份且未保存口令，任务会静默不执行。
func TestInstallScriptPS1UsesSystemPrincipal(t *testing.T) {
	if !strings.Contains(installScriptPS1, `-UserId "SYSTEM" -LogonType ServiceAccount`) {
		t.Fatal("install.ps1 应以 SYSTEM 身份注册计划任务，否则开机不登录时不启动")
	}
	if strings.Contains(installScriptPS1, "[Parameter(Mandatory=$true)]") {
		t.Fatal("-Uninstall 模式不应强制要求 -Server/-Token")
	}
	if !strings.Contains(installScriptPS1, "Unregister-ScheduledTask") {
		t.Fatal("install.ps1 应支持卸载（-Uninstall）")
	}
	if !strings.Contains(installScriptPS1, "IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)") {
		t.Fatal("install.ps1 应检测管理员权限并给出明确提示")
	}
}

// 用户明确要求：重复执行安装命令要能清理旧安装、装上新版本并正确启动。
// 这三件事分别对应：先停旧实例 / 换新二进制 / 装完确认存活。
func TestInstallScriptIsReinstallable(t *testing.T) {
	for _, want := range []string{"stop_existing", "purge_units", "agent_running", "--uninstall", "systemctl restart frpeasy-agent"} {
		if !strings.Contains(installScript, want) {
			t.Fatalf("安装脚本缺少 %q —— 重复执行/升级路径不完整", want)
		}
	}
	// 不能再用 set -e：这是修复脚本，中途退出会留下半装状态
	if strings.Contains(installScript, "\nset -e") {
		t.Fatal("安装脚本不应使用 set -e（修复场景下需要在失败后继续清理并给出可读错误）")
	}
	// 下载必须校验能跑，才允许动旧安装
	if !strings.Contains(installScript, `"$TMP" --version`) {
		t.Fatal("安装脚本必须在替换旧二进制前用 --version 校验新文件")
	}
	idxVerify := strings.Index(installScript, `"$TMP" --version`)
	idxStop := strings.Index(installScript, "\nstop_existing\n")
	if idxVerify < 0 || idxStop < 0 || idxVerify > idxStop {
		t.Fatal("校验必须发生在停止旧实例之前，否则下载失败会把用户正在跑的客户端干掉")
	}
}

// Android 支持：同一份脚本要认得出 Android、走对路径、用得上设备上真有的工具。
func TestInstallScriptSupportsAndroid(t *testing.T) {
	for _, want := range []string{
		"/system/build.prop",            // Android 识别
		"/data/local/tmp/frpeasy-agent", // 免 root 安装位置（adb shell 可写可执行）
		"com.termux",                    // Termux 分支
		"toybox wget",                   // Android 上常见的下载器
		"rc-update",                     // Alpine / OpenRC
	} {
		if !strings.Contains(installScript, want) {
			t.Fatalf("安装脚本缺少 Android/Linux 分支关键字 %q", want)
		}
	}
	// 卸载会 rm -rf 数据目录，必须有护栏
	if !strings.Contains(installScript, "refusing to uninstall with suspicious data dir") {
		t.Fatal("卸载路径应对可疑的 --data 做拒绝，避免误删根目录")
	}
}

// Android 上**不注册任何自启、不挂看护进程**，启动由用户自己控制。
//
// 这是产品的明确决定，不是省事：root 方案（Magisk / KernelSU / APatch / adbsu）
// 的自启目录各不相同、系统版本差异又大，猜错会留下失效脚本甚至影响开机行为。
// 这条用例用禁止列表守住它 —— 以后谁想加回自启，这里会立刻红。
func TestAndroidRegistersNoAutostart(t *testing.T) {
	// 全文级：任何地方都不该再写自启文件（清理动作只允许 rm）。
	forbidden := map[string]string{
		`mkdir -p /data/adb/service.d`: "不得创建 Magisk 自启目录",
		`> "$MAGISK_BOOT"`:             "不得写 Magisk service.d 自启脚本",
		`> "$HOME/.termux/boot/`:       "不得写 Termux:Boot 自启脚本",
	}
	for frag, why := range forbidden {
		if strings.Contains(installScript, frag) {
			t.Fatalf("违反了「不自启」设计：%s（命中 %q）", why, frag)
		}
	}

	// 分支级：Android 安装分支里不得起守护、不得挂自启。
	// 注意 spawn_supervise 这个函数本身要保留 —— 它服务于「无 init 系统的 Linux」，
	// 所以只能按分支判断，不能全文匹配。
	iStart := strings.Index(installScript, `if [ "$IS_ANDROID" -eq 1 ]; then`+"\n  # Android：不注册任何自启")
	iEnd := strings.Index(installScript, "elif have_systemd; then")
	if iStart < 0 || iEnd < 0 || iEnd < iStart {
		t.Fatal("找不到 Android 安装分支，脚本结构变了，请同步更新这条断言")
	}
	branch := installScript[iStart:iEnd]
	for frag, why := range map[string]string{
		"spawn_supervise": "Android 分支不得自动起看护进程",
		"supervise":       "Android 分支不得挂 supervisor",
		"MAGISK_BOOT":     "Android 分支不得引用 Magisk 自启路径",
		".termux/boot":    "Android 分支不得引用 Termux 自启路径",
	} {
		if strings.Contains(branch, frag) {
			t.Fatalf("Android 分支违反了「不自启」设计：%s（命中 %q）", why, frag)
		}
	}

	// 装完仍然要正常启动一次，否则用户装完什么也没发生
	if !strings.Contains(branch, `"$RUNSH" start`) {
		t.Fatal("Android 分支应正常启动一次（run.sh start），只是不注册自启")
	}
	if !strings.Contains(branch, "no autostart") {
		t.Fatal("安装结果应明确告知用户 Android 上没有自启，启停需自己控制")
	}
	// 旧版本可能在设备上留过自启文件，升级时必须清掉，否则会一直跑旧版本
	if !strings.Contains(installScript, "removed legacy autostart") {
		t.Fatal("应清理旧版本遗留的自启脚本（仅内容含 frpeasy 的）")
	}
}

// su 的调用形状因实现而异（su -c / su 0 / su root -c），脚本必须按行为探测，
// 不能像以前那样在面板侧写死 `su -c '...'`。
func TestElevateDoesNotAssumeSuShape(t *testing.T) {
	if !strings.Contains(installScript, `for shape in "-c" "0" "root -c"`) {
		t.Fatal("--elevate 必须依次探测常见的 su 调用形状，而不是假设某一种")
	}
	if !strings.Contains(installScript, "FRPEASY_ELEVATED") {
		t.Fatal("提权重入需要防递归标记，否则 su 失败会无限自我调用")
	}
	if !strings.Contains(installScript, "could not obtain root with any known su shape") {
		t.Fatal("所有 su 形状都失败时要明确报错并给出手动退路")
	}
}

// Windows 侧同样要能重装/升级：验证 + 备份 + 回滚。
func TestInstallScriptPS1UpgradeAndRollback(t *testing.T) {
	for _, want := range []string{
		"Get-ExeVersion",           // 用 --version 校验下载物
		"frpeasy-agent.exe.bak",    // 替换前备份
		"Rolling back",             // 起不来就回滚
		"Stop-AgentProcess",        // 换文件前先停进程（否则文件被占用）
		"Unregister-ScheduledTask", // 重复执行时清掉旧任务
	} {
		if !strings.Contains(installScriptPS1, want) {
			t.Fatalf("install.ps1 缺少 %q —— 重装/升级/回滚链路不完整", want)
		}
	}
	// 必须先校验再动旧文件
	iVerify := strings.Index(installScriptPS1, "$newVer = Get-ExeVersion $tmp")
	iStop := strings.Index(installScriptPS1, "if (-not (Stop-AgentProcess))")
	if iVerify < 0 || iStop < 0 || iVerify > iStop {
		t.Fatal("install.ps1 必须在停止旧进程之前完成新二进制校验")
	}
}

// Windows 一键命令必须能在「本机系统代理对裸 IP 一律 502」的机器上跑通。
//
// 真机故障：WinINET 代理 127.0.0.1:10809 只放行白名单域名，对
// http://47.80.4.36:28080 回 502；同一 URL 直连是 200 / 0.5s。旧命令直接用
// iwr，于是「面板活着，装什么都是 502」，用户完全无从判断。
//
// 同一处还有第二个坑：旧命令用 `;` 串联下载与执行，而 PS 5.1 在
// `-ErrorAction Stop` 失败后**仍会执行后面的语句**（已实测：探针脚本照样跑）。
// 于是 %TEMP% 里上一次残留的旧脚本被当成新的执行，报的错与本次无关。
func TestWindowsInstallCommandIsProxySafe(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("install_token", "tok123")
	_ = p.db.SetSetting("public_addr", "1.2.3.4:8080")

	for name, cmd := range map[string]string{
		"install":   p.buildInstallCommandWindows(nil),
		"uninstall": p.buildUninstallCommandWindows(nil),
	} {
		if !strings.Contains(cmd, "DefaultWebProxy=New-Object System.Net.WebProxy") {
			t.Fatalf("%s: 必须先清空默认代理直连（系统代理对裸 IP 会回 502）: %s", name, cmd)
		}
		if !strings.Contains(cmd, "GetSystemWebProxy") {
			t.Fatalf("%s: 直连失败要回落到系统代理（确实只能走代理的网络）: %s", name, cmd)
		}
		if strings.Contains(cmd, "UseBasicParsing; & $env:TEMP") {
			t.Fatalf("%s: 不能用 ; 串联下载与执行 —— PS 5.1 下载失败后仍会执行 %%TEMP%% 里的旧脚本: %s", name, cmd)
		}
		if !strings.Contains(cmd, "exit 1") {
			t.Fatalf("%s: 下载失败必须终止，不能继续往下执行: %s", name, cmd)
		}
		// 命令既可能粘进 cmd 也可能粘进 PowerShell；后者的外层会先把 $p 展开成空串
		if strings.Contains(cmd, "$p=") || strings.Contains(cmd, "$d=") || strings.Contains(cmd, "$f=") {
			t.Fatalf("%s: 不能引入自定义变量（粘进 PowerShell 会被外层展开）: %s", name, cmd)
		}
	}

	install := p.buildInstallCommandWindows(nil)
	if !strings.Contains(install, "-Server http://1.2.3.4:8080") || !strings.Contains(install, "-Token tok123") {
		t.Fatalf("安装命令应带上服务器地址与安装令牌: %s", install)
	}
	if !strings.Contains(p.buildUninstallCommandWindows(nil), "-Uninstall") {
		t.Fatalf("卸载命令应带 -Uninstall: %s", p.buildUninstallCommandWindows(nil))
	}
}

// Android 的安装命令要能在设备上落地并执行，且带着服务器与令牌。
func TestBuildInstallCommands(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("install_token", "tok123")
	_ = p.db.SetSetting("public_addr", "1.2.3.4:8080")

	android := p.buildInstallCommandAndroid(nil)
	if !strings.Contains(android, "adb shell") {
		t.Fatalf("Android 命令应以 adb shell 开头: %s", android)
	}
	if !strings.Contains(android, "/data/local/tmp/frpeasy-install.sh") {
		t.Fatalf("Android 命令应把脚本落到设备上再执行: %s", android)
	}
	if !strings.Contains(android, "toybox wget") {
		t.Fatalf("Android 命令需要 toybox wget 兜底（设备上通常没有 curl）: %s", android)
	}
	if !strings.Contains(android, "--server http://1.2.3.4:8080") || !strings.Contains(android, "--token tok123") {
		t.Fatalf("Android 命令应带上服务器地址与安装令牌: %s", android)
	}

	// Android 不再有"root 版"命令：那边的 agent 不做安装（不注册自启、不挂看护），
	// 只是把二进制放下去跑起来，是否需要 root 由用户自己决定（加 --elevate）。
	if strings.Contains(android, "--elevate") {
		t.Fatalf("默认的 Android 命令不该带 --elevate（提权是用户的选择，不是默认）: %s", android)
	}
	// 面板侧绝不能写死 su 的调用形式：Magisk / KernelSU / APatch / adbsu 各不相同，
	// 猜错就是一条看不出原因的失败命令。
	if strings.Contains(android, "su -c") || strings.Contains(android, "su 0") {
		t.Fatalf("面板侧不应假设 su 的调用形状（应交给脚本探测）: %s", android)
	}
	// 下载失败要明确退出，而不是继续去 sh 一个不存在的文件
	if !strings.Contains(android, "[ -s ") || !strings.Contains(android, "download failed on device") {
		t.Fatalf("命令应在脚本落地失败时明确退出: %s", android)
	}

	linux := p.buildInstallCommand(nil)
	if !strings.Contains(linux, "sh -s -- --server") || !strings.Contains(linux, "--token tok123") {
		t.Fatalf("Linux 命令格式不对: %s", linux)
	}
}
