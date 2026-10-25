package panel

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"frpeasy/internal/shared"
)

// installHandler GET /install —— 返回客户端一键安装脚本（POSIX sh，覆盖 Linux / Android / Termux）。
// 用法：curl -sSL <panel>/install | sh -s -- --server <panel> --token <install_token>
func (p *Panel) installHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 用 io.WriteString 而非 fmt.Fprint：脚本正文里有 %s 之类的字面量，
	// fmt 家族会被 vet 判为可疑的格式化指令。
	_, _ = io.WriteString(w, installScript)
}

// installPs1Handler GET /install.ps1 —— Windows 一键安装脚本（PowerShell）。
//
// 编码注意：脚本正文必须是纯 ASCII（见 install_test.go 的断言）。
// Windows PowerShell 5.1 读取 .ps1 时，若文件没有 BOM 就按系统 ANSI 代码页
// （简体中文机器为 GBK）解码。UTF-8 的中文提示会被解成乱码，其中某些字节
// 序列还会吞掉引号，导致 "表达式或语句中包含意外的标记" 这类语法错误。
// 而且 `irm -OutFile` 是原样落字节、不带 BOM，无法依赖 BOM 自救。
// 所以这里只用英文提示，让字节在任何代码页下都一致。
func (p *Panel) installPs1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, installScriptPS1)
}

// agentBinHandler GET /api/agent/bin/{os}/{arch} —— 下发对应平台的 agent 二进制。
//
// android 是 linux 二进制的别名：Go 无法为 GOOS=android 构建独立可执行文件，
// Android 上的 agent 就是 linux 静态二进制（armeabi-v7a 对应 arm，arm64-v8a 对应 arm64）。
// 这样安装脚本可以按设备真实平台拼 URL，而不用自己记住这层映射。
func (p *Panel) agentBinHandler(w http.ResponseWriter, r *http.Request, osName, arch string) {
	// 文件名与别名映射统一走 shared：安装路由与心跳里的升级下发必须
	// 得到同一个结果，否则会出现"装的时候能下、升级时 404"。
	name := shared.AgentBinFileName(osName, arch)
	if name == "" {
		http.Error(w, "os / arch 参数无效", http.StatusBadRequest)
		return
	}
	// 查找位置由 agentBinDirs 统一给出（面板同级 / 同级 bin/ / 数据目录 bin/）。
	// 与升级下发共用同一套规则：否则会出现"装的时候能下、升级时 404"。
	path := p.findAgentBin(osName, arch)
	if path == "" {
		http.Error(w, fmt.Sprintf("未找到 agent 二进制 %s，请放到以下任一位置：\n  %s\n（make build-alpine 生成到 bin/，make stage-agent 复制到数据目录）",
			name, strings.Join(p.agentBinDirs(), "\n  ")), http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "打开 agent 二进制失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// installScript 客户端安装脚本（POSIX sh）。Linux / Android / Termux 共用。
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
const installScript = `#!/bin/sh
# frpeasy agent installer / upgrader
# Targets: Linux (systemd / OpenRC / plain), Android (adb shell / su), Termux.
#
# Android: no autostart and no watchdog is registered. The installer only puts the
# binary in place and starts it once; start/stop is under your control:
#   sh <data dir>/run.sh start|stop|restart|status|log
#
#   curl -sSL <panel>/install | sh -s -- --server <panel> --token <token>
#   sh frpeasy-install.sh --server <panel> --token <token>     (re-run to upgrade / repair)
#   sh frpeasy-install.sh --uninstall
#   sh frpeasy-install.sh --server ... --token ... --foreground  (debug in console)
#
# Safe to run repeatedly: it converges to the target state and never removes a
# working installation unless the new binary was downloaded and verified first.
set -u

SERVER=""
TOKEN=""
DATA_OVERRIDE=""
UNINSTALL=0
FOREGROUND=0
ELEVATE=0

say()  { printf '%s\n' "[frpeasy] $*"; }
warn() { printf '%s\n' "[frpeasy] WARN: $*" >&2; }
die()  { printf '%s\n' "[frpeasy] ERROR: $*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
[frpeasy] agent installer

  --server <url>    panel base url, e.g. http://1.2.3.4:8080   (required)
  --token <tok>     install token from the panel               (required)
  --data <dir>      override the agent data directory
  --elevate         Android only: obtain root first (su variant auto-detected)
  --uninstall       remove the agent, its service and its data
  --foreground      run in this console instead of installing a service
  -h, --help        this text

Re-running the installer upgrades / repairs the existing installation.
USAGE
}

need_val() { [ $# -ge 2 ] || die "option $1 requires a value"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --server)     need_val "$@"; SERVER="$2"; shift 2 ;;
    --token)      need_val "$@"; TOKEN="$2"; shift 2 ;;
    --data)       need_val "$@"; DATA_OVERRIDE="$2"; shift 2 ;;
    --elevate)    ELEVATE=1; shift ;;
    --uninstall)  UNINSTALL=1; shift ;;
    --foreground) FOREGROUND=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            die "unknown argument: $1 (try --help)" ;;
  esac
done

# ---------------------------------------------------------------- platform

UNAME_S=$(uname -s 2>/dev/null | tr 'A-Z' 'a-z')
UNAME_M=$(uname -m 2>/dev/null)
case "$UNAME_M" in
  x86_64|amd64)     ARCH=amd64 ;;
  aarch64|arm64)    ARCH=arm64 ;;
  armv7*|armv8l|arm) ARCH=arm ;;
  i386|i486|i586|i686|x86) ARCH=386 ;;
  *) die "unsupported CPU architecture: $UNAME_M" ;;
esac

IS_ANDROID=0
if [ "$UNAME_S" = "linux" ]; then
  if [ -f /system/build.prop ] || [ -n "${ANDROID_ROOT:-}" ] || [ -d /system/app ]; then
    IS_ANDROID=1
  fi
fi

IS_TERMUX=0
case "${PREFIX:-}" in *com.termux*) IS_TERMUX=1 ;; esac

# 面板端只存 linux 与 windows 两套二进制；android 是 linux 的别名。
case "$UNAME_S" in
  linux|android) OSKEY=linux ;;
  darwin)        die "macOS is not a supported client platform (no launchd integration yet). Run the agent manually with --foreground from a checkout if you just want to test." ;;
  *)             die "unsupported OS: $UNAME_S" ;;
esac

if [ $IS_ANDROID -eq 1 ] && [ $IS_TERMUX -eq 1 ]; then
  PLATFORM="Android / Termux"
elif [ $IS_ANDROID -eq 1 ]; then
  PLATFORM="Android"
else
  PLATFORM="$UNAME_S"
fi

# ---------------------------------------------------------------- paths

if [ "$IS_ANDROID" -eq 1 ]; then
  if [ "$IS_TERMUX" -eq 1 ]; then
    BIN="${PREFIX}/bin/frpeasy-agent"
    DATA_DIR="${PREFIX}/var/lib/frpeasy-agent"
  else
    BIN="/data/local/tmp/frpeasy-agent/frpeasy-agent"
    DATA_DIR="/data/local/tmp/frpeasy-agent/data"
  fi
else
  BIN="/usr/local/bin/frpeasy-agent"
  DATA_DIR="/var/lib/frpeasy-agent"
fi
[ -n "$DATA_OVERRIDE" ] && DATA_DIR="$DATA_OVERRIDE"
PIDFILE="$DATA_DIR/agent.pid"
RUNSH="$DATA_DIR/run.sh"
LOGFILE="$DATA_DIR/agent.log"

UNIT_SYSTEMD="/etc/systemd/system/frpeasy-agent.service"
UNIT_OPENRC="/etc/init.d/frpeasy-agent"
MAGISK_BOOT="/data/adb/service.d/frpeasy-agent.sh"

IS_ROOT=0
[ "$(id -u 2>/dev/null)" = "0" ] && IS_ROOT=1
SUDO=""
if [ "$IS_ROOT" -eq 0 ] && command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
fi

# 需要写的父目录不可写、又没有 sudo/root，就别走到一半才失败。
dir_writable() {
  d="$1"
  [ -d "$d" ] || return 1
  [ -w "$d" ]
}
parent_of() { printf '%s' "${1%/*}"; }

ensure_bin_dir() {
  d=$(parent_of "$BIN")
  [ -d "$d" ] || $SUDO mkdir -p "$d" 2>/dev/null || mkdir -p "$d" 2>/dev/null || die "cannot create $d"
  dir_writable "$d" && return 0
  # 非 root 但有 sudo 时照样能装（后面所有写操作都带 $SUDO）
  [ -n "$SUDO" ] && return 0
  die "no write permission on $d. Re-run as root, or install sudo, or pass --data to a writable directory."
}

# 后台拉起看护进程。没有 nohup 的精简系统（部分 Android ROM）直接放后台。
spawn_supervise() {
  if command -v nohup >/dev/null 2>&1; then
    nohup "$RUNSH" supervise >/dev/null 2>&1 &
  else
    "$RUNSH" supervise >/dev/null 2>&1 &
  fi
}

# ---------------------------------------------------------------- process ctl

have_systemd() {
  [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1
}
have_openrc() {
  command -v rc-update >/dev/null 2>&1 && [ -d /etc/init.d ]
}

pid_alive() {
  [ -f "$PIDFILE" ] || return 1
  p=$(cat "$PIDFILE" 2>/dev/null)
  [ -n "$p" ] || return 1
  kill -0 "$p" 2>/dev/null
}

# 按二进制绝对路径精确匹配，避免误杀机器上其它 frpeasy 实例。
pkill_bin() {
  command -v pkill >/dev/null 2>&1 || return 0
  if [ -n "${1:-}" ]; then
    pkill "$1" -f "$BIN" >/dev/null 2>&1 || true
  else
    pkill -f "$BIN" >/dev/null 2>&1 || true
  fi
}

agent_running() {
  if have_systemd && [ -f "$UNIT_SYSTEMD" ]; then
    systemctl is-active --quiet frpeasy-agent 2>/dev/null && return 0
  fi
  pid_alive && return 0
  if command -v pgrep >/dev/null 2>&1; then
    pgrep -f "$BIN" >/dev/null 2>&1 && return 0
  fi
  return 1
}

# 停掉一切形态的旧实例：systemd / OpenRC / pidfile / 野进程。
stop_existing() {
  say "stopping any existing agent instance ..."
  if have_systemd && [ -f "$UNIT_SYSTEMD" ]; then
    $SUDO systemctl stop frpeasy-agent >/dev/null 2>&1 || true
    $SUDO systemctl disable frpeasy-agent >/dev/null 2>&1 || true
  fi
  if [ -f "$UNIT_OPENRC" ]; then
    $SUDO "$UNIT_OPENRC" stop >/dev/null 2>&1 || true
    command -v rc-update >/dev/null 2>&1 && $SUDO rc-update del frpeasy-agent default >/dev/null 2>&1 || true
  fi
  if pid_alive; then
    kill "$(cat "$PIDFILE")" 2>/dev/null || true
  fi
  pkill_bin ""
  # 等它退出（最多 5 秒），然后强杀
  i=0
  while [ $i -lt 5 ]; do
    agent_running || break
    sleep 1
    i=$((i+1))
  done
  if agent_running; then
    warn "agent did not exit gracefully, sending SIGKILL"
    pkill_bin "-9"
    sleep 1
  fi
  rm -f "$PIDFILE"
  return 0
}

# 清掉旧的、可能已损坏的自启注册，避免"新版本装上了但开机跑的还是旧的服务定义"。
purge_units() {
  if [ -f "$UNIT_SYSTEMD" ]; then
    # 先 disable 再删，否则会留下指向不存在 unit 的符号链接
    $SUDO systemctl disable frpeasy-agent >/dev/null 2>&1 || true
    $SUDO rm -f "$UNIT_SYSTEMD"
    have_systemd && $SUDO systemctl daemon-reload >/dev/null 2>&1 || true
    have_systemd && $SUDO systemctl reset-failed frpeasy-agent >/dev/null 2>&1 || true
    say "removed old systemd unit"
  fi
  for f in /lib/systemd/system/frpeasy-agent.service /usr/lib/systemd/system/frpeasy-agent.service; do
    [ -f "$f" ] && $SUDO rm -f "$f"
  done
  if [ -f "$UNIT_OPENRC" ]; then
    command -v rc-update >/dev/null 2>&1 && $SUDO rc-update del frpeasy-agent default >/dev/null 2>&1 || true
    $SUDO rm -f "$UNIT_OPENRC"
    say "removed old OpenRC service"
  fi
  # Android：清掉旧版本可能留下的自启文件。
  # 只删内容里含 frpeasy 的文件，避免误伤用户自己写的启动脚本。
  # 注意不判断 su 是哪种实现：文件在哪就查哪，查到了才删，不做任何推断。
  if [ -f "$MAGISK_BOOT" ] && grep -q frpeasy "$MAGISK_BOOT" 2>/dev/null; then
    $SUDO rm -f "$MAGISK_BOOT" 2>/dev/null || rm -f "$MAGISK_BOOT" 2>/dev/null
    say "removed legacy autostart: $MAGISK_BOOT"
  fi
  if [ -f "$HOME/.termux/boot/frpeasy-agent.sh" ]; then
    rm -f "$HOME/.termux/boot/frpeasy-agent.sh" 2>/dev/null
    say "removed legacy autostart: $HOME/.termux/boot/frpeasy-agent.sh"
  fi
  return 0
}

# ---------------------------------------------------------------- download

fetch() {
  url="$1"; dst="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --connect-timeout 15 --retry 2 --retry-delay 2 "$url" -o "$dst"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -T 30 -O "$dst" "$url"
  elif command -v toybox >/dev/null 2>&1 && toybox wget --help >/dev/null 2>&1; then
    toybox wget -O "$dst" "$url"
  else
    return 127
  fi
}

# ---------------------------------------------------------------- uninstall

do_uninstall() {
  # 卸载会 rm -rf 数据目录，先确认它确实是个具体路径，
  # 避免 --data "" / --data / 这类误传把根目录带走。
  case "$DATA_DIR" in
    ""|"/"|"/."|"/usr"|"/usr/local"|"/var"|"/data") die "refusing to uninstall with suspicious data dir: '$DATA_DIR'" ;;
  esac
  say "uninstalling frpeasy agent ..."
  stop_existing
  purge_units
  for f in "$MAGISK_BOOT"; do
    [ -f "$f" ] && $SUDO rm -f "$f" && say "removed $f"
  done
  [ -f "$HOME/.termux/boot/frpeasy-agent.sh" ] && rm -f "$HOME/.termux/boot/frpeasy-agent.sh" && say "removed Termux boot script"
  if [ -f "$BIN" ]; then
    $SUDO rm -f "$BIN" && say "removed $BIN"
  fi
  if [ -d "$DATA_DIR" ]; then
    $SUDO rm -rf "$DATA_DIR" && say "removed $DATA_DIR"
  fi
  say "uninstalled. Nothing of frpeasy is left on this machine."
  exit 0
}

[ "$UNINSTALL" -eq 1 ] && do_uninstall

[ -n "$SERVER" ] || die "--server is required (panel base url, e.g. http://1.2.3.4:8080)"
[ -n "$TOKEN" ]  || die "--token is required (copy the whole install command from the panel)"
SERVER="${SERVER%/}"

# ---------------------------------------------------------------- elevate (optional)
# --elevate 表示"我要以 root 身份装/跑"。这里**不预设 su 是哪种实现**：
# Magisk / KernelSU / APatch / adbsu 的调用形状不同（有的要 su -c "cmd"，
# 有的要 su 0 cmd，有的要 su root -c "cmd"），面板侧写死任何一种都是在猜。
# 所以按行为依次试：每种都必须真的拿到 uid 0 才算成功（内层脚本自检，不是 root 就 die），
# 全失败则明确报错并给出「自己进 root shell 手跑」的退路，而不是猜个形状硬跑。
if [ "$ELEVATE" -eq 1 ]; then
  if [ "$IS_ANDROID" -ne 1 ]; then
    warn "--elevate is Android-only; on other platforms the installer uses sudo when needed"
  elif [ "$(id -u 2>/dev/null)" != "0" ]; then
    if [ "${FRPEASY_ELEVATED:-0}" = "1" ]; then
      die "elevation failed: su exited but we are still not root"
    fi
    REARGS="--server $SERVER --token $TOKEN --elevate"
    [ -n "$DATA_OVERRIDE" ] && REARGS="$REARGS --data $DATA_OVERRIDE"
    say "root requested; detecting which su invocation shape this device supports ..."
    for shape in "-c" "0" "root -c"; do
      FRPEASY_ELEVATED=1 su $shape "sh $0 $REARGS" 2>/dev/null && exit 0
    done
    die "could not obtain root with any known su shape (tried: su -c, su 0, su root -c).
  Your device's su works differently. Run the installer from a root shell instead:
    adb shell
    su
    sh $0 --server $SERVER --token $TOKEN
  Or just drop --elevate: the agent does not need root to run."
  fi
fi

say "platform : $PLATFORM / $ARCH"
say "panel    : $SERVER"
say "binary   : $BIN"
say "data dir : $DATA_DIR"

# ---------------------------------------------------------------- fetch + verify

mkdir -p "$DATA_DIR" 2>/dev/null || $SUDO mkdir -p "$DATA_DIR" 2>/dev/null || die "cannot create $DATA_DIR"
ensure_bin_dir

TMP="$DATA_DIR/.agent.new.$$"
rm -f "$TMP"

URL="$SERVER/api/agent/bin/$OSKEY/$ARCH"
say "downloading $URL ..."
# 注意：不能写成 "if ! fetch ...; then rc=$?" —— 叹号会把退出码取反，
# rc 永远是 0，下面「没有下载器」的专门提示就永远走不到。先跑再取码。
fetch "$URL" "$TMP"
rc=$?
if [ "$rc" -ne 0 ]; then
  rm -f "$TMP"
  if [ "$rc" = "127" ]; then
    die "no downloader found (need curl, wget or toybox wget).
  On Android get curl via Termux, or push the binary manually:
    1) on the panel host: keep bin/frpeasy-agent-linux-$ARCH next to frpeasy-panel
    2) adb push frpeasy-agent-linux-$ARCH /data/local/tmp/frpeasy-agent/frpeasy-agent
    3) adb shell chmod 755 /data/local/tmp/frpeasy-agent/frpeasy-agent"
  fi
  die "download failed (rc=$rc). The existing installation, if any, was left untouched."
fi

SZ=$(wc -c < "$TMP" 2>/dev/null | tr -d ' ')
[ -n "$SZ" ] || SZ=0
if [ "$SZ" -lt 500000 ]; then
  HEAD=$(head -c 300 "$TMP" 2>/dev/null | tr -d '\r')
  rm -f "$TMP"
  die "downloaded file is too small ($SZ bytes). The panel probably has no $OSKEY/$ARCH agent staged
  (run 'make stage-agent' on the panel host). Panel said: $HEAD"
fi

chmod 755 "$TMP" 2>/dev/null || true
VOUT=$("$TMP" --version 2>&1)
if [ $? -ne 0 ]; then
  rm -f "$TMP"
  die "downloaded binary cannot run on this machine: $VOUT
  Usual causes: wrong architecture (this box is $ARCH), or a noexec mount on $DATA_DIR."
fi
case "$VOUT" in
  frpeasy-agent*) : ;;
  *) rm -f "$TMP"; die "unexpected --version output: $VOUT" ;;
esac
say "verified  : $VOUT"

OLD_VER=""
if [ -x "$BIN" ]; then
  OLD_VER=$("$BIN" --version 2>/dev/null | head -1)
fi

# ---------------------------------------------------------------- foreground (debug)

if [ "$FOREGROUND" -eq 1 ]; then
  say "running in foreground, Ctrl+C to stop (no service is registered)"
  exec "$TMP" --server "$SERVER" --token "$TOKEN" --data "$DATA_DIR" --name "$(hostname 2>/dev/null || echo agent)"
fi

# ---------------------------------------------------------------- install

stop_existing
purge_units

# 同目录内 rename，避免 cp 到一半被中断留下半个可执行文件
NEW="$BIN.new"
$SUDO rm -f "$NEW" 2>/dev/null || rm -f "$NEW" 2>/dev/null
if [ -n "$SUDO" ]; then
  $SUDO cp -f "$TMP" "$NEW" && $SUDO chmod 755 "$NEW"
else
  cp -f "$TMP" "$NEW" && chmod 755 "$NEW"
fi
[ $? -eq 0 ] || die "cannot write $NEW"
$SUDO mv -f "$NEW" "$BIN" || mv -f "$NEW" "$BIN" || die "cannot install $BIN"
rm -f "$TMP"
say "installed : $BIN"

# run.sh —— 无 systemd 平台（Android / 容器 / 精简 Linux）的唯一入口。
# 用单引号 heredoc 写，避免 $ 被外层展开。
write_runsh() {
  cat > "$RUNSH" <<RUNEOF
#!/bin/sh
# generated by frpeasy installer -- do not edit
BIN="$BIN"
DATA="$DATA_DIR"
PIDFILE="$DATA_DIR/agent.pid"
LOG="$DATA_DIR/agent.log"
SERVER="$SERVER"
TOKEN="$TOKEN"
NAME="$AGENT_NAME"
running() { [ -f "\$PIDFILE" ] && kill -0 "\$(cat "\$PIDFILE" 2>/dev/null)" 2>/dev/null; }
start() {
  running && { echo "[frpeasy] already running (pid \$(cat "\$PIDFILE"))"; return 0; }
  rm -f "\$PIDFILE"
  # 精简 Android / 容器里不一定有 nohup（toybox 未必编入那一项）。
  # 直接写 "nohup cmd &" 时，没有 nohup 会让整条命令失败、取不到后台 pid，
  # 表面上执行完了、实际什么都没起。所以先探测再决定怎么起。
  if command -v nohup >/dev/null 2>&1; then
    nohup "\$BIN" --server "\$SERVER" --token "\$TOKEN" --data "\$DATA" --name "\$NAME" >>"\$LOG" 2>&1 &
  else
    "\$BIN" --server "\$SERVER" --token "\$TOKEN" --data "\$DATA" --name "\$NAME" >>"\$LOG" 2>&1 &
  fi
  echo \$! > "\$PIDFILE"
  sleep 2
  running && echo "[frpeasy] started (pid \$(cat "\$PIDFILE"))" || { echo "[frpeasy] failed to start, see \$LOG"; return 1; }
}
stop() {
  if running; then kill "\$(cat "\$PIDFILE")" 2>/dev/null; sleep 1; fi
  if running; then kill -9 "\$(cat "\$PIDFILE")" 2>/dev/null; fi
  rm -f "\$PIDFILE"
  echo "[frpeasy] stopped"
}
supervise() {
  # 崩溃自动拉起。Android 上系统可能把进程杀掉，靠这个循环拉回来。
  trap 'stop; exit 0' INT TERM HUP
  while :; do
    "\$BIN" --server "\$SERVER" --token "\$TOKEN" --data "\$DATA" --name "\$NAME" >>"\$LOG" 2>&1 &
    echo \$! > "\$PIDFILE"
    wait \$!
    sleep 5
  done
}
case "\${1:-start}" in
  start)     start ;;
  stop)      stop ;;
  restart)   stop; sleep 1; start ;;
  status)    running && echo "running (pid \$(cat "\$PIDFILE"))" || echo "not running" ;;
  supervise) supervise ;;
  log)       tail -n 40 "\$LOG" 2>/dev/null || echo "no log yet" ;;
  *)         echo "usage: \$0 {start|stop|restart|status|log|supervise}" ;;
esac
RUNEOF
  chmod 755 "$RUNSH"
}

AGENT_NAME=$(hostname 2>/dev/null || echo agent)
AGENT_NAME=$(printf '%s' "$AGENT_NAME" | tr -d '"')
write_runsh
say "wrote     : $RUNSH (manual control: sh $RUNSH status|start|stop|restart|log)"

started_by=""

if [ "$IS_ANDROID" -eq 1 ]; then
  # Android：不注册任何自启、不挂看护循环，启动完全交给用户。
  #
  # 不猜 su 是哪种实现：Magisk / KernelSU / APatch / adbsu 的自启目录各不相同，
  # 系统版本差异又大，写错位置要么静默无效，要么在系统升级后留下一份失效脚本。
  # 这类"猜配置"一旦猜错，用户面对的是一台行为不可预期的设备，代价远大于收益。
  # 所以这里只做一件事：正常启动一次。之后由用户自己控制：
  #     sh <DATA_DIR>/run.sh start|stop|restart|status|log
  "$RUNSH" start >/dev/null 2>&1 || true
  started_by="manual only -- no autostart. control with: sh $RUNSH start|stop|status"
elif have_systemd; then
  cat > "$DATA_DIR/frpeasy-agent.service" <<UNITEOF
[Unit]
Description=frpeasy agent (panel-driven frpc)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN --server $SERVER --token $TOKEN --data $DATA_DIR --name $AGENT_NAME
Restart=always
RestartSec=5
# 面板地址暂时不可达时不要反复重启到 systemd 放弃 (start-limit)
StartLimitIntervalSec=0

[Install]
WantedBy=multi-user.target
UNITEOF
  $SUDO install -m 0644 "$DATA_DIR/frpeasy-agent.service" "$UNIT_SYSTEMD" || die "cannot write $UNIT_SYSTEMD"
  rm -f "$DATA_DIR/frpeasy-agent.service"
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable frpeasy-agent >/dev/null 2>&1 || true
  # restart 而不是 start：即使旧 unit 处于 active 状态也保证跑的是新二进制
  $SUDO systemctl restart frpeasy-agent || $SUDO systemctl start frpeasy-agent
  started_by="systemd (frpeasy-agent.service)"
elif have_openrc; then
  cat > "$DATA_DIR/frpeasy-agent.openrc" <<RCEOF
#!/sbin/openrc-run
name="frpeasy-agent"
command="$BIN"
command_args="--server $SERVER --token $TOKEN --data $DATA_DIR --name $AGENT_NAME"
command_background="yes"
pidfile="/run/\$RC_SVCNAME.pid"
respawn_delay=5
respawn_max=0
depend() { need net; after firewall; }
RCEOF
  $SUDO install -m 0755 "$DATA_DIR/frpeasy-agent.openrc" "$UNIT_OPENRC" || die "cannot write $UNIT_OPENRC"
  rm -f "$DATA_DIR/frpeasy-agent.openrc"
  $SUDO rc-update add frpeasy-agent default >/dev/null 2>&1 || true
  $SUDO "$UNIT_OPENRC" restart >/dev/null 2>&1 || $SUDO "$UNIT_OPENRC" start
  started_by="OpenRC (frpeasy-agent)"
else
  spawn_supervise
  started_by="watchdog (run.sh supervise); no init system found, so autostart on boot is manual"
fi

# ---------------------------------------------------------------- confirm

i=0
ok=0
while [ $i -lt 20 ]; do
  sleep 1
  i=$((i+1))
  if agent_running; then ok=1; break; fi
done

if [ "$ok" -ne 1 ]; then
  warn "agent did not show up as running within 20s."
  [ -f "$LOGFILE" ] && { warn "last lines of $LOGFILE:"; tail -n 20 "$LOGFILE" >&2 2>/dev/null; }
  die "start failed. Re-run this installer, or check: sh $RUNSH log"
fi

if [ -f "$UNIT_SYSTEMD" ] && have_systemd; then
  say "check     : systemctl status frpeasy-agent"
elif [ -f "$UNIT_OPENRC" ]; then
  say "check     : rc-service frpeasy-agent status"
else
  say "check     : sh $RUNSH status   (log: sh $RUNSH log)"
fi

say "done. started by: $started_by"
if [ -n "$OLD_VER" ] && [ "$OLD_VER" != "$VOUT" ]; then
  say "upgraded: $OLD_VER  ->  $VOUT"
fi
say "Open the panel now: this machine should appear under Clients within a few seconds."
say "Uninstall later with: sh frpeasy-install.sh --uninstall"
`

// installScriptPS1 Windows 客户端一键安装脚本（PowerShell 5.1+）。
//
// 四个关键设计（都是踩过坑的）：
//  1. 用「开机启动的计划任务」而非 sc.exe 服务：普通控制台程序不实现 SCM 协议，
//     直接注册为 Windows 服务会启动失败（错误 1053）。
//  2. 以 SYSTEM 身份运行（LogonType ServiceAccount）：AtStartup 触发时用户可能
//     还没登录，若用交互用户身份、口令又没保存，任务会静默不跑。
//  3. 正文纯 ASCII。安装命令是 `iwr -OutFile` 落地再执行的，PS 5.1 读无 BOM 文件
//     按 ANSI 代码页解码，中文会变成乱码并破坏引号配对，直接语法报错。
//  4. 可重复执行且可回滚：先把新二进制下到 .new 并跑 `--version` 验证，再备份旧
//     exe 为 .bak、停旧任务与进程、替换、启动；若新进程起不来就把 .bak 换回去并
//     重启旧版本。用户重跑安装命令既不会把机器搞成"半个客户端"，也不会因为一次
//     下载失败而丢掉可用安装。
const installScriptPS1 = `# frpeasy agent installer / upgrader for Windows (PowerShell 5.1+)
# Must run in an elevated prompt (Run as Administrator).
#
#   .\install.ps1 -Server http://PANEL:8080 -Token <install-token>
#   .\install.ps1 -Server http://PANEL:8080 -Token <token> -Foreground   (debug)
#   .\install.ps1 -Uninstall
#
# Re-running upgrades / repairs the install. The new binary is verified with
# --version before the running one is touched; if it fails to start, the
# previous binary is restored automatically.
#
# Notes: a scheduled task is used instead of a Windows service, because a plain
# console program does not implement the SCM protocol (sc create would fail 1053).
[CmdletBinding()]
param(
  [string]$Server = "",
  [string]$Token  = "",
  [switch]$Uninstall,
  [switch]$Foreground
)

$ErrorActionPreference = "Stop"
$TaskName = "frpeasy-agent"
$dir = Join-Path $env:ProgramData "frpeasy-agent"
$exe = Join-Path $dir "frpeasy-agent.exe"
$bak = Join-Path $dir "frpeasy-agent.exe.bak"

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $pr = New-Object Security.Principal.WindowsPrincipal($id)
  return $pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-AgentProcess {
  return @(Get-Process -Name "frpeasy-agent" -ErrorAction SilentlyContinue)
}

function Remove-AgentTask {
  $t = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  if ($t) {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
  }
  # A stale registration can survive an interrupted install; clear it too.
  $stale = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  if ($stale) { Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue }
}

function Stop-AgentProcess {
  $p = Get-AgentProcess
  if (-not $p) { return $true }
  $p | Stop-Process -Force -ErrorAction SilentlyContinue
  for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 250
    if (-not (Get-AgentProcess)) { return $true }
  }
  # Last resort: taskkill with the tree flag, then report honestly.
  & taskkill /F /IM frpeasy-agent.exe /T 2>$null | Out-Null
  Start-Sleep -Milliseconds 500
  return (-not (Get-AgentProcess))
}

function Fail($msg) {
  Write-Host "[frpeasy] ERROR: $msg" -ForegroundColor Red
  exit 1
}

function Get-ExeVersion($path) {
  try {
    $out = & $path --version 2>&1
    if ($LASTEXITCODE -ne 0) { return $null }
    $line = [string]($out | Select-Object -First 1)
    if ($line -like "frpeasy-agent *") { return $line.Trim() }
    return $null
  } catch {
    return $null
  }
}

function Start-AgentTask($exePath, $argLine) {
  $act = New-ScheduledTaskAction -Execute $exePath -Argument $argLine -WorkingDirectory $dir
  $trg = New-ScheduledTaskTrigger -AtStartup
  $set = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Seconds 0)
  $prn = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest
  Register-ScheduledTask -TaskName $TaskName -Action $act -Trigger $trg -Settings $set -Principal $prn -Force | Out-Null
  Start-ScheduledTask -TaskName $TaskName
}

if (-not (Test-Admin)) {
  Fail "administrator rights required. Right-click PowerShell and choose 'Run as Administrator'."
}

if ($Uninstall) {
  Remove-AgentTask
  [void](Stop-AgentProcess)
  if (Test-Path $dir) { Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue }
  Write-Host "[frpeasy] uninstalled (task, process and $dir removed)."
  exit 0
}

if ($Server -eq "" -or $Token -eq "") {
  Fail "-Server and -Token are required. Example: .\install.ps1 -Server http://1.2.3.4:8080 -Token abc123"
}
$Server = $Server.TrimEnd("/")

$arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { $arch = "arm64" }

Write-Host "[frpeasy] panel  : $Server"
Write-Host "[frpeasy] target : windows/$arch"

New-Item -ItemType Directory -Force -Path $dir | Out-Null

# ---- 1. download to a temp name and verify it before touching the old install
$tmp = "$exe.new"
if (Test-Path $tmp) { Remove-Item $tmp -Force -ErrorAction SilentlyContinue }
$tryArch = @($arch)
if ($arch -eq "arm64") { $tryArch += "amd64" }
$newVer = $null
foreach ($a in $tryArch) {
  $url = "$Server/api/agent/bin/windows/$a"
  Write-Host "[frpeasy] downloading $url ..."
  try {
    Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing -TimeoutSec 300
    $sz = (Get-Item $tmp).Length
    if ($sz -lt 1000000) { throw "downloaded file is too small ($sz bytes)" }
    $newVer = Get-ExeVersion $tmp
    if (-not $newVer) { throw "downloaded file cannot run (wrong architecture or corrupt download)" }
    Write-Host "[frpeasy] verified : $newVer ($([math]::Round($sz/1MB,2)) MB, windows/$a)"
    break
  } catch {
    Write-Host "[frpeasy]   failed: $($_.Exception.Message)" -ForegroundColor Yellow
    $newVer = $null
    if (Test-Path $tmp) { Remove-Item $tmp -Force -ErrorAction SilentlyContinue }
  }
}
if (-not $newVer) {
  Fail "could not download a working agent binary. The existing installation, if any, was left untouched. Make sure the panel has the Windows agent staged (run 'make stage-agent' next to the panel)."
}

$oldVer = $null
if (Test-Path $exe) { $oldVer = Get-ExeVersion $exe }

# ---- 2. foreground debug mode (no service, nothing registered)
$taskArgs = '--server ' + $Server + ' --token ' + $Token + ' --data "' + $dir + '"'
if ($Foreground) {
  [void](Remove-AgentTask)
  [void](Stop-AgentProcess)
  Move-Item -Force $tmp $exe
  Write-Host "[frpeasy] running in foreground (Ctrl+C to stop) ..."
  & $exe --server $Server --token $Token --data $dir
  exit $LASTEXITCODE
}

# ---- 3. stop the old instance, keep a rollback copy, swap in the new binary
[void](Remove-AgentTask)
if (-not (Stop-AgentProcess)) {
  Fail "could not stop the running agent process (still holding $exe). Reboot or kill frpeasy-agent.exe manually, then re-run."
}
if (Test-Path $bak) { Remove-Item $bak -Force -ErrorAction SilentlyContinue }
$hadOld = Test-Path $exe
if ($hadOld) {
  try { Move-Item -Force $exe $bak } catch { Fail "could not back up the old binary: $($_.Exception.Message)" }
}
try {
  Move-Item -Force $tmp $exe
} catch {
  if ($hadOld) { Move-Item -Force $bak $exe -ErrorAction SilentlyContinue }
  Fail "could not install the new binary: $($_.Exception.Message)"
}

# ---- 4. register the scheduled task and start it
Start-AgentTask $exe $taskArgs

$ok = $false
for ($i = 0; $i -lt 20; $i++) {
  Start-Sleep -Seconds 1
  if (Get-AgentProcess) { $ok = $true; break }
}

if ($ok) {
  if (Test-Path $bak) { Remove-Item $bak -Force -ErrorAction SilentlyContinue }
  $p = Get-AgentProcess
  Write-Host "[frpeasy] agent is running (PID $($p[0].Id)), starts automatically at boot."
  if ($oldVer -and $oldVer -ne $newVer) { Write-Host "[frpeasy] upgraded: $oldVer  ->  $newVer" }
  Write-Host "[frpeasy] done. Open the panel: this machine should show up as a client within a few seconds."
  Write-Host "[frpeasy] uninstall later with: .\install.ps1 -Uninstall"
  exit 0
}

# ---- 5. new binary did not come up: roll back instead of leaving a dead client
Write-Host "[frpeasy] WARNING: the new agent did not start. Rolling back ..." -ForegroundColor Yellow
[void](Remove-AgentTask)
[void](Stop-AgentProcess)
if (Test-Path $bak) {
  Move-Item -Force $bak $exe -ErrorAction SilentlyContinue
  $restoreVer = Get-ExeVersion $exe
  if ($restoreVer) {
    Start-AgentTask $exe $taskArgs
    Start-Sleep -Seconds 3
    if (Get-AgentProcess) {
      Write-Host "[frpeasy] rolled back to $restoreVer, the old agent is running again."
    } else {
      Write-Host "[frpeasy] rollback binary restored but did not start either. Check Task Scheduler library > $TaskName." -ForegroundColor Yellow
    }
  }
} else {
  Write-Host "[frpeasy] there was no previous installation to roll back to." -ForegroundColor Yellow
}
Write-Host "[frpeasy] check: Get-ScheduledTask -TaskName $TaskName | Get-ScheduledTaskInfo"
exit 1
`
