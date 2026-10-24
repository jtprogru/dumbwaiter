package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
)

func TestObjectURL(t *testing.T) {
	tests := []struct {
		endpoint  string
		pathStyle bool
		key       string
		want      string
	}{
		{"https://storage.yandexcloud.net", true, "index.html", "https://storage.yandexcloud.net/site/index.html"},
		{"https://s3.eu-west-1.amazonaws.com", false, "docs/index.html", "https://site.s3.eu-west-1.amazonaws.com/docs/index.html"},
		{"http://minio.storage.svc:9000", true, "a b/ü+(1).html", "http://minio.storage.svc:9000/site/a%20b/%C3%BC%2B%281%29.html"},
		{"http://gateway/s3/", true, "x", "http://gateway/s3/site/x"},
	}
	for _, tt := range tests {
		c, err := New(config.S3{Endpoint: tt.endpoint, Region: "r", PathStyle: tt.pathStyle}, metrics.New())
		if err != nil {
			t.Fatal(err)
		}
		if got := c.objectURL("site", tt.key); got != tt.want {
			t.Errorf("objectURL(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestRequestMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/site/slow" {
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name     string
		endpoint string
		ctx      context.Context
		key      string
		code     string
	}{
		{"status", upstream.URL, t.Context(), "missing", "404"},
		{"timeout", upstream.URL, t.Context(), "slow", metrics.S3Timeout},
		{"canceled", upstream.URL, canceled, "missing", metrics.S3Canceled},
		{"refused", closed.URL, t.Context(), "missing", metrics.S3Error},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metrics.New()
			c, err := New(config.S3{Endpoint: tt.endpoint, PathStyle: true, Timeout: 50 * time.Millisecond}, m)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := c.Do(tt.ctx, http.MethodGet, "site", tt.key, nil); err == nil {
				resp.Body.Close()
			}

			var out strings.Builder
			m.WritePrometheus(&out)
			want := `dumbwaiter_s3_requests_total{bucket="site",method="GET",code="` + tt.code + `"} 1`
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %s in\n%s", want, out.String())
			}
		})
	}
}
