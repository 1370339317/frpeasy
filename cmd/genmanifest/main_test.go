package main

import "testing"

// 文件名解析错一次，清单里就会塞进一个"客户端永远下不到"或者
// "架构不对、装上就崩"的资产。清单是发布的唯一契约，这里必须咬死。
func TestParseAssetName(t *testing.T) {
	ok := []struct {
		name           string
		kind, os, arch string
	}{
		{"frpeasy-panel-linux-amd64", "panel", "linux", "amd64"},
		{"frpeasy-panel-linux-arm64", "panel", "linux", "arm64"},
		{"frpeasy-agent-linux-arm", "agent", "linux", "arm"},
		{"frpeasy-agent-linux-386", "agent", "linux", "386"},
		{"frpeasy-agent-windows-amd64.exe", "agent", "windows", "amd64"},
		{"frpeasy-agent-windows-arm64.exe", "agent", "windows", "arm64"},
	}
	for _, c := range ok {
		k, o, a, good := parseAssetName(c.name)
		if !good || k != c.kind || o != c.os || a != c.arch {
			t.Errorf("parseAssetName(%q)=(%q,%q,%q,%v) want (%q,%q,%q)", c.name, k, o, a, good, c.kind, c.os, c.arch)
		}
	}

	bad := []string{
		"manifest.json",
		"frpeasy-panel-linux",              // 缺 arch
		"frpeasy-panel-linux-amd64.sig",    // 附属文件，不能当资产
		"frpeasy-panel-linux-mips",         // 不支持的架构
		"frpeasy-updater-linux-amd64",      // 未知类型
		"other-panel-linux-amd64",          // 不是我们的产物
		"frpeasy-panel-linux-amd64.tar.gz", // 发布的是解包后的二进制，不是压缩包
	}
	for _, name := range bad {
		if _, _, _, good := parseAssetName(name); good {
			t.Errorf("parseAssetName(%q) 应被拒绝", name)
		}
	}
}
