#!/bin/sh
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
# 二进制来源：panel = 经面板下发（默认），git = 直接从更新源/镜像取。
SOURCE="panel"
GIT_BASE=""

say()  { printf '%s\n' "[frpeasy] $*"; }
warn() { printf '%s\n' "[frpeasy] WARN: $*" >&2; }
die()  { printf '%s\n' "[frpeasy] ERROR: $*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
[frpeasy] agent installer

  --server <url>    panel base url, e.g. http://1.2.3.4:8080   (required)
  --token <tok>     install token from the panel               (required)
  --data <dir>      override the agent data directory
  --source <s>      where to download the agent + frpc binaries from:
                      panel (default) - served by the panel; works on intranet-only boxes
                      git             - straight from the update source (needs --git-base)
  --git-base <url>  base url used with --source git, e.g.
                      https://github.com/you/frpeasy/releases/latest/download
                      (a mirror prefix is already folded in by the panel)
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
    --source)     need_val "$@"; SOURCE="$2"; shift 2 ;;
    --git-base)   need_val "$@"; GIT_BASE="$2"; shift 2 ;;
    --elevate)    ELEVATE=1; shift ;;
    --uninstall)  UNINSTALL=1; shift ;;
    --foreground) FOREGROUND=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            die "unknown argument: $1 (try --help)" ;;
  esac
done

# ---------------------------------------------------------------- binary source

# 为什么要有这个开关：面板的出口带宽是所有客户端共享的，装一台就要发 6.5MB 的
# agent、之后还要发 20MB 的 frpc，慢起来整条链路都在等同一个窄口子。反过来也有
# 只有面板可达的环境（纯内网），那种情况下必须由面板转发。所以交给装的人选。
#
# 换的只是"从哪儿取这个文件"，版本与内容仍由面板决定 —— agent 会把选择记下来，
# 之后拉 frpc 用同一个来源，不需要再选一次。
case "$SOURCE" in
  panel|git) : ;;
  *) die "--source must be 'panel' or 'git' (got '$SOURCE')" ;;
esac
SRC_ARGS=""
AGENT_SRC_ARGS=""
if [ "$SOURCE" = "git" ]; then
  [ -n "$GIT_BASE" ] || die "--source git requires --git-base <url>"
  # SRC_ARGS 给脚本自己用（拼下载地址、提权后重跑）；
  # AGENT_SRC_ARGS 只把 --source 传给 agent —— agent 取 frpc 用的是面板下发的
  # mirror_base + frp_version，用不上 git-base。
  SRC_ARGS="--source git --git-base $GIT_BASE"
  AGENT_SRC_ARGS="--source git"
fi

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
  # 先向面板报备「我要走了」，再删文件。
  #
  # 不报备的话，面板只能看到心跳停了 —— 而「被卸载」和「关机了」在数据上
  # 完全一样，那条记录会一直挂着，让人分不清该不该清。
  # 用二进制自己读 agent.state（里面有面板地址和凭据），脚本不解析 JSON；
  # 老版本二进制不认识这个参数，报错被忽略，行为与以前一致。
  if [ -x "$BIN" ]; then
    "$BIN" --goodbye --data "$DATA_DIR" >/dev/null 2>&1 || true
  fi
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
    REARGS="--server $SERVER --token $TOKEN --elevate $SRC_ARGS"
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
if [ "$SOURCE" = "git" ]; then
  say "source   : git ($GIT_BASE)"
else
  say "source   : panel"
fi
say "binary   : $BIN"
say "data dir : $DATA_DIR"

# ---------------------------------------------------------------- fetch + verify

mkdir -p "$DATA_DIR" 2>/dev/null || $SUDO mkdir -p "$DATA_DIR" 2>/dev/null || die "cannot create $DATA_DIR"
ensure_bin_dir

TMP="$DATA_DIR/.agent.new.$$"
rm -f "$TMP"

if [ "$SOURCE" = "git" ]; then
  # 与面板拼法一致：发布产物就叫 frpeasy-agent-<os>-<arch>，放在 release 根目录。
  # 这里不做任何"面板取不到再回退"的判断 —— 选了什么就是什么，
  # 否则日志里看不出这次走的哪条路（这正是上一版删掉静默回退的原因）。
  URL="$GIT_BASE/frpeasy-agent-$OSKEY-$ARCH"
else
  URL="$SERVER/api/agent/bin/$OSKEY/$ARCH"
fi
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
  if [ "$SOURCE" = "git" ]; then
    die "download failed (rc=$rc) from the git source you picked: $URL
  Nothing was changed on this machine.
  You asked for --source git, so the binary comes straight from the update source instead
  of the panel. This box may simply not be able to reach that host (intranet-only machine,
  DNS, TLS interception).
  Two ways out:
    1) re-run with --source panel instead of --source git (the panel already has the binary
       cached, so it costs the panel no extra download)
    2) if the URL above does open on this machine, check whether it returns an error page:
       curl -i $URL"
  fi
  die "download failed (rc=$rc). The existing installation, if any, was left untouched.
  Most likely the panel has no $OSKEY/$ARCH agent binary and could not fetch one from its
  update source. Check panel -> Settings -> version & update has an update source set.
  To see the exact reason, run this on any machine that can reach the panel:
    curl -i $URL"
fi

SZ=$(wc -c < "$TMP" 2>/dev/null | tr -d ' ')
[ -n "$SZ" ] || SZ=0
if [ "$SZ" -lt 500000 ]; then
  HEAD=$(head -c 300 "$TMP" 2>/dev/null | tr -d '\r')
  rm -f "$TMP"
  die "downloaded file is too small ($SZ bytes) - this is an error page, not a binary.
  The panel said: $HEAD
  Fix: the panel needs to be able to fetch the $OSKEY/$ARCH agent binary. Set an update
  source in panel -> Settings -> version & update, or place the file next to the panel binary."
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
  exec "$TMP" --server "$SERVER" --token "$TOKEN" --data "$DATA_DIR" --name "$(hostname 2>/dev/null || echo agent)" $AGENT_SRC_ARGS
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
AGENT_SRC_ARGS="$AGENT_SRC_ARGS"
running() { [ -f "\$PIDFILE" ] && kill -0 "\$(cat "\$PIDFILE" 2>/dev/null)" 2>/dev/null; }
start() {
  running && { echo "[frpeasy] already running (pid \$(cat "\$PIDFILE"))"; return 0; }
  rm -f "\$PIDFILE"
  # 精简 Android / 容器里不一定有 nohup（toybox 未必编入那一项）。
  # 直接写 "nohup cmd &" 时，没有 nohup 会让整条命令失败、取不到后台 pid，
  # 表面上执行完了、实际什么都没起。所以先探测再决定怎么起。
  if command -v nohup >/dev/null 2>&1; then
    nohup "\$BIN" --server "\$SERVER" --token "\$TOKEN" --data "\$DATA" --name "\$NAME" \$AGENT_SRC_ARGS >>"\$LOG" 2>&1 &
  else
    "\$BIN" --server "\$SERVER" --token "\$TOKEN" --data "\$DATA" --name "\$NAME" \$AGENT_SRC_ARGS >>"\$LOG" 2>&1 &
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
    "\$BIN" --server "\$SERVER" --token "\$TOKEN" --data "\$DATA" --name "\$NAME" \$AGENT_SRC_ARGS >>"\$LOG" 2>&1 &
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
ExecStart=$BIN --server $SERVER --token $TOKEN --data $DATA_DIR --name $AGENT_NAME $AGENT_SRC_ARGS
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
command_args="--server $SERVER --token $TOKEN --data $DATA_DIR --name $AGENT_NAME $AGENT_SRC_ARGS"
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
