#!/usr/bin/env bash
# frpeasy 端到端集成测试：真 panel + 真 agent 二进制 + 真实 HTTP 调用。
# 每条断言输出 PASS/FAIL，末尾统计；有任何 FAIL 则退出码非 0。
# 切到脚本所在目录：脚本里所有路径都相对于仓库根，
# 写死绝对路径会让别人 clone 下来直接跑不了。
cd "$(dirname "$0")" || exit 1

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
R=./it-result.txt
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

say "== 4b) 云端卸载指令：随心跳下发，且只发一次 =="
# 用一台临时「新版」客户端测：上面 AGID 上报的是 0.3.0，会被版本门槛拦下（那也是有意的，
# 老 agent 解码时会静默忽略卸载字段，点下去只会「没反应」）。测完立刻删掉，
# 否则后面「唯一 agent 自动绑定」那条会因存在两台客户端而失败。
REG2=$(curl -s -X POST $B/api/agent/register -H 'Content-Type: application/json' -d "{\"install_token\":\"$TOKEN\",\"os\":\"windows\",\"arch\":\"amd64\",\"name\":\"uninstall-test\",\"agent_version\":\"0.5.9\"}")
AG2=$(echo "$REG2" | jget "d['agent_id']")
AG2TOK=$(echo "$REG2" | jget "d['agent_token']")
chk "下发卸载指令 ok" True "$(curl -s -b cj.txt -X POST $B/api/agents/$AG2/uninstall | jget "d['ok']")"
# 下发后、心跳前：处于「等待领取」，sent_at 还没有值
AG2Q="[a for a in d['agents'] if a['id']=='$AG2'][0]"
chk "等待下发时 requested=true" True "$(curl -s -b cj.txt $B/api/agents | jget "$AG2Q['uninstall_requested']")"
chk "等待下发时 sent_at 为空" None "$(curl -s -b cj.txt $B/api/agents | jget "$AG2Q.get('uninstall_sent_at')")"
chk "心跳带回 uninstall=true" True "$(curl -s -X POST $B/api/agent/heartbeat -H 'Content-Type: application/json' -d "{\"agent_id\":\"$AG2\",\"agent_token\":\"$AG2TOK\",\"status\":[]}" | jget "d.get('uninstall')")"
# 被领走之后必须留下痕迹：否则面板分不清「从没卸载过」和「已让它卸载、等结果」，
# 卸载完的客户端就只能显示成普通「离线」，用户看不出成功还是失败。
chk "被领走后 requested 清零" False "$(curl -s -b cj.txt $B/api/agents | jget "$AG2Q['uninstall_requested']")"
chk "被领走后 sent_at 有值" 0 "$(curl -s -b cj.txt $B/api/agents | jget "0 if $AG2Q.get('uninstall_sent_at') else 1")"
# 取出即清除：否则客户端每 15 秒重复收到同一指令，权限不足时会无限重试刷日志
chk "第二次心跳不再下发" None "$(curl -s -X POST $B/api/agent/heartbeat -H 'Content-Type: application/json' -d "{\"agent_id\":\"$AG2\",\"agent_token\":\"$AG2TOK\",\"status\":[]}" | jget "d.get('uninstall')")"
# 再点一次「卸载」= 上次没成功，状态要回到「等待下发」
curl -s -b cj.txt -X POST $B/api/agents/$AG2/uninstall >/dev/null
chk "重新下发清掉旧 sent_at" None "$(curl -s -b cj.txt $B/api/agents | jget "$AG2Q.get('uninstall_sent_at')")"
# 排队中的指令可以撤销：离线点错了否则只能等它开机把自己删掉
chk "撤销排队中的卸载 200" 200 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X DELETE $B/api/agents/$AG2/uninstall)"
chk "撤销后 requested 归零" False "$(curl -s -b cj.txt $B/api/agents | jget "$AG2Q['uninstall_requested']")"
chk "撤销后心跳不再下发" None "$(curl -s -X POST $B/api/agent/heartbeat -H 'Content-Type: application/json' -d "{\"agent_id\":\"$AG2\",\"agent_token\":\"$AG2TOK\",\"status\":[]}" | jget "d.get('uninstall')")"
# 已送出去的不能撤：那是既成事实，清掉标记只会让界面看不出那台机器为什么没了
curl -s -b cj.txt -X POST $B/api/agents/$AG2/uninstall >/dev/null
curl -s -X POST $B/api/agent/heartbeat -H 'Content-Type: application/json' -d "{\"agent_id\":\"$AG2\",\"agent_token\":\"$AG2TOK\",\"status\":[]}" >/dev/null
chk "已领走的卸载无法撤销 409" 409 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X DELETE $B/api/agents/$AG2/uninstall)"
chk "无法撤销时 sent_at 仍在" 1 "$(curl -s -b cj.txt $B/api/agents | jget "1 if $AG2Q.get('uninstall_sent_at') else 0")"
chk "不存在的客户端下发 404" 404 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X POST $B/api/agents/nosuchagent/uninstall)"
# 0.3.0 的客户端：必须明确拒绝，而不是「已下发」然后客户端毫无反应
chk "过旧客户端被拒 409" 409 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X POST $B/api/agents/$AGID/uninstall)"
curl -s -b cj.txt -X DELETE $B/api/agents/$AG2 >/dev/null
IC2=$(curl -s -b cj.txt $B/api/tunnels | jget "d['uninstall_command']")
chk "卸载命令已下发" 1 "$(case "$IC2" in *--uninstall*) echo 1;; *) echo 0;; esac)"
chk "卸载命令确实没带 --token" 0 "$(case "$IC2" in *--token*) echo 1;; *) echo 0;; esac)"

say "== 5) 探测服务出现在列表 =="
chk "agent.services 已存储" "127.0.0.1:8123" "$(curl -s -b cj.txt $B/api/tunnels | jget "[a for a in d['agents'] if a['id']=='$AGID'][0]['services'][0]['addr']")"
chk "agent 自身版本已落库（升级判定的地基）" "0.3.0" "$(curl -s -b cj.txt $B/api/agents | jget "[a for a in d['agents'] if a['id']=='$AGID'][0]['agent_version']")"

say "== 6) 空 local + 指定 agent -> 自动选探测主服务 =="
T1=$(curl -s -b cj.txt -X POST $B/api/tunnels -H 'Content-Type: application/json' -d "{\"name\":\"自动探测\",\"agent_id\":\"$AGID\"}")
T1ID=$(echo "$T1" | jget "d['tunnel']['id']")
chk "自动填充 127.0.0.1:8123" "127.0.0.1:8123" "$(echo "$T1" | jget "d['tunnel']['local_addr']")"
chk "自动分配端口 21000" 21000 "$(echo "$T1" | jget "d['tunnel']['public_port']")"
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
chk "config 端口集合 {21000,21001}" "21000,21001" "$(echo "$CFG" | jget "','.join(str(p) for p in sorted(t['public_port'] for t in d['tunnels']))")"
chk "config 错令牌被拒 401" 401 "$(curl -s -o /dev/null -w '%{http_code}' "$B/api/agent/config?agent_id=$AGID&token=bad")"

say "== 11) 空 local + 不存在的 agent -> 期望 400 =="
chk "无探测可用的 agent 被拒" 400 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X POST $B/api/tunnels -H 'Content-Type: application/json' -d '{"name":"y","agent_id":"nope"}')"

say "== 12) 端口冲突（改成另一条隧道的 21001） -> 期望 400 =="
chk "端口冲突被拒" 400 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X PUT $B/api/tunnels/$T1ID -H 'Content-Type: application/json' -d '{"manual_port":21001}')"

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
chk "未暂存平台 502" 502 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/linux/riscv64)"
chk "已暂存平台 200" 200 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/linux/arm64 2>/dev/null)"

say "== 16) Android 客户端二进制（android/* 是 linux/* 的别名） =="
chk "android/arm64 可下载" 200 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/android/arm64)"
chk "android/未暂存架构 502" 502 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/agent/bin/android/mips)"

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
chk "清单含 frps 端口 27900" 1 "$(echo "$PORTJ" | jget "1 if 27900 in (d['required_ports'] or []) else 0")"
chk "清单含隧道公网端口 21000" 1 "$(echo "$PORTJ" | jget "1 if 21000 in (d['required_ports'] or []) else 0")"
chk "dashboard 出现在清单但不要求放行" 1 "$(echo "$PORTJ" | jget "1 if any(i['purpose']=='internal' and not i['required'] for i in d['items']) else 0")"
chk "已删除隧道的端口不在清单里" 1 "$(echo "$PORTJ" | jget "1 if 21001 not in (d['required_ports'] or []) else 0")"
chk "识别防火墙字段存在" 1 "$(echo "$PORTJ" | jget "1 if d['firewall'].get('type') else 0")"
chk "给出放行命令" 1 "$(echo "$PORTJ" | jget "1 if len(d['commands'] or [])>0 else 0")"
chk "放行命令覆盖隧道端口" 1 "$(echo "$PORTJ" | jget "1 if any('21000' in c['cmd'] for c in d['commands']) else 0")"
chk "命令标了推荐项（识别到的防火墙那套）" 1 "$(echo "$PORTJ" | jget "1 if (d['firewall']['type'] not in ('ufw','firewalld','iptables','nftables')) or any(c['recommended'] for c in d['commands']) else 0")"
chk "推荐项排在最前" 1 "$(echo "$PORTJ" | jget "1 if (d['firewall']['type'] not in ('ufw','firewalld','iptables','nftables')) or d['commands'][0]['recommended'] else 0")"

say "== 19) 端口可达性检测（不启外部回访，避免慢/限流） =="
CHK=$(curl -s -b cj.txt -X POST $B/api/ports/check -H 'Content-Type: application/json' -d '{"ports":[27900,21000,18080],"external":false}')
chk "返回 3 条结果" 3 "$(echo "$CHK" | jget "len(d['results'])")"
chk "面板端口本机在监听" 1 "$(echo "$CHK" | jget "1 if [r for r in d['results'] if r['port']==18080][0]['listening'] else 0")"
chk "未监听的隧道端口判 no_service" "no_service" "$(echo "$CHK" | jget "[r for r in d['results'] if r['port']==21000][0]['state']")"
chk "公网地址是环回时判 unknown 而非可达" "unknown" "$(echo "$CHK" | jget "[r for r in d['results'] if r['port']==18080][0]['state']")"

say "== 20) 设置公网地址后，探测目标随之改变 =="
curl -s -b cj.txt -X POST $B/api/settings -H 'Content-Type: application/json' -d '{"public_addr":"1.2.3.4:8080"}' >/dev/null
PORTJ2=$(curl -s -b cj.txt $B/api/ports)
chk "探测目标变成公网地址" "1.2.3.4" "$(echo "$PORTJ2" | jget "d['access_host']")"
chk "允许外部回访" "True" "$(echo "$PORTJ2" | jget "d['probe_host_ok']")"
chk "端口清单不受公网地址影响" 1 "$(echo "$PORTJ2" | jget "1 if 21000 in (d['required_ports'] or []) else 0")"

say "== 21) 文件页：托管文件清单 / 删除保护 =="
FILESJ=$(curl -s -b cj.txt $B/api/files)
chk "文件接口可用" 200 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt $B/api/files)"
chk "每个支持平台都有 agent 条目" 6 "$(echo "$FILESJ" | jget "len([i for i in d['items'] if i['kind']=='agent'])")"
chk "每个支持平台都有 frpc 条目" 6 "$(echo "$FILESJ" | jget "len([i for i in d['items'] if i['kind']=='frpc'])")"
chk "三类都列出" "agent,frpc,frps" "$(echo "$FILESJ" | jget "','.join(sorted({i['kind'] for i in d['items']}))")"
chk "未下载的项不可删除" 0 "$(echo "$FILESJ" | jget "1 if any(i['can_delete'] and not i['present'] for i in d['items']) else 0")"
chk "删除不存在的文件 404" 404 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X DELETE "$B/api/files?kind=frpc&os=linux&arch=amd64")"
chk "kind 非法 400" 400 "$(curl -s -o /dev/null -w '%{http_code}' -b cj.txt -X DELETE "$B/api/files?kind=xx&os=linux&arch=amd64")"
# frpc 托管接口必须存在。上游没有该架构时应回 502（而不是 404）——
# 客户端按 5xx 决定回退直连，404 会被理解成"这个平台本来就没有"。
# frpc 托管接口的语义：非法平台 400、合法但取不到 5xx，**绝不能是 404** ——
# 客户端把 404 理解成「面板本来就不提供这个平台」，会直接放弃并继续回退，
# 而真实原因（更新源没配、镜像不通）就永远查不出来。
# 不要断言具体是 502：那取决于上游此刻有没有这个架构，网络通不通都能让断言翻脸。
chk "frpc 托管：非法平台 400" 400 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/frpc/bin/nosuchos/nosucharch)"
chk "frpc 托管：合法平台不是 404" 1 "$(case $(curl -s -o /dev/null -w '%{http_code}' --max-time 60 $B/api/frpc/bin/linux/amd64) in 404) echo 0;; *) echo 1;; esac)"

# 前端「我这个标签页是不是过期了」的自检接口。
# 没有它，面板自更新后已打开的页面会一直跑旧 JS，用户看到「更新成功」却找不到新菜单。
chk "前端产物自检接口可用" 200 "$(curl -s -o /dev/null -w '%{http_code}' $B/api/ui/build)"
chk "自检接口带 asset 字段" 1 "$(curl -s $B/api/ui/build | jget "1 if 'asset' in d else 0")"
chk "自检接口带版本号" 1 "$(curl -s $B/api/ui/build | jget "1 if d.get('version') else 0")"

kill $PANEL_PID 2>/dev/null || true
# Windows 上 kill 只作用于 bash 的作业，原生 exe 会留下来继续心跳（并锁住 exe），
# 下一轮就构建失败。按镜像名兜底清理，保证脚本可反复执行。
taskkill //IM it-agent-bin.exe //F >/dev/null 2>&1
taskkill //IM it-panel-bin.exe //F >/dev/null 2>&1
say ""
say "===== PASS=$PASS FAIL=$FAIL ====="
[ "$FAIL" -eq 0 ] || exit 1
