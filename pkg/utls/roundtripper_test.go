package utls_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/nezhahq/agent/pkg/util"
	utlsx "github.com/nezhahq/agent/pkg/utls"
)

// 用本地 httptest 服务覆盖 uTLS RoundTripper，取代原先访问外网（patreon.com）的检测用例。

type tlsRequestObservation struct {
	userAgent string
	alpn      string
	protocol  string
}

func TestUTLSRoundTripper_rejectsInvalidTLS(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	client := newUTLSClient(t, new(utls.Config))
	invalidTLSURL := strings.Replace(server.URL, "http://", "https://", 1)

	response, err := client.Get(invalidTLSURL)

	if err == nil {
		response.Body.Close()
		t.Fatal("GET plaintext server over TLS succeeded, want handshake error")
	}
	if response != nil {
		response.Body.Close()
		t.Fatalf("response = %#v, want nil after handshake error", response)
	}
	t.Logf("invalid TLS rejected with error=%q", err)
}

var ordinaryHTTPProtocolCases = []struct {
	name          string
	enableHTTP2   bool
	nextProtocols []string
	wantALPN      string
	wantProtocol  string
}{
	{name: "HTTP/1.1", nextProtocols: []string{"http/1.1"}, wantALPN: "http/1.1", wantProtocol: "HTTP/1.1"},
	{name: "HTTP/2", enableHTTP2: true, nextProtocols: []string{"h2", "http/1.1"}, wantALPN: "h2", wantProtocol: "HTTP/2.0"},
}

func TestUTLSRoundTripper_selectsOrdinaryHTTPProtocol(t *testing.T) {
	for _, test := range ordinaryHTTPProtocolCases {
		t.Run(test.name, func(t *testing.T) {
			server, observed := startObservedTLSServer(t, test.enableHTTP2, test.nextProtocols)
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			client := newUTLSClient(t, &utls.Config{RootCAs: roots})

			body := getBody(t, client, server.URL)
			observation := <-observed

			if string(body) != "local tls response" {
				t.Fatalf("body = %q, want %q", body, "local tls response")
			}
			if observation.userAgent != util.MacOSChromeUA {
				t.Fatalf("User-Agent = %q, want %q", observation.userAgent, util.MacOSChromeUA)
			}
			if observation.alpn != test.wantALPN || observation.protocol != test.wantProtocol {
				t.Fatalf("ALPN/protocol = %q/%q, want %q/%q", observation.alpn, observation.protocol, test.wantALPN, test.wantProtocol)
			}
			t.Logf("observed User-Agent=%q ALPN=%q protocol=%q body=%q", observation.userAgent, observation.alpn, observation.protocol, body)
		})
	}
}

func newUTLSClient(t *testing.T, config *utls.Config) *http.Client {
	t.Helper()
	backdrop := &http.Transport{Proxy: nil}
	t.Cleanup(backdrop.CloseIdleConnections)
	return &http.Client{
		Transport: utlsx.NewUTLSHTTPRoundTripperWithProxy(utls.HelloChrome_Auto, config, backdrop, nil, util.BrowserHeaders()),
		Timeout:   5 * time.Second,
	}
}

// startObservedTLSServer 启动本地 TLS 服务，记录每个请求的 UA、协商的 ALPN 与 HTTP 协议版本。
func startObservedTLSServer(t *testing.T, enableHTTP2 bool, nextProtocols []string) (*httptest.Server, <-chan tlsRequestObservation) {
	t.Helper()
	observed := make(chan tlsRequestObservation, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		observed <- tlsRequestObservation{userAgent: request.UserAgent(), alpn: request.TLS.NegotiatedProtocol, protocol: request.Proto}
		if _, err := response.Write([]byte("local tls response")); err != nil {
			t.Errorf("write local TLS response: %v", err)
		}
	}))
	server.EnableHTTP2 = enableHTTP2
	server.TLS = &tls.Config{NextProtos: nextProtocols}
	server.StartTLS()
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server, observed
}

func getBody(t *testing.T, client *http.Client, url string) []byte {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET local TLS server: %v", err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read local TLS response: %v", err)
	}
	return body
}
