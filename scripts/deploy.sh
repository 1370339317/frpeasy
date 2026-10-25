#!/usr/bin/env bash
# frpeasy 面板一键部署 / 升级脚本。
#
#   curl -fsSL https://github.com/1370339317/frpeasy/releases/latest/download/deploy.sh | sudo bash
#
# 它做四件事：从 Release 拉最新版 → 校验 sha256 → 装到 /opt/frpeasy → 起 systemd 服务。
# 重复执行就是升级：二进制换掉，data 目录（含数据库）原样保留。
#
# 可覆盖的参数（环境变量或命令行），都有合理默认值：
#   BASE=https://...   更新源。默认指向本仓库最新 Release，可换镜像或自建静态目录
#   PREFIX=/opt/frpeasy   安装目录
#   PORT=8080          面板网页端口
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
BASE=${BASE:-$DEFAULT_BASE}
PREFIX=${PREFIX:-/opt/frpeasy}
PORT=${PORT:-8080}
NO_SERVICE=${NO_SERVICE:-0}
SERVICE_NAME=${SERVICE_NAME:-frpeasy}
# 监听地址：默认只听所有地址的 PORT。只想要本机访问就 LISTEN=127.0.0.1:8080
LISTEN=${LISTEN:-:$PORT}
# DATA 的默认值依赖 PREFIX，而 PREFIX 可能被下面的命令行参数改掉，
# 所以这里先留空，等参数解析完再兜底。
DATA=${DATA:-}

# ---- 解析命令行（覆盖环境变量）----
while [ $# -gt 0 ]; do
  case "$1" in
    --base) BASE=$2; shift 2 ;;
    --prefix) PREFIX=$2; shift 2 ;;
    --port) PORT=$2; LISTEN=":$2"; shift 2 ;;
    --listen) LISTEN=$2; shift 2 ;;
    --data) DATA=$2; shift 2 ;;
    --no-service) NO_SERVICE=1; shift ;;
    --service-name) SERVICE_NAME=$2; shift 2 ;;
    -h|--help)
      sed -n '2,30p' "$0" 2>/dev/null || echo "见脚本头部注释"
      exit 0 ;;
    *) echo "未知参数: $1（-h 看用法）" >&2; exit 2 ;;
  esac
done
# --prefix 改了之后 data 要跟着走（除非用户显式指定过）
if [ -z "$DATA" ]; then DATA=$PREFIX/data; fi
BASE=${BASE%/}

say()  { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die()  { printf '\n错误：%s\n' "$*" >&2; exit 1; }

# ---- 0. 环境检查 ----
case "$(uname -s)" in
  Linux) ;;
  *) die "本脚本只支持 Linux。Windows 请直接下载 frpeasy-panel-windows-*.exe 双击运行。" ;;
esac

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
  || die "需要 curl 或 wget。Alpine 上先 apk add curl"

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
SERVICE_FILE="/etc/systemd/system/$SERVICE_NAME.service"
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

say ""
say "======================================================"
say " frpeasy 已就绪"
say "   版本    ${VER_RUNNING:-$VERSION}"
say "   地址    http://<这台机器的IP>:$PORT"
say "   目录    $PREFIX   （数据在 $DATA）"
say "   服务    systemctl status $SERVICE_NAME"
say "======================================================"
say ""
say "首次登录用 admin / admin —— 页面顶部会红条提醒，登录后立刻改口令。"
say "更新源已写进服务环境变量，以后在面板「设置 → 版本与更新」点一下就自动升级。"
