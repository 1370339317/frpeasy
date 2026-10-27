package shared

import "testing"

// frp 的日志每行都带 SGR；浏览器和日志文件都不该看到它们。
// 这个函数是唯一的清洗点（面板 TailLog 与客户端 lineLogger 共用）。
func TestStripANSI(t *testing.T) {
	cases := map[string]string{
		"\x1b[0m\x1b[1;34m2026-10-27 [I] hello\x1b[0m": "2026-10-27 [I] hello",
		"plain text":                                  "plain text",
		"":                                            "",
		"\x1b[2K\x1b[1;31merror\x1b[0m":               "error",
		"中文\x1b[0m日志":                                "中文日志",
	}
	for in, want := range cases {
		if got := StripANSI(in); got != want {
			t.Fatalf("StripANSI(%q) = %q，期望 %q", in, got, want)
		}
	}
}
