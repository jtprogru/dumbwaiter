package server

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
)

// observe records metrics for every request and, unless log is nil, writes
// the access log line. writeIdle, when positive, is a per-write deadline for
// the client: a download that keeps progressing is never cut, a client that
// stopped reading is dropped.
func observe(stats *metrics.HTTP, sites Sites, log *slog.Logger, writeIdle time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		stats.Start()
		// The proxy never consumes request bodies. Avoid net/http draining an
		// unread body inside Write before it can send the response. H2/H3
		// independently close the request stream when the handler returns.
		if r.ProtoMajor == 1 && (r.ContentLength != 0 || len(r.TransferEncoding) > 0) {
			w.Header().Set("Connection", "close")
		}
		rec := &recorder{ResponseWriter: w, writeIdle: writeIdle}
		var host string
		if sites.HasHost(r.Host) {
			host = config.NormalizeHost(r.Host)
		}
		defer func() {
			p := recover()
			outcome := responseOutcome(r, rec, p)
			elapsed := time.Since(start)
			stats.Done(r, host, rec.status, rec.bytes, elapsed)
			stats.ResponseFinished(host, outcome)
			if log != nil {
				log.Info("request",
					"proto", r.Proto, "method", r.Method, "host", r.Host,
					"path", r.URL.Path, "status", rec.status, "bytes", rec.bytes,
					"outcome", outcome, "duration_ms", elapsed.Milliseconds(),
					"remote", r.RemoteAddr, "user_agent", r.UserAgent())
			}
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(rec, r)
		if r.Context().Err() != nil || rec.finish() != nil {
			panic(http.ErrAbortHandler)
		}
	})
}

func responseOutcome(r *http.Request, rec *recorder, p any) string {
	switch {
	case r.Context().Err() != nil:
		return "canceled"
	case rec.err != nil:
		return "write_error"
	case p == http.ErrAbortHandler: //nolint:errorlint // net/http recognizes only the exact panic sentinel.
		return "aborted"
	case p != nil:
		return "panic"
	default:
		return "complete"
	}
}

type recorder struct {
	http.ResponseWriter
	status    int
	bytes     int64
	writeIdle time.Duration
	err       error
	flushed   bool
}

func (r *recorder) WriteHeader(code int) {
	if r.refreshDeadline() != nil {
		panic(http.ErrAbortHandler)
	}
	if r.status == 0 {
		r.status = code
	}
	r.flushed = false
	r.ResponseWriter.WriteHeader(code)
	if r.clearDeadline() != nil {
		panic(http.ErrAbortHandler)
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if err := r.refreshDeadline(); err != nil {
		return 0, err
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.flushed = false
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		r.err = err
	}
	return n, r.clearDeadline()
}

// FlushError lets ResponseController flush inside the proxy's admission scope.
// Repeated flushes without another write do not repeat a blocking operation.
func (r *recorder) FlushError() error {
	if r.err != nil || r.flushed {
		return r.err
	}
	if err := r.refreshDeadline(); err != nil {
		return err
	}
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.err = http.NewResponseController(r.ResponseWriter).Flush()
	r.flushed = r.err == nil
	return r.clearDeadline()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) refreshDeadline() error {
	if r.err == nil && r.writeIdle > 0 {
		r.err = http.NewResponseController(r.ResponseWriter).SetWriteDeadline(time.Now().Add(r.writeIdle))
	}
	return r.err
}

// A completed downstream operation must not leave a timer running while the
// next upstream Read is pending. Preserve the first write/deadline error.
func (r *recorder) clearDeadline() error {
	if r.writeIdle > 0 {
		err := http.NewResponseController(r.ResponseWriter).SetWriteDeadline(time.Time{})
		if r.err == nil {
			r.err = err
		}
	}
	return r.err
}

// Flush buffered headers and body while still in ServeHTTP, so errors reach
// accounting. Keep the deadline through the server's final protocol flush;
// net/http resets it before reusing an HTTP/1 connection, H2/H3 use streams.
func (r *recorder) finish() error {
	if err := r.FlushError(); err != nil {
		return err
	}
	// The proxy already flushed admitted responses while holding its slots.
	// Only arm the deadline for the server's final protocol framing here.
	return r.refreshDeadline()
}
