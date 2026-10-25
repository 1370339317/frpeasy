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

echo "=== 4. 卸载 --keep-data：程序清掉、数据库留下 ==="
mkdir -p "$PREFIX/data"
echo "db" > "$PREFIX/data/frpeasy.db"
if BASE="$BASE" PREFIX="$PREFIX" NO_SERVICE=1 \
   bash "$REL/deploy.sh" --uninstall --keep-data --yes >"$WORK/un1.log" 2>&1; then
  ok "卸载退出码 0"
else
  bad "卸载失败"; cat "$WORK/un1.log"
fi
[ -e "$PREFIX/frpeasy-panel" ] && bad "二进制还在" || ok "二进制已删除"
[ -e "$PREFIX/frpeasy-panel.prev" ] && bad ".prev 备份还在（会残留）" || ok ".prev 备份已删除"
[ -f "$PREFIX/data/frpeasy.db" ] && ok "数据库被保留（--keep-data 生效）" \
  || bad "数据库被删掉了 —— --keep-data 没起作用"

echo "=== 5. 重新安装后全量卸载：像没装过一样 ==="
if BASE="$BASE" PREFIX="$PREFIX" NO_SERVICE=1 bash "$REL/deploy.sh" >"$WORK/run4.log" 2>&1; then
  ok "重装成功"
else
  bad "重装失败"; cat "$WORK/run4.log"
fi
mkdir -p "$PREFIX/data" && echo "db" > "$PREFIX/data/frpeasy.db"
# 造一个假的 frp 缓存目录，验证 ·/bin 也被带走
mkdir -p "$PREFIX/bin" && echo "frps" > "$PREFIX/bin/frps"

if BASE="$BASE" PREFIX="$PREFIX" NO_SERVICE=1 \
   bash "$REL/deploy.sh" --uninstall --yes >"$WORK/un2.log" 2>&1; then
  ok "卸载退出码 0"
else
  bad "卸载失败"; cat "$WORK/un2.log"
fi
[ -e "$PREFIX" ] && { bad "程序目录还在：$(ls -A "$PREFIX" 2>/dev/null | tr '\n' ' ')"; } \
  || ok "程序目录整个消失"
[ -e "$PREFIX/data/frpeasy.db" ] && bad "数据库残留" || ok "数据库已删除"
[ -e "$PREFIX/bin/frps" ] && bad "frp 二进制残留" || ok "frp 二进制已删除"

echo "=== 6. 忘了装在哪：从 systemd unit 反查真实位置 ==="
FORGOT="$WORK/forgotten"
mkdir -p "$FORGOT/bin" "$FORGOT/data" "$WORK/units"
cp bin/frpeasy-panel-linux-amd64 "$FORGOT/frpeasy-panel"
chmod +x "$FORGOT/frpeasy-panel"
echo "db" > "$FORGOT/data/frpeasy.db"
echo "frps" > "$FORGOT/bin/frps"
cat > "$WORK/units/frpeasy-forgot.service" <<UNIT
[Unit]
Description=frpeasy (test)
[Service]
ExecStart=$FORGOT/frpeasy-panel --data $FORGOT/data --listen :8080
[Install]
WantedBy=multi-user.target
UNIT
# 故意给一组完全错误的 --prefix / --data：脚本必须以 unit 为准，
# 否则用户「忘了装在哪」就永远卸不干净。
if PREFIX=/tmp/wrong-prefix DATA=/tmp/wrong-prefix/data \
   SERVICE_NAME=frpeasy-forgot UNIT_DIR="$WORK/units" BASE="$BASE" \
   bash "$REL/deploy.sh" --uninstall --yes >"$WORK/un3.log" 2>&1; then
  ok "卸载退出码 0"
else
  bad "卸载失败"; cat "$WORK/un3.log"
fi
grep -q "修正安装目录" "$WORK/un3.log" && ok "日志说明已依据 unit 修正路径" \
  || { bad "没提示修正路径"; tail -6 "$WORK/un3.log"; }
[ -e "$FORGOT" ] && { bad "反查到的目录没被删掉"; } || ok "按 unit 反查的目录已清除"
[ -e "$WORK/units/frpeasy-forgot.service" ] && bad "unit 文件残留" || ok "unit 文件已删除"

echo "=== 7. 拒绝在可疑路径上卸载（防止手滑删系统目录）==="
if PREFIX=/opt BASE="$BASE" bash "$REL/deploy.sh" --uninstall --yes >"$WORK/un4.log" 2>&1; then
  bad "PREFIX=/opt 竟然被接受了"
else
  ok "拒绝 PREFIX=/opt"
  grep -q "拒绝" "$WORK/un4.log" && ok "错误信息说明了拒绝原因" || bad "没说清拒绝原因"
fi

echo "=== 8. 篡改产物后必须拒绝安装（sha256 校验有效）==="
# 换一个字节但保持大小不变，模拟「清单与产物不是同一批发布」
printf 'X' | dd of="$REL/frpeasy-panel-linux-amd64" bs=1 seek=200 conv=notrunc status=none
if BASE="$BASE" PREFIX="$WORK/install2" NO_SERVICE=1 bash "$REL/deploy.sh" >"$WORK/run3.log" 2>&1; then
  bad "篡改后的产物竟然装成功了 —— sha256 校验没生效"
else
  ok "拒绝安装被篡改的产物"
  grep -q "sha256\|大小不符" "$WORK/run3.log" && ok "错误信息指明了原因" || {
    bad "错误信息没说清原因"; tail -5 "$WORK/run3.log"; }
fi

echo "=== 9. 升级时沿用已有安装的目录 / 数据目录 / 端口（不变成重装）==="
# 恢复第 8 步被篡改的产物，继承测试需要真下载
cp bin/frpeasy-panel-linux-amd64 "$REL/"
UPG="$WORK/upg-prefix"
UPGDATA="$WORK/upg-data"
mkdir -p "$UPG" "$UPGDATA" "$WORK/units2"
echo "db" > "$UPGDATA/frpeasy.db"
cat > "$WORK/units2/frpeasy.service" <<UNIT
[Unit]
Description=frpeasy (upgrade test)
[Service]
ExecStart=$UPG/frpeasy-panel --data $UPGDATA --listen :7777
[Install]
WantedBy=multi-user.target
UNIT
# 不指定 PREFIX/DATA/LISTEN —— 脚本必须从 unit 反查继承，否则会装到 /opt/frpeasy
# 读一个空库，表现为「升级完配置全丢了」。
if BASE="$BASE" UNIT_DIR="$WORK/units2" NO_SERVICE=1 bash "$REL/deploy.sh" >"$WORK/upg.log" 2>&1; then
  ok "升级执行退出码 0"
else
  bad "升级失败"; cat "$WORK/upg.log"
fi
grep -q "沿用安装目录" "$WORK/upg.log" && ok "日志说明沿用了原安装目录" \
  || { bad "没提示沿用目录"; tail -6 "$WORK/upg.log"; }
[ -x "$UPG/frpeasy-panel" ] && ok "二进制装回原目录 $UPG" || bad "没装回原目录"
grep -q -- "--data $UPGDATA" "$WORK/upg.log" && ok "数据目录沿用 $UPGDATA（不会读空库）" \
  || bad "数据目录没沿用：$(grep -o -- '--data[^ ]*' "$WORK/upg.log" | head -1)"
grep -q -- "--listen :7777" "$WORK/upg.log" && ok "监听端口沿用 :7777" || bad "端口没沿用"
[ -f "$UPGDATA/frpeasy.db" ] && ok "原数据库完好" || bad "原数据库被动过"

echo ""
if [ "$FAIL" -eq 0 ]; then
  echo "deploy-smoke 全部通过"
else
  echo "deploy-smoke 失败 $FAIL 项"
  exit 1
fi
