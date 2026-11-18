package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
	"github.com/jtprogru/dumbwaiter/internal/proxy"
	"github.com/jtprogru/dumbwaiter/internal/testcert"
)

func TestTruncatedResponseAbortsEveryProtocol(t *testing.T) {
	for _, protocol := range []string{"http1", "http2", "http3"} {
		for _, framing := range []string{"length", "chunked"} {
			t.Run(protocol+"/"+framing, func(t *testing.T) {
				release := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "good") {
						io.WriteString(w, "hello")
						return
					}
					if strings.HasSuffix(r.URL.Path, "cached") {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					if framing == "length" {
						w.Header().Set("Content-Length", "131072")
					}
					io.WriteString(w, strings.Repeat("x", 64<<10))
					w.(http.Flusher).Flush()
					<-release
					panic(http.ErrAbortHandler)
				}))
				t.Cleanup(upstream.Close)
				srv, m, tlsConfig := transferServer(t, upstream.URL)
				client := &http.Client{Timeout: 5 * time.Second}
				url, listener, major := "http://"+srv.HTTPAddr(), "http", 1
				if protocol == "http3" {
					tr := &http3.Transport{TLSClientConfig: tlsConfig}
					t.Cleanup(func() { tr.Close() })
					client.Transport = tr
					url, listener, major = "https://"+srv.HTTP3Addr(), "http3", 3
				} else {
					tr := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: protocol == "http2"}
					t.Cleanup(tr.CloseIdleConnections)
					client.Transport = tr
					if protocol == "http2" {
						url, listener, major = "https://"+srv.HTTPSAddr(), "https", 2
					}
				}
				defer unblock()
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/broken", nil)
				req.Host = "example.com"
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.ProtoMajor != major || resp.StatusCode != http.StatusOK {
					t.Fatalf("unexpected initial response: %s %d", resp.Proto, resp.StatusCode)
				}
				unblock()
				if body, err := io.ReadAll(resp.Body); err == nil {
					t.Fatalf("truncated response looks complete: %d bytes", len(body))
				}
				var out bytes.Buffer
				m.WritePrometheus(&out)
				for _, want := range []string{
					`dumbwaiter_http_response_outcomes_total{listener="` + listener + `",host="example.com",outcome="aborted"} 1`,
					`dumbwaiter_http_requests_in_flight{listener="` + listener + `"} 0`,
				} {
					if !strings.Contains(out.String(), want) {
						t.Errorf("missing metric %s: %s", want, &out)
					}
				}
				// Header-only responses and another request after their write
				// deadline must still work on the same transport.
				if res := do(t, client, http.MethodHead, url+"/good"); res.status != 200 || res.body != "" {
					t.Fatalf("HEAD: %+v", res)
				}
				if res := do(t, client, http.MethodGet, url+"/cached"); res.status != 304 || res.body != "" {
					t.Fatalf("304: %+v", res)
				}
				time.Sleep(150 * time.Millisecond)
				if res := do(t, client, http.MethodGet, url+"/good"); res.body != "hello" || res.protoMajor != major {
					t.Fatalf("next request: %+v", res)
				}
			})
		}
	}
}

func transferServer(t *testing.T, upstream string) (*Server, *metrics.Metrics, *tls.Config) {
	t.Helper()
	crtPath, keyPath, cert := testcert.Write(t, t.TempDir(), "tls", "example.com")
	cfg, err := config.Parse([]byte(`
server:
  http: {addr: "127.0.0.1:0"}
  admin: {addr: "127.0.0.1:0"}
  https:
    addr: "127.0.0.1:0"
    http3: true
    certificates: [{cert: "` + crtPath + `", key: "` + keyPath + `"}]
  write_idle_timeout: 100ms
  shutdown_delay: 1ms
s3: {endpoint: "` + upstream + `", path_style: true}
sites: [{hosts: [example.com], bucket: site}]
`))
	if err != nil {
		t.Fatal(err)
	}
	m, log := metrics.New(), slog.New(slog.DiscardHandler)
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
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	waitReady(t, "http://"+srv.AdminAddr()+"/readyz")
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return srv, m, &tls.Config{RootCAs: pool, ServerName: "example.com"}
}

type observedHandler struct{ http.Handler }

func (observedHandler) HasHost(host string) bool { return host == "example.com" }

type faultWriter struct {
	deadlineWriter
	deadlineErr, flushErr error
	flushed               bool
}

func (w *faultWriter) SetWriteDeadline(d time.Time) error {
	w.deadlineWriter.SetWriteDeadline(d)
	return w.deadlineErr
}
func (w *faultWriter) FlushError() error { w.flushed = true; return w.flushErr }

func TestObserveAccountsAbortAndFlushFailure(t *testing.T) {
	for _, tt := range []struct {
		name, outcome         string
		deadlineErr, flushErr error
		panicValue            any
	}{
		{name: "header only", outcome: "complete"},
		{name: "flush fails", outcome: "write_error", flushErr: io.ErrClosedPipe, panicValue: http.ErrAbortHandler},
		{name: "deadline unsupported", outcome: "write_error", deadlineErr: http.ErrNotSupported, panicValue: http.ErrAbortHandler},
		{name: "upstream abort", outcome: "aborted", panicValue: http.ErrAbortHandler},
		{name: "unexpected panic", outcome: "panic", panicValue: "bug"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics.New()
			w := &faultWriter{deadlineWriter: deadlineWriter{header: http.Header{}}, deadlineErr: tt.deadlineErr, flushErr: tt.flushErr}
			sites := observedHandler{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotModified)
				if tt.name == "upstream abort" || tt.name == "unexpected panic" {
					panic(tt.panicValue)
				}
			})}
			var caught any
			func() {
				defer func() { caught = recover() }()
				observe(m.HTTP("http"), sites, nil, time.Second, sites).ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "http://example.com/", nil))
			}()
			if caught != tt.panicValue {
				t.Fatalf("panic=%v, want %v", caught, tt.panicValue)
			}
			var out bytes.Buffer
			m.WritePrometheus(&out)
			if !strings.Contains(out.String(), `outcome="`+tt.outcome+`"} 1`) || !strings.Contains(out.String(), `dumbwaiter_http_requests_in_flight{listener="http"} 0`) {
				t.Fatalf("incorrect accounting: %s", &out)
			}
			if tt.panicValue == nil && (!w.flushed || len(w.deadlines) < 2 || w.deadlines[len(w.deadlines)-1].IsZero()) {
				t.Fatal("header/final flush not protected by deadline")
			}
		})
	}
}

func TestRecorderDoesNotIgnoreWriteFailure(t *testing.T) {
	w := &faultWriter{deadlineWriter: deadlineWriter{header: http.Header{}}, deadlineErr: io.ErrClosedPipe}
	r := &recorder{ResponseWriter: w, writeIdle: time.Second}
	if _, err := r.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write: %v", err)
	}
	if err := r.finish(); !errors.Is(err, io.ErrClosedPipe) || w.flushed {
		t.Fatalf("finish after failed write: %v", err)
	}
}
