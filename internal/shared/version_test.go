package shared

import "testing"

// 版本比较是整套自更新的判据：判错要么该升的不升，要么把旧版当新版推下去。
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		why  string
	}{
		{"0.4.0", "0.3.0", 1, "正常升级"},
		{"v0.4.0", "0.3.0", 1, "带 v 前缀与不带要能比"},
		{"V0.4.0", "v0.4.0", 0, "前缀大小写不影响"},
		{"0.3.0", "0.3.0", 0, "相等"},
		{"0.3.1", "0.3.0", 1, "补丁位"},
		{"0.10.0", "0.9.0", 1, "数值比较不是字符串比较（字符串会判 0.10 < 0.9）"},
		{"0.4", "0.4.0", 0, "缺位补 0"},
		{"1.0.0", "0.99.99", 1, "主版本优先"},

		// 预发布必须低于同号正式版，否则会把 beta 推给所有人
		{"0.4.0-beta.1", "0.4.0", -1, "预发布低于正式版"},
		{"0.4.0", "0.4.0-beta.1", 1, "正式版高于预发布"},
		{"0.4.0-rc1+build7", "0.4.0-rc1", 0, "build metadata 不参与比较"},

		// 无法解析的一方永远更旧：这样 "dev" 会被引导升级到正式版
		{"dev", "0.3.0", -1, "dev 视为最旧"},
		{"0.3.0", "dev", 1, "正式版比 dev 新"},
		{"", "0.3.0", -1, "空版本视为最旧（旧 agent 没上报字段）"},
		{"dev", "dev", 0, "两个都无法解析时不乱动"},
		{"", "dev", 0, "同上"},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d (%s)", c.a, c.b, got, c.want, c.why)
		}
	}
}

// IsNewer 给的是「要不要升级」，与 CompareVersions 的差别在无法解析时：
// 这里必须 false，否则一个乱码版本号能让整批客户端去下一个不存在的东西。
func TestIsNewer(t *testing.T) {
	if !IsNewer("0.4.0", "0.3.0") {
		t.Fatal("0.4.0 应比 0.3.0 新")
	}
	if IsNewer("0.3.0", "0.4.0") {
		t.Fatal("不许降级")
	}
	if IsNewer("0.3.0", "0.3.0") {
		t.Fatal("同版本不算更新")
	}
	if IsNewer("garbage", "0.3.0") {
		t.Fatal("乱码版本不得触发更新")
	}
	if IsNewer("", "0.3.0") {
		t.Fatal("空版本不得触发更新")
	}
	// 反方向：当前是乱码（未注入版本的开发构建），目标合法 → 应升级
	if !IsNewer("0.4.0", "dev") {
		t.Fatal("dev 构建应被引导升级到正式版")
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		"v0.4.0":     "0.4.0",
		"0.4":        "0.4.0",
		"0.4.0-rc1":  "0.4.0-rc1",
		"dev":        "dev", // 无法解析的原样保留，UI 要能看到
		"":           "",
		"  v1.2.3  ": "1.2.3",
	}
	for in, want := range cases {
		if got := NormalizeVersion(in); got != want {
			t.Errorf("NormalizeVersion(%q)=%q want %q", in, got, want)
		}
	}
}
