#!/usr/bin/env bash
# frpeasy 面板一键部署 / 升级 / 卸载脚本。
#
#   curl -fsSL https://github.com/1370339317/frpeasy/releases/latest/download/deploy.sh | sudo bash
#
# 它做四件事：从 Release 拉最新版 → 校验 sha256 → 装到 /opt/frpeasy → 起 systemd 服务。
# 重复执行就是升级：二进制换掉，data 目录（含数据库）原样保留。
#
# 卸载（删服务、程序、数据，像没装过）：
#   curl -fsSL .../deploy.sh | sudo bash -s -- --uninstall
#   curl -fsSL .../deploy.sh | sudo bash -s -- --uninstall --keep-data   # 留数据库
#   非交互环境（CI、批量脚本）必须加 --yes
#
# 可覆盖的参数（环境变量或命令行），都有合理默认值：
#   BASE=https://...   更新源。默认指向本仓库最新 Release，可换镜像或自建静态目录
#   PREFIX=/opt/frpeasy   安装目录
#   PORT=28080         面板网页端口
#   DATA=$PREFIX/data  数据目录
#   NO_SERVICE=1       不注册 systemd，只放二进制并前台提示怎么跑
#   SERVICE_NAME=frpeasy   unit 名
#
# 用法举例：
#   curl -fsSL .../deploy.sh | sudo PORT=9000 bash
#   curl -fsSLO .../deploy.sh && sudo bash deploy.sh --port 9000 --prefix /srv/frpeasy
set -eu

# ---- 默认参数 ----
DEFAULT_BASE="https://github.com/1370339317/frpeasy/releases/latest/download"
# 记下哪些值来自用户（环境变量或命令行）。升级时只有「用户没指定过」的项
# 才允许从已有安装继承 —— 否则用户这次的意图会被旧配置覆盖掉。
SET_PREFIX=0; SET_PORT=0; SET_LISTEN=0; SET_DATA=0
[ -n "${PREFIX:-}" ] && SET_PREFIX=1
[ -n "${PORT:-}" ] && SET_PORT=1
[ -n "${LISTEN:-}" ] && SET_LISTEN=1
[ -n "${DATA:-}" ] && SET_DATA=1

BASE=${BASE:-$DEFAULT_BASE}
PREFIX=${PREFIX:-/opt/frpeasy}
PORT=${PORT:-28080}
NO_SERVICE=${NO_SERVICE:-0}
SERVICE_NAME=${SERVICE_NAME:-frpeasy}
# 监听地址：默认只听所有地址的 PORT。只想要本机访问就 LISTEN=127.0.0.1:28080
LISTEN=${LISTEN:-:$PORT}
# DATA 的默认值依赖 PREFIX，而 PREFIX 可能被下面的命令行参数改掉，
# 所以这里先留空，等参数解析完再兜底。
DATA=${DATA:-}
# 卸载开关。默认 KEEP_DATA=0：用户要的是「像没装过」，残留一个 SQLite
# 就不算干净。但删数据必须显式确认，见 do_uninstall。
UNINSTALL=${UNINSTALL:-0}
KEEP_DATA=${KEEP_DATA:-0}
ASSUME_YES=${ASSUME_YES:-0}
# unit 目录。真机上永远是 /etc/systemd/system，可覆盖只是为了让
# tests/deploy-smoke.sh 能在临时目录里造一个 unit 来验证「反查安装位置」。
UNIT_DIR=${UNIT_DIR:-/etc/systemd/system}

# ---- 解析命令行（覆盖环境变量）----
while [ $# -gt 0 ]; do
  case "$1" in
    --base) BASE=$2; shift 2 ;;
    --prefix) PREFIX=$2; SET_PREFIX=1; shift 2 ;;
    --port) PORT=$2; LISTEN=":$2"; SET_PORT=1; SET_LISTEN=1; shift 2 ;;
    --listen) LISTEN=$2; SET_LISTEN=1; shift 2 ;;
    --data) DATA=$2; SET_DATA=1; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    --service-name) SERVICE_NAME=$2; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    --keep-data) KEEP_DATA=1; shift ;;
    -y|--yes) ASSUME_YES=1; shift ;;
    -h|--help)
      sed -n '2,32p' "$0" 2>/dev/null || echo "见脚本头部注释"
      exit 0 ;;
    *) echo "未知参数: $1（-h 看用法）" >&2; exit 2 ;;
  esac
done
# DATA 的兜底推迟到 inherit_existing 之后：如果已经有安装，要用它记录的
# 数据目录，而不是 $PREFIX/data —— 否则升级会读到空库，看起来像配置全丢了。
BASE=${BASE%/}

say()  { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die()  { printf '\n错误：%s\n' "$*" >&2; exit 1; }

# ============================================================
# 卸载
#
# 目标是「像没装过一样」：服务、unit、二进制、frp 缓存、数据库全清掉。
# 但绝不能靠猜路径 —— 用户可能用 --prefix / --data 装到别处，也可能早就
# 忘了装在哪。所以优先从 systemd unit 的 ExecStart 反查真实位置，查不到
# 才用命令行参数（含默认值）。
#
# 删数据是有意的默认行为，但必须显式确认：非交互场景要求 --yes，
# 免得某个自动化脚本把它顺手带走。
# ============================================================

# 从 unit 里取 ExecStart 的可执行文件路径
unit_exec() { sed -n 's/^ExecStart=\([^ ]*\).*/\1/p' "$1" 2>/dev/null | head -1; }
# 从 unit 里取 --data 的参数值
unit_data() { sed -n 's/.*--data[[:space:]][[:space:]]*\([^ ]*\).*/\1/p' "$1" 2>/dev/null | head -1; }
# 从 unit 里取 --listen 的参数值
unit_listen() { sed -n 's/.*--listen[[:space:]][[:space:]]*\([^ ]*\).*/\1/p' "$1" 2>/dev/null | head -1; }

# ==== 沿用已有安装的位置（升级的前提）====
# 用户当初可能是 `--prefix /srv/frpeasy --data /data/frpeasy` 装的。升级时若只跑
# `curl ... | sudo bash`，默认值会把它装到 /opt/frpeasy，起一个读空库的新实例：
# 表现为「升级完口令、隧道、客户端全没了，又要重新设一遍」，而旧实例还占着端口。
# 所以先按 unit 里的 ExecStart 反查出真实位置，只继承用户这次没显式指定的项。
inherit_existing() {
  local unit="$UNIT_DIR/$SERVICE_NAME.service"
  [ -f "$unit" ] || return 0

  local exe realp reald reallisten
  exe=$(unit_exec "$unit")
  realp=""
  [ -n "$exe" ] && realp=$(dirname "$exe")
  reald=$(unit_data "$unit")
  reallisten=$(unit_listen "$unit")

  if [ "$SET_PREFIX" -eq 0 ] && [ -n "$realp" ] && [ "$realp" != "$PREFIX" ]; then
    say "    检测到已有安装：沿用安装目录 $realp（默认是 $PREFIX）"
    PREFIX=$realp
  fi
  if [ "$SET_DATA" -eq 0 ] && [ -n "$reald" ]; then
    DATA=$reald
  fi
  if [ "$SET_LISTEN" -eq 0 ] && [ -n "$reallisten" ]; then
    LISTEN=$reallisten
    [ "$SET_PORT" -eq 0 ] && PORT=${reallisten##*:}
  fi
}

# 防止手滑把 /opt 甚至 / 删掉。至少要求「/父/子」两层。
refuse_path() {
  case "$2" in
    ""|"/"|"/."|"/.."|"/usr"|"/usr/local"|"/usr/bin"|"/usr/sbin"|"/bin"|"/sbin"| \
    "/etc"|"/var"|"/var/lib"|"/root"|"/home"|"/opt"|"/srv"|"/data"|"/tmp")
      die "拒绝在可疑路径上操作：$1 = '$2'" ;;
  esac
  printf '%s' "$2" | grep -qE '^/[^/]+/.+' \
    || die "拒绝在过浅的路径上操作：$1 = '$2'（至少要 /父/子 两层）"
}

do_uninstall() {
  local unit="$UNIT_DIR/$SERVICE_NAME.service"

  # ---- 1. 先确定真实安装位置 ----
  if [ -f "$unit" ]; then
    local exe realp reald
    exe=$(unit_exec "$unit")
    if [ -n "$exe" ]; then
      realp=$(dirname "$exe")
      if [ "$realp" != "$PREFIX" ]; then
        say "    依据 systemd unit 修正安装目录：$PREFIX -> $realp"
        PREFIX=$realp
      fi
    fi
    reald=$(unit_data "$unit")
    if [ -n "$reald" ] && [ "$reald" != "$DATA" ]; then
      say "    依据 systemd unit 修正数据目录：$DATA -> $reald"
      DATA=$reald
    fi
  fi

  refuse_path "程序目录 PREFIX" "$PREFIX"
  refuse_path "数据目录 DATA" "$DATA"

  step "将卸载 frpeasy 面板"
  say "    服务      $SERVICE_NAME（$unit）"
  say "    程序目录  $PREFIX"
  if [ "$KEEP_DATA" -eq 1 ]; then
    say "    数据目录  $DATA  （保留：数据库与隧道配置不动）"
  else
    say "    数据目录  $DATA  （含数据库与隧道配置，一并删除）"
  fi

  if [ "$ASSUME_YES" -eq 0 ]; then
    if [ -r /dev/tty ]; then
      printf '\n确认删除以上内容请输入 yes: '
      local ans=""
      read -r ans </dev/tty || ans=""
      [ "$ans" = "yes" ] || die "已取消，什么都没动。"
    else
      die "非交互环境。确认卸载请加 --yes；只想留数据库请加 --keep-data。"
    fi
  fi

  # ---- 2. 停服务 ----
  step "停止服务"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
  fi
  # 裸跑的实例（NO_SERVICE=1 或绿色包）也要收掉。按完整路径认领，
  # 避免把别人的同名进程杀了。
  pkill -f "^$PREFIX/frpeasy-panel" 2>/dev/null || true
  sleep 1

  # ---- 3. 收拾面板拉起的 frp 子进程 ----
  #
  # 面板收到 SIGTERM 时会自己停 frps；但被 SIGKILL（systemd 超时）时来不及，
  # 会留下一个还在监听 7000 的孤儿 —— 重装就撞端口，而且报错完全指不到它。
  # 按可执行文件路径匹配，不用 `pkill frps`（那会杀掉别人的 frps）。
  step "清理残留 frp 进程"
  local f
  for f in "$PREFIX/bin/frps" "$DATA/bin/frps"; do
    pkill -f "^$f" 2>/dev/null || true
  done

  # ---- 4. 删 unit ----
  if [ -f "$unit" ]; then
    rm -f "$unit"
    say "    removed $unit"
  fi
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi

  # ---- 5. 删数据 / 程序目录 ----
  # 默认 DATA 就在 PREFIX 里面，所以「留数据」时必须逐项删 PREFIX、
  # 跳过 data，否则 rm -rf PREFIX 会把要留的东西一起带走。
  local data_inside=0
  case "$DATA" in
    "$PREFIX"/*) data_inside=1 ;;
  esac

  if [ "$KEEP_DATA" -eq 1 ]; then
    say "    保留数据目录 $DATA"
  elif [ -d "$DATA" ]; then
    rm -rf "$DATA"
    say "    removed $DATA"
  fi

  if [ -d "$PREFIX" ]; then
    if [ "$KEEP_DATA" -eq 1 ] && [ "$data_inside" -eq 1 ]; then
      local entry
      for entry in "$PREFIX"/* "$PREFIX"/.[!.]*; do
        [ -e "$entry" ] || continue
        [ "$entry" = "$DATA" ] && continue
        rm -rf "$entry"
      done
      say "    cleared $PREFIX（仅保留 $DATA）"
    else
      rm -rf "$PREFIX"
      say "    removed $PREFIX"
    fi
  fi

  # ---- 6. 报告 ----
  step "卸载完成"
  say "自查（下面这些都应该报「不存在」）："
  say "    systemctl status $SERVICE_NAME"
  say "    ls -d $PREFIX"
  [ "$KEEP_DATA" -eq 0 ] && say "    ls -d $DATA"
  say ""
  if [ "$KEEP_DATA" -eq 1 ]; then
    say "数据库与隧道配置保留在 $DATA。确认不再需要时手动删除："
    say "    rm -rf $DATA"
    say ""
  fi
  say "面板占用的 $PORT 端口已释放。防火墙/安全组里的放行规则不会自动撤销，"
  say "需要的话自己去云控制台或 ufw/firewalld 里删掉。"
  exit 0
}

# ---- 0. 环境检查 ----
case "$(uname -s)" in
  Linux) ;;
  *) die "本脚本只支持 Linux。Windows 请直接下载 frpeasy-panel-windows-*.exe 双击运行。" ;;
esac

# 先沿用已有安装的位置：升级要装回原处，卸载也要知道当初装在哪。
inherit_existing
# DATA 兜底放在继承之后，否则会把已有安装的数据目录覆盖成 $PREFIX/data。
if [ -z "$DATA" ]; then DATA=$PREFIX/data; fi

# 卸载放在架构判断之前：卸载不需要架构匹配（面板装的是哪个平台都该能卸）。
[ "$UNINSTALL" -eq 1 ] && do_uninstall

case "$(uname -m)" in
  x86_64|amd64)   ARCH=amd64 ;;
  aarch64|arm64)  ARCH=arm64 ;;
  armv7l|armv7|arm) ARCH=arm ;;
  i386|i686)      ARCH=386 ;;
  *) die "不认识的架构 $(uname -m)。可以下载源码自己编译，或直接下绿色包手动跑。" ;;
esac
OS=linux

# 面板只有 amd64/arm64（服务端没必要发 32 位）。arm/386 的人应下载客户端。
case "$ARCH" in
  amd64|arm64) ;;
  *) die "面板没有 $ARCH 平台的产物（只有 amd64 和 arm64）。
  $ARCH 通常是客户端设备 —— 面板请装在那台 amd64/arm64 的服务器上，
  客户端由面板生成安装命令下发。" ;;
esac

if [ "$(id -u)" != "0" ] && [ ! -w "$(dirname "$PREFIX")" ]; then
  die "装到 $PREFIX 需要 root。请用 sudo 重跑：curl -fsSL $BASE/deploy.sh | sudo bash"
fi

command -v curl >/dev/null || command -v wget >/dev/null \
  || die "需要 curl 或 wget。Alpine 上先 apk add curl bash"

fetch() { # fetch <url> <输出文件>
  if command -v curl >/dev/null; then
    curl -fsSL --retry 2 --connect-timeout 15 "$1" -o "$2"
  else
    wget -q -O "$2" "$1"
  fi
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---- 1. 拉清单 ----
step "读取版本清单 $BASE/manifest.json"
fetch "$BASE/manifest.json" "$TMP/manifest.json" \
  || die "拉不到清单。更新源是否可访问？可以从面板里换成国内镜像再试，或手动下载绿色包。"
VERSION=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$TMP/manifest.json" | head -1)
[ -n "$VERSION" ] || die "清单里没有 version 字段，格式不对。"
say "    最新版本 $VERSION"

# ---- 2. 从清单里挑出本平台的 panel 资产 ----
# 不用 jq：目标机（Alpine 最小安装）通常没有，而解析一份我们自己生成的、
# 字段顺序固定的 JSON 用 awk 足够，也少一个安装依赖。
ASSET=$(awk -F'"' '
  /"kind":/   { kind=$4 }
  /"os":/     { os=$4 }
  /"arch":/   { arch=$4 }
  /"file":/   { file=$4 }
  /"sha256":/ { sha=$4 }
  /"size":/   { s=$3; sub(/^:[ \t]*/, "", s); gsub(/[ ,]/, "", s); size=s }
  /^[ \t]*\}/ {
    if (kind == "panel" && os == wantos && arch == wantarch && file != "")
      printf "%s %s %s\n", file, sha, size
    kind=""; os=""; arch=""; file=""; sha=""; size=""
  }
' wantos="$OS" wantarch="$ARCH" "$TMP/manifest.json")

[ -n "$ASSET" ] || die "清单里没有 $OS/$ARCH 的面板产物。"
AFILE=$(echo "$ASSET" | cut -d' ' -f1)
ASHA=$(echo  "$ASSET" | cut -d' ' -f2)
ASIZE=$(echo "$ASSET" | cut -d' ' -f3)
say "    产物 $AFILE（$ASIZE 字节）"

# ---- 3. 下载并校验 ----
step "下载 $AFILE"
fetch "$BASE/$AFILE" "$TMP/panel" || die "下载失败：$BASE/$AFILE"

GOT_SIZE=$(wc -c < "$TMP/panel" | tr -d ' ')
[ "$GOT_SIZE" = "$ASIZE" ] || die "大小不符：期望 $ASIZE，实际 $GOT_SIZE（下载被截断或中间有代理）"

if command -v sha256sum >/dev/null; then
  echo "$ASHA  $TMP/panel" | sha256sum -c - >/dev/null 2>&1 \
    || die "sha256 校验失败。文件损坏，或清单与产物不是同一次发布 —— 请联系发布者。"
elif command -v shasum >/dev/null; then
  echo "$ASHA  $TMP/panel" | shasum -a 256 -c - >/dev/null 2>&1 \
    || die "sha256 校验失败。文件损坏，或清单与产物不是同一次发布。"
else
  say "    警告：系统没有 sha256sum/shasum，跳过哈希校验（已核对文件大小）"
fi
chmod 0755 "$TMP/panel"
say "    校验通过"

# 预检：跑一下 --build，确认这份二进制真的能在这台机器上执行
# （架构错、glibc 不兼容、传输中损坏，都会在这一步暴露，而不是等替换完才发现）
if ! "$TMP/panel" --build >/dev/null 2>&1; then
  die "下载的二进制无法在此机器上执行（架构或依赖问题）。当前系统：$(uname -m) / $(uname -s)"
fi

# ---- 4. 停掉正在运行的实例 ----
SERVICE_FILE="$UNIT_DIR/$SERVICE_NAME.service"
if [ -f "$SERVICE_FILE" ] && command -v systemctl >/dev/null; then
  step "停止已有服务 $SERVICE_NAME"
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
else
  # 裸跑（绿色包 / nohup / 宝塔）的情况：按完整路径认领，避免误杀别的东西
  if pgrep -f "^$PREFIX/frpeasy-panel" >/dev/null 2>&1; then
    step "停止正在运行的面板进程"
    pkill -f "^$PREFIX/frpeasy-panel" 2>/dev/null || true
    sleep 1
  fi
fi

# ---- 5. 安装 ----
step "安装到 $PREFIX"
mkdir -p "$PREFIX" "$DATA"
# 先备份：新版本起不来时能立刻换回去（自更新有自动回滚，这里是一键脚本的兜底）
if [ -f "$PREFIX/frpeasy-panel" ]; then
  cp -f "$PREFIX/frpeasy-panel" "$PREFIX/frpeasy-panel.prev" 2>/dev/null || true
fi
cp -f "$TMP/panel" "$PREFIX/frpeasy-panel"
chmod 0755 "$PREFIX/frpeasy-panel"
say "    $PREFIX/frpeasy-panel"

# ---- 6. 注册服务 / 或只提示怎么跑 ----
if [ "$NO_SERVICE" = "1" ]; then
  step "未注册 systemd（NO_SERVICE=1）"
  say "手动启动："
  say "  FRPEASY_UPDATE_BASE='$BASE' '$PREFIX/frpeasy-panel' --data '$DATA' --listen '$LISTEN'"
  say ""
  say "装好了。要以后能自动更新，启动时带上 FRPEASY_UPDATE_BASE 这个环境变量即可。"
  exit 0
fi

if ! command -v systemctl >/dev/null; then
  step "这台机器没有 systemd，只放好二进制"
  say "手动启动："
  say "  FRPEASY_UPDATE_BASE='$BASE' '$PREFIX/frpeasy-panel' --data '$DATA' --listen '$LISTEN'"
  exit 0
fi

step "配置 systemd 服务 $SERVICE_NAME"
# unit 模板从更新源取，保证只有一份可维护的版本（bundle 里那份是同一文件的渲染结果）
fetch "$BASE/frpeasy.service" "$TMP/frpeasy.service" || die "拉不到 frpeasy.service 模板"
esc() { printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'; }
sed -e "s|@@PREFIX@@|$(esc "$PREFIX")|g" \
    -e "s|@@DATA@@|$(esc "$DATA")|g" \
    -e "s|@@LISTEN@@|$(esc "$LISTEN")|g" \
    -e "s|@@UPDATE_BASE@@|$(esc "$BASE")|g" \
    "$TMP/frpeasy.service" > "$SERVICE_FILE"
# unit 里的服务名要与文件名一致，否则 FRPEASY_SERVICE 指错，自更新时
# 交接器会 systemctl start 一个不存在/不对的单元
sed -i "s|^Environment=FRPEASY_SERVICE=.*|Environment=FRPEASY_SERVICE=$SERVICE_NAME.service|" "$SERVICE_FILE"

systemctl daemon-reload
systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || true
systemctl restart "$SERVICE_NAME"

# ---- 7. 等它真的起来 ----
step "等待面板就绪"
i=0
while [ $i -lt 20 ]; do
  if command -v curl >/dev/null && curl -fsS -m 2 "http://127.0.0.1:$PORT/api/healthz" >/dev/null 2>&1; then
    break
  fi
  i=$((i + 1))
  sleep 1
done

if [ $i -ge 20 ]; then
  say ""
  say "面板没在 20 秒内就绪。查看原因："
  say "  systemctl status $SERVICE_NAME"
  say "  journalctl -u $SERVICE_NAME -n 50 --no-pager"
  say ""
  say "要回到上一个版本："
  say "  cp $PREFIX/frpeasy-panel.prev $PREFIX/frpeasy-panel && systemctl restart $SERVICE_NAME"
  exit 1
fi

VER_RUNNING=""
if command -v curl >/dev/null; then
  VER_RUNNING=$(curl -fsS -m 2 "http://127.0.0.1:$PORT/api/healthz" 2>/dev/null \
    | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
fi

# 猜一个能打开的地址。取「路由到外网时使用的源地址」，比 hostname -I 的第一个更靠谱
# （后者在多网卡机器上常给 docker0 之类）。云服务器的公网 IP 通常不在网卡上（NAT），
# 所以这里拿到的多半是内网地址 —— 下面会提示用控制台里的公网 IP。
guess_ip() {
  local ip=""
  if command -v ip >/dev/null 2>&1; then
    ip=$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)
  fi
  if [ -z "$ip" ] && command -v hostname >/dev/null 2>&1; then
    ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  fi
  printf '%s' "$ip"
}
HOST_IP=$(guess_ip)

say ""
say "======================================================"
say " frpeasy 已就绪"
say "   版本    ${VER_RUNNING:-$VERSION}"
if [ -n "$HOST_IP" ]; then
  say "   地址    http://$HOST_IP:$PORT"
else
  say "   地址    http://<这台机器的IP>:$PORT"
fi
say "   目录    $PREFIX   （数据在 $DATA）"
say "   服务    systemctl status $SERVICE_NAME"
say "======================================================"
say ""
say "还差三步才能真正用起来："
say ""
say "  1. 放行端口 TCP $PORT —— 云服务器要在控制台「安全组」和机器防火墙两处都开，"
say "     少一处就是浏览器打不开（内网访问正常、公网超时，最容易被误判成程序挂了）。"
say ""
say "  2. 浏览器打开面板，用 admin / admin 登录，顶部有红条提醒，立刻改口令。"
say ""
say "  3. 设置页填「公网地址」：填你用浏览器访问面板时用的那个地址（云服务器就是公网 IP）。"
say "     填了它，生成的客户端安装命令、客户端连 frps 的地址才会是对的。"
say ""
say "其余要放行的端口（frps 接入、每条隧道的公网端口）在面板左侧「端口 / 防火墙」页"
say "会列全，并给出可直接粘贴的 ufw / firewalld / nftables / iptables 放行命令。"
say ""
say "更新源已写进服务环境变量，以后在面板「设置 → 版本与更新」点一下就自动升级。"
