#!/usr/bin/env bash
# 面板自更新端到端演练：真实编译两个版本的二进制，真实替换，真实重启。
#
# 为什么必须有这个脚本：单元测试只能证明 swapBinary / waitHealthy 各自对，
# 证明不了"换完之后面板真的还活着"。而这正是自更新唯一重要的事。
#
# 覆盖三条路径：
#   A. 正常升级   0.3.0 → 0.4.0，健康检查通过，state=succeeded
#   B. 启动失败   0.5.0 的二进制预检能过、但一启动就退出 → 必须自动回滚到 0.3.0
#   C. 预检拦截   二进制自报版本与清单不符 → 在替换前就拒绝，磁盘上不留东西
#
# 每次运行使用独立端口与目录，可反复执行。
set -u

ROOT=$(cd "$(dirname "$0")" && pwd)
cd "$ROOT" || exit 1

GOOS=$(go env GOOS)
GOARCH=$(go env GOARCH)
SUF=""
[ "$GOOS" = "windows" ] && SUF=".exe"

TMP=$(mktemp -d 2>/dev/null || echo "/tmp/frpeasy-su-$$")
mkdir -p "$TMP"
PUB="$TMP/pub"
DATA="$TMP/data"
mkdir -p "$PUB" "$DATA"

PANEL_PORT=${PANEL_PORT:-18171}
PUB_PORT=${PUB_PORT:-18172}
BASE="http://127.0.0.1:$PANEL_PORT"
LIVE="$TMP/frpeasy-panel$SUF"

PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad() { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n     expect=%s actual=%s\n' "$1" "$2" "$3"; }
chk() { [ "$2" = "$3" ] && ok "$1" || bad "$1" "$2" "$3"; }
say() { printf '\n\033[36m== %s\033[0m\n' "$1"; }

cleanup() {
  [ -n "${PUBPID:-}" ] && kill "$PUBPID" 2>/dev/null
  for p in $(netstat -ano 2>/dev/null | grep LISTENING | grep ":$PANEL_PORT " | awk '{print $5}' | sort -u); do
    taskkill //PID "$p" //F >/dev/null 2>&1 || kill -9 "$p" 2>/dev/null
  done
  [ -n "${PANELPID:-}" ] && kill "$PANELPID" 2>/dev/null
  # 回滚测试会留下被强杀的进程，按名字补一刀（只杀本脚本起的那个路径）
  sleep 1
}
trap cleanup EXIT

wait_health() { # $1=期望版本 $2=超时秒
  local want=$1 limit=$2 i=0
  while [ $i -lt $((limit*4)) ]; do
    v=$(curl -s --noproxy '*' "$BASE/api/healthz" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
    [ "$v" = "$want" ] && return 0
    sleep 0.25
    i=$((i+1))
  done
  return 1
}

# 等 state.json 进入某个终态。
#
# 这一步不能省，也不能用"healthz 报出旧版本"来替代：回滚场景里旧进程在
# 交接开始前本来就在应答旧版本，只看 healthz 会立刻"成功"，
# 于是根本没等到回滚发生就下了结论 —— 这正是第一版脚本踩的坑。
wait_state() { # $1=期望阶段 $2=超时秒
  local want=$1 limit=$2 i=0
  while [ $i -lt $((limit*2)) ]; do
    st=$(sed -n 's/.*"stage": "\([^"]*\)".*/\1/p' "$DATA/update/state.json" 2>/dev/null)
    [ "$st" = "$want" ] && return 0
    sleep 0.5
    i=$((i+1))
  done
  echo "    (state 停在: ${st:-无} )"
  return 1
}

# 从标准输入 JSON 里取一个字段（避免 sed 贪婪匹配抓错对象）
jget() { python -c "
import sys, json
d = json.load(sys.stdin)
print(eval(sys.argv[1], {'d': d}))
" "$1" 2>/dev/null; }

login() { curl -s --noproxy '*' -c "$TMP/ck" -X POST "$BASE/api/login" \
    -H 'Content-Type: application/json' -d '{"user":"admin","pass":"admin"}' -o /dev/null -w '%{http_code}'; }

sha_of() { sha256sum "$1" | cut -d' ' -f1; }

# ------------------------------------------------------------------ 准备二进制
say "编译两个版本的 panel"
go build -ldflags "-s -w -X main.version=0.3.0 -X main.buildTime=2026-10-25T00:00:00Z" \
   -o "$LIVE" ./cmd/panel || { echo "编译 0.3.0 失败"; exit 1; }
go build -ldflags "-s -w -X main.version=0.4.0 -X main.buildTime=2026-10-25T00:00:00Z" \
   -o "$PUB/frpeasy-panel-$GOOS-$GOARCH$SUF" ./cmd/panel || { echo "编译 0.4.0 失败"; exit 1; }
echo "  0.3.0: $("$LIVE" -build)"
echo "  0.4.0: $("$PUB/frpeasy-panel-$GOOS-$GOARCH$SUF" -build)"

# 发布目录起静态服务（manifest.json 与二进制就放在里面）
(cd "$PUB" && exec python -m http.server "$PUB_PORT" --bind 127.0.0.1 >"$TMP/pub.log" 2>&1) &
PUBPID=$!
sleep 1.5

write_manifest() { # $1=版本 $2=二进制文件名
  local sha size
  sha=$(sha_of "$PUB/$2")
  size=$(wc -c <"$PUB/$2" | tr -d ' ')
  cat >"$PUB/manifest.json" <<EOF
{
  "version": "$1",
  "released_at": "2026-10-25T00:00:00Z",
  "notes": "e2e test build",
  "min_agent_version": "0.3.0",
  "assets": [
    {"kind":"panel","os":"$GOOS","arch":"$GOARCH","file":"$2","sha256":"$sha","size":$size}
  ]
}
EOF
}

# ------------------------------------------------------------------ A. 正常升级
say "A. 正常升级 0.3.0 → 0.4.0"
write_manifest "0.4.0" "frpeasy-panel-$GOOS-$GOARCH$SUF"
"$LIVE" -listen "127.0.0.1:$PANEL_PORT" -data "$DATA" >"$TMP/panel.log" 2>&1 &
PANELPID=$!
chk "0.3.0 已启动" "0.3.0" "$(curl -s --noproxy '*' "$BASE/api/healthz" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"

chk "登录" "200" "$(login)"
# 先造一条业务数据：升级后必须还在（SQLite 不能被换二进制搞坏）
curl -s --noproxy '*' -b "$TMP/ck" -X POST "$BASE/api/settings" \
  -H 'Content-Type: application/json' \
  -d "{\"update_base\":\"http://127.0.0.1:$PUB_PORT\"}" -o /dev/null -w '' 
chk "更新源已保存" "http://127.0.0.1:$PUB_PORT" \
  "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/settings" | sed -n 's/.*"update_base":"\([^"]*\)".*/\1/p')"

VER=$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/version")
chk "面板看到新版" "0.4.0" "$(echo "$VER" | jget "d['panel']['latest']")"
chk "可更新标记" "True" "$(echo "$VER" | jget "d['panel']['update_available']")"
chk "本平台有面板资产" "True" "$(echo "$VER" | jget "d['panel']['can_update']")"

printf '  触发更新…\n'
curl -s --noproxy '*' -b "$TMP/ck" -X POST "$BASE/api/panel/update" -w ' [%{http_code}]\n' | head -c 300

if wait_state "succeeded" 120; then
  ok "交接流程报告成功"
else
  bad "交接应报告成功" "state=succeeded" "见上方 state"
fi
if wait_health "0.4.0" 30; then
  ok "面板已升级到 0.4.0（healthz 确认）"
else
  got=$(curl -s --noproxy '*' "$BASE/api/healthz" 2>/dev/null)
  bad "面板未升级到 0.4.0" "healthz version=0.4.0" "${got:-无响应}"
fi

chk "旧版本留档 .prev" "0.3.0" "$("$LIVE.prev" -build 2>/dev/null | sed -n 's/.* \([0-9][^ ]*\) (.*/\1/p')"
chk "state 记录成功" "succeeded" "$(sed -n 's/.*"stage": "\([^"]*\)".*/\1/p' "$DATA/update/state.json" 2>/dev/null)"

# 重启后会话是内存态，必须重新登录
chk "升级后可再次登录（数据库完好）" "200" "$(login)"
chk "设置项在升级后仍在" "http://127.0.0.1:$PUB_PORT" \
  "$(curl -s --noproxy '*' -b "$TMP/ck" "$BASE/api/settings" | sed -n 's/.*"update_base":"\([^"]*\)".*/\1/p')"

# 停掉这一轮
for p in $(netstat -ano 2>/dev/null | grep LISTENING | grep ":$PANEL_PORT " | awk '{print $5}' | sort -u); do
  taskkill //PID "$p" //F >/dev/null 2>&1 || kill -9 "$p" 2>/dev/null
done
kill "$PANELPID" 2>/dev/null
sleep 1.5

# ------------------------------------------------------------------ B. 启动失败必须回滚
say "B. 新版本能过预检但一启动就崩 → 必须自动回滚"
# 造一个"骗子"二进制：-build 报 0.5.0（预检能过），正常启动直接 exit 1。
# 它完全不认识 --handoff —— 这正是重点：交接必须由**旧版副本**完成，
# 不能指望新二进制自己会交接（最初那版就是这么写的，被本用例打穿）。
BADSRC="$TMP/badsrc"
mkdir -p "$BADSRC"
cat >"$BADSRC/main.go" <<'GOEOF'
package main

import (
	"fmt"
	"os"
)

func main() {
	for _, a := range os.Args {
		if a == "-build" {
			fmt.Println("frpeasy-panel 0.5.0 (" + "linux/amd64" + ") built 2026-10-25T00:00:00Z")
			return
		}
	}
	fmt.Println("simulated startup failure")
	os.Exit(1)
}
GOEOF
(cd "$BADSRC" && go mod init badpanel >/dev/null 2>&1 && go build -o "$PUB/frpeasy-panel-$GOOS-$GOARCH$SUF" .) \
  || { echo "编译骗子二进制失败"; }

# 把运行中的 exe 换回 0.3.0（上一轮已变成 0.4.0）
rm -f "$LIVE"
cp "$LIVE.prev" "$LIVE" 2>/dev/null || mv "$LIVE.prev" "$LIVE"
chk "已换回 0.3.0 继续测试" "0.3.0" "$("$LIVE" -build | sed -n 's/.* \([0-9][^ ]*\) (.*/\1/p')"

write_manifest "0.5.0" "frpeasy-panel-$GOOS-$GOARCH$SUF"
rm -f "$TMP/panel.log"
"$LIVE" -listen "127.0.0.1:$PANEL_PORT" -data "$DATA" >"$TMP/panel.log" 2>&1 &
PANELPID=$!
sleep 1.5
login >/dev/null
printf '  触发更新…\n'
curl -s --noproxy '*' -b "$TMP/ck" -X POST "$BASE/api/panel/update" -o /dev/null -w '  [%{http_code}]\n'

# 新版本必然崩，健康检查会失败，交接进程应当回滚并重新拉起 0.3.0
if wait_state "rolled-back" 150; then
  ok "交接流程报告已回滚"
else
  bad "交接应报告回滚" "state=rolled-back" "见上方 state"
fi
if wait_health "0.3.0" 40; then
  ok "已回滚并恢复服务（面板仍是 0.3.0 且在应答）"
else
  bad "回滚后服务应恢复" "healthz version=0.3.0" "$(curl -s --noproxy '*' "$BASE/api/healthz" 2>/dev/null || echo 无响应)"
fi
chk "回滚后旧二进制仍是 0.3.0" "0.3.0" "$("$LIVE" -build | sed -n 's/.* \([0-9][^ ]*\) (.*/\1/p')"
printf '  handoff 日志尾部:\n'
tail -8 "$DATA/update/handoff.log" 2>/dev/null | sed 's/^/    /'

for p in $(netstat -ano 2>/dev/null | grep LISTENING | grep ":$PANEL_PORT " | awk '{print $5}' | sort -u); do
  taskkill //PID "$p" //F >/dev/null 2>&1 || kill -9 "$p" 2>/dev/null
done
kill "$PANELPID" 2>/dev/null
sleep 1.5

# ------------------------------------------------------------------ C. 预检拦截
say "C. 二进制自报版本与清单不符 → 替换前就拒绝"
# 这里复用 0.3.0 的二进制，但清单声称是 0.6.0
cp "$LIVE" "$PUB/frpeasy-panel-$GOOS-$GOARCH$SUF"
write_manifest "0.6.0" "frpeasy-panel-$GOOS-$GOARCH$SUF"
rm -f "$TMP/panel.log"
"$LIVE" -listen "127.0.0.1:$PANEL_PORT" -data "$DATA" >"$TMP/panel.log" 2>&1 &
PANELPID=$!
sleep 1.5
login >/dev/null
BEFORE_SHA=$(sha_of "$LIVE")
curl -s --noproxy '*' -b "$TMP/ck" -X POST "$BASE/api/panel/update" -o /dev/null -w '  [%{http_code}]\n'
sleep 4
chk "面板仍在运行且未被替换" "0.3.0" "$(curl -s --noproxy '*' "$BASE/api/healthz" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
chk "二进制内容未变" "$BEFORE_SHA" "$(sha_of "$LIVE")"
chk "state 记录失败原因" "failed" "$(sed -n 's/.*"stage": "\([^"]*\)".*/\1/p' "$DATA/update/state.json" 2>/dev/null)"

# ------------------------------------------------------------------ 汇总
say "结果"
echo "  通过 $PASS / 失败 $FAIL"
[ "$FAIL" -eq 0 ] && echo "  自更新端到端：全部通过" || echo "  自更新端到端：有失败项"
exit $([ "$FAIL" -eq 0 ] && echo 0 || echo 1)
