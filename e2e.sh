#!/usr/bin/env bash
# frpeasy 真实端到端穿透测试（隔离运行：独立端口 + 独立数据目录，可重复执行不互相污染）
#   下载并启动真实 frps -> 真实 agent 下载 frpc -> 建隧道 -> 通过公网端口实际访问本地服务
# 可用环境变量：MIRROR=镜像前缀（默认 ghfast.top，直连 GitHub 太慢时用）
cd /c/Users/13703/WorkBuddy/frpeasy || exit 1

# 强制用当前源码重新构建被测二进制：否则很容易跑到「上次构建的过期 exe」，
# 排查半天以为是业务逻辑坏了（真实踩过：旧 agent 里"每 15s 无谓重启 frpc"的
# bug 已修，但脚本用的根目录 exe 还是旧的，导致穿透一直不通）。
# 构建到独立文件名：Windows 上正在运行的同名 exe 无法被覆盖。
PBIN=./e2e-panel-bin.exe
ABIN=./e2e-agent-bin.exe
go build -trimpath -ldflags "-s -w" -o $PBIN ./cmd/panel || { echo "构建 panel 失败"; exit 1; }
go build -trimpath -ldflags "-s -w" -o $ABIN ./cmd/agent || { echo "构建 agent 失败"; exit 1; }

MIRROR=${MIRROR:-https://ghfast.top/}
STAMP=$$_$(date +%s)
PANEL_PORT=$(( 19000 + ($$ % 800) ))
FRPS_PORT=$(( 17000 + ($$ % 800) ))
TUN_PORT_START=$(( 21000 + ($$ % 800) ))
WWW_PORT=$(( 23000 + ($$ % 500) ))
DATA=./e2e-data-$STAMP
ADATA=./e2e-agent-$STAMP
WWW=./e2e-www-$STAMP

PASS=0; FAIL=0
R=/c/Users/13703/WorkBuddy/frpeasy/e2e-result.txt
: > "$R"
say() { echo "$*" | tee -a "$R"; }
chk() { if [ "$2" = "$3" ]; then say "  PASS  $1"; PASS=$((PASS+1)); else say "  FAIL  $1 (期望 [$2] 实际 [$3])"; FAIL=$((FAIL+1)); fi; }
jget() { python -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

say "端口规划: panel=$PANEL_PORT frps=$FRPS_PORT 隧道端口起=$TUN_PORT_START 本地服务=$WWW_PORT 镜像=$MIRROR"

mkdir -p "$WWW" && echo "FRPEASY_E2E_OK" > "$WWW/index.html"
python -m http.server $WWW_PORT --bind 127.0.0.1 --directory "$WWW" > e2e-www.log 2>&1 &
WWW_PID=$!
# 无论怎么退出（含被强杀）都要收掉子进程：残留的 python http.server 会一直占着
# 端口，下一次运行就会连到「旧目录」上，表现为 frpc 明明 start proxy success，
# 公网端口却取不到预期页面（真实踩过，排查了很久）。
cleanup() {
  kill $A 2>/dev/null; kill $P 2>/dev/null; kill $WWW_PID 2>/dev/null
  wait $A $P $WWW_PID 2>/dev/null
}
trap cleanup EXIT

$PBIN --data "$DATA" --listen ":$PANEL_PORT" > e2e-panel.log 2>&1 &
P=$!
sleep 2
B=http://127.0.0.1:$PANEL_PORT

say "== 1) 登录、设置公网地址 / 端口 / 镜像源 =="
chk "登录" True "$(curl -s -c e2e-cj -X POST $B/api/login -H 'Content-Type: application/json' -d '{"user":"admin","pass":"admin"}' | jget "d['ok']")"
curl -s -b e2e-cj -X POST $B/api/settings -H 'Content-Type: application/json' \
  -d "{\"public_addr\":\"127.0.0.1\",\"frps_bind_port\":\"$FRPS_PORT\",\"port_range_start\":\"$TUN_PORT_START\",\"port_range_end\":\"$((TUN_PORT_START+50))\",\"mirror_base\":\"$MIRROR\"}" >/dev/null
chk "镜像源已生效" "$MIRROR" "$(curl -s -b e2e-cj $B/api/settings | jget "d['mirror_base']")"
chk "Windows 安装脚本可取" 1 "$(curl -s $B/install.ps1 | grep -c 'Register-ScheduledTask')"
chk "安装命令含 Windows 版本" 1 "$(curl -s -b e2e-cj $B/api/tunnels | grep -c 'install.ps1')"

say "== 2) 经镜像下载并启动真实 frps =="
# 显式启动：设置变更只会“重启正在运行的 frps”，不会把停止状态的服务拉起来
# （这是刻意的产品语义 —— 用户停掉的服务不该因为改设置就自己跑起来）。
curl -s -b e2e-cj -X POST "$B/api/frps?action=start" >/dev/null
T0=$SECONDS
alive=False
for i in $(seq 1 120); do
  FS=$(curl -s -b e2e-cj $B/api/frps)
  alive=$(echo "$FS" | jget "d['port_alive']")
  [ "$alive" = "True" ] && break
  if [ $((i % 10)) -eq 0 ]; then say "  ...第 $((SECONDS-T0))s 仍在拉取/启动"; fi
  sleep 5
done
say "  拉取+启动耗时 $((SECONDS-T0))s"
chk "frps 二进制就绪" True "$(echo "$FS" | jget "d['binary_ready']")"
chk "frps 进程在跑" True "$(echo "$FS" | jget "d['running']")"
chk "frps 端口可连" True "$alive"

say "== 3) 起真实 agent（自动下载 frpc 并注册） =="
IC=$(curl -s -b e2e-cj $B/api/tunnels | jget "d['install_command']")
TOKEN=$(echo "$IC" | sed -n 's/.*--token \([^ ]*\).*/\1/p')
$ABIN --server $B --token "$TOKEN" --name e2e-pc --data "$ADATA" > e2e-agent.log 2>&1 &
A=$!
AGID=""
for i in $(seq 1 45); do
  AGID=$(curl -s -b e2e-cj $B/api/tunnels | jget "d['agents'][0]['id'] if d['agents'] else ''")
  [ -n "$AGID" ] && break
  sleep 2
done
chk "agent 已注册" 32 "${#AGID}"

say "== 4) 建隧道（绑定该 agent + 指向本地 8123） =="
T=$(curl -s -b e2e-cj -X POST $B/api/tunnels -H 'Content-Type: application/json' \
    -d "{\"name\":\"e2e\",\"local_addr\":\"127.0.0.1:$WWW_PORT\",\"agent_id\":\"$AGID\"}")
PORT=$(echo "$T" | jget "d['tunnel']['public_port']")
chk "端口从自定义池分配" "$TUN_PORT_START" "$PORT"

say "== 5) 等 frpc 连上 frps 并真实穿透（最多 8 分钟） =="
OK=0; T0=$SECONDS
for i in $(seq 1 160); do
  BODY=$(curl -s -m 3 "http://127.0.0.1:$PORT/" 2>/dev/null)
  case "$BODY" in *FRPEASY_E2E_OK*) OK=1; break;; esac
  if [ $((i % 15)) -eq 0 ]; then say "  ...第 $((SECONDS-T0))s 仍未通"; fi
  sleep 3
done
chk "通过公网端口 $PORT 取到本地页面" 1 "$OK"
TID=$(echo "$T" | jget "d['tunnel']['id']")
# 面板状态来自 frps dashboard 轮询，最多给它 30s 收敛（穿透本身已在上一步验证为真通）
ONLINE=0
for i in $(seq 1 6); do
  ONLINE=$(curl -s -b e2e-cj $B/api/tunnels | jget "[t for t in d['tunnels'] if t['id']==$TID][0]['online']")
  [ "$ONLINE" = "True" ] && break
  sleep 5
done
chk "面板显示隧道在线" True "$ONLINE"

say ""
say "--- frps 日志关键行 ---"
grep -aiE "start|listen|login|proxy|error" "$DATA/frps.log" 2>/dev/null | tail -10 | tee -a "$R" || say "  (无)"
say "--- agent 日志关键行 ---"
grep -aiE "已注册|frpc|失败|error" e2e-agent.log 2>/dev/null | tail -10 | tee -a "$R"

# 子进程由 trap cleanup 收掉
say ""
say "===== PASS=$PASS FAIL=$FAIL ====="
say "数据目录: $DATA / $ADATA"
[ "$FAIL" -eq 0 ] || exit 1
