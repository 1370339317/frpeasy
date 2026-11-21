package panel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EdgeOne 有两个互相独立的账号体系：腾讯云国内站（cloud.tencent.com）和国际站
// （edgeone.ai / intl.cloud.tencent.com）。两边的站点数据不互通，API 域名也不同。
// 用国内站域名去查国际站账号，最常见的结果就是"签名通过、站点列表为空" ——
// 看起来像"账号下没有站点"，实际是连错了端点。
const (
	edgeOneEndpointCN   = "https://teo.tencentcloudapi.com"
	edgeOneEndpointIntl = "https://teo.intl.tencentcloudapi.com"
)

// edgeOneBase 是默认端点，测试用它替换成 httptest 地址。
var edgeOneBase = edgeOneEndpointCN

// edgeOneVersion 是 EdgeOne（teo）的 API 版本。写死在这里而不是做成配置项：
// 腾讯云各产品的版本号是按接口族固定的，跟套餐/站点无关，暴露给用户只会让人填错。
const edgeOneVersion = "2022-09-01"

// edgeOne 是腾讯云 EdgeOne 的最小客户端。
//
// 只用标准库自己实现签名，不引官方 SDK：tencentcloud-sdk-go 是一个包含全部产品的
// 单体仓库，为一个 teo 客户端拉进来会让模块缓存和编译时间成倍增长，而 TC3 签名
// 本身的算法是固定的（HMAC-SHA256 四段派生），自己写反而更可控。
type edgeOne struct {
	secretID  string
	secretKey string
	region    string
	base      string
	client    *http.Client
}

func newEdgeOne(secretID, secretKey, region, base string) *edgeOne {
	if strings.TrimSpace(base) == "" {
		base = edgeOneBase
	}
	return &edgeOne{
		secretID:  secretID,
		secretKey: secretKey,
		region:    strings.TrimSpace(region),
		base:      base,
		client:    &http.Client{Timeout: 20 * time.Second},
	}
}

// tc3Signature 计算 TC3-HMAC-SHA256 的 Authorization 头。
//
// 只签 content-type 与 host 两个头：EdgeOne 的所有接口都用 POST + JSON，签名覆盖
// 的字段越少越好核对。ts 单独传入是为了让测试能构造确定性输入。
//
// 派生链（腾讯云文档固定写法，不能改顺序）：
//
//	SecretDate    = HMAC-SHA256("TC3"+SecretKey, Date)
//	SecretService = HMAC-SHA256(SecretDate, Service)      // service = "teo"
//	SecretSigning = HMAC-SHA256(SecretService, "tc3_request")
//	Signature     = HEX(HMAC-SHA256(SecretSigning, StringToSign))
func tc3Signature(ts int64, body []byte, secretID, secretKey, host string) string {
	const (
		service     = "teo"
		contentType = "application/json; charset=utf-8"
		signedKeys  = "content-type;host"
	)
	date := time.Unix(ts, 0).UTC().Format("2006-01-02")
	hashedPayload := sha256Hex(body)

	// 注意 CanonicalHeaders 每行自带 "\n"，随后还要再补一个空行才接 SignedHeaders ——
	// 少这一个空行是 TC3 最常见的手写错误，表现是入参全对但一直 SignatureFailure。
	canonicalHeaders := "content-type:" + contentType + "\nhost:" + host + "\n"
	canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\n" + signedKeys + "\n" + hashedPayload

	scope := date + "/" + service + "/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" + fmt.Sprintf("%d", ts) + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))

	kDate := hmacSHA256([]byte("TC3"+secretKey), date)
	kService := hmacSHA256(kDate, service)
	kSigning := hmacSHA256(kService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return "TC3-HMAC-SHA256 Credential=" + secretID + "/" + scope +
		", SignedHeaders=" + signedKeys + ", Signature=" + signature
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// call 调一个 EdgeOne 接口，返回 Response 对象内部的原始 JSON。
//
// 失败时把腾讯云的 Code/Message 原样带回上层：一套没有联调过的云 API 客户端，
// 报错原文比任何自定义文案都有用（InvalidParameter.XXX 能直接指向出错字段）。
func (e *edgeOne) call(action string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ts := time.Now().Unix()
	host := strings.TrimPrefix(strings.TrimPrefix(e.base, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")

	req, err := http.NewRequest(http.MethodPost, e.base, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Host", host)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", edgeOneVersion)
	req.Header.Set("X-TC-Timestamp", fmt.Sprintf("%d", ts))
	// Region 为空就不发：teo 的 Region 是可选参数，硬填一个值可能在部分账号上
	// 被当作资源过滤条件，表现为"站点列表为空"而签名完全正常。
	if e.region != "" {
		req.Header.Set("X-TC-Region", e.region)
	}
	req.Header.Set("Authorization", tc3Signature(ts, body, e.secretID, e.secretKey, host))

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 EdgeOne 失败：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}

	var wrap struct {
		Response json.RawMessage `json:"Response"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("EdgeOne 返回的不是 JSON（HTTP %d）：%s", resp.StatusCode, snippet(raw))
	}
	if len(wrap.Response) == 0 {
		return nil, fmt.Errorf("EdgeOne 返回缺少 Response 字段（HTTP %d）：%s", resp.StatusCode, snippet(raw))
	}
	var meta struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
		RequestID string `json:"RequestId"`
	}
	_ = json.Unmarshal(wrap.Response, &meta)
	if meta.Error != nil {
		return nil, fmt.Errorf("EdgeOne 错误 %s：%s（RequestId %s）",
			meta.Error.Code, meta.Error.Message, meta.RequestID)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("EdgeOne HTTP %d：%s", resp.StatusCode, snippet(raw))
	}
	// 只返回 Response 内部的对象：上层各接口直接按业务字段解，
	// 不需要每处都再拆一层外壳（拆漏一处就是一个静默的空结果）。
	return wrap.Response, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// Zone 一个 EdgeOne 站点。
type Zone struct {
	ZoneID   string `json:"ZoneId"`
	ZoneName string `json:"ZoneName"`
	Area     string `json:"Area"`
	Status   string `json:"Status"`
	Type     string `json:"Type"`
}

// ListZones 取站点列表。这是「验签」用的最小只读接口：
// 只要密钥错了、Region 错了、时钟偏了，这里立刻会给出明确错误。
// ZoneList 带诊断信息的站点列表。
//
// 空列表必须能自解释：调用方要能区分「账号下确实没有站点」和「连错了端点 /
// 分页参数被忽略 / 返回字段变了」。否则用户只会看到一句"没有站点"，然后去
// 控制台反复确认自己明明有站点。
type ZoneList struct {
	Zones    []Zone `json:"zones"`
	Total    int    `json:"total"`
	Endpoint string `json:"endpoint"`
	Raw      string `json:"raw,omitempty"`
}

func (e *edgeOne) ListZones() (*ZoneList, error) {
	raw, err := e.call("DescribeZones", map[string]any{"Offset": 0, "Limit": 100})
	if err != nil {
		return nil, err
	}
	var out struct {
		Zones      []Zone `json:"Zones"`
		TotalCount int    `json:"TotalCount"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析站点列表失败：%w", err)
	}
	zl := &ZoneList{Zones: out.Zones, Total: out.TotalCount, Endpoint: e.base}
	if len(out.Zones) == 0 {
		zl.Raw = snippet(raw)
	}
	return zl, nil
}

// AccelDomain 一条加速域名的状态。
type AccelDomain struct {
	Name         string `json:"DomainName"`
	Cname        string `json:"Cname"`
	Status       string `json:"DomainStatus"`
	IdentifyStat string `json:"IdentificationStatus"`
	HttpOrigin   int    `json:"HttpOriginPort"`
	HttpsOrigin  int    `json:"HttpsOriginPort"`
}

// GetAccelDomain 查单个加速域名。未找到时返回 (nil, nil)，不当作错误。
func (e *edgeOne) GetAccelDomain(zoneID, domain string) (*AccelDomain, error) {
	raw, err := e.call("DescribeAccelerationDomains", map[string]any{
		"ZoneId": zoneID,
		"Filters": []map[string]any{
			{"Name": "domain-name", "Values": []string{domain}},
		},
		"Offset": 0,
		"Limit":  20,
	})
	if err != nil {
		return nil, err
	}
	var probe struct {
		AccelerationDomains []AccelDomain `json:"AccelerationDomains"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	for i := range probe.AccelerationDomains {
		if strings.EqualFold(probe.AccelerationDomains[i].Name, domain) {
			return &probe.AccelerationDomains[i], nil
		}
	}
	return nil, nil
}

// CreateAccelDomain 创建一条加速域名，回源指向 originHost:originPort。
//
// originScheme 取 "HTTP" 或 "HTTPS"：frps 的 websocket 入口默认是明文 HTTP，
// 所以默认走 HTTP 回源；CDN 到用户那一段仍是 HTTPS，由边缘节点终结 TLS。
// 返回 CDN 分配的 CNAME（要加到 DNS 里）。
func (e *edgeOne) CreateAccelDomain(zoneID, domain, originHost string, originPort int, originScheme string) (string, error) {
	if originScheme == "" {
		originScheme = "HTTP"
	}
	payload := map[string]any{
		"ZoneId":     zoneID,
		"DomainName": domain,
		"OriginInfo": map[string]any{
			"OriginType": "IP_DOMAIN",
			"Origin":     originHost,
		},
		"OriginProtocol": originScheme,
	}
	if originScheme == "HTTPS" {
		payload["HttpsOriginPort"] = originPort
	} else {
		payload["HttpOriginPort"] = originPort
	}
	raw, err := e.call("CreateAccelerationDomain", payload)
	if err != nil {
		return "", err
	}
	var probe struct {
		Cname string `json:"Cname"`
	}
	_ = json.Unmarshal(raw, &probe)
	return probe.Cname, nil
}

// EnableWebSocket 打开站点级 WebSocket 并把空闲超时设成 timeoutSec。
//
// 这是 frp 走 CDN 的前提：EdgeOne 默认关闭 WebSocket，客户端会直接收到
// 非 101 的响应，frpc 只会报 "connection refused/EOF"，看不出是 CDN 关的。
func (e *edgeOne) EnableWebSocket(zoneID string, timeoutSec int) error {
	if timeoutSec <= 0 {
		timeoutSec = 300
	}
	_, err := e.call("ModifyL7AccSetting", map[string]any{
		"ZoneId": zoneID,
		"ZoneConfig": map[string]any{
			"WebSocket": map[string]any{
				"Switch":  "on",
				"Timeout": timeoutSec,
			},
		},
	})
	return err
}

// ZoneProbe 一条权限探针结果：用只读动作判断子账号被授到什么程度。
type ZoneProbe struct {
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// probePermissions 在"站点列表为空"时判断到底是没站点还是权限被收窄。
//
// 只用只读动作：DescribePacks（套餐）、DescribeAccountInfo（账号）、DescribeUserStatus（开通状态）。
// 这三个都是 QcloudTEOFullAccess 必然允许的；被拒就说明子账号策略被收窄了 ——
// 这正是"控制台明明有站点、API 却返回空列表"的典型成因。
func (e *edgeOne) probePermissions() []ZoneProbe {
	var out []ZoneProbe
	for _, act := range []string{"DescribePacks", "DescribeAccountInfo", "DescribeUserStatus"} {
		raw, err := e.call(act, map[string]any{})
		if err != nil {
			out = append(out, ZoneProbe{Action: act, OK: false, Detail: err.Error()})
			continue
		}
		out = append(out, ZoneProbe{Action: act, OK: true, Detail: snippet(raw)})
	}
	return out
}

// ListAccelDomains 列出站点下所有加速域名。
//
// 向导第三步要"点选已有域名"，而不是让用户手打全名 —— 手打既容易拼错，
// 也容易把一个尚未添加到该站点的域名填进来（后面创建必然失败）。
func (e *edgeOne) ListAccelDomains(zoneID string) ([]AccelDomain, error) {
	raw, err := e.call("DescribeAccelerationDomains", map[string]any{
		"ZoneId": zoneID,
		"Offset": 0,
		"Limit":  100,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		AccelerationDomains []AccelDomain `json:"AccelerationDomains"`
		TotalCount          int           `json:"TotalCount"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析加速域名列表失败：%w", err)
	}
	return out.AccelerationDomains, nil
}

// WebSocketState 读站点当前的 WebSocket 配置。
//
// 必须先读再决定要不要写：ModifyL7AccSetting 改的是**站点级**配置，腾讯云文档明确
// "该参数中的配置会对站点下的所有域名生效" —— 每调用一次，这个站点下所有加速域名
// 都会被重新部署一遍。对已经有生产域名的站点来说，无脑写一次就是一次全站抖动。
func (e *edgeOne) WebSocketState(zoneID string) (bool, int, error) {
	raw, err := e.call("DescribeL7AccSetting", map[string]any{"ZoneId": zoneID})
	if err != nil {
		return false, 0, err
	}
	var out struct {
		ZoneConfig struct {
			WebSocket struct {
				Switch  string `json:"Switch"`
				Timeout int    `json:"Timeout"`
			} `json:"WebSocket"`
		} `json:"ZoneConfig"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, 0, fmt.Errorf("解析站点加速配置失败：%w", err)
	}
	ws := out.ZoneConfig.WebSocket
	return strings.EqualFold(ws.Switch, "on"), ws.Timeout, nil
}
