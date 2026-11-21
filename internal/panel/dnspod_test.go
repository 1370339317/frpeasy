package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 空记录是正常态：腾讯云返回 ResourceNotFound.NoDataOfRecord 且 body 里没有业务字段。
// 这条路径必须能继续走到 CreateRecord，否则第一次添加解析永远失败（线上踩过）。
func TestDNSPodEnsureCNAMEOnEmptyList(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-TC-Action") {
		case "DescribeRecordList":
			_, _ = w.Write([]byte(`{"Response":{"Error":{"Code":"ResourceNotFound.NoDataOfRecord","Message":"记录列表为空。"},"RequestId":"r1"}}`))
		case "CreateRecord":
			created = true
			_, _ = w.Write([]byte(`{"Response":{"RecordId":12345,"RequestId":"r2"}}`))
		default:
			t.Errorf("意外的 action: %s", r.Header.Get("X-TC-Action"))
		}
	}))
	defer srv.Close()

	d := newDNSPod("AK", "SK")
	d.base = srv.URL
	ok, note, err := d.EnsureCNAME("dvvb.cn", "ecsfrp", "ecsfrp.dvvb.cn.eo.dnse0.com", 600)
	if err != nil {
		t.Fatalf("空列表应继续创建，实际报错: %v", err)
	}
	if !ok || !created {
		t.Fatalf("应创建记录，实际 ok=%v created=%v note=%s", ok, created, note)
	}
}

// 已存在同值记录：幂等返回，不再创建。
func TestDNSPodEnsureCNAMEIdempotent(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-TC-Action") == "CreateRecord" {
			created = true
		}
		_, _ = w.Write([]byte(`{"Response":{"RecordList":[{"Name":"ecsfrp","Value":"ecsfrp.dvvb.cn.eo.dnse0.com.","Line":"默认"}],"RequestId":"r"}}`))
	}))
	defer srv.Close()

	d := newDNSPod("AK", "SK")
	d.base = srv.URL
	ok, note, err := d.EnsureCNAME("dvvb.cn", "ecsfrp", "ecsfrp.dvvb.cn.eo.dnse0.com", 600)
	if err != nil || ok || created {
		t.Fatalf("同值记录不应重复创建：ok=%v created=%v err=%v note=%s", ok, created, err, note)
	}
}

// 同名但指向别处：必须报错而不是覆盖。
func TestDNSPodEnsureCNAMERefusesOverwrite(t *testing.T) {
	var created bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-TC-Action") == "CreateRecord" {
			created = true
		}
		_, _ = w.Write([]byte(`{"Response":{"RecordList":[{"Name":"ecsfrp","Value":"other.example.com","Line":"默认"}],"RequestId":"r"}}`))
	}))
	defer srv.Close()

	d := newDNSPod("AK", "SK")
	d.base = srv.URL
	_, _, err := d.EnsureCNAME("dvvb.cn", "ecsfrp", "ecsfrp.dvvb.cn.eo.dnse0.com", 600)
	if err == nil {
		t.Fatal("指向别处时应报错")
	}
	if created {
		t.Fatal("绝不能覆盖已有记录")
	}
}
