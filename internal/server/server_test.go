package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
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
			`dumbwaiter_tls_certificate_expiration_timestamp_seconds{cert="` + crtPath + `"} ` + strconv.FormatInt(cert.NotAfter.Unix(), 10),
			`dumbwaiter_tls_certificate_reload_errors_total 0`,
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

// reviewConfig is a config with tight timeouts, so slow-client tests do not
// wait for the production defaults.
func reviewConfig(t *testing.T, upstream string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
server:
  http: {addr: "127.0.0.1:0"}
  admin: {addr: "127.0.0.1:0"}
  read_header_timeout: 50ms
  read_timeout: 50ms
  write_idle_timeout: 50ms
  idle_timeout: 50ms
  shutdown_delay: 1ms
  shutdown_timeout: 200ms
s3: {endpoint: "` + upstream + `", path_style: true, timeout: 50ms, read_timeout: 50ms}
sites: [{hosts: [example.com], bucket: site}]
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A client that announces a body and never sends it holds the connection in
// the post-handler drain, which read_header_timeout does not cover.
func TestUnreadRequestBodyIsCutOff(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()

	cfg := reviewConfig(t, upstream.URL)
	m := metrics.New()
	log := slog.New(slog.DiscardHandler)
	sites, err := proxy.New(cfg, m, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, sites, m, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}()
	waitReady(t, "http://"+srv.AdminAddr()+"/readyz")

	// Unknown host answers before any S3 call, so the body is never needed.
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.HTTPAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: unknown.invalid\r\nContent-Length: 1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatalf("no response: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("response held for %v, want it bounded by read_timeout", elapsed)
	}
}

// A client that reads the response very slowly is dropped by the write idle
// deadline instead of holding the goroutine and the S3 stream.
func TestSlowReaderIsCutOff(t *testing.T) {
	body := strings.Repeat("x", 32<<20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		io.WriteString(w, body)
	}))
	defer upstream.Close()

	cfg := reviewConfig(t, upstream.URL)
	m := metrics.New()
	log := slog.New(slog.DiscardHandler)
	sites, err := proxy.New(cfg, m, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, sites, m, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}()
	waitReady(t, "http://"+srv.AdminAddr()+"/readyz")

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.HTTPAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A tiny receive buffer, and no reads at all after the request.
	conn.(*net.TCPConn).SetReadBuffer(1024)
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res := do(t, http.DefaultClient, http.MethodGet, "http://"+srv.AdminAddr()+"/metrics")
		if strings.Contains(res.body, `dumbwaiter_http_requests_in_flight{listener="http"} 0`) {
			return // the stalled download was dropped
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("stalled download still in flight, write idle deadline did not fire")
}

// quic-go has no header read timeout, so the connection settings are what
// bounds a client that opens streams and never finishes the headers: the
// limit must reach the QUIC server and be enforced on the wire.
func TestHTTP3StreamLimit(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()

	cfg := reviewConfig(t, upstream.URL)
	crtPath, keyPath, cert := testcert.Write(t, t.TempDir(), "tls", "example.com")
	cfg.Server.HTTPS = &config.HTTPS{
		Addr:                   "127.0.0.1:0",
		PublicPort:             443,
		HTTP3:                  true,
		Certificates:           []config.Certificate{{Cert: crtPath, Key: keyPath}},
		H3MaxIncomingStreams:   2,
		H3IdleTimeout:          5 * time.Second,
		H3HandshakeIdleTimeout: 5 * time.Second,
	}

	m := metrics.New()
	log := slog.New(slog.DiscardHandler)
	sites, err := proxy.New(cfg, m, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, sites, m, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}()
	waitReady(t, "http://"+srv.AdminAddr()+"/readyz")

	if got := srv.h3Srv.QUICConfig.MaxIncomingStreams; got != 2 {
		t.Fatalf("MaxIncomingStreams = %d, want 2", got)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	conn, err := quic.DialAddr(t.Context(), srv.HTTP3Addr(), &tls.Config{RootCAs: pool, ServerName: "example.com", NextProtos: []string{http3.NextProtoH3}}, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")

	for i := range 2 {
		if _, err := conn.OpenStreamSync(t.Context()); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
	// The third stream must not open: the limit is what keeps a client from
	// parking an unbounded number of half-sent requests.
	ctxTimeout, cancelTimeout := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancelTimeout()
	if _, err := conn.OpenStreamSync(ctxTimeout); err == nil {
		t.Fatal("third stream opened, the QUIC stream limit is not enforced")
	}
}

// deadlineWriter records the write deadlines the recorder sets.
type deadlineWriter struct {
	header    http.Header
	deadlines []time.Time
}

func (w *deadlineWriter) Header() http.Header         { return w.header }
func (w *deadlineWriter) WriteHeader(int)             {}
func (w *deadlineWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.deadlines = append(w.deadlines, t)
	return nil
}

// The deadline must be refreshed on every write. Refreshing it once, as an
// absolute write timeout would, kills long downloads that keep progressing.
func TestRecorderRefreshesWriteDeadline(t *testing.T) {
	w := &deadlineWriter{header: http.Header{}}
	rec := &recorder{ResponseWriter: w, writeIdle: 50 * time.Millisecond}

	if _, err := rec.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := rec.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}

	if len(w.deadlines) != 2 {
		t.Fatalf("deadlines set %d times, want one per write", len(w.deadlines))
	}
	if !w.deadlines[1].After(w.deadlines[0]) {
		t.Fatalf("second deadline %v is not after the first %v", w.deadlines[1], w.deadlines[0])
	}
}

// A response that takes longer than write_idle_timeout must still arrive in
// full when data keeps flowing. The slow part is the upstream, not the socket:
// gaps between writes stay tiny while the whole transfer dwarfes the deadline,
// so a per-request absolute deadline would truncate it and an idle one does
// not.
func TestSlowButProgressingResponseCompletes(t *testing.T) {
	const (
		chunks = 100
		chunk  = 16 << 10
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(chunks*chunk))
		w.WriteHeader(http.StatusOK)
		for range chunks {
			io.WriteString(w, strings.Repeat("x", chunk))
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	// write_idle_timeout is 50ms, the transfer takes about 500ms.
	cfg := reviewConfig(t, upstream.URL)
	m := metrics.New()
	log := slog.New(slog.DiscardHandler)
	sites, err := proxy.New(cfg, m, log)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(t.Context(), cfg, sites, m, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	}()
	waitReady(t, "http://"+srv.AdminAddr()+"/readyz")

	start := time.Now()
	res := do(t, http.DefaultClient, http.MethodGet, "http://"+srv.HTTPAddr()+"/")
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if want := chunks * chunk; len(res.body) != want {
		t.Fatalf("got %d bytes, want %d: the response was cut short", len(res.body), want)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("transfer took %v, too fast to exceed write_idle_timeout", elapsed)
	}
}
