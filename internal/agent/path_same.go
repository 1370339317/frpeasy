package agent

import "os"

// samePath 比较两个路径是否指向同一个文件（大小写、符号链接都容错）。
//
// 用 os.SameFile 而不是字符串比较：macOS 的 /tmp 是 /private/tmp 的软链，
// Windows 路径大小写不敏感 —— 字符串比较会误判成"不是同一个"，
// 于是永远清不掉残留，或者更糟：把无关进程当成本目录的 frpc。
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
