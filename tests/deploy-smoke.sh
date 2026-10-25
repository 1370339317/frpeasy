#!/usr/bin/env bash
# 端到端验证 scripts/deploy.sh —— 用一个本地静态目录冒充 GitHub Release。
#
# 只能在 Linux 上跑：deploy.sh 本身明确拒绝非 Linux（Windows 走绿色 exe），
# 而且它下载的是 linux 二进制，需要能在本机执行才能验证预检那一步。
# 所以这个脚本给 CI 的 ubuntu runner 用，本地 Windows 开发机上跑不了。
#
#   bash tests/deploy-smoke.sh
set -eu
cd "$(dirname "$0")/.."

FAIL=0
ok()   { printf '  OK   %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*"; FAIL=$((FAIL + 1)); }

if [ "$(uname -s)" != "Linux" ]; then
  echo "跳过：deploy.sh 只支持 Linux（当前 $(uname -s)）"
  exit 0
fi

for f in bin/manifest.json bin/frpeasy-panel-linux-amd64 scripts/deploy.sh scripts/frpeasy.service; do
  [ -f "$f" ] || { echo "缺少 $f，先跑 make dist"; exit 1; }
done

REL=$(mktemp -d)
WORK=$(mktemp -d)
SRV_PID=""
APID=""
cleanup() {
  [ -n "$APID" ] && kill "$APID" 2>/dev/null || true
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null || true
  rm -rf "$REL" "$WORK"
}
trap cleanup EXIT

# ---- 造一个"Release 目录"：就是 deploy.sh 期望在 BASE 下看到的那几个文件 ----
cp bin/manifest.json "$REL/"
cp bin/frpeasy-panel-linux-amd64 "$REL/"
cp scripts/frpeasy.service "$REL/"
cp scripts/deploy.sh "$REL/"

freeport() {
  python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'
}
PORT=$(freeport)
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$REL" >/dev/null 2>&1 &
SRV_PID=$!
BASE="http://127.0.0.1:$PORT"

# 等 http 服务就绪（最多 10s）
i=0
while [ $i -lt 40 ]; do
  python3 - "$PORT" <<'PY' 2>/dev/null && break
import sys, urllib.request
urllib.request.urlopen("http://127.0.0.1:%s/manifest.json" % sys.argv[1], timeout=1)
PY
  i=$((i + 1)); sleep 0.25
done

PREFIX="$WORK/install"

echo "=== 1. 首次安装（NO_SERVICE=1，装到临时目录）==="
if BASE="$BASE" PREFIX="$PREFIX" NO_SERVICE=1 bash "$REL/deploy.sh" >"$WORK/run1.log" 2>&1; then
  ok "脚本退出码 0"
else
  bad "脚本失败，日志："; cat "$WORK/run1.log"
fi

[ -x "$PREFIX/frpeasy-panel" ] && ok "二进制已就位且可执行" || bad "二进制不存在或不可执行"

VER=$( "$PREFIX/frpeasy-panel" --build 2>/dev/null | head -1 || true )
case "$VER" in
  *frpeasy-panel*) ok "--build 可执行：$VER" ;;
  *) bad "--build 输出异常：$VER" ;;
esac

# 清单里的版本要和二进制自报的一致 —— 不一致说明清单与产物不是同一批（发布事故）
MAN_VER=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$REL/manifest.json" | head -1)
case "$VER" in
  *"$MAN_VER"*) ok "版本与清单一致（$MAN_VER）" ;;
  *) bad "版本不一致：清单 $MAN_VER，二进制 $VER" ;;
esac

echo "=== 2. 真跑起来并响应 healthz ==="
APORT=$(freeport)
"$PREFIX/frpeasy-panel" --data "$WORK/data" --listen "127.0.0.1:$APORT" >"$WORK/panel.log" 2>&1 &
APID=$!
GOT=""
i=0
while [ $i -lt 40 ]; do
  GOT=$(python3 - "$APORT" <<'PY' 2>/dev/null || true
import sys, urllib.request
print(urllib.request.urlopen("http://127.0.0.1:%s/api/healthz" % sys.argv[1], timeout=1).read().decode())
PY
)
  [ -n "$GOT" ] && break
  i=$((i + 1)); sleep 0.25
done
if [ -n "$GOT" ]; then
  ok "healthz 有响应"
  case "$GOT" in
    *"\"version\":\"$MAN_VER\""*) ok "healthz 报告版本 $MAN_VER" ;;
    *) bad "healthz 版本不符：$GOT" ;;
  esac
else
  bad "healthz 无响应"; tail -20 "$WORK/panel.log"
fi
kill "$APID" 2>/dev/null || true
wait "$APID" 2>/dev/null || true
APID=""

echo "=== 3. 重复执行 = 升级（幂等，且保留数据目录）==="
echo "sentinel" > "$WORK/data/sentinel.txt"
if BASE="$BASE" PREFIX="$PREFIX" NO_SERVICE=1 bash "$REL/deploy.sh" >"$WORK/run2.log" 2>&1; then
  ok "第二次执行退出码 0"
else
  bad "第二次执行失败"; cat "$WORK/run2.log"
fi
[ -f "$WORK/data/sentinel.txt" ] && ok "数据目录被保留" || bad "数据目录被清掉了"
[ -f "$PREFIX/frpeasy-panel.prev" ] && ok "留下了 .prev 备份" || bad "没留 .prev 备份（回滚无路）"

echo "=== 4. 篡改产物后必须拒绝安装（sha256 校验有效）==="
# 换一个字节但保持大小不变，模拟「清单与产物不是同一批发布」
printf 'X' | dd of="$REL/frpeasy-panel-linux-amd64" bs=1 seek=200 conv=notrunc status=none
if BASE="$BASE" PREFIX="$WORK/install2" NO_SERVICE=1 bash "$REL/deploy.sh" >"$WORK/run3.log" 2>&1; then
  bad "篡改后的产物竟然装成功了 —— sha256 校验没生效"
else
  ok "拒绝安装被篡改的产物"
  grep -q "sha256\|大小不符" "$WORK/run3.log" && ok "错误信息指明了原因" || {
    bad "错误信息没说清原因"; tail -5 "$WORK/run3.log"; }
fi

echo ""
if [ "$FAIL" -eq 0 ]; then
  echo "deploy-smoke 全部通过"
else
  echo "deploy-smoke 失败 $FAIL 项"
  exit 1
fi
