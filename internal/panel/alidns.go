package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// aliyunDNSBase 可被测试替换为 httptest 服务器地址（生产用 https://alidns.aliyuncs.com/）。
var aliyunDNSBase = "https://alidns.aliyuncs.com/"

// 时间与随机串做成变量而不是直接调 time.Now/crypto-rand：这两个值都会进入待签串，
// 测试里不钉死就没法复现同一份签名，也就没法断言「同样参数签名一致」。
var (
	aliyunNowFn   = func() time.Time { return time.Now().UTC() }
	aliyunNonceFn = func() string { return aliyunNewNonce() }
)

// aliyunHTTPClient 单独一个 client：DNS 接口是面板接入 CDN 时的旁路依赖，
// 不能因为阿里云抽风把面板的请求链路一起挂住。
var aliyunHTTPClient = &http.Client{Timeout: 15 * time.Second}

// defaultAliyunTTL 阿里云免费版解析允许的最小 TTL 就是 600，传 0 会被拒绝。
const defaultAliyunTTL = 600

// aliyunDNS 极简云解析客户端，只实现「加一条记录」「查记录」两个动作。
//
// 不引官方 SDK 是有意的：面板只需要两个 API，为它拉进一个带签名、重试、Region
// 全套抽象的依赖（以及它的传递依赖）不划算，出问题时也不好对着日志核对签名。
type aliyunDNS struct {
	akID  string
	akSec string
}

func newAliyunDNS(akID, akSec string) *aliyunDNS {
	return &aliyunDNS{akID: akID, akSec: akSec}
}

// aliyunDNSRecord 云解析返回的记录。RRKeyWord 在服务端是模糊匹配（%keyword%），
// 所以调用方拿到的合集里可能混着 cdn2、old-cdn 之类，必须自己再比一次 RR。
type aliyunDNSRecord struct {
	RecordID string `json:"RecordId"`
	RR       string `json:"RR"`
	Type     string `json:"Type"`
	Value    string `json:"Value"`
}

// aliyunDNSResp 同时承载成功体与错误体的字段。
//
// 阿里云成功时不返回 Code/Message，失败时不返回业务字段，用一个结构体一次解码
// 就能区分两种结果，省掉「先探一遍 body 再解一遍」的麻烦。
type aliyunDNSResp struct {
	Code          string `json:"Code"`
	Message       string `json:"Message"`
	RecordID      string `json:"RecordId"`
	DomainRecords struct {
		Record []aliyunDNSRecord `json:"Record"`
	} `json:"DomainRecords"`
}

// AddRecord 添加一条解析记录，返回 RecordId。已存在同 RR+Type+Value 的记录时返回已有 RecordId 而不报错（幂等）。
func (a *aliyunDNS) AddRecord(domain, rr, recType, value string, ttl int) (string, error) {
	if ttl <= 0 {
		ttl = defaultAliyunTTL
	}

	recs, err := a.describe(domain, rr, recType)
	if err != nil {
		return "", err
	}
	for _, r := range recs {
		// 同一 RR 上的 CNAME 只能有一条，所以按 RR+Type 判定「已经接好了」即可。
		// 如果严格比 Value，用户把 CDN 加速域名换过一次之后再点接入，就会撞上
		// 阿里云的 DomainRecordDuplicate 而不是复用现有记录。
		if r.RecordID != "" && r.RR == rr && strings.EqualFold(r.Type, recType) {
			return r.RecordID, nil
		}
	}

	res, err := a.call("AddDomainRecord", map[string]string{
		"DomainName": domain,
		"RR":         rr,
		"Type":       recType,
		"Value":      value,
		"TTL":        strconv.Itoa(ttl),
	})
	if err != nil {
		return "", err
	}
	if res.RecordID == "" {
		// 没有 RecordId 的记录建不出来，直接报错比返回空串让上层当成成功好。
		return "", fmt.Errorf("阿里云 DNS AddDomainRecord 未返回 RecordId（%s %s）", rr, domain)
	}
	return res.RecordID, nil
}

// DescribeRecords 查询指定 RR 的记录，返回所有 Value（可能为空切片）。
func (a *aliyunDNS) DescribeRecords(domain, rr string) ([]string, error) {
	recs, err := a.describe(domain, rr, "")
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(recs))
	for _, r := range recs {
		// RRKeyWord 是模糊匹配，这里必须精确过滤，否则 cdn 会把 cdn2 的值也带出来。
		if r.RR == rr {
			values = append(values, r.Value)
		}
	}
	return values, nil
}

// describe 拉取指定 RR 的记录列表。recType 为空时不加 TypeKeyWord，让调用方拿到全部类型。
func (a *aliyunDNS) describe(domain, rr, recType string) ([]aliyunDNSRecord, error) {
	params := map[string]string{
		"DomainName": domain,
		"RRKeyWord":  rr,
		// 默认每页 20 条，一个 RR 前缀下攒出几十条历史记录很常见，
		// 截断会让「已存在」判定漏掉，进而重复添加。
		"PageSize": "100",
	}
	if recType != "" {
		params["TypeKeyWord"] = recType
	}
	res, err := a.call("DescribeDomainRecords", params)
	if err != nil {
		return nil, err
	}
	return res.DomainRecords.Record, nil
}

// call 走阿里云 RPC 风格 v1 签名：公共参数 + 业务参数拼成一个 GET 请求。
func (a *aliyunDNS) call(action string, params map[string]string) (*aliyunDNSResp, error) {
	q := make(map[string]string, len(params)+8)
	for k, v := range params {
		q[k] = v
	}
	q["Action"] = action
	q["Version"] = "2015-01-09"
	q["Format"] = "JSON"
	q["AccessKeyId"] = a.akID
	q["SignatureMethod"] = "HMAC-SHA1"
	q["SignatureVersion"] = "1.0"
	q["SignatureNonce"] = aliyunNonceFn()
	// 必须显式转 UTC：阿里云按 UTC 校验 Timestamp，带上本地时区（比如 +08:00）
	// 会被当成过期请求直接拒绝，而且报错只说签名错误，很难查。
	q["Timestamp"] = aliyunNowFn().UTC().Format("2006-01-02T15:04:05Z")

	// 签名用的待签串里不能有 Signature 本身，所以先签再塞进去。
	q["Signature"] = a.sign(q)

	endpoint := strings.TrimSuffix(aliyunDNSBase, "/") + "/?" + encodeQuery(q)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("阿里云 DNS %s 构造请求失败: %w", action, err)
	}

	resp, err := aliyunHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("阿里云 DNS %s 请求失败: %w", action, err)
	}
	defer resp.Body.Close()

	// 限长读取：接口正常响应只有几 KB，万一被代理塞回一个巨大 HTML 页，
	// 也不至于把内存吃满。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("阿里云 DNS %s 读取响应失败: %w", action, err)
	}

	var out aliyunDNSResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("阿里云 DNS %s 响应解析失败（HTTP %d）: %w", action, resp.StatusCode, err)
	}
	// 阿里云错误体一定同时带 Code 与 Message；成功体两者都没有。
	// 注意不能只看 HTTP 状态码：部分限流/参数错误也会返回 200。
	if out.Code != "" && out.Message != "" {
		return nil, fmt.Errorf("阿里云 DNS 错误 %s: %s", out.Code, out.Message)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("阿里云 DNS %s 返回 HTTP %d: %s", action, resp.StatusCode, aliyunSnippet(body))
	}
	return &out, nil
}

// sign 计算 Signature = Base64(HMAC-SHA1(AccessKeySecret+"&", StringToSign))。
func (a *aliyunDNS) sign(q map[string]string) string {
	// StringToSign 的参与项：HTTP 方法、规范化后的 URI、规范化后的查询串。
	// q 里此时还没有 Signature，因此 encodeQuery 的结果天然就是 CanonicalizedQueryString。
	stringToSign := "GET&" + percentEncode("/") + "&" + percentEncode(encodeQuery(q))
	mac := hmac.New(sha1.New, []byte(a.akSec+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// encodeQuery 把参数按 key 字典序拼成 k=v&k=v，两端都做 RFC3986 编码。
func encodeQuery(q map[string]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.Grow(len(q) * 24)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(percentEncode(k))
		b.WriteByte('=')
		b.WriteString(percentEncode(q[k]))
	}
	return b.String()
}

const percentUpperHex = "0123456789ABCDEF"

// percentEncode 按 RFC3986 编码：只保留 A-Za-z0-9-_.~，其余字节转成大写 %XX。
//
// 不能用 net/url.QueryEscape：那个按表单规则把空格编成 "+"，而阿里云签名要求
// 空格必须是 %20，"+" 会导致签名对不上；同理它也不保证对 "*" 编码，而阿里云
// 把 "*" 视为需要转义的字符（%2A）。所以这里逐字节自己处理，中文之类的多字节
// 字符也自然变成 UTF-8 字节序列的 %XX。
func percentEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' ||
			c >= 'a' && c <= 'z' ||
			c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(percentUpperHex[c>>4])
		b.WriteByte(percentUpperHex[c&0x0f])
	}
	return b.String()
}

// aliyunNewNonce 生成签名随机串。阿里云要求每次请求唯一（去横线的 UUID 等价物），
// 否则重放窗口内会被判为重放。
func aliyunNewNonce() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 在支持的平台上不会失败；真失败也不能退化成重复值，
		// 用纳秒时间戳至少保证同一进程内不重复。
		return strconv.FormatInt(aliyunNowFn().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// aliyunSnippet 截断响应体，避免把整页 HTML 塞进错误信息。
func aliyunSnippet(body []byte) string {
	const max = 200
	s := strings.TrimSpace(string(body))
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
