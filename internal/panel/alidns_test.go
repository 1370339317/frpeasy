package panel

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// aliyunTestEnv 把 base 地址和签名输入都换成可控值，并在测试结束时还原，
// 避免污染同包其它用例（这些是包级变量）。
func aliyunTestEnv(t *testing.T, base string) {
	t.Helper()
	oldBase, oldNow, oldNonce := aliyunDNSBase, aliyunNowFn, aliyunNonceFn
	t.Cleanup(func() {
		aliyunDNSBase, aliyunNowFn, aliyunNonceFn = oldBase, oldNow, oldNonce
	})
	aliyunDNSBase = base
	aliyunNowFn = func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}
	aliyunNonceFn = func() string { return "fixednonce0123456789abcdef" }
}

func TestAliyunDNSRequestAndSignature(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"DomainRecords":{"Record":[]}}`))
	}))
	defer srv.Close()

	aliyunTestEnv(t, srv.URL+"/")
	c := newAliyunDNS("test-ak", "test-secret")

	for i := 0; i < 2; i++ {
		vals, err := c.DescribeRecords("example.com", "cdn")
		if err != nil {
			t.Fatalf("DescribeRecords: %v", err)
		}
		if len(vals) != 0 {
			t.Fatalf("空响应应返回空切片，实际 %v", vals)
		}
	}

	if len(queries) != 2 {
		t.Fatalf("应发出 2 次请求，实际 %d", len(queries))
	}
	for i, q := range queries {
		for _, k := range []string{
			"Action", "Version", "Format", "AccessKeyId",
			"SignatureMethod", "SignatureVersion", "SignatureNonce",
			"Timestamp", "Signature", "DomainName", "RRKeyWord",
		} {
			if q.Get(k) == "" {
				t.Fatalf("第 %d 次请求缺少公共参数 %s，query=%v", i+1, k, q)
			}
		}
		if q.Get("Version") != "2015-01-09" || q.Get("SignatureMethod") != "HMAC-SHA1" || q.Get("SignatureVersion") != "1.0" {
			t.Fatalf("公共参数取值不对: %v", q)
		}
		if q.Get("Format") != "JSON" {
			t.Fatalf("Format 应为 JSON: %v", q)
		}
		if q.Get("Action") != "DescribeDomainRecords" {
			t.Fatalf("Action 不对: %v", q.Get("Action"))
		}

		ts := q.Get("Timestamp")
		// 末尾必须是 Z：带 +08:00 之类偏移会被阿里云当成过期请求拒掉。
		if !strings.HasSuffix(ts, "Z") {
			t.Fatalf("Timestamp 必须以 Z 结尾，实际 %q", ts)
		}
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			t.Fatalf("Timestamp 不是合法 RFC3339: %q (%v)", ts, err)
		}

		sig := q.Get("Signature")
		raw, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			t.Fatalf("Signature 不是合法 Base64: %q (%v)", sig, err)
		}
		if len(raw) != sha1.Size {
			t.Fatalf("HMAC-SHA1 签名应为 %d 字节，实际 %d", sha1.Size, len(raw))
		}

		// 用测试里独立实现的编码逻辑重算一遍，验证待签串的排序、编码、密钥拼接都对。
		if want := testAliyunSign("test-secret", q); want != sig {
			t.Fatalf("签名不一致\nwant=%s\ngot =%s\nquery=%v", want, sig, q)
		}
	}

	if first, second := queries[0].Get("Signature"), queries[1].Get("Signature"); first != second {
		t.Fatalf("参数完全相同（时间与 nonce 已固定），签名应一致: %q vs %q", first, second)
	}
}

func TestAliyunDNSAddRecordReturnsID(t *testing.T) {
	var (
		mu       sync.Mutex
		addCalls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("Action") {
		case "DescribeDomainRecords":
			_, _ = w.Write([]byte(`{"DomainRecords":{"Record":[]}}`))
		case "AddDomainRecord":
			mu.Lock()
			addCalls++
			mu.Unlock()
			if got := r.URL.Query().Get("TTL"); got != "600" {
				t.Errorf("ttl<=0 时应补成 600，实际 %q", got)
			}
			_, _ = w.Write([]byte(`{"RequestId":"req-1","RecordId":"rec-12345"}`))
		default:
			t.Errorf("意外的 Action %q", r.URL.Query().Get("Action"))
		}
	}))
	defer srv.Close()

	aliyunTestEnv(t, srv.URL+"/")
	c := newAliyunDNS("ak", "sec")

	id, err := c.AddRecord("example.com", "cdn", "CNAME", "cdn.example.com.w.cdngslb.com", 0)
	if err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if id != "rec-12345" {
		t.Fatalf("应返回接口给的 RecordId，实际 %q", id)
	}
	mu.Lock()
	defer mu.Unlock()
	if addCalls != 1 {
		t.Fatalf("应调用一次 AddDomainRecord，实际 %d", addCalls)
	}
}

func TestAliyunDNSAddRecordIdempotent(t *testing.T) {
	var (
		mu       sync.Mutex
		addCalls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("Action") {
		case "DescribeDomainRecords":
			q := r.URL.Query()
			if q.Get("RRKeyWord") != "cdn" || q.Get("TypeKeyWord") != "CNAME" {
				t.Errorf("查重参数不对: %v", q)
			}
			// 故意混入一条 RR 只是前缀相同的记录（服务器端是模糊匹配），
			// 用来验证 RR 必须精确比对。
			_, _ = w.Write([]byte(`{"DomainRecords":{"Record":[
				{"RecordId":"rec-cdn2","RR":"cdn2","Type":"CNAME","Value":"other.example.com"},
				{"RecordId":"rec-exist","RR":"cdn","Type":"CNAME","Value":"cdn.example.com.w.cdngslb.com"}
			]}}`))
		case "AddDomainRecord":
			mu.Lock()
			addCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"RecordId":"rec-new"}`))
		}
	}))
	defer srv.Close()

	aliyunTestEnv(t, srv.URL+"/")
	c := newAliyunDNS("ak", "sec")

	id, err := c.AddRecord("example.com", "cdn", "CNAME", "cdn.example.com.w.cdngslb.com", 600)
	if err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if id != "rec-exist" {
		t.Fatalf("命中已有记录应返回其 RecordId，实际 %q", id)
	}
	mu.Lock()
	defer mu.Unlock()
	if addCalls != 0 {
		t.Fatalf("已存在时不应再调 AddDomainRecord，实际调用 %d 次", addCalls)
	}
}

func TestAliyunDNSDescribeRecordsExactRR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"DomainRecords":{"Record":[
			{"RecordId":"1","RR":"cdn","Type":"CNAME","Value":"a.example.com"},
			{"RecordId":"2","RR":"cdn","Type":"A","Value":"1.2.3.4"},
			{"RecordId":"3","RR":"cdn2","Type":"CNAME","Value":"b.example.com"}
		]}}`))
	}))
	defer srv.Close()

	aliyunTestEnv(t, srv.URL+"/")
	c := newAliyunDNS("ak", "sec")

	vals, err := c.DescribeRecords("example.com", "cdn")
	if err != nil {
		t.Fatalf("DescribeRecords: %v", err)
	}
	want := []string{"a.example.com", "1.2.3.4"}
	if strings.Join(vals, ",") != strings.Join(want, ",") {
		t.Fatalf("应只返回 RR 完全等于 cdn 的记录，want=%v got=%v", want, vals)
	}
}

func TestAliyunDNSErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"RequestId":"req-x","Code":"InvalidAccessKeyId.NotFound","Message":"Specified access key is not found."}`))
	}))
	defer srv.Close()

	aliyunTestEnv(t, srv.URL+"/")
	c := newAliyunDNS("bad", "sec")

	_, err := c.DescribeRecords("example.com", "cdn")
	if err == nil {
		t.Fatal("错误响应应返回 error")
	}
	if !strings.Contains(err.Error(), "InvalidAccessKeyId.NotFound") {
		t.Fatalf("错误信息应包含 Code，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Specified access key is not found.") {
		t.Fatalf("错误信息应包含 Message，实际 %q", err.Error())
	}
}

func TestPercentEncode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a b", "a%20b"},           // 空格必须 %20，不能是 +
		{"*", "%2A"},               // 阿里云要求 * 转义
		{"~", "~"},                 // RFC3986 的非保留字符
		{"a-b_c.d~e", "a-b_c.d~e"}, // 非保留字符原样保留
		{"中", "%E4%B8%AD"},         // 中文按 UTF-8 逐字节编码
		{"a/b", "a%2Fb"},           // 路径分隔符也要编码
		{"a+b", "a%2Bb"},           // 加号本身要编码
		{"A1z9", "A1z9"},           // 字母数字不动
		{"=&#", "%3D%26%23"},       // 保留字符转大写十六进制
		{"", ""},                   // 空串
	}
	for _, c := range cases {
		if got := percentEncode(c.in); got != c.want {
			t.Errorf("percentEncode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// testAliyunSign 在测试侧独立实现一遍签名，不复用被测代码的 percentEncode，
// 这样签名链路真的写错了才能被抓到。
func testAliyunSign(secret string, q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == "Signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var canon strings.Builder
	for i, k := range keys {
		if i > 0 {
			canon.WriteByte('&')
		}
		canon.WriteString(testPercentEncode(k))
		canon.WriteByte('=')
		canon.WriteString(testPercentEncode(q.Get(k)))
	}
	stringToSign := "GET&" + testPercentEncode("/") + "&" + testPercentEncode(canon.String())
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func testPercentEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}
