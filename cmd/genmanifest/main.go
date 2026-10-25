// genmanifest 生成自更新清单（manifest.json）。
//
// 发布时只要跑一次：
//
//	make dist VERSION=0.4.0      # 交叉编译到 dist/
//	go run ./cmd/genmanifest -dir dist -version 0.4.0 -notes "修复 xx"
//
// 然后把手写的清单连同二进制一起上传到静态目录（或 GitHub Release）。
// 面板设置里的更新源填那个目录即可，不需要 API、不需要 token、不会被限流。
//
// 为什么要单独一个工具而不是让面板自己算：清单是**发布方**的产物，
// 哈希必须来自发布者本地的文件，不能由运行中的面板猜。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"frpeasy/internal/shared"
)

func main() {
	dir := flag.String("dir", "dist", "二进制所在目录")
	version := flag.String("version", "", "版本号（必填，如 0.4.0）")
	notes := flag.String("notes", "", "本次更新说明")
	minAgent := flag.String("min-agent", "", "客户端最低可升级版本（可选）")
	out := flag.String("out", "", "输出路径（默认 <dir>/manifest.json）")
	flag.Parse()

	if *version == "" {
		fmt.Fprintln(os.Stderr, "缺少 -version（例：-version 0.4.0）")
		os.Exit(1)
	}

	entries, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取目录失败:", err)
		os.Exit(1)
	}

	m := shared.Manifest{
		Version:         *version,
		ReleasedAt:      time.Now().UTC().Format(time.RFC3339),
		Notes:           *notes,
		MinAgentVersion: *minAgent,
		Assets:          []shared.Asset{},
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "frpeasy-") {
			continue
		}
		kind, osName, arch, ok := parseAssetName(name)
		if !ok {
			// 名字不符合约定就跳过并提示，而不是猜一个平台塞进清单
			// （猜错的后果是客户端下到别的架构、装上就崩）。
			fmt.Fprintf(os.Stderr, "跳过无法识别的文件: %s\n", name)
			continue
		}
		path := filepath.Join(*dir, name)
		sha, err := shared.FileSHA256(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "计算 %s 哈希失败: %v\n", name, err)
			os.Exit(1)
		}
		st, err := os.Stat(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 %s 失败: %v\n", name, err)
			os.Exit(1)
		}
		m.Assets = append(m.Assets, shared.Asset{
			Kind: kind, OS: osName, Arch: arch,
			File: name, SHA256: sha, Size: st.Size(),
		})
	}

	if len(m.Assets) == 0 {
		fmt.Fprintln(os.Stderr, "目录里没有 frpeasy-* 产物，先跑 make dist")
		os.Exit(1)
	}
	// 顺序固定，便于 diff 与人工核对
	sort.Slice(m.Assets, func(i, j int) bool {
		if m.Assets[i].Kind != m.Assets[j].Kind {
			return m.Assets[i].Kind < m.Assets[j].Kind
		}
		if m.Assets[i].OS != m.Assets[j].OS {
			return m.Assets[i].OS < m.Assets[j].OS
		}
		return m.Assets[i].Arch < m.Assets[j].Arch
	})

	if *out == "" {
		*out = filepath.Join(*dir, "manifest.json")
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "序列化失败:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写文件失败:", err)
		os.Exit(1)
	}

	fmt.Printf("已生成 %s（%s，%d 个资产）\n", *out, *version, len(m.Assets))
	for _, a := range m.Assets {
		fmt.Printf("  %-6s %-8s %-6s %10d  %s\n", a.Kind, a.OS, a.Arch, a.Size, a.SHA256[:12]+"…")
	}

	// 本平台缺资产是最常见的发布事故：面板能起来但客户端下载 404。
	selfOS, selfArch := runtime.GOOS, runtime.GOARCH
	havePanel, haveAgent := false, false
	for _, a := range m.Assets {
		if strings.EqualFold(a.Kind, shared.AssetPanel) && a.OS == selfOS && a.Arch == selfArch {
			havePanel = true
		}
		if strings.EqualFold(a.Kind, shared.AssetAgent) && a.OS == selfOS && a.Arch == selfArch {
			haveAgent = true
		}
	}
	if !havePanel {
		fmt.Printf("\n警告：清单里没有 %s/%s 的面板资产，本机无法自更新\n", selfOS, selfArch)
	}
	if !haveAgent {
		fmt.Printf("警告：清单里没有 %s/%s 的 agent 资产，本机客户端升级会 404\n", selfOS, selfArch)
	}
}

// parseAssetName 拆 frpeasy-<kind>-<os>-<arch>[.exe]。
//
// 不用正则：格式是固定的四段，用切分更直白，也更容易在出错时给出确切原因。
func parseAssetName(name string) (kind, osName, arch string, ok bool) {
	base := strings.TrimSuffix(name, ".exe")
	parts := strings.Split(base, "-")
	// frpeasy / panel|agent / linux / amd64  → 恰好 4 段
	if len(parts) != 4 || parts[0] != "frpeasy" {
		return "", "", "", false
	}
	kind = parts[1]
	if kind != shared.AssetPanel && kind != shared.AssetAgent {
		return "", "", "", false
	}
	osName, arch = parts[2], parts[3]
	// 粗校验：arch 必须是我们认识的，避免把 frpeasy-agent-linux-amd64.sig
	// 之类的附属文件也塞进清单
	switch arch {
	case "amd64", "arm64", "arm", "386":
	default:
		return "", "", "", false
	}
	switch osName {
	case "linux", "windows", "darwin":
	default:
		return "", "", "", false
	}
	return kind, osName, arch, true
}
