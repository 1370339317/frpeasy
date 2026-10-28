package shared

import (
	"strconv"
	"strings"
)

// 版本比较：不引第三方库。
//
// 为什么自己写而不引 golang.org/x/mod/semver 或 Masterminds/semver：
// 本项目只在一个地方需要「谁更新」，规则简单且必须对畸形容错 ——
// 客户端上报的版本可能是 "dev"（ldflags 没注入）、可能是 ""（旧版没这个字段）、
// 也可能是 v 前缀大小写不一。第三方库遇到这些会报错，而这里必须给出可用的结论：
// 无法解析的一方永远判为「更旧」，从而触发一次升级（升级到能正确上报版本的新版）。
//
// 支持的形态：
//
//	0.4.0 / v0.4.0 / V0.4.0
//	v0.4.0-beta.2  （预发布，低于同号正式版）
//	0.4            （缺位补 0）
//	dev / "" / 其它无法解析 → 视为最低
//
// 不支持 4 段（1.2.3.4）这类非 semver，遇到按前 3 段比较，忽略多余段。
const (
	// maxVersionSegments 只比较 3 段，多余段忽略。
	maxVersionSegments = 3
)

// verParts 把版本串拆成可比较的结构。
//
// pre 非空表示预发布（- 后面那截）。prerelease 的语义：0.4.0-beta < 0.4.0。
type verParts struct {
	nums []int
	pre  string
	ok   bool
}

func parseVersion(s string) verParts {
	s = strings.TrimSpace(s)
	if s == "" {
		return verParts{}
	}
	// 去掉前缀 v / V（容忍大小写，git tag 两种写法都常见）
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
	}
	if s == "" {
		return verParts{}
	}
	// 拆出预发布部分
	core := s
	pre := ""
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core = s[:i]
		// + 后面是 build metadata，semver 规定不参与比较，直接丢；
		// - 后面才是预发布。两者可能同时出现（1.0.0-rc1+build7），
		// 所以只取 - 到 + 之间的那段。
		rest := s[i+1:]
		if i2 := strings.Index(rest, "+"); i2 >= 0 {
			rest = rest[:i2]
		}
		if s[i] == '-' {
			pre = rest
		}
	}
	if core == "" {
		return verParts{}
	}
	fields := strings.Split(core, ".")
	if len(fields) > maxVersionSegments {
		fields = fields[:maxVersionSegments]
	}
	nums := make([]int, 0, maxVersionSegments)
	for _, f := range fields {
		// 允许 "0-SNAPSHOT" 这类已在上一步处理；这里必须全是数字。
		// 前导零（v0.04.0）按数值处理，strconv 天然接受。
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return verParts{}
		}
		nums = append(nums, n)
	}
	// 补齐到 3 段，方便逐位比较（0.4 → 0.4.0）
	for len(nums) < maxVersionSegments {
		nums = append(nums, 0)
	}
	return verParts{nums: nums, pre: pre, ok: true}
}

// CompareVersions 比较两个版本串，返回 -1 / 0 / 1（a 比 b 旧 / 相同 / 新）。
//
// 关键约定：**无法解析的一方视为更旧**。
// 这样 "dev" 与 "0.4.0" 比较会得到 -1，即"该升级"—— 正是我们想要的：
// 没注入版本号的构建应当被引导升级到能上报版本的正式版，而不是被认为是最新。
// 两边都无法解析时返回 0（谁也不知道，别乱动）。
func CompareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	switch {
	case !pa.ok && !pb.ok:
		return 0
	case !pa.ok:
		return -1
	case !pb.ok:
		return 1
	}
	for i := 0; i < maxVersionSegments; i++ {
		if pa.nums[i] != pb.nums[i] {
			if pa.nums[i] < pb.nums[i] {
				return -1
			}
			return 1
		}
	}
	// 数值相等，看预发布：有预发布标记的更低
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1 // a 是正式版，b 是预发布
	case pb.pre == "":
		return -1
	default:
		// 都是预发布，按字符串比（beta.10 与 beta.9 会得到错误顺序，
		// 但预发布之间比大小不是本项目的需求，保持简单）
		return strings.Compare(pa.pre, pb.pre)
	}
}

// MinAgentTransportVersion 支持"隧道传输层参数下发"（tcpMux / poolCount /
// compression）的最低 agent 版本。
//
// 用途：面板要把 frps 的 tcpMux 改成 false 时，必须先确认所有客户端都认这个字段。
// frp 的 tcpMux 两端必须一致 —— 老客户端仍会用默认 true，然后握手后被服务端断开，
// 表现为"隧道全断、服务端日志一条错误都不留"，是那种最难查的故障。
const MinAgentTransportVersion = "0.7.34"

// VersionKnown 这个版本串能不能参与比较（"dev"、git hash 这类不能）。
//
// 用途：做「最低版本」门槛时必须区分「确实是旧版」和「版本号根本认不出来」。
// 后者（本地构建的 dev）按旧版拦掉会给出荒唐的提示（"版本 dev 低于 0.7.29"），
// 而这类新字段本身是加法式的 —— 老客户端最多忽略它，界面另有超时说明。
func VersionKnown(s string) bool {
	return parseVersion(s).ok
}

// IsNewer 判断 candidate 是否比 current 新（无法解析的 candidate 永远不算新）。
//
// 与 CompareVersions 的差别：这里用于「要不要更新到这个版本」，
// 所以 candidate 解析失败时必须返回 false —— 否则一个乱码版本号
// 会让整批客户端去下载一个不存在的东西。
func IsNewer(candidate, current string) bool {
	if !parseVersion(candidate).ok {
		return false
	}
	return CompareVersions(candidate, current) > 0
}

// NormalizeVersion 统一版本串的写法，便于存库与展示。
// "v0.4.0" → "0.4.0"；无法解析的原样返回（比如 "dev" 要保留，以便 UI 能显示出来）。
func NormalizeVersion(s string) string {
	s = strings.TrimSpace(s)
	if !parseVersion(s).ok {
		return s
	}
	p := parseVersion(s)
	out := strconv.Itoa(p.nums[0]) + "." + strconv.Itoa(p.nums[1]) + "." + strconv.Itoa(p.nums[2])
	if p.pre != "" {
		out += "-" + p.pre
	}
	return out
}
