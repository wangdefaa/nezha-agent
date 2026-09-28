package utls_test

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/nezhahq/agent/pkg/util"
	utlsx "github.com/nezhahq/agent/pkg/utls"
)

// newTrustingClient 构造与 agent 相同的 uTLS 客户端，额外信任 httptest 内置证书。
func newTrustingClient(srv *httptest.Server, timeout time.Duration) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	rt := utlsx.NewUTLSHTTPRoundTripperWithProxy(utls.HelloChrome_Auto,
		&utls.Config{RootCAs: pool}, http.DefaultTransport, nil, util.BrowserHeaders())
	return &http.Client{Transport: rt, Timeout: timeout}
}

// startTLSBlackhole 只接受 TCP 连接、从不回应 ClientHello。
func startTLSBlackhole(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln.Addr().String()
}

func newOKServer(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 黑洞目标必须按客户端超时返回，且不能拖住随后对其它目标的拨测。
func TestBlackholeDoesNotBlockOtherTargets(t *testing.T) {
	ok := newOKServer(t)
	client := newTrustingClient(ok, time.Second)
	start := time.Now()
	if _, err := client.Get("https://" + startTLSBlackhole(t) + "/"); err == nil {
		t.Fatal("黑洞目标不应成功")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("黑洞请求应随超时返回，实际 %v", d)
	}
	resp, err := client.Get(newOKServer(t).URL)
	if err != nil {
		t.Fatalf("黑洞之后的健康目标不应失败: %v", err)
	}
	resp.Body.Close()
}

// 经 uTLS 拨号的响应也要带上 TLS 状态，拨测才能回报证书签发者与到期时间。
func TestResponseCarriesTLSState(t *testing.T) {
	ok := newOKServer(t)
	resp, err := newTrustingClient(ok, 5*time.Second).Get(ok.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("resp.TLS 缺失对端证书")
	}
}
