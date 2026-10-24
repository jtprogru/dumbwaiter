package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
	"github.com/jtprogru/dumbwaiter/internal/proxy"
	"github.com/jtprogru/dumbwaiter/internal/testcert"
)

func TestEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/site/index.html" {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "hello")
	}))
	defer upstream.Close()

	crtPath, keyPath, cert := testcert.Write(t, t.TempDir(), "tls", "example.com")

	cfg, err := config.Parse([]byte(`
server:
  http: {addr: "127.0.0.1:0", redirect_to_https: true}
  admin: {addr: "127.0.0.1:0"}
  https:
    addr: "127.0.0.1:0"
    http3: true
    certificates: [{cert: "` + crtPath + `", key: "` + keyPath + `"}]
  shutdown_delay: 1ms
s3: {endpoint: "` + upstream.URL + `", path_style: true}
sites: [{hosts: [example.com], bucket: site}]
`))
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	m := metrics.New()
	m.BuildInfo("test")
	sites, err := proxy.New(cfg, m, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, sites, m, log)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()

	waitReady(t, "http://"+srv.AdminAddr()+"/readyz")

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	tlsConf := &tls.Config{RootCAs: pool, ServerName: "example.com"}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	t.Run("http redirects to https", func(t *testing.T) {
		c := &http.Client{CheckRedirect: noRedirect}
		res := do(t, c, http.MethodGet, "http://"+srv.HTTPAddr()+"/a?b=c")
		if res.status != http.StatusMovedPermanently || res.header.Get("Location") != "https://example.com/a?b=c" {
			t.Errorf("got %d Location=%q", res.status, res.header.Get("Location"))
		}
	})

	t.Run("https uses http2 and advertises http3", func(t *testing.T) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf, ForceAttemptHTTP2: true}}
		res := do(t, c, http.MethodGet, "https://"+srv.HTTPSAddr()+"/")
		if res.status != http.StatusOK || res.body != "hello" {
			t.Errorf("got %d %q", res.status, res.body)
		}
		if res.protoMajor != 2 {
			t.Errorf("proto major = %d, want HTTP/2", res.protoMajor)
		}
		if alt := res.header.Get("Alt-Svc"); !strings.Contains(alt, `h3=":443"`) {
			t.Errorf("Alt-Svc = %q, want public port 443", alt)
		}
	})

	t.Run("http3", func(t *testing.T) {
		tr := &http3.Transport{TLSClientConfig: tlsConf}
		defer tr.Close()
		res := do(t, &http.Client{Transport: tr}, http.MethodGet, "https://"+srv.HTTP3Addr()+"/")
		if res.status != http.StatusOK || res.body != "hello" || res.protoMajor != 3 {
			t.Errorf("got HTTP/%d %d %q", res.protoMajor, res.status, res.body)
		}
	})

	t.Run("http3 rejects post", func(t *testing.T) {
		tr := &http3.Transport{TLSClientConfig: tlsConf}
		defer tr.Close()
		res := do(t, &http.Client{Transport: tr}, http.MethodPost, "https://"+srv.HTTP3Addr()+"/")
		if res.status != http.StatusMethodNotAllowed {
			t.Errorf("status = %d", res.status)
		}
	})

	t.Run("unknown host", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.HTTPAddr()+"/", nil)
		req.Host = "scanner.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d", resp.StatusCode)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		res := do(t, http.DefaultClient, http.MethodGet, "http://"+srv.AdminAddr()+"/metrics")
		if res.status != http.StatusOK {
			t.Fatalf("status = %d", res.status)
		}
		for _, want := range []string{
			`dumbwaiter_http_requests_total{listener="http",proto="HTTP/1",host="example.com",method="GET",code="301"} 1`,
			`dumbwaiter_http_requests_total{listener="https",proto="HTTP/2",host="example.com",method="GET",code="200"} 1`,
			`dumbwaiter_http_requests_total{listener="http3",proto="HTTP/3",host="example.com",method="GET",code="200"} 1`,
			`dumbwaiter_http_requests_total{listener="http3",proto="HTTP/3",host="example.com",method="OTHER",code="405"} 1`,
			`dumbwaiter_http_requests_total{listener="http",proto="HTTP/1",host="unknown",method="GET",code="404"} 1`,
			`dumbwaiter_http_request_duration_seconds_count{listener="https",host="example.com"} 1`,
			`dumbwaiter_http_response_bytes_total{listener="https",host="example.com"} 5`,
			`dumbwaiter_http_requests_in_flight{listener="https"} 0`,
			`dumbwaiter_s3_requests_total{bucket="site",method="GET",code="200"} 2`,
			`dumbwaiter_s3_request_duration_seconds_count{bucket="site",method="GET"} 2`,
			`dumbwaiter_build_info{version="test",goversion="` + runtime.Version() + `"} 1`,
			"\ngo_goroutines ",
		} {
			if !strings.Contains(res.body, want) {
				t.Errorf("metrics lack %s", want)
			}
		}
		if strings.Contains(res.body, "scanner.test") {
			t.Error("a host from the request leaked into labels")
		}
	})

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

type result struct {
	status     int
	protoMajor int
	header     http.Header
	body       string
}

// do sends a request with Host: example.com and reads the whole response.
func do(t *testing.T, c *http.Client, method, url string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "example.com"
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return result{status: resp.StatusCode, protoMajor: resp.ProtoMajor, header: resp.Header, body: string(body)}
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not become ready")
}
