package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/jtprogru/dumbwaiter/internal/config"
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
	sites, err := proxy.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, sites, log)
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
