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
	verify := flag.Bool("verify", false, "只校验已有清单与目录文件是否一致（发布前守卫），不重新生成")
	flag.Parse()

	// 发布前守卫。存在的理由：交叉编译完某个平台后单独重建过一次，
	// 清单里的 sha256 就与实际文件对不上了 —— 而客户端拿到文件后会校验
	// sha256，不匹配就抛错不装，表现为「升级一直失败」，且面板侧看起来
	// 一切正常（清单版本比客户端新，于是不停下发）。这种事故在发布现场
	// 极难定位，必须在发布前拦住。
	if *verify {
		if err := verifyManifest(*dir, *out); err != nil {
			fmt.Fprintln(os.Stderr, "校验失败:", err)
			os.Exit(1)
		}
		return
	}

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

// verifyManifest 校验已有清单与目录里的二进制是否严格一致。
//
// 检查四件事：
//  1. 清单登记的每个文件都在目录里
//  2. 每个文件的大小与 sha256 都与清单一致
//  3. 目录里没有清单未登记的平台产物（漏发=别人下不到）
//  4. 清单没有重复登记同一个文件
//
// 只看「版本号对不对」是不够的：版本号相同而二进制不同完全可能（重编译一次
// 就会变），而客户端校验的是 sha256。
func verifyManifest(dir, out string) error {
	if out == "" {
		out = filepath.Join(dir, "manifest.json")
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return fmt.Errorf("读清单失败（还没生成过？先不带 -verify 跑一次）: %w", err)
	}
	var m shared.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("解析清单失败: %w", err)
	}
	if len(m.Assets) == 0 {
		return fmt.Errorf("清单里没有任何资产")
	}

	listed := map[string]bool{}
	var problems []string
	for _, a := range m.Assets {
		if listed[a.File] {
			problems = append(problems, a.File+": 清单重复登记")
			continue
		}
		listed[a.File] = true

		path := filepath.Join(dir, a.File)
		st, err := os.Stat(path)
		if err != nil {
			problems = append(problems, a.File+": 清单里有但文件不存在")
			continue
		}
		if st.Size() != a.Size {
			problems = append(problems, fmt.Sprintf("%s: 大小不符（清单 %d，实际 %d）", a.File, a.Size, st.Size()))
			continue
		}
		sha, err := shared.FileSHA256(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: 计算哈希失败: %v", a.File, err))
			continue
		}
		if sha != a.SHA256 {
			problems = append(problems, fmt.Sprintf("%s: sha256 不符（清单 %s，实际 %s）",
				a.File, short(a.SHA256), short(sha)))
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读目录失败: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "frpeasy-") {
			continue
		}
		if _, _, _, ok := parseAssetName(e.Name()); !ok {
			continue
		}
		if !listed[e.Name()] {
			problems = append(problems, e.Name()+": 目录里有但清单未登记")
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "  FAIL "+p)
		}
		return fmt.Errorf("%d 处不一致，重跑 genmanifest 重算清单", len(problems))
	}
	fmt.Printf("清单一致：%s（版本 %s，%d 个资产全部匹配）\n", out, m.Version, len(m.Assets))
	return nil
}

// short 取哈希前 12 位用于人眼比对，完整值在清单里。
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
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
