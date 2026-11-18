package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
	"github.com/jtprogru/dumbwaiter/internal/proxy"
	"github.com/jtprogru/dumbwaiter/internal/server"
)

// Exercise the logger selected by the binary through actual HTTP requests,
// including both S3 error paths reported by CodeQL go/log-injection.
func TestRequestValuesCannotForgeLogRecords(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/site/broken") {
					w.Header().Set("Content-Length", "6")
					io.WriteString(w, "hello")
					w.(http.Flusher).Flush()
					panic(http.ErrAbortHandler)
				}
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "<Error><Code>InternalError</Code></Error>")
			}))
			t.Cleanup(upstream.Close)
			cfg, err := config.Parse([]byte(`
server:
  http: {addr: "127.0.0.1:0"}
  admin: {addr: "127.0.0.1:0"}
  shutdown_delay: 1ms
s3: {endpoint: "` + upstream.URL + `", path_style: true}
sites: [{hosts: [example.com], bucket: site}]
log: {format: ` + format + `, access: true}
`))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			log := newLogger(cfg.Log, &output)
			m := metrics.New()
			sites, err := proxy.New(cfg, m, log)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			srv, err := server.New(ctx, cfg, sites, m, log)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- srv.Run(ctx) }()
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
			// CR/LF are accepted in an escaped URL path. HTTP rejects them in
			// header values; tab, quotes and Unicode separators remain valid.
			payload := "\r\nlevel=ERROR msg=forged\t\x1b[31m\"\\\u2028\u2029"
			userAgent := "agent\tlevel=ERROR msg=\"forged\"\\\u2028\u2029"
			for _, name := range []string{"broken", "unexpected"} {
				u := url.URL{Scheme: "http", Host: srv.HTTPAddr(), Path: "/" + name + payload}
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u.String(), nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "example.com"
				req.Header.Set("User-Agent", userAgent)
				resp, err := client.Do(req)
				if name == "unexpected" && (err != nil || resp.StatusCode != http.StatusBadGateway) {
					t.Fatalf("expected unexpected S3 response, got resp=%v err=%v", resp, err)
				}
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
			// Shutdown waits for handlers and logging before reading the buffer.
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server did not stop")
			}
			counts := map[string]int{}
			for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
				if strings.ContainsAny(line, "\r\t\x00\x1b\u2028\u2029") {
					t.Fatalf("unescaped control character in log: %q", line)
				}
				var fields map[string]any
				if format == "json" {
					if err := json.Unmarshal([]byte(line), &fields); err != nil {
						t.Fatalf("invalid JSON record: %v: %q", err, line)
					}
				}
				checkField := func(key, value string) {
					t.Helper()
					if format == "json" {
						if fields[key] != value {
							t.Errorf("%s=%q, want %q", key, fields[key], value)
						}
					} else if !strings.Contains(line, key+"="+strconv.Quote(value)) {
						t.Errorf("missing quoted %s=%q in %q", key, value, line)
					}
				}
				for msg, name := range map[string]string{"copy response body": "broken", "unexpected s3 response": "unexpected", "request": ""} {
					textMsg := strconv.Quote(msg)
					if msg == "request" {
						textMsg = msg
					}
					if (format == "json" && fields["msg"] == msg) || (format == "text" && strings.Contains(line, " msg="+textMsg+" ")) {
						counts[msg]++
						if msg == "request" {
							checkField("user_agent", userAgent)
							name = "unexpected"
							if strings.Contains(line, "broken") {
								name = "broken"
							}
							checkField("path", "/"+name+payload)
						} else {
							checkField("key", name+payload)
						}
					}
				}
			}
			if counts["request"] != 2 || counts["copy response body"] != 1 || counts["unexpected s3 response"] != 1 {
				t.Fatalf("expected exactly two access and two error records, got %v\n%s", counts, &output)
			}
		})
	}
}
