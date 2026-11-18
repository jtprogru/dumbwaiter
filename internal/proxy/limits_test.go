package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
)

type storeFunc func(context.Context, string, string, string, http.Header) (*http.Response, error)

func (f storeFunc) Do(ctx context.Context, method, bucket, key string, hdr http.Header) (*http.Response, error) {
	return f(ctx, method, bucket, key, hdr)
}

func testResponse(status int, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: body}
}

func limitedHandler(store objectStore, m *metrics.Metrics, global, perSite int) *Handler {
	h := &Handler{sites: map[string]*site{}, slots: make(chan struct{}, global)}
	for _, hosts := range [][]string{{"a.test", "alias.test"}, {"b.test"}} {
		s := newSite(config.Site{Hosts: hosts, Bucket: "site", Index: "index.html", MaxInFlight: perSite, LookupTimeout: time.Minute}, store, m, slog.New(slog.DiscardHandler))
		for _, host := range hosts {
			h.sites[host] = s
		}
	}
	return h
}

type waitingBody struct {
	ctx     context.Context
	started chan<- struct{}
	release <-chan struct{}
}

func (b *waitingBody) Read([]byte) (int, error) {
	b.started <- struct{}{}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.release:
		return 0, io.EOF
	}
}

func (*waitingBody) Close() error { return nil }

func TestAdmissionLimits(t *testing.T) {
	for _, tt := range []struct {
		name, scope     string
		global, perSite int
		otherStatus     int
		cancelClient    bool
	}{
		{name: "site aliases share a limit", scope: "site", global: 2, perSite: 1, otherStatus: 200},
		{name: "global limit spans sites", scope: "global", global: 1, perSite: 2, otherStatus: 503},
		{name: "cancel releases slots", scope: "site", global: 2, perSite: 1, otherStatus: 200, cancelClient: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			started, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int64
			store := storeFunc(func(ctx context.Context, _, _, key string, _ http.Header) (*http.Response, error) {
				calls.Add(1)
				if key == "blocked" {
					return testResponse(200, &waitingBody{ctx: ctx, started: started, release: release}), nil
				}
				return testResponse(200, io.NopCloser(strings.NewReader("ok"))), nil
			})
			m := metrics.New()
			h := limitedHandler(store, m, tt.global, tt.perSite)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if p := recover(); p != nil && (!tt.cancelClient || p != http.ErrAbortHandler) { //nolint:errorlint // net/http requires the exact panic sentinel.
						t.Errorf("unexpected panic: %v", p)
					}
				}()
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "GET", "http://a.test/blocked", nil))
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("stream did not start")
			}
			rec := do(h, "GET", "http://alias.test/rejected", nil)
			if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || rec.Header().Get("Cache-Control") != "no-store" || calls.Load() != 1 {
				t.Fatalf("rejected request reached S3 or wrong response: status=%d headers=%v calls=%d", rec.Code, rec.Header(), calls.Load())
			}
			if got := do(h, "GET", "http://unknown.test/", nil).Code; got != 404 {
				t.Fatalf("unknown host during overload: %d", got)
			}
			if got := do(h, "POST", "http://a.test/", nil).Code; got != 405 {
				t.Fatalf("method restriction during overload: %d", got)
			}
			if calls.Load() != 1 {
				t.Fatal("unknown host or rejected method reached S3")
			}
			if got := do(h, "GET", "http://b.test/", nil).Code; got != tt.otherStatus {
				t.Fatalf("other site: %d, want %d", got, tt.otherStatus)
			}
			var out bytes.Buffer
			m.WritePrometheus(&out)
			if !strings.Contains(out.String(), `dumbwaiter_admission_rejected_total{host="alias.test",scope="`+tt.scope+`"} 1`) {
				t.Fatalf("missing rejection metric: %s", &out)
			}
			if tt.cancelClient {
				cancel()
			} else {
				unblock()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stream did not stop")
			}
			if got := do(h, "GET", "http://alias.test/", nil).Code; got != 200 {
				t.Fatalf("slot leaked after stream finished: %d", got)
			}
		})
	}
}

func TestConcurrentAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const clients, limit = 64, 4
		started, release := make(chan struct{}, clients), make(chan struct{})
		defer close(release)
		var calls atomic.Int64
		store := storeFunc(func(ctx context.Context, _, _, _ string, _ http.Header) (*http.Response, error) {
			calls.Add(1)
			return testResponse(200, &waitingBody{ctx: ctx, started: started, release: release}), nil
		})
		h := limitedHandler(store, metrics.New(), limit, limit)
		results := make(chan int, clients)
		for range clients {
			go func() { results <- do(h, "GET", "http://a.test/file", nil).Code }()
		}
		synctest.Wait()
		if calls.Load() != limit || len(started) != limit || len(results) != clients-limit {
			t.Fatalf("concurrent admission: calls=%d active=%d rejected=%d", calls.Load(), len(started), len(results))
		}
		for range clients - limit {
			if status := <-results; status != 503 {
				t.Fatalf("expected rejection while streams occupy slots, got %d", status)
			}
		}
	})
}

func TestLookupBudgetAcrossFallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		store := storeFunc(func(ctx context.Context, _, _, _ string, _ http.Header) (*http.Response, error) {
			calls++
			select {
			case <-time.After(200 * time.Millisecond):
				return testResponse(404, http.NoBody), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		m := metrics.New()
		h := limitedHandler(store, m, 2, 1)
		h.sites["a.test"].cfg.LookupTimeout = 300 * time.Millisecond
		h.sites["a.test"].cfg.NotFound = "404.html"
		start := time.Now()
		rec := do(h, "GET", "http://a.test/missing", nil)
		if rec.Code != 504 || calls != 2 || time.Since(start) != 300*time.Millisecond {
			t.Fatalf("lookup budget not shared: status=%d calls=%d elapsed=%s", rec.Code, calls, time.Since(start))
		}
		var out bytes.Buffer
		m.WritePrometheus(&out)
		if !strings.Contains(out.String(), `dumbwaiter_lookup_timeouts_total{host="a.test"} 1`) {
			t.Fatalf("missing timeout metric: %s", &out)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("timeout must not be cached")
		}
	})
}

type delayedBody struct {
	ctx  context.Context
	done bool
}

func (b *delayedBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	b.done = true
	time.Sleep(time.Second)
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return copy(p, "complete"), nil
}

func (*delayedBody) Close() error { return nil }

func TestLookupBudgetDoesNotLimitStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := storeFunc(func(ctx context.Context, _, _, _ string, _ http.Header) (*http.Response, error) {
			return testResponse(200, &delayedBody{ctx: ctx}), nil
		})
		h := limitedHandler(store, metrics.New(), 2, 1)
		h.sites["a.test"].cfg.LookupTimeout = 100 * time.Millisecond
		rec := do(h, "GET", "http://a.test/file", nil)
		if rec.Code != 200 || rec.Body.String() != "complete" {
			t.Fatalf("lookup clock interrupted body: %d %q", rec.Code, rec.Body.String())
		}
	})
}

func TestLateLookupCannotStartResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := storeFunc(func(context.Context, string, string, string, http.Header) (*http.Response, error) {
			// Even if the store returns headers concurrently with cancellation,
			// the handler must not commit a successful response after expiry.
			time.Sleep(time.Second)
			return testResponse(200, io.NopCloser(strings.NewReader("late"))), nil
		})
		h := limitedHandler(store, metrics.New(), 2, 1)
		h.sites["a.test"].cfg.LookupTimeout = 100 * time.Millisecond
		if rec := do(h, "GET", "http://a.test/file", nil); rec.Code != 504 {
			t.Fatalf("expired lookup produced %d %q", rec.Code, rec.Body.String())
		}
	})
}

func TestCanceledLookupDoesNotContactS3(t *testing.T) {
	calls := 0
	store := storeFunc(func(context.Context, string, string, string, http.Header) (*http.Response, error) {
		calls++
		return testResponse(200, http.NoBody), nil
	})
	m := metrics.New()
	h := limitedHandler(store, m, 2, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", "http://a.test/file", nil))
	if calls != 0 || rec.Body.Len() != 0 {
		t.Fatalf("canceled lookup did work: calls=%d body=%q", calls, rec.Body.String())
	}
	var out bytes.Buffer
	m.WritePrometheus(&out)
	if strings.Contains(out.String(), "lookup_timeouts") {
		t.Fatal("client cancel counted as lookup timeout")
	}
}

func TestDirectoryRedirectControlsS3Calls(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, tt := range []struct {
			path, body string
			status     int
			calls      int
		}{
			{path: "/missing.js", body: "not found", status: 404, calls: 2},
			{path: "/route", body: "home", status: 200, calls: 2},
			{path: "/docs/", body: "docs", status: 200, calls: 1},
		} {
			t.Run(tt.path+"/redirect="+map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
				fake := &fakeS3{objects: map[string]object{
					"/site/index.html": {body: "home"}, "/site/docs/index.html": {body: "docs"}, "/site/404.html": {body: "not found"},
				}}
				h := newTestHandler(t, fake, config.Site{SPA: true, NotFound: "404.html", DirectoryRedirect: &enabled})
				rec := do(h, "GET", "http://example.com"+tt.path, nil)
				wantCalls := tt.calls
				if enabled && !strings.HasSuffix(tt.path, "/") {
					wantCalls++
				}
				if rec.Code != tt.status || rec.Body.String() != tt.body || len(fake.requested()) != wantCalls {
					t.Fatalf("status=%d body=%q calls=%v, want %d %q %d", rec.Code, rec.Body.String(), fake.requested(), tt.status, tt.body, wantCalls)
				}
			})
		}
	}
}
