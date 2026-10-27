package shared

import "regexp"

// ansiEscape 匹配 CSI 序列（颜色、光标、清行等）。
//
// frp 的日志里每行都带 SGR（`ESC[0m ESC[1;34m`），浏览器不解析它们，
// 原样展示就成了每行前面挂一串 `[0m[1;34m` —— 用户会以为日志本身坏了。
// 两类消费方都要洗：面板读 frps 日志（TailLog）、客户端把 frpc 输出转进
// agent.log。放在 shared 里，避免两边各写一份正则、其中一边漏掉光标类序列。
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// StripANSI 去掉字符串里的 ANSI 转义序列。
func StripANSI(s string) string {
	if s == "" {
		return s
	}
	return ansiEscape.ReplaceAllString(s, "")
}
