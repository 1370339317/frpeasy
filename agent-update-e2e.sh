#!/usr/bin/env bash
# 客户端（agent）自更新端到端演练：真实面板 + 真实 agent 二进制，真实替换、真实交接。
#
# 为什么必须单独演练（面板自更新的脚本覆盖不到）：
#   agent 的替换发生在**客户端自己身上**，它要按面板下发的 sha256 校验、
#   要试跑 --version 确认架构匹配、要在替换后把自己重新拉起来。
#   单元测试只能证明各函数对，证明不了"换完之后客户端还在正常心跳"。
#
# 覆盖：
#   A. 0.3.0 → 0.4.0 自更新：下载 → 校验 → 替换 → 交接 → 新版本继续心跳
#   B. 面板自带版本更旧时不降级（防止把客户端降坏）
#   C. 更新后仍只有一条客户端记录（身份延续，不产生重复登记）
#   D. 更新前留档 .prev，可用于人工回滚
#
# 每次运行使用独立端口与目录，可反复执行。
set -u

ROOT=$(cd "$(dirname "$0")" && pwd)
cd "$ROOT" || exit 1

GOOS=$(go env GOOS)
GOARCH=$(go env GOARCH)
SUF=""
[ "$GOOS" = "windows" ] && SUF=".exe"
AGENT_ASSET="frpeasy-agent-$GOOS-$GOARCH$SUF"

TMP=$(mktemp -d 2>/dev/null || echo "/tmp/frpeasy-agup-$$")
mkdir -p "$TMP"
PANEL_DIR="$TMP/panel"   # 面板自身 + 待下发的 agent 产物
LIVE_DIR="$TMP/live"     # 正在运行的 agent（升级要在原地替换它）
DATA_P="$TMP/pdata"
DATA_A="$TMP/adata"
mkdir -p "$PANEL_DIR" "$LIVE_DIR" "$DATA_P" "$DATA_A"

PORT=${PORT:-18191}
BASE="http://127.0.0.1:$PORT"
PANEL="$PANEL_DIR/frpeasy-panel$SUF"
LIVE="$LIVE_DIR/frpeasy-agent$SUF"

PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n     expect=%s actual=%s\n' "$1" "$2" "$3"; }
chk() { [ "$2" = "$3" ] && ok "$1" || bad "$1" "$2" "$3"; }
say() { printf '\n\033[36m== %s\033[0m\n' "$1"; }

cleanup() {
  [ -n "${AGPID:-}" ] && taskkill //PID "$AGPID" //F >/dev/null 2>&1
  for p in $(netstat -ano 2>/dev/null | grep LISTENING | grep ":$PORT " | awk '{print $5}' | sort -u); do
    taskkill //PID "$p" //F >/dev/null 2>&1
  done
  [ -n "${PANELPID:-}" ] && kill "$PANELPID" 2>/dev/null
  sleep 0.5
}
trap cleanup EXIT

jget() { python -c "
import sys, json
d = json.load(sys.stdin)
print(eval(sys.argv[1], {'d': d}))
" "$1" 2>/dev/null; }
login() { curl -s --noproxy '*' -c "$TMP/ck" -X POST "$BASE/api/login" \
    -H 'Content-Type: application/json' -d '{"user":"admin","pass":"admin"}' -o /dev/null -w '%{http_code}'; }
# --version 输出两行（版本行 + built 行），只取第一行，否则断言会拿到构建时间
ver_of() { "$1" --version 2>/dev/null | head -1 | awk '{print $2}'; }

# 轮询客户端在面板上登记到的版本
wait_agent_version() { # $1=期望版本 $2=超时秒
  local want=$1 limit=$2 i=0
  while [ $i -lt $((limit*2)) ]; do
    v=$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/agents" | jget "d['agents'][0]['agent_version'] if d['agents'] else ''")
    [ "$v" = "$want" ] && return 0
    sleep 0.5
    i=$((i+1))
  done
  echo "    (面板上仍是: ${v:-无记录})"
  return 1
}

# ------------------------------------------------------------------ 准备二进制
say "编译：面板 0.4.0 + agent 0.3.0（运行中）/ 0.4.0（待下发）"
LD030="-s -w -X main.version=0.3.0 -X main.buildTime=2026-10-25T00:00:00Z"
LD040="-s -w -X main.version=0.4.0 -X main.buildTime=2026-10-25T00:00:00Z"
go build -ldflags "$LD040" -o "$PANEL" ./cmd/panel || { echo "编译面板失败"; exit 1; }
go build -ldflags "$LD030" -o "$LIVE" ./cmd/agent || { echo "编译 agent 0.3.0 失败"; exit 1; }
go build -ldflags "$LD040" -o "$PANEL_DIR/$AGENT_ASSET" ./cmd/agent || { echo "编译 agent 0.4.0 失败"; exit 1; }
echo "  面板      : $("$PANEL" -build)"
echo "  客户端旧版: $("$LIVE" --version | head -1)"
echo "  待下发版本: $("$PANEL_DIR/$AGENT_ASSET" --version | head -1)"

# ------------------------------------------------------------------ A. 正常自更新
say "A. 客户端 0.3.0 → 0.4.0 自更新"
"$PANEL" -listen "127.0.0.1:$PORT" -data "$DATA_P" >"$TMP/panel.log" 2>&1 &
PANELPID=$!
chk "面板已启动" "0.4.0" "$(curl -s --noproxy '*' "$BASE/api/healthz" | jget "d['version']")"
chk "登录" "200" "$(login)"

TOKEN=$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/tunnels" \
  | jget "d['install_command'].split('--token ')[1].split()[0].strip(\"'\")")
if [ -z "${TOKEN:-}" ]; then echo "取不到 install token，退出"; exit 1; fi
echo "  install token: ${TOKEN:0:8}…"

# 面板应主动报告"自带 0.4.0、有 0 台客户端"
chk "面板自带 agent 版本" "0.4.0" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/version" | jget "d['agent']['hosted']")"
chk "自动更新开关默认开" "True" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/version" | jget "d['agent']['auto_update']")"

"$LIVE" --server "$BASE" --token "$TOKEN" --data "$DATA_A" --name e2e-up >"$TMP/agent.log" 2>&1 &
AGPID=$!
if wait_agent_version "0.3.0" 30; then ok "客户端 0.3.0 已注册并在心跳"
else bad "客户端 0.3.0 应注册" "agent_version=0.3.0" "见上"; fi
chk "面板判定有 1 台客户端落后" "1" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/version" | jget "d['agent']['outdated']")"

printf '  等待自更新完成（下载→校验→替换→交接）…\n'
if wait_agent_version "0.4.0" 90; then ok "客户端已自更新到 0.4.0 并继续心跳"
else bad "客户端应自更新到 0.4.0" "agent_version=0.4.0" "见上"; fi

chk "磁盘上的 agent 已是 0.4.0" "0.4.0" "$(ver_of "$LIVE")"
chk "旧版本留档 .prev = 0.3.0" "0.3.0" "$(ver_of "$LIVE.prev")"
chk "只有一条客户端记录（身份延续）" "1" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/agents" | jget "len(d['agents'])")"
chk "客户端仍在线" "True" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/agents" | jget "d['agents'][0]['online']")"
chk "更新成功留痕（memo）" "0.4.0" "$(jget "d['version']" <"$DATA_A/update/memo.json" 2>/dev/null)"
chk "面板不再认为有客户端落后" "0" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/version" | jget "d['agent']['outdated']")"

# ------------------------------------------------------------------ B. 不降级
say "B. 面板自带版本更旧时，客户端不得降级"
go build -ldflags "-s -w -X main.version=0.2.0 -X main.buildTime=2026-10-25T00:00:00Z" \
   -o "$PANEL_DIR/$AGENT_ASSET" ./cmd/agent || echo "  编译 0.2.0 失败（继续）"
chk "面板现在自带 0.2.0" "0.2.0" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/version" | jget "d['agent']['hosted']")"
printf '  等两个心跳周期（≈20s）观察是否被降级…\n'
sleep 22
chk "客户端仍是 0.4.0（未降级）" "0.4.0" "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/agents" | jget "d['agents'][0]['agent_version']")"
chk "磁盘上的 agent 仍是 0.4.0" "0.4.0" "$(ver_of "$LIVE")"

# ------------------------------------------------------------------ C. 旧进程已退出
say "C. 交接后旧进程必须退出（不能残留两份 agent）"
if [ -n "${AGPID:-}" ]; then
  if tasklist //FI "PID eq $AGPID" 2>/dev/null | grep -q "$AGPID"; then
    bad "旧 agent 进程应已退出" "pid $AGPID 不存在" "仍在运行"
  else
    ok "旧 agent 进程已退出（pid $AGPID）"
  fi
fi

printf '  客户端日志尾部:\n'
tail -8 "$TMP/agent.log" 2>/dev/null | sed 's/^/    /'

# ------------------------------------------------------------------ 汇总
say "结果"
echo "  通过 $PASS / 失败 $FAIL"
[ "$FAIL" -eq 0 ] && echo "  客户端自更新端到端：全部通过" || echo "  客户端自更新端到端：有失败项"
exit $([ "$FAIL" -eq 0 ] && echo 0 || echo 1)
