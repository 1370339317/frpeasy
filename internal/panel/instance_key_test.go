package panel

import "testing"

// 实例键决定"同机装几份"：同一面板必须得到同一个键（否则每次安装都会新建一份），
// 不同面板必须得到不同键（否则会互相覆盖）。
func TestInstanceKey(t *testing.T) {
	same := []string{
		"http://1.2.3.4:28080",
		"http://1.2.3.4:28080/",
		"HTTP://1.2.3.4:28080",
		" http://1.2.3.4:28080 ",
	}
	base := instanceKey(same[0])
	for _, v := range same[1:] {
		if got := instanceKey(v); got != base {
			t.Fatalf("归一化失败：%q 得到 %s，期望 %s", v, got, base)
		}
	}
	if instanceKey("http://1.2.3.4:28081") == base {
		t.Fatal("不同端口必须得到不同键，否则两台面板会装成同一实例")
	}
	if instanceKey("") != "default" {
		t.Fatal("空地址应回落为 default")
	}
	// 默认端口等价：http://h 与 https://h:443 归一化后相同
	if instanceKey("http://h") != instanceKey("https://h:443") {
		t.Fatal("默认端口应被归一化掉")
	}
}
