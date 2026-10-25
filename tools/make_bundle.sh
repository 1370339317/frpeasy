#!/usr/bin/env bash
# 为每个 linux 面板产物打一个「绿色包」tar.gz。
#
# 用法：make_bundle.sh <bin目录> <版本> <输出目录> [更新源地址]
#
# 为什么要单独做绿色包：Release 里的裸二进制适合脚本和自更新，但人手动下载时
# 只有一个光秃秃的可执行文件，没有 unit、没有说明。绿色包把三样东西放一起：
# 二进制 + systemd unit + 一份「怎么跑」的说明，解压即用。
#
# 输出到 <bin>/bundles/ 而不是 <bin>/：genmanifest 只扫 bin/ 顶层，
# 放进子目录就不会被当成「未登记的平台产物」而让 check-manifest 报错。
set -eu

BIN=${1:-bin}
VER=${2:-0.0.0}
OUT=${3:-$BIN/bundles}
UPDATE_BASE=${4:-}

# 更新源地址要拼进 systemd unit 的 Environment，URL 里的 & 会被 sed 当成
# 「整个匹配串」而造成静默损坏，所以先转义。
esc() { printf '%s' "$1" | sed -e 's/[\\&|]/\\&/g'; }
BASE_ESC=$(esc "$UPDATE_BASE")

mkdir -p "$OUT"
rm -f "$OUT"/*.tar.gz

unit_src="scripts/frpeasy.service"
if [ ! -f "$unit_src" ]; then
  echo "找不到 $unit_src（请在仓库根目录执行）" >&2
  exit 1
fi

count=0
for f in "$BIN"/frpeasy-panel-linux-*; do
  [ -f "$f" ] || continue
  case "$f" in
    *.tar.gz) continue ;;
  esac
  arch=${f##*-}
  stage=$(mktemp -d)
  trap 'rm -rf "$stage"' EXIT

  cp "$f" "$stage/frpeasy-panel"
  chmod 0755 "$stage/frpeasy-panel"

  sed -e "s|@@PREFIX@@|/opt/frpeasy|g" \
      -e "s|@@DATA@@|/opt/frpeasy/data|g" \
      -e "s|@@LISTEN@@|:8080|g" \
      -e "s|@@UPDATE_BASE@@|$BASE_ESC|g" \
      "$unit_src" > "$stage/frpeasy.service"

  cat > "$stage/README.txt" <<EOF
frpeasy 面板 v$VER (linux-$arch)
================================================================

绿色包：解压后就是可执行文件，不需要安装，不依赖任何运行库
（静态编译，Alpine / Debian / Ubuntu / 群晖都能直接跑）。

【最快：直接运行】
  tar xzf frpeasy-panel-$VER-linux-$arch.tar.gz
  chmod +x frpeasy-panel
  ./frpeasy-panel --data ./data --listen :8080

  浏览器打开  http://<服务器IP>:8080
  默认账号 admin / admin —— 登录后页面顶部会红条提醒你改口令。

【要开机自启：交给 systemd】
  sudo mkdir -p /opt/frpeasy
  sudo cp frpeasy-panel /opt/frpeasy/
  sudo cp frpeasy.service /etc/systemd/system/
  sudo systemctl daemon-reload
  sudo systemctl enable --now frpeasy
  sudo systemctl status frpeasy     # 看是否 active (running)
  journalctl -u frpeasy -f          # 跟日志

  unit 里写死了 /opt/frpeasy 和 :8080。换目录或换端口，
  改 ExecStart 那一行，并把 WorkingDirectory 一起改。

【自动更新】
  面板内「设置 → 版本与更新」会去更新源检查新版，点按钮就自动完成
  「下载 → 校验 sha256 → 试跑预检 → 替换 → 重启 → 校验版本」，
  起不来会自动回滚到旧版本。

  更新源写在 unit 的 Environment=FRPEASY_UPDATE_BASE 里：
    ${UPDATE_BASE:-（未设置，可在面板里手动填）}
  你也可以在面板设置里改成自建静态目录或镜像。

  裸跑（不走 systemd）同样能点更新，面板会自己完成替换和重启。

【数据放在哪】
  全部在一个 SQLite 文件里：<--data 指定的目录>/frpeasy.db
  备份就是拷这一个文件。升级不会动它。

【要开哪些端口】
  - 面板网页端口：默认 8080（本文件里的 unit 就是它）
  - frps 的客户端接入端口：默认 7000（面板会提示你放行）
  - 隧道的公网端口：默认从 10000-20000 里自动分配
  面板「端口/防火墙」页会把当前需要放行的端口列出来，并给出对应的
  ufw / firewalld / iptables 命令。
EOF

  name="frpeasy-panel-$VER-linux-$arch.tar.gz"
  tar -C "$stage" -czf "$OUT/$name" frpeasy-panel frpeasy.service README.txt
  echo "  $name"
  count=$((count + 1))
  rm -rf "$stage"
  trap - EXIT
done

if [ "$count" -eq 0 ]; then
  echo "没找到 frpeasy-panel-linux-* 产物，先跑 make build-alpine" >&2
  exit 1
fi
echo "共 $count 个绿色包 -> $OUT"
