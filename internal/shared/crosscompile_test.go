package shared_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 交叉编译守卫：每个目标平台都必须能编过。
//
// 为什么要写成测试而不是只放在 Makefile 里：
//
//	写自更新的过程中，proc_unix.go / proc_windows.go 里各留了一个未使用的
//	import。在 Windows 上 `go build` 一切正常，直到要发布时才交叉编译 ——
//	这时才发现 linux/arm64、linux/arm 全都编不过，而它们恰恰是 Alpine 服务器
//	与 Android 客户端这两个最主力目标。Windows 单独编译永远暴露不了，
//	"在我机器上是好的"在这里是必然发生的。
//
// 所以把跨平台编译检查放进 go test ./...，改完就跑，不依赖有人记得交叉编译。
func TestCrossCompile(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("PATH 上没有 go，跳过交叉编译检查")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// 三个主力目标：服务器（linux 两种架构）+ 客户端（windows）。
	// 其余目标（arm/386）与它们同一套 build tag，被这三条覆盖。
	targets := []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"windows", "amd64"},
	}
	for _, tg := range targets {
		tg := tg
		t.Run(tg.goos+"_"+tg.goarch, func(t *testing.T) {
			// vet 会做与编译相同的类型检查（未使用 import、类型不匹配都会报），
			// 但不会往磁盘写二进制，比 go build 更适合放进测试。
			cmd := exec.Command(goBin, "vet", "./internal/...", "./cmd/...")
			cmd.Dir = root
			cmd.Env = append(os.Environ(),
				"CGO_ENABLED=0",
				"GOOS="+tg.goos,
				"GOARCH="+tg.goarch,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s/%s 编译不过:\n%s", tg.goos, tg.goarch, strings.TrimSpace(string(out)))
			}
		})
	}
}
