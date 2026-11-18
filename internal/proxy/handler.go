// Package proxy serves static sites from S3 buckets, routed by Host.
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
	"github.com/jtprogru/dumbwaiter/internal/s3"
)

// Request headers forwarded to S3. S3 handles ranges and conditional
// requests itself, so 206 and 304 come back ready to pass through.
var forwardHeaders = []string{
	"Range",
	"If-Range",
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
}

// Response headers copied from S3. Everything else, x-amz-* included, is dropped.
var passHeaders = []string{
	"Accept-Ranges",
	"Cache-Control",
	"Content-Disposition",
	"Content-Encoding",
	"Content-Language",
	"Content-Length",
	"Content-Range",
	"Content-Type",
	"Etag",
	"Expires",
	"Last-Modified",
}

// Validators make no sense on an error page served in place of another URL.
var errorPageSkip = map[string]bool{"Etag": true, "Last-Modified": true, "Accept-Ranges": true}

type objectStore interface {
	Do(ctx context.Context, method, bucket, key string, hdr http.Header) (*http.Response, error)
}

type Handler struct {
	sites map[string]*site
	slots chan struct{}
}

type site struct {
	cfg     config.Site
	store   objectStore
	headers http.Header
	metrics *metrics.Metrics
	log     *slog.Logger
	slots   chan struct{}
}

func New(cfg *config.Config, m *metrics.Metrics, log *slog.Logger) (*Handler, error) {
	clients := map[config.S3]*s3.Client{}
	h := &Handler{sites: map[string]*site{}, slots: make(chan struct{}, cfg.Server.MaxInFlight)}

	for _, sc := range cfg.Sites {
		storage := sc.Storage(cfg.S3)
		client, ok := clients[storage]
		if !ok {
			var err error
			if client, err = s3.New(storage, m); err != nil {
				return nil, err
			}
			clients[storage] = client
		}
		s := newSite(sc, client, m, log)
		for _, host := range sc.Hosts {
			h.sites[host] = s
		}
	}
	return h, nil
}

func newSite(cfg config.Site, store objectStore, m *metrics.Metrics, log *slog.Logger) *site {
	hdr := http.Header{}
	for k, v := range cfg.Headers {
		hdr.Set(k, v)
	}
	return &site{
		cfg:     cfg,
		store:   store,
		headers: hdr,
		metrics: m,
		log:     log.With("bucket", cfg.Bucket),
		slots:   make(chan struct{}, cfg.MaxInFlight),
	}
}

// HasHost reports whether the host (with or without port) is configured.
func (h *Handler) HasHost(host string) bool {
	_, ok := h.sites[config.NormalizeHost(host)]
	return ok
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s, ok := h.sites[config.NormalizeHost(r.Host)]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Reserve the site's slot first: a saturated site must not occupy global
	// slots even briefly. Both limits remain held until streaming finishes.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.reject(w, r, "site")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		s.reject(w, r, "global")
		return
	}
	s.serve(w, r)
}

func (s *site) reject(w http.ResponseWriter, r *http.Request, scope string) {
	s.metrics.AdmissionRejected(config.NormalizeHost(r.Host), scope)
	w.Header().Set("Retry-After", "1")
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
}

func (s *site) serve(w http.ResponseWriter, r *http.Request) {
	r, budget := newLookup(r, s.cfg.LookupTimeout)
	defer budget.close()
	for k := range s.headers {
		w.Header().Set(k, s.headers.Get(k))
	}
	// Content types come from object metadata or an extension guess, so the
	// browser must not sniff a different one. A configured header wins.
	if _, fromConfig := s.headers["X-Content-Type-Options"]; !fromConfig {
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}

	// path.Clean resolves "..", so a request cannot leave the site prefix.
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	isDir := rel == "" || strings.HasSuffix(r.URL.Path, "/")

	key := s.cfg.Prefix + rel
	if isDir {
		key = s.cfg.Prefix + dirPrefix(rel) + s.cfg.Index
	}
	if s.tryServe(w, r, budget, key, http.StatusOK) {
		return
	}

	// /docs without a slash, but docs/index.html exists: redirect the way
	// nginx and S3 website hosting do, so relative links keep working.
	if !isDir && s.cfg.DirectoryRedirectEnabled() {
		resp, ok := s.fetch(w, r, http.MethodHead, s.cfg.Prefix+rel+"/"+s.cfg.Index, nil)
		if !ok {
			return
		}
		if resp != nil {
			resp.Body.Close()
			budget.finish()
			if s.lookupFailed(w, r) {
				return
			}
			loc := (&url.URL{Path: "/" + rel + "/", RawQuery: r.URL.RawQuery}).String()
			http.Redirect(w, r, loc, http.StatusMovedPermanently)
			return
		}
	}

	// SPA routes look like /users/42, missing assets like /app.js.
	// Only the former falls back to the root index.
	if s.cfg.SPA && path.Ext(rel) == "" && key != s.cfg.Prefix+s.cfg.Index && s.tryServe(w, r, budget, s.cfg.Prefix+s.cfg.Index, http.StatusOK) {
		return
	}

	if s.cfg.NotFound != "" && key != s.cfg.Prefix+s.cfg.NotFound && s.tryServe(w, r, budget, s.cfg.Prefix+s.cfg.NotFound, http.StatusNotFound) {
		return
	}

	budget.finish()
	if !s.lookupFailed(w, r) {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// tryServe writes the object to the client and reports true, or reports
// false when the object does not exist and nothing was written yet.
// status is the code to send for a found object: 200 for regular content,
// 404 for the error page.
func (s *site) tryServe(w http.ResponseWriter, r *http.Request, budget *lookup, key string, status int) bool {
	errorPage := status != http.StatusOK

	var hdr http.Header
	if !errorPage {
		hdr = forwarded(r)
	}
	resp, ok := s.fetch(w, r, r.Method, key, hdr)
	if !ok {
		return true
	}
	if resp == nil {
		return false
	}
	defer resp.Body.Close()
	budget.finish()
	if s.lookupFailed(w, r) {
		return true
	}

	switch resp.StatusCode {
	case http.StatusPreconditionFailed, http.StatusRequestedRangeNotSatisfiable:
		// S3 puts an XML error into the body, the client does not need it.
		http.Error(w, http.StatusText(resp.StatusCode), resp.StatusCode)
		return true
	case http.StatusPartialContent, http.StatusNotModified:
		if !errorPage {
			status = resp.StatusCode
		}
	}

	h := w.Header()
	for _, k := range passHeaders {
		if _, fromConfig := s.headers[k]; fromConfig || (errorPage && errorPageSkip[k]) {
			continue
		}
		if v := resp.Header.Values(k); len(v) > 0 {
			h[k] = v
		}
	}
	if _, fromConfig := s.headers["Content-Type"]; !fromConfig {
		fixContentType(h, key)
	}

	w.WriteHeader(status)
	if r.Method == http.MethodHead || status == http.StatusNotModified {
		return true
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		if r.Context().Err() == nil {
			s.log.Warn("copy response body", "key", key, "err", err)
		}
		// Headers are committed. Terminate the connection / reset the stream
		// so a truncated chunked body cannot look like a complete response.
		panic(http.ErrAbortHandler)
	}
	return true
}

// fetch asks S3 for the object. It returns (resp, true) when the object
// exists, (nil, true) when it does not, and (nil, false) when the request
// failed and an error response has already been written.
func (s *site) fetch(w http.ResponseWriter, r *http.Request, method, key string, hdr http.Header) (*http.Response, bool) {
	if s.lookupFailed(w, r) {
		return nil, false
	}
	resp, err := s.store.Do(r.Context(), method, s.cfg.Bucket, key, hdr)
	if err != nil {
		if s.lookupFailed(w, r) {
			return nil, false
		}
		s.log.Error("s3 request failed", "key", key, "err", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return nil, false
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified,
		http.StatusPreconditionFailed, http.StatusRequestedRangeNotSatisfiable:
		return resp, true
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, true
	case http.StatusForbidden:
		// Without s3:ListBucket a missing key yields AccessDenied, not
		// NoSuchKey. Other 403 codes mean broken credentials or clock skew.
		// HEAD responses carry no body, so the code is empty there.
		code := s3.ErrorCode(resp)
		if s.lookupFailed(w, r) {
			return nil, false
		}
		if code == "AccessDenied" || code == "" {
			// Served as 404 so the bucket contents do not leak, but this is
			// still a permission problem worth an alert, not a missing key.
			s.metrics.S3AccessDeniedMasked(s.cfg.Bucket)
			s.log.Warn("s3 denied access, serving not found", "key", key, "code", code)
			return nil, true
		}
		s.log.Error("s3 rejected request", "key", key, "status", resp.StatusCode, "code", code)
	default:
		code := s3.ErrorCode(resp)
		if s.lookupFailed(w, r) {
			return nil, false
		}
		s.log.Error("unexpected s3 response", "key", key, "status", resp.StatusCode, "code", code)
	}
	http.Error(w, "bad gateway", http.StatusBadGateway)
	return nil, false
}

func (s *site) lookupFailed(w http.ResponseWriter, r *http.Request) bool {
	cause := context.Cause(r.Context())
	if cause == nil {
		return false
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		s.metrics.LookupTimeout(config.NormalizeHost(r.Host))
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
	}
	// A canceled client no longer needs a response.
	return true
}

// fixContentType replaces the generic type that S3 assigns to objects
// uploaded without one, guessing by the extension of the key.
func fixContentType(h http.Header, key string) {
	switch h.Get("Content-Type") {
	case "", "binary/octet-stream", "application/octet-stream":
		if t := mime.TypeByExtension(path.Ext(key)); t != "" {
			h.Set("Content-Type", t)
		}
	}
}

func forwarded(r *http.Request) http.Header {
	out := http.Header{}
	for _, k := range forwardHeaders {
		if v := r.Header.Values(k); len(v) > 0 {
			out[k] = v
		}
	}
	return out
}

func dirPrefix(rel string) string {
	if rel == "" {
		return ""
	}
	return rel + "/"
}
