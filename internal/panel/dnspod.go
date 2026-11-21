package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DNSPod（腾讯云）的 API 与 EdgeOne 是两套域名，但签名规则完全相同（TC3），
// 所以这里复用同一套签名逻辑，只换 service 与 version。
//
// 为什么必须自己写记录：EdgeOne 在 dnsPodAccess 模式下确实会替控制台添加的域名
// 自动写解析，但**通过 API 创建的加速域名不会被自动补记录**（2026-11-21 实测：
// 同一站点下控制台加的 car.dvvb.cn 能解析，我们 API 建的 ecsfrp.dvvb.cn 一直
// NXDOMAIN）。所以这一步只能我们自己补。
const (
	dnspodBase    = "https://dnspod.tencentcloudapi.com"
	dnspodVersion = "2021-03-23"
	dnspodService = "dnspod"
)

type dnspod struct {
	secretID  string
	secretKey string
	base      string
	client    *http.Client
}

func newDNSPod(secretID, secretKey string) *dnspod {
	return &dnspod{
		secretID:  secretID,
		secretKey: secretKey,
		base:      dnspodBase,
		client:    &http.Client{Timeout: 20 * time.Second},
	}
}

func (d *dnspod) call(action string, payload any) (json.RawMessage, error) {
	return tc3Call(d.client, d.base, dnspodService, dnspodVersion, "", action,
		d.secretID, d.secretKey, payload)
}

// EnsureCNAME 确保 rootDomain 下存在 subdomain → target 的 CNAME。
//
// 幂等：已存在同值记录直接返回；已存在但指向别处则报错而不是覆盖 ——
// 覆盖别人的解析记录属于不可接受的破坏性操作，交给人判断。
func (d *dnspod) EnsureCNAME(rootDomain, subdomain, target string, ttl int) (bool, string, error) {
	if ttl <= 0 {
		ttl = 600
	}
	// 只传文档里确实存在的参数：多传一个未识别的字段，腾讯云会整单拒绝
	// （实测 RecordStatus 会报 UnknownParameter），而不是忽略。
	raw, err := d.call("DescribeRecordList", map[string]any{
		"Domain":     rootDomain,
		"Subdomain":  subdomain,
		"RecordType": "CNAME",
		"Limit":      100,
	})
	// "查不到记录"是正常情况（还没建），腾讯云却以错误码返回，且此时 raw 为空。
	// 必须容错并且跳过反序列化，否则第一次添加永远走不到 CreateRecord。
	var out struct {
		RecordList []struct {
			Name  string `json:"Name"`
			Value string `json:"Value"`
			Line  string `json:"Line"`
		} `json:"RecordList"`
	}
	if err != nil {
		if !isNoRecord(err) {
			return false, "", err
		}
	} else if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return false, "", fmt.Errorf("解析 DNS 记录失败：%w", uerr)
	}
	for _, rec := range out.RecordList {
		if !strings.EqualFold(rec.Name, subdomain) {
			continue
		}
		if strings.EqualFold(strings.TrimSuffix(rec.Value, "."), strings.TrimSuffix(target, ".")) {
			return false, "已存在同值 CNAME，无需重复添加", nil
		}
		return false, "", fmt.Errorf(
			"%s.%s 已有一条 CNAME 指向 %s，面板不会覆盖它；请先在 DNSPod 处理这条记录",
			subdomain, rootDomain, rec.Value)
	}

	_, err = d.call("CreateRecord", map[string]any{
		"Domain":     rootDomain,
		"SubDomain":  subdomain,
		"RecordType": "CNAME",
		"RecordLine": "默认",
		"Value":      target,
		"TTL":        ttl,
	})
	if err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("已创建 %s.%s → %s（TTL %d）", subdomain, rootDomain, target, ttl), nil
}

// isNoRecord 判断错误是否为"该查询没有记录"。
//
// 腾讯云对空结果返回 ResourceNotFound.NoDataOfRecord，属于业务上的正常态，
// 不是故障。用字符串匹配而不用类型断言，是因为 call() 把错误统一成文本，
// 引入自定义错误类型会让所有调用点都要改。
func isNoRecord(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "NoDataOfRecord") || strings.Contains(msg, "ResourceNotFound")
}
