package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(m *Metrics) string {
	var b strings.Builder
	m.WritePrometheus(&b)
	return b.String()
}

func TestSeriesEscapesLabelValues(t *testing.T) {
	got := series("m", "a", "x\"y\\z\nw", "b", "")
	want := `m{a="x\"y\\z\nw",b=""}`
	if got != want {
		t.Errorf("series = %s, want %s", got, want)
	}

	// The library panics on a name it cannot parse.
	m := New()
	m.S3Request(`bad"bucket`, http.MethodGet, "200", time.Millisecond)
	if !strings.Contains(scrape(m), `bucket="bad\"bucket"`) {
		t.Error("escaped bucket missing from the output")
	}
}

func TestHTTPLabelsAreBounded(t *testing.T) {
	m := New()
	h := m.HTTP("http")
	r := httptest.NewRequestWithContext(context.Background(), "PROPFIND", "/", nil)
	h.Start()
	h.Done(r, "", http.StatusMethodNotAllowed, 19, time.Millisecond)

	out := scrape(m)
	want := `dumbwaiter_http_requests_total{listener="http",proto="HTTP/1",host="unknown",method="OTHER",code="405"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s in\n%s", want, out)
	}
	if !strings.Contains(out, `dumbwaiter_http_requests_in_flight{listener="http"} 0`) {
		t.Error("in-flight gauge did not return to 0")
	}
}

func TestCanceledS3RequestIsNotTimed(t *testing.T) {
	m := New()
	m.S3Request("site", http.MethodGet, S3Canceled, time.Second)

	out := scrape(m)
	if !strings.Contains(out, `dumbwaiter_s3_requests_total{bucket="site",method="GET",code="canceled"} 1`) {
		t.Errorf("canceled request not counted:\n%s", out)
	}
	if strings.Contains(out, "dumbwaiter_s3_request_duration_seconds") {
		t.Errorf("canceled request got into the histogram:\n%s", out)
	}
}
