package loadtest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the actual k6 executor: interrupted iterations never reach the JS
// checks, so testing the request function alone cannot detect a false success.
func TestLoadCompletion(t *testing.T) {
	k6, err := exec.LookPath("k6")
	if err != nil {
		if os.Getenv("REQUIRE_LOAD_TOOLS") == "1" {
			t.Fatal("k6 is required")
		}
		t.Skip("k6 not installed; use make check-loadtest to require it")
	}
	script, err := filepath.Abs("../../scripts/loadtest.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		grace    string
		firstOK  bool
		wantFail bool
	}{
		{name: "default grace follows request timeout"},
		{name: "no completed iterations", grace: "200ms", wantFail: true},
		{name: "partial completion", grace: "200ms", firstOK: true, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first := calls.Add(1) == 1
				w.Header().Set("Content-Length", "5")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				if !tc.firstOK || !first {
					select {
					case <-time.After(1500 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				io.WriteString(w, "hello")
			}))
			t.Cleanup(upstream.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			summaryPath := filepath.Join(t.TempDir(), "summary.json")
			cmd := exec.CommandContext(ctx, k6, "run", "--quiet", "--no-color", "--no-usage-report", "--summary-export", summaryPath, script)
			cmd.Env = append(os.Environ(),
				"BASE_URL="+upstream.URL, "RATE=2", "DURATION=1s", "REQUEST_TIMEOUT=4s",
				"GRACEFUL_STOP="+tc.grace, "PREALLOCATED_VUS=4", "MAX_VUS=4",
				"P95_MS=5000", "P99_MS=5000", "EXPECTED_STATUS=200", "EXPECTED_PROTOCOL=HTTP/1.1",
				"METHOD=GET", "OBJECT_PATH=/", "HOST_HEADER=", "RANGE=", "IF_NONE_MATCH=",
				"K6_WEB_DASHBOARD=false",
			)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("k6 did not finish: %v\n%s", ctx.Err(), out)
			}
			if !tc.wantFail {
				if err != nil {
					t.Fatalf("completed load failed: %v\n%s", err, out)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 99 {
					t.Fatalf("incomplete load must fail thresholds: err=%v\n%s", err, out)
				}
			}
			raw, err := os.ReadFile(summaryPath)
			if err != nil {
				t.Fatal(err)
			}
			var summary struct {
				Metrics map[string]struct {
					Count float64 `json:"count"`
				} `json:"metrics"`
			}
			if err := json.Unmarshal(raw, &summary); err != nil {
				t.Fatal(err)
			}
			completed := summary.Metrics["iterations"].Count
			unfinished := summary.Metrics["unfinished_iterations"].Count
			if tc.wantFail {
				wantCompleted := float64(0)
				if tc.firstOK {
					wantCompleted = 1
				}
				if completed != wantCompleted || unfinished < 1 {
					t.Fatalf("completed=%v unfinished=%v\n%s", completed, unfinished, out)
				}
			} else if completed < 1 || unfinished != 0 {
				t.Fatalf("completed=%v unfinished=%v\n%s", completed, unfinished, out)
			}
		})
	}
}
