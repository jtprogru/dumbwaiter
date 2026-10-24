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
}

type site struct {
	cfg     config.Site
	store   objectStore
	headers http.Header
	log     *slog.Logger
}

func New(cfg *config.Config, log *slog.Logger) (*Handler, error) {
	clients := map[config.S3]*s3.Client{}
	h := &Handler{sites: map[string]*site{}}

	for _, sc := range cfg.Sites {
		storage := sc.Storage(cfg.S3)
		client, ok := clients[storage]
		if !ok {
			var err error
			if client, err = s3.New(storage); err != nil {
				return nil, err
			}
			clients[storage] = client
		}
		s := newSite(sc, client, log)
		for _, host := range sc.Hosts {
			h.sites[host] = s
		}
	}
	return h, nil
}

func newSite(cfg config.Site, store objectStore, log *slog.Logger) *site {
	hdr := http.Header{}
	for k, v := range cfg.Headers {
		hdr.Set(k, v)
	}
	return &site{
		cfg:     cfg,
		store:   store,
		headers: hdr,
		log:     log.With("bucket", cfg.Bucket),
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
	s.serve(w, r)
}

func (s *site) serve(w http.ResponseWriter, r *http.Request) {
	for k := range s.headers {
		w.Header().Set(k, s.headers.Get(k))
	}

	// path.Clean resolves "..", so a request cannot leave the site prefix.
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	isDir := rel == "" || strings.HasSuffix(r.URL.Path, "/")

	key := s.cfg.Prefix + rel
	if isDir {
		key = s.cfg.Prefix + dirPrefix(rel) + s.cfg.Index
	}
	if s.tryServe(w, r, key, http.StatusOK) {
		return
	}

	// /docs without a slash, but docs/index.html exists: redirect the way
	// nginx and S3 website hosting do, so relative links keep working.
	if !isDir {
		resp, ok := s.fetch(w, r, http.MethodHead, s.cfg.Prefix+rel+"/"+s.cfg.Index, nil)
		if !ok {
			return
		}
		if resp != nil {
			resp.Body.Close()
			loc := (&url.URL{Path: "/" + rel + "/", RawQuery: r.URL.RawQuery}).String()
			http.Redirect(w, r, loc, http.StatusMovedPermanently)
			return
		}
	}

	// SPA routes look like /users/42, missing assets like /app.js.
	// Only the former falls back to the root index.
	if s.cfg.SPA && path.Ext(rel) == "" && s.tryServe(w, r, s.cfg.Prefix+s.cfg.Index, http.StatusOK) {
		return
	}

	if s.cfg.NotFound != "" && s.tryServe(w, r, s.cfg.Prefix+s.cfg.NotFound, http.StatusNotFound) {
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

// tryServe writes the object to the client and reports true, or reports
// false when the object does not exist and nothing was written yet.
// status is the code to send for a found object: 200 for regular content,
// 404 for the error page.
func (s *site) tryServe(w http.ResponseWriter, r *http.Request, key string, status int) bool {
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
	if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
		s.log.Warn("copy response body", "key", key, "err", err)
	}
	return true
}

// fetch asks S3 for the object. It returns (resp, true) when the object
// exists, (nil, true) when it does not, and (nil, false) when the request
// failed and an error response has already been written.
func (s *site) fetch(w http.ResponseWriter, r *http.Request, method, key string, hdr http.Header) (*http.Response, bool) {
	resp, err := s.store.Do(r.Context(), method, s.cfg.Bucket, key, hdr)
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			// The client went away, nobody is left to answer.
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
		if code == "AccessDenied" || code == "" {
			s.log.Debug("s3 denied access, treating as not found", "key", key, "code", code)
			return nil, true
		}
		s.log.Error("s3 rejected request", "key", key, "status", resp.StatusCode, "code", code)
	default:
		s.log.Error("unexpected s3 response", "key", key, "status", resp.StatusCode, "code", s3.ErrorCode(resp))
	}
	http.Error(w, "bad gateway", http.StatusBadGateway)
	return nil, false
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
