package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
)

// observe records metrics for every request and, unless log is nil, writes
// the access log line.
func observe(stats *metrics.HTTP, sites Sites, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		stats.Start()
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		elapsed := time.Since(start)

		var host string
		if sites.HasHost(r.Host) {
			host = config.NormalizeHost(r.Host)
		}
		stats.Done(r, host, rec.status, rec.bytes, elapsed)

		if log == nil {
			return
		}
		log.Info("request",
			"proto", r.Proto,
			"method", r.Method,
			"host", r.Host,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", elapsed.Milliseconds(),
			"remote", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		)
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
