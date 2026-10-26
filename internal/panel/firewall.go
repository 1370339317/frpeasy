package panel

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 防火墙识别与放行命令
// ---------------------------------------------------------------------------

// FirewallInfo 面板所在主机上识别到的防火墙。
type FirewallInfo struct {
	Type      string `json:"type"` // ufw | firewalld | nftables | iptables | none | unsupported
	Active    bool   `json:"active"`
	Detail    string `json:"detail"`
	CheckedAt string `json:"checked_at"`
}

func runQuick(name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err == nil
}

// detectFirewall 尽力识别主机防火墙。
//
// 只做只读探测（status / list），不改任何规则；没权限或没装就返回 unsupported，
// 由 UI 提示用户手动确认，而不是假装"没有防火墙"。
func detectFirewall() FirewallInfo {
	now := time.Now().Format("15:04:05")
	if runtime.GOOS != "linux" {
		return FirewallInfo{
			Type: "unsupported", Detail: "面板只检测 Linux 上的 ufw / firewalld / nftables / iptables；当前系统请在系统防火墙里确认放行",
			CheckedAt: now,
		}
	}

	if out, ok := runQuick("ufw", "status"); ok {
		active := strings.Contains(out, "Status: active")
		detail := "ufw 未启用（可通过 sudo ufw enable 打开）"
		if active {
			detail = "ufw 已启用"
		}
		return FirewallInfo{Type: "ufw", Active: active, Detail: detail, CheckedAt: now}
	}
	if out, ok := runQuick("firewall-cmd", "--state"); ok || strings.Contains(strings.ToLower(out), "running") {
		active := strings.Contains(strings.ToLower(out), "running")
		return FirewallInfo{Type: "firewalld", Active: active, Detail: "firewalld " + map[bool]string{true: "已启用", false: "未运行"}[active], CheckedAt: now}
	}
	if out, ok := runQuick("nft", "list", "ruleset"); ok && len(strings.TrimSpace(out)) > 0 {
		return FirewallInfo{Type: "nftables", Active: true, Detail: "nftables 有规则集", CheckedAt: now}
	}
	if out, ok := runQuick("iptables", "-S"); ok {
		// 默认只有 3 条 -P 策略行，超过说明有人加过规则
		n := len(strings.Split(strings.TrimSpace(out), "\n"))
		if n > 4 {
			return FirewallInfo{Type: "iptables", Active: true, Detail: fmt.Sprintf("iptables 已有 %d 条规则", n), CheckedAt: now}
		}
		return FirewallInfo{Type: "iptables", Active: false, Detail: "iptables 只有默认策略，未配置放行规则", CheckedAt: now}
	}
	return FirewallInfo{
		Type: "none", Detail: "未检测到 ufw / firewalld / nft / iptables（容器里可能没有，或需要 root 才能读）",
		CheckedAt: now,
	}
}

// FWCommand 一条可直接复制的放行命令。
type FWCommand struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Cmd         string `json:"cmd"`
	Note        string `json:"note"`
	Recommended bool   `json:"recommended"`
}

// buildFWCommands 按端口生成各防火墙的放行命令。
//
// 默认全给（用户不一定知道自己机器上是哪套），但把识别到的那套标为推荐并排最前。
// 全部使用"逐端口放行"而不是直接开一个范围：范围更省事，但会把端口池里
// 目前没用的端口也暴露出去，跟这个面板"最小暴露面"的取向相反。
func buildFWCommands(ports []int, fw FirewallInfo, labels map[int]string, rangeStart, rangeEnd int) []FWCommand {
	if len(ports) == 0 {
		return nil
	}
	var cmds []FWCommand
	rec := fw.Type

	// ufw
	var lines []string
	for _, p := range ports {
		c := ""
		if l := labels[p]; l != "" {
			c = " comment 'frpeasy: " + strings.ReplaceAll(l, "'", "") + "'"
		}
		lines = append(lines, fmt.Sprintf("sudo ufw allow %d/tcp%s", p, c))
	}
	cmds = append(cmds, FWCommand{
		Kind: "ufw", Name: "ufw（Debian / Ubuntu）",
		Cmd:  strings.Join(lines, "\n"),
		Note: "即时生效且重启后保留",
	})

	// firewalld
	lines = nil
	for _, p := range ports {
		lines = append(lines, fmt.Sprintf("sudo firewall-cmd --permanent --add-port=%d/tcp", p))
	}
	lines = append(lines, "sudo firewall-cmd --reload")
	cmds = append(cmds, FWCommand{
		Kind: "firewalld", Name: "firewalld（CentOS / RHEL / Fedora / openEuler）",
		Cmd:  strings.Join(lines, "\n"),
		Note: "--permanent 后必须 --reload 才生效",
	})

	// iptables
	var chunks []string
	for i := 0; i < len(ports); i += 15 {
		end := i + 15
		if end > len(ports) {
			end = len(ports)
		}
		var ss []string
		for _, p := range ports[i:end] {
			ss = append(ss, strconv.Itoa(p))
		}
		chunks = append(chunks, fmt.Sprintf("sudo iptables -I INPUT -p tcp -m multiport --dports %s -j ACCEPT", strings.Join(ss, ",")))
	}
	cmds = append(cmds, FWCommand{
		Kind: "iptables", Name: "iptables（通用）",
		Cmd:  strings.Join(chunks, "\n"),
		Note: "重启后失效；需 root 持久化（iptables-persistent / netfilter-persistent save）",
	})

	// nftables
	var ss []string
	for _, p := range ports {
		ss = append(ss, strconv.Itoa(p))
	}
	cmds = append(cmds, FWCommand{
		Kind: "nftables", Name: "nftables（新版 Debian / Alpine 可选）",
		Cmd:  "sudo nft add rule inet filter input tcp dport { " + strings.Join(ss, ", ") + " } accept",
		Note: "表/链名按你机器上的实际配置调整",
	})

	for i := range cmds {
		if cmds[i].Kind == rec {
			cmds[i].Recommended = true
		}
	}
	// 推荐的排最前
	sort.SliceStable(cmds, func(i, j int) bool { return cmds[i].Recommended && !cmds[j].Recommended })

	if rangeStart > 0 && rangeEnd >= rangeStart {
		cmds = append(cmds, FWCommand{
			Kind: "range", Name: "省事方案：整段端口池",
			Cmd: fmt.Sprintf("sudo ufw allow %d:%d/tcp", rangeStart, rangeEnd),
			Note: "一次性放开整个端口池（当前是 " + strconv.Itoa(rangeStart) + "-" + strconv.Itoa(rangeEnd) +
				"），以后加隧道不用再改防火墙；代价是这段端口全部对外可见。已逐端口放行的话不需要它。",
		})
	}
	return cmds
}

// ---------------------------------------------------------------------------
// 探测结论
// ---------------------------------------------------------------------------

// 端口探测结论。
const (
	PortStateOpen      = "open"       // 已证实可达
	PortStateBlocked   = "blocked"    // 本机在监听，但公网回访不通
	PortStateNoService = "no_service" // 本机根本没有进程监听
	PortStateUnknown   = "unknown"    // 探测条件不足，无法判定
)

// PortCheck 单个端口的检测结果。
type PortCheck struct {
	Port      int             `json:"port"`
	Proto     string          `json:"proto"`
	Purpose   string          `json:"purpose"`
	Label     string          `json:"label"`
	State     string          `json:"state"`
	Evidence  string          `json:"evidence"` // external | self | self_local | none
	Listening bool            `json:"listening"`
	Self      DialOutcome     `json:"self"`
	External  *ExtProbeResult `json:"external,omitempty"`
	Detail    string          `json:"detail"`
}
