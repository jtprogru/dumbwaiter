package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/jtprogru/dumbwaiter/internal/metrics"
	"github.com/jtprogru/dumbwaiter/internal/proxy"
)

func TestUpstreamPauseDoesNotExpireWriteDeadline(t *testing.T) {
	for _, protocol := range []string{"http1", "http2", "http3"} {
		for _, firstChunk := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/first-chunk=%t", protocol, firstChunk), func(t *testing.T) {
				prefix := ""
				if firstChunk {
					prefix = strings.Repeat("x", 32<<10)
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Length", strconv.Itoa(len(prefix)+5))
					w.WriteHeader(http.StatusOK)
					io.WriteString(w, prefix)
					w.(http.Flusher).Flush()
					select {
					case <-time.After(300 * time.Millisecond):
						io.WriteString(w, "hello")
					case <-r.Context().Done():
					}
				}))
				t.Cleanup(upstream.Close)
				// write idle is 100ms; the S3 read budget is 30s.
				srv, _, tlsConfig := transferServer(t, upstream.URL)
				client := &http.Client{Timeout: 5 * time.Second}
				url, major := "http://"+srv.HTTPAddr()+"/file", 1
				if protocol == "http3" {
					tr := &http3.Transport{TLSClientConfig: tlsConfig}
					t.Cleanup(func() { tr.Close() })
					client.Transport = tr
					url, major = "https://"+srv.HTTP3Addr()+"/file", 3
				} else {
					tr := &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: protocol == "http2"}
					t.Cleanup(tr.CloseIdleConnections)
					client.Transport = tr
					if protocol == "http2" {
						url, major = "https://"+srv.HTTPSAddr()+"/file", 2
					}
				}
				res := do(t, client, http.MethodGet, url)
				if res.status != http.StatusOK || res.protoMajor != major || res.body != prefix+"hello" {
					t.Fatalf("incomplete response: HTTP/%d status=%d bytes=%d", res.protoMajor, res.status, len(res.body))
				}
			})
		}
	}
}

func TestAdmissionCoversHTTP2FinalFlush(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprintf("complete=%t", complete), func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				io.WriteString(w, "x")
			}))
			t.Cleanup(upstream.Close)
			cfg := reviewConfig(t, upstream.URL)
			cfg.Server.HTTP.H2C = true
			cfg.Server.MaxInFlight, cfg.Sites[0].MaxInFlight = 1, 1
			cfg.Server.WriteIdleTimeout = 5 * time.Second
			cfg.Server.ReadTimeout = 5 * time.Second
			cfg.Server.IdleTimeout = 5 * time.Second
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
			conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.HTTPAddr())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			io.WriteString(conn, http2.ClientPreface)
			fr := http2.NewFramer(conn, conn)
			fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
			if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0}); err != nil {
				t.Fatal(err)
			}
			for i := range 5 {
				id := uint32(1 + 2*i)
				sendBlockedRequest(t, fr, id)
				want := "503"
				if i == 0 {
					want = "200"
				}
				if got := readHTTP2Status(t, fr, id); got != want {
					t.Fatalf("stream %d: status=%s, want %s", id, got, want)
				}
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("max_in_flight=1 admitted %d blocked responses to S3", got)
			}
			if complete {
				// Let only the admitted stream drain; rejected streams remain
				// blocked, but must not occupy admission slots.
				fr.WriteWindowUpdate(1, 1)
				var body bytes.Buffer
				for {
					frame, err := fr.ReadFrame()
					if err != nil {
						t.Fatal(err)
					}
					if data, ok := frame.(*http2.DataFrame); ok && data.StreamID == 1 {
						body.Write(data.Data())
						if data.StreamEnded() {
							break
						}
					}
				}
				if body.String() != "x" {
					t.Fatalf("body=%q", &body)
				}
			} else {
				conn.Close()
				waitNoPublicRequests(t, m)
			}
			client := &http.Client{Timeout: time.Second}
			if res := do(t, client, http.MethodGet, "http://"+srv.HTTPAddr()+"/next"); res.status != 200 || res.body != "x" {
				t.Fatalf("admission slot not released: status=%d body=%q", res.status, res.body)
			}
		})
	}
}

func sendBlockedRequest(t *testing.T, fr *http2.Framer, id uint32) {
	t.Helper()
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":authority", Value: "example.com"}, {Name: ":path", Value: "/file"},
	} {
		enc.WriteField(field)
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, EndStream: true, EndHeaders: true, BlockFragment: block.Bytes()}); err != nil {
		t.Fatal(err)
	}
}

func readHTTP2Status(t *testing.T, fr *http2.Framer, id uint32) string {
	t.Helper()
	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				fr.WriteSettingsAck()
			}
		case *http2.MetaHeadersFrame:
			if f.StreamID == id {
				return f.PseudoValue("status")
			}
		case *http2.RSTStreamFrame:
			t.Fatalf("unexpected reset on stream %d: %s", f.StreamID, f.ErrCode)
		}
	}
}

func waitNoPublicRequests(t *testing.T, m *metrics.Metrics) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var out bytes.Buffer
		m.WritePrometheus(&out)
		if strings.Contains(out.String(), `dumbwaiter_http_requests_in_flight{listener="http"} 0`) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("requests did not release after cancellation")
}
