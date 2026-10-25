#!/usr/bin/env bash
# frpeasy 端到端集成测试：真 panel + 真 agent 二进制 + 真实 HTTP 调用。
# 每条断言输出 PASS/FAIL，末尾统计；有任何 FAIL 则退出码非 0。
cd /c/Users/13703/WorkBuddy/frpeasy || exit 1

# 用当前源码重新构建被测二进制（独立文件名，避免与正在运行的实例抢文件锁），
# 防止跑到过期的本地 exe 上得出错误结论。
PBIN=./it-panel-bin.exe
ABIN=./it-agent-bin.exe

# 上一轮可能留下还活着的 agent：Windows 上 bash 的 kill 杀不掉原生进程，
# 残留实例会一直心跳、污染数据库，还会占用 exe 文件锁让本轮构建失败。
taskkill //IM it-agent-bin.exe //F >/dev/null 2>&1
for port in 18080; do
  for p in $(netstat -ano 2>/dev/null | grep LISTENING | grep ":$port " | awk '{print $5}' | sort -u); do
    taskkill //PID "$p" //F >/dev/null 2>&1
  done
done
sleep 1

go build -trimpath -ldflags "-s -w" -o $PBIN ./cmd/panel || { echo "构建 panel 失败"; exit 1; }
go build -trimpath -ldflags "-s -w" -o $ABIN ./cmd/agent || { echo "构建 agent 失败"; exit 1; }

PASS=0; FAIL=0
R=/c/Users/13703/WorkBuddy/frpeasy/it-result.txt
: > "$R"
say() { echo "$*" | tee -a "$R"; }
chk() { # chk <描述> <期望> <实际>
  if [ "$2" = "$3" ]; then say "  PASS  $1"; PASS=$((PASS+1));
  else say "  FAIL  $1 (期望 [$2] 实际 [$3])"; FAIL=$((FAIL+1)); fi
}
jget() { python -c "import sys,json;d=json.load(sys.stdin);print($1)" 2>/dev/null; }

rm -rf it-data agent-data cj.txt
$PBIN --data ./it-data --listen :18080 > it-panel.log 2>&1 &
PANEL_PID=$!
sleep 2
B=http://127.0.0.1:18080

say "== 1) 登录 =="
chk "登录成功" True "$(curl -s -c cj.txt -X POST $B/api/login -H 'Content-Type: application/json' -d '{"user":"admin","pass":"admin"}' | jget "d['ok']")"
chk "未登录访问被拦 401" 401 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/tunnels)"

say "== 1b) 已登录但无任何 Agent 时，空 local_addr 建隧道 -> 期望 400 =="
chk "空 local 且无 Agent 被拒" 400 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X POST $B/api/tunnels -H 'Content-Type: application/json' -d '{"name":"x"}')"

say "== 2) 取安装令牌 =="
IC=$(curl -s -b cj.txt $B/api/tunnels | jget "d['install_command']")
TOKEN=$(echo "$IC" | sed -n 's/.*--token \([^ ]*\).*/\1/p')
chk "安装命令含令牌" 1 "$([ -n "$TOKEN" ] && echo 1 || echo 0)"

say "== 3) 注册 Agent =="
REG=$(curl -s -X POST $B/api/agent/register -H 'Content-Type: application/json' -d "{\"install_token\":\"$TOKEN\",\"os\":\"linux\",\"arch\":\"amd64\",\"name\":\"test-pc\",\"agent_version\":\"0.3.0\"}")
AGID=$(echo "$REG" | jget "d['agent_id']")
AGTOK=$(echo "$REG" | jget "d['agent_token']")
chk "注册返回 agent_id(32 位)" 32 "${#AGID}"
chk "注册返回 agent_token" 1 "$([ -n "$AGTOK" ] && echo 1 || echo 0)"

say "== 4) 心跳上报探测服务 =="
chk "心跳 ok" True "$(curl -s -X POST $B/api/agent/heartbeat -H 'Content-Type: application/json' \
  -d "{\"agent_id\":\"$AGID\",\"agent_token\":\"$AGTOK\",\"status\":[],\"services\":[{\"host\":\"127.0.0.1\",\"port\":8123,\"addr\":\"127.0.0.1:8123\",\"name\":\"Web(HTTP)\",\"confidence\":3}]}" | jget "d['ok']")"
chk "错误令牌被拒 401" 401 "$(curl -s -o /dev/null -w '%{http_code}' -X POST $B/api/agent/heartbeat -H 'Content-Type: application/json' -d "{\"agent_id\":\"$AGID\",\"agent_token\":\"bad\",\"status\":[]}")"

say "== 5) 探测服务出现在列表 =="
chk "agent.services 已存储" "127.0.0.1:8123" "$(curl -s -b cj.txt $B/api/tunnels | jget "[a for a in d['agents'] if a['id']=='$AGID'][0]['services'][0]['addr']")"
chk "agent 自身版本已落库（升级判定的地基）" "0.3.0" "$(curl -s -b cj.txt $B/api/agents | jget "[a for a in d['agents'] if a['id']=='$AGID'][0]['agent_version']")"

say "== 6) 空 local + 指定 agent -> 自动选探测主服务 =="
T1=$(curl -s -b cj.txt -X POST $B/api/tunnels -H 'Content-Type: application/json' -d "{\"name\":\"自动探测\",\"agent_id\":\"$AGID\"}")
T1ID=$(echo "$T1" | jget "d['tunnel']['id']")
chk "自动填充 127.0.0.1:8123" "127.0.0.1:8123" "$(echo "$T1" | jget "d['tunnel']['local_addr']")"
chk "自动分配端口 10000" 10000 "$(echo "$T1" | jget "d['tunnel']['public_port']")"
chk "created_at 非零值" 0 "$(echo "$T1" | jget "1 if d['tunnel']['created_at'].startswith('0001') else 0")"

say "== 7) 手填 local + 不指定机器（唯一 agent 自动绑定） =="
T2=$(curl -s -b cj.txt -X POST $B/api/tunnels -H 'Content-Type: application/json' -d '{"name":"手填","local_addr":"127.0.0.1:9000"}')
T2ID=$(echo "$T2" | jget "d['tunnel']['id']")
chk "手填地址保留" "127.0.0.1:9000" "$(echo "$T2" | jget "d['tunnel']['local_addr']")"
chk "唯一 agent 自动绑定" "$AGID" "$(echo "$T2" | jget "d['tunnel']['agent_id']")"

say "== 8) 隧道列表 =="
chk "列表 2 条" 2 "$(curl -s -b cj.txt $B/api/tunnels | jget "len(d['tunnels'])")"

say "== 9) 后期修改（改名 + 改地址） =="
U=$(curl -s -b cj.txt -X PUT $B/api/tunnels/$T1ID -H 'Content-Type: application/json' -d '{"name":"改后","local_addr":"127.0.0.1:8080"}')
chk "改名生效" "改后" "$(echo "$U" | jget "d['tunnel']['name']")"
chk "改地址生效" "127.0.0.1:8080" "$(echo "$U" | jget "d['tunnel']['local_addr']")"

say "== 9b) 显式解绑 -> 变「未部署」，不再下发给 Agent =="
chk "解绑后 agent_id 为空" "" "$(curl -s -b cj.txt -X PUT $B/api/tunnels/$T2ID -H 'Content-Type: application/json' -d '{"clear_agent":true}' | jget "d['tunnel']['agent_id']")"
chk "解绑后 config 只剩 1 条" 1 "$(curl -s "$B/api/agent/config?agent_id=$AGID&token=$AGTOK" | jget "len(d['tunnels'])")"
chk "重新绑回成功" "$AGID" "$(curl -s -b cj.txt -X PUT $B/api/tunnels/$T2ID -H 'Content-Type: application/json' -d "{\"agent_id\":\"$AGID\"}" | jget "d['tunnel']['agent_id']")"

say "== 10) Agent 配置只含本机隧道 =="
CFG=$(curl -s "$B/api/agent/config?agent_id=$AGID&token=$AGTOK")
chk "config 含 2 条" 2 "$(echo "$CFG" | jget "len(d['tunnels'])")"
chk "config 端口集合 {10000,10001}" "10000,10001" "$(echo "$CFG" | jget "','.join(str(p) for p in sorted(t['public_port'] for t in d['tunnels']))")"
chk "config 错令牌被拒 401" 401 "$(curl -s -o /dev/null -w '%{http_code}' "$B/api/agent/config?agent_id=$AGID&token=bad")"

say "== 11) 空 local + 不存在的 agent -> 期望 400 =="
chk "无探测可用的 agent 被拒" 400 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X POST $B/api/tunnels -H 'Content-Type: application/json' -d '{"name":"y","agent_id":"nope"}')"

say "== 12) 端口冲突（改成另一条隧道的 10001） -> 期望 400 =="
chk "端口冲突被拒" 400 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X PUT $B/api/tunnels/$T1ID -H 'Content-Type: application/json' -d '{"manual_port":10001}')"

say "== 13) 删除隧道 =="
chk "删除 ok" True "$(curl -s -b cj.txt -X DELETE $B/api/tunnels/$T2ID | jget "d['ok']")"
chk "剩余 1 条" 1 "$(curl -s -b cj.txt $B/api/tunnels | jget "len(d['tunnels'])")"

say "== 14) 真实 agent 二进制跑 8s（注册 + 探测不崩） =="
$ABIN --server $B --token "$TOKEN" --name realpc --data ./agent-data > it-agent.log 2>&1 &
AG_PID=$!
sleep 8
kill $AG_PID 2>/dev/null || true
taskkill //PID $AG_PID //F >/dev/null 2>&1 || true
chk "真实 agent 注册成功" 1 "$(grep -ac '已注册' it-agent.log)"
chk "无 panic" 0 "$(grep -ac 'panic' it-agent.log)"

say "== 15) 面板下发 agent 二进制（数据目录兜底查找） =="
mkdir -p it-data/bin && cp ../frpeasy/bin/frpeasy-agent-linux-arm64 it-data/bin/ 2>/dev/null || mkdir -p it-data/bin
# 注意用**我们从不构建**的平台组合：面板除了数据目录，还会去查自身同级目录
# 与同级 bin/（发布布局），所以"bin/ 里恰好有 linux/amd64"时那条不算未暂存。
chk "未暂存平台 404" 404 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/linux/riscv64)"
chk "已暂存平台 200" 200 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/linux/arm64 2>/dev/null)"

say "== 16) Android 客户端二进制（android/* 是 linux/* 的别名） =="
chk "android/arm64 可下载" 200 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/android/arm64)"
chk "android/未暂存架构 404" 404 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/android/mips)"

say "== 17) Android 安装命令下发 =="
AND=$(curl -s -b cj.txt $B/api/tunnels | jget "d['install_command_android']")
chk "含 adb shell" 1 "$(echo "$AND" | grep -c 'adb shell')"
chk "含设备侧落盘路径" 1 "$(echo "$AND" | grep -c '/data/local/tmp/frpeasy-install.sh')"
chk "含 toybox wget 兜底" 1 "$(echo "$AND" | grep -c 'toybox wget')"
chk "含安装令牌" 1 "$(echo "$AND" | grep -c -- "--token $TOKEN")"
ANDROOT=$(curl -s -b cj.txt $B/api/tunnels | jget "d['install_command_android_root']")
chk "root 命令带 --elevate" 1 "$(echo "$ANDROOT" | grep -c -- '--elevate')"
chk "root 命令不写死 su 形状" 0 "$(echo "$ANDROOT" | grep -c 'su -c')"
chk "安装脚本落地失败时明确退出" 1 "$(echo "$AND" | grep -c 'download failed on device')"

say "== 18) 端口 / 防火墙清单 =="
chk "未登录取端口被拦 401" 401 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/ports)"
PORTJ=$(curl -s -b cj.txt $B/api/ports)
chk "清单含面板端口 18080" 1 "$(echo "$PORTJ" | jget "1 if 18080 in (d['required_ports'] or []) else 0")"
chk "清单含 frps 端口 7000" 1 "$(echo "$PORTJ" | jget "1 if 7000 in (d['required_ports'] or []) else 0")"
chk "清单含隧道公网端口 10000" 1 "$(echo "$PORTJ" | jget "1 if 10000 in (d['required_ports'] or []) else 0")"
chk "dashboard 出现在清单但不要求放行" 1 "$(echo "$PORTJ" | jget "1 if any(i['purpose']=='internal' and not i['required'] for i in d['items']) else 0")"
chk "已删除隧道的端口不在清单里" 1 "$(echo "$PORTJ" | jget "1 if 10001 not in (d['required_ports'] or []) else 0")"
chk "识别防火墙字段存在" 1 "$(echo "$PORTJ" | jget "1 if d['firewall'].get('type') else 0")"
chk "给出放行命令" 1 "$(echo "$PORTJ" | jget "1 if len(d['commands'] or [])>0 else 0")"
chk "放行命令覆盖隧道端口" 1 "$(echo "$PORTJ" | jget "1 if any('10000' in c['cmd'] for c in d['commands']) else 0")"
chk "命令标了推荐项（识别到的防火墙那套）" 1 "$(echo "$PORTJ" | jget "1 if (d['firewall']['type'] not in ('ufw','firewalld','iptables','nftables')) or any(c['recommended'] for c in d['commands']) else 0")"
chk "推荐项排在最前" 1 "$(echo "$PORTJ" | jget "1 if (d['firewall']['type'] not in ('ufw','firewalld','iptables','nftables')) or d['commands'][0]['recommended'] else 0")"

say "== 19) 端口可达性检测（不启外部回访，避免慢/限流） =="
CHK=$(curl -s -b cj.txt -X POST $B/api/ports/check -H 'Content-Type: application/json' -d '{"ports":[7000,10000,18080],"external":false}')
chk "返回 3 条结果" 3 "$(echo "$CHK" | jget "len(d['results'])")"
chk "面板端口本机在监听" 1 "$(echo "$CHK" | jget "1 if [r for r in d['results'] if r['port']==18080][0]['listening'] else 0")"
chk "未监听的隧道端口判 no_service" "no_service" "$(echo "$CHK" | jget "[r for r in d['results'] if r['port']==10000][0]['state']")"
chk "公网地址是环回时判 unknown 而非可达" "unknown" "$(echo "$CHK" | jget "[r for r in d['results'] if r['port']==18080][0]['state']")"

say "== 20) 设置公网地址后，探测目标随之改变 =="
curl -s -b cj.txt -X POST $B/api/settings -H 'Content-Type: application/json' -d '{"public_addr":"1.2.3.4:8080"}' >/dev/null
PORTJ2=$(curl -s -b cj.txt $B/api/ports)
chk "探测目标变成公网地址" "1.2.3.4" "$(echo "$PORTJ2" | jget "d['access_host']")"
chk "允许外部回访" "True" "$(echo "$PORTJ2" | jget "d['probe_host_ok']")"
chk "端口清单不受公网地址影响" 1 "$(echo "$PORTJ2" | jget "1 if 10000 in (d['required_ports'] or []) else 0")"

kill $PANEL_PID 2>/dev/null || true
# Windows 上 kill 只作用于 bash 的作业，原生 exe 会留下来继续心跳（并锁住 exe），
# 下一轮就构建失败。按镜像名兜底清理，保证脚本可反复执行。
taskkill //IM it-agent-bin.exe //F >/dev/null 2>&1
taskkill //IM it-panel-bin.exe //F >/dev/null 2>&1
say ""
say "===== PASS=$PASS FAIL=$FAIL ====="
[ "$FAIL" -eq 0 ] || exit 1
