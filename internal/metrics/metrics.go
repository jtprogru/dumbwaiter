// Package metrics collects the proxy metrics and serves them in the
// Prometheus text format, which Prometheus, VictoriaMetrics and vmagent all
// scrape. Every metric name and label lives here.
package metrics

import (
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	vm "github.com/VictoriaMetrics/metrics"
)

// Values of the code label for S3 requests that got no response.
const (
	S3Error   = "error"
	S3Timeout = "timeout"
	// S3Canceled means the client went away before S3 answered.
	S3Canceled = "canceled"
)

// unknownHost is the host label of requests that matched no site. Labels
// carry configured hosts only, so a scanner cannot inflate the series count.
const unknownHost = "unknown"

type Metrics struct {
	set *vm.Set
	// Upper bounds of the latency histograms, in seconds. S3 answers in tens
	// of milliseconds, the tail is for large files sent to slow clients and
	// for the default 30s S3 timeout.
	buckets []float64
}

func New() *Metrics {
	return &Metrics{
		set:     vm.NewSet(),
		buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}
}

// Handler serves the proxy metrics together with the go_* and process_* ones.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.WritePrometheus(w)
		vm.WriteProcessMetrics(w)
	})
}

// WritePrometheus writes the proxy metrics only, without the process ones.
func (m *Metrics) WritePrometheus(w io.Writer) { m.set.WritePrometheus(w) }

// BuildInfo exposes dumbwaiter_build_info, always 1, with the version labels.
func (m *Metrics) BuildInfo(version string) {
	name := series("dumbwaiter_build_info", "version", version, "goversion", runtime.Version())
	m.set.GetOrCreateGauge(name, func() float64 { return 1 })
}

// S3Request records a request to S3. code is the response status or one of
// S3Error, S3Timeout and S3Canceled. A canceled request says nothing about
// S3 latency, so it is counted but kept out of the histogram.
func (m *Metrics) S3Request(bucket, method, code string, d time.Duration) {
	m.set.GetOrCreateCounter(series("dumbwaiter_s3_requests_total",
		"bucket", bucket, "method", method, "code", code)).Inc()
	if code != S3Canceled {
		m.set.GetOrCreatePrometheusHistogramExt(series("dumbwaiter_s3_request_duration_seconds",
			"bucket", bucket, "method", method), m.buckets).Update(d.Seconds())
	}
}

// HTTP records requests to one public listener: http, https or http3.
type HTTP struct {
	m        *Metrics
	listener string
	inFlight *vm.Gauge
}

func (m *Metrics) HTTP(listener string) *HTTP {
	return &HTTP{
		m:        m,
		listener: listener,
		inFlight: m.set.GetOrCreateGauge(series("dumbwaiter_http_requests_in_flight", "listener", listener), nil),
	}
}

// Start counts a request in flight until Done.
func (h *HTTP) Start() { h.inFlight.Inc() }

// Done records a finished request. host is the configured host the request
// was routed to, or "" when it matched no site. d covers the whole response
// including the body, so large files to slow clients make it long.
func (h *HTTP) Done(r *http.Request, host string, code int, bytes int64, d time.Duration) {
	h.inFlight.Dec()
	if host == "" {
		host = unknownHost
	}
	set := h.m.set
	set.GetOrCreateCounter(series("dumbwaiter_http_requests_total",
		"listener", h.listener, "proto", proto(r.ProtoMajor), "host", host,
		"method", method(r.Method), "code", strconv.Itoa(code))).Inc()
	set.GetOrCreatePrometheusHistogramExt(series("dumbwaiter_http_request_duration_seconds",
		"listener", h.listener, "host", host), h.m.buckets).Update(d.Seconds())
	set.GetOrCreateCounter(series("dumbwaiter_http_response_bytes_total",
		"listener", h.listener, "host", host)).AddInt64(bytes)
}

// TLS records reloads of the certificates the proxy serves.
type TLS struct {
	m                 *Metrics
	reloads, failures *vm.Counter
}

// TLS creates the reload counters, so they read 0 before the first reload
// instead of appearing out of nowhere.
func (m *Metrics) TLS() *TLS {
	return &TLS{
		m:        m,
		reloads:  m.set.GetOrCreateCounter("dumbwaiter_tls_certificate_reloads_total"),
		failures: m.set.GetOrCreateCounter("dumbwaiter_tls_certificate_reload_errors_total"),
	}
}

// Reloaded counts a reload that put new certificates in place.
func (t *TLS) Reloaded() { t.reloads.Inc() }

// Failed counts a reload that kept the old certificates because of an error.
func (t *TLS) Failed() { t.failures.Inc() }

// Certificate exposes the expiration time of the certificate in the file.
// notAfter is called on every scrape and must be safe for concurrent use.
func (t *TLS) Certificate(path string, notAfter func() time.Time) {
	t.m.set.GetOrCreateGauge(series("dumbwaiter_tls_certificate_expiration_timestamp_seconds", "cert", path),
		func() float64 { return float64(notAfter().Unix()) })
}

// method keeps the label bounded: anything but GET and HEAD gets 405 anyway.
func method(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead:
		return m
	}
	return "OTHER"
}

func proto(major int) string {
	switch major {
	case 2:
		return "HTTP/2"
	case 3:
		return "HTTP/3"
	}
	return "HTTP/1"
}

// series builds name{k="v",...} from label pairs, escaping the values the
// way the text format wants. Hosts, buckets and paths come from the config
// and may hold anything.
func series(name string, labels ...string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labels[i])
		b.WriteString(`="`)
		v := labels[i+1]
		for j := range len(v) {
			switch c := v[j]; c {
			case '\\', '"':
				b.WriteByte('\\')
				b.WriteByte(c)
			case '\n':
				b.WriteString(`\n`)
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
