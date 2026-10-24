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
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
)

// fakeS3 serves objects by "/bucket/key" in path style. It relies on
// http.ServeContent for ranges and conditional requests, like real S3.
type fakeS3 struct {
	t       *testing.T
	objects map[string]object
	// deny makes missing keys return 403 AccessDenied, like a bucket
	// without s3:ListBucket.
	deny bool
	// errCode makes every request fail with 403 and this code.
	errCode string
	creds   *aws.Credentials

	mu   sync.Mutex
	keys []string
}

type object struct {
	body        string
	contentType string
	headers     map[string]string
}

var modTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.keys = append(f.keys, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if f.creds != nil {
		f.checkSignature(r)
	} else if r.Header.Get("Authorization") != "" {
		f.t.Errorf("anonymous client sent Authorization header")
	}

	if f.errCode != "" {
		s3Error(w, http.StatusForbidden, f.errCode)
		return
	}
	obj, ok := f.objects[r.URL.Path]
	if !ok {
		if f.deny {
			s3Error(w, http.StatusForbidden, "AccessDenied")
		} else {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
		}
		return
	}
	ct := obj.contentType
	if ct == "" {
		ct = "text/html"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", `"etag-`+strings.ReplaceAll(r.URL.Path, "/", "-")+`"`)
	w.Header().Set("X-Amz-Request-Id", "req-1")
	for k, v := range obj.headers {
		w.Header().Set(k, v)
	}
	http.ServeContent(w, r, "", modTime, strings.NewReader(obj.body))
}

// checkSignature signs the received request again with the same
// credentials and time. A mismatch means the path or headers changed
// between signing and sending, the classic S3 escaping bug.
func (f *fakeS3) checkSignature(r *http.Request) {
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential="+f.creds.AccessKeyID+"/") {
		f.t.Errorf("unexpected Authorization header %q", got)
		return
	}
	ts, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		f.t.Errorf("bad X-Amz-Date: %v", err)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), r.Method, "http://"+r.Host+r.RequestURI, nil)
	for k, v := range r.Header {
		if k != "Authorization" && k != "User-Agent" {
			req.Header[k] = v
		}
	}
	err = v4.NewSigner().SignHTTP(r.Context(), *f.creds, req, emptyHash, "s3", "test-region", ts,
		func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err != nil {
		f.t.Errorf("re-sign: %v", err)
		return
	}
	if want := req.Header.Get("Authorization"); want != got {
		f.t.Errorf("signature mismatch for %s\n got: %s\nwant: %s", r.RequestURI, got, want)
	}
}

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>`+code+`</Code><Message>x</Message></Error>`)
}

func (f *fakeS3) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func newTestHandler(t *testing.T, fake *fakeS3, site config.Site) *Handler {
	t.Helper()
	fake.t = t
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)

	if site.Hosts == nil {
		site.Hosts = []string{"example.com"}
	}
	if site.Bucket == "" {
		site.Bucket = "site"
	}
	if site.Index == "" {
		site.Index = "index.html"
	}
	storage := config.S3{
		Endpoint:  upstream.URL,
		Region:    "test-region",
		PathStyle: true,
		Timeout:   5 * time.Second,
	}
	if fake.creds != nil {
		storage.AccessKeyID = fake.creds.AccessKeyID
		storage.SecretAccessKey = fake.creds.SecretAccessKey
	}
	h, err := New(&config.Config{S3: storage, Sites: []config.Site{site}}, metrics.New(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServe(t *testing.T) {
	fake := &fakeS3{objects: map[string]object{
		"/site/index.html":          {body: "home"},
		"/site/docs/index.html":     {body: "docs"},
		"/site/style.css":           {body: "body{}", contentType: "text/css"},
		"/site/app.js":              {body: "js", contentType: "binary/octet-stream"},
		"/site/404.html":            {body: "custom 404"},
		"/site/with space/ü.html":   {body: "escaped"},
		"/site/gz.js":               {body: "\x1f\x8b", contentType: "text/javascript", headers: map[string]string{"Content-Encoding": "gzip"}},
		"/site/cached.html":         {body: "c", headers: map[string]string{"Cache-Control": "max-age=60"}},
		"/site/download/report.pdf": {body: "pdf", contentType: "application/pdf"},
	}}
	h := newTestHandler(t, fake, config.Site{
		NotFound: "404.html",
		Headers:  map[string]string{"X-Frame-Options": "DENY"},
	})

	tests := []struct {
		name       string
		method     string
		target     string
		hdr        map[string]string
		wantStatus int
		wantBody   string
		wantHeader map[string]string
	}{
		{name: "root index", target: "/", wantStatus: 200, wantBody: "home",
			wantHeader: map[string]string{"Content-Type": "text/html", "X-Frame-Options": "DENY", "X-Amz-Request-Id": ""}},
		{name: "dir with slash", target: "/docs/", wantStatus: 200, wantBody: "docs"},
		{name: "dir without slash redirects", target: "/docs?x=1", wantStatus: 301,
			wantHeader: map[string]string{"Location": "/docs/?x=1"}},
		{name: "file", target: "/style.css", wantStatus: 200, wantBody: "body{}",
			wantHeader: map[string]string{"Content-Type": "text/css", "Etag": `"etag--site-style.css"`}},
		{name: "octet-stream guessed by extension", target: "/app.js", wantStatus: 200,
			wantHeader: map[string]string{"Content-Type": "text/javascript; charset=utf-8"}},
		{name: "gzip passes untouched", target: "/gz.js", wantStatus: 200, wantBody: "\x1f\x8b",
			wantHeader: map[string]string{"Content-Encoding": "gzip"}},
		{name: "object cache-control", target: "/cached.html", wantStatus: 200,
			wantHeader: map[string]string{"Cache-Control": "max-age=60"}},
		{name: "escaped key", target: "/with%20space/%C3%BC.html", wantStatus: 200, wantBody: "escaped"},
		{name: "missing uses not_found page", target: "/nope", wantStatus: 404, wantBody: "custom 404",
			wantHeader: map[string]string{"Etag": "", "X-Frame-Options": "DENY"}},
		{name: "traversal stays in bucket", target: "/../../etc/passwd", wantStatus: 404, wantBody: "custom 404"},
		{name: "head", method: "HEAD", target: "/style.css", wantStatus: 200, wantBody: "",
			wantHeader: map[string]string{"Content-Length": "6"}},
		{name: "if-none-match", target: "/style.css", hdr: map[string]string{"If-None-Match": `"etag--site-style.css"`},
			wantStatus: 304, wantBody: ""},
		{name: "range", target: "/download/report.pdf", hdr: map[string]string{"Range": "bytes=1-2"},
			wantStatus: 206, wantBody: "df", wantHeader: map[string]string{"Content-Range": "bytes 1-2/3"}},
		{name: "unsatisfiable range", target: "/style.css", hdr: map[string]string{"Range": "bytes=100-"},
			wantStatus: 416},
		{name: "post rejected", method: "POST", target: "/", wantStatus: 405,
			wantHeader: map[string]string{"Allow": "GET, HEAD"}},
		{name: "delete rejected", method: "DELETE", target: "/style.css", wantStatus: 405},
		{name: "unknown host", target: "http://other.com/", wantStatus: 404, wantBody: "not found\n"},
		{name: "host with port", target: "http://example.com:8080/", wantStatus: 200, wantBody: "home"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = "GET"
			}
			target := tt.target
			if strings.HasPrefix(target, "/") {
				target = "http://example.com" + target
			}
			rec := do(h, method, target, tt.hdr)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body %q", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantBody != "" || method == "HEAD" || tt.wantStatus == 304 {
				if got := rec.Body.String(); got != tt.wantBody {
					t.Errorf("body = %q, want %q", got, tt.wantBody)
				}
			}
			for k, want := range tt.wantHeader {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestPrefixAndSPA(t *testing.T) {
	fake := &fakeS3{objects: map[string]object{
		"/site/v2/index.html": {body: "spa"},
		"/site/v2/main.js":    {body: "js", contentType: "text/javascript"},
	}}
	h := newTestHandler(t, fake, config.Site{Prefix: "v2/", SPA: true})

	if rec := do(h, "GET", "http://example.com/users/42", nil); rec.Code != 200 || rec.Body.String() != "spa" {
		t.Errorf("spa route: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "http://example.com/main.js", nil); rec.Code != 200 || rec.Body.String() != "js" {
		t.Errorf("asset: %d %q", rec.Code, rec.Body.String())
	}
	// A missing asset must not turn into HTML, or the browser gets a
	// confusing MIME error instead of a plain 404.
	if rec := do(h, "GET", "http://example.com/missing.js", nil); rec.Code != 404 {
		t.Errorf("missing asset: %d %q", rec.Code, rec.Body.String())
	}
	for _, k := range fake.requested() {
		if !strings.HasPrefix(k, "GET /site/v2/") && !strings.HasPrefix(k, "HEAD /site/v2/") {
			t.Errorf("request outside prefix: %s", k)
		}
	}
}

func TestAccessDeniedMeansNotFound(t *testing.T) {
	fake := &fakeS3{deny: true, objects: map[string]object{"/site/index.html": {body: "home"}}}
	h := newTestHandler(t, fake, config.Site{})

	if rec := do(h, "GET", "http://example.com/nope", nil); rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec := do(h, "GET", "http://example.com/", nil); rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestBrokenCredentialsAreBadGateway(t *testing.T) {
	fake := &fakeS3{errCode: "SignatureDoesNotMatch"}
	h := newTestHandler(t, fake, config.Site{})

	if rec := do(h, "GET", "http://example.com/", nil); rec.Code != 502 {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestSignedRequests(t *testing.T) {
	fake := &fakeS3{
		creds: &aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"},
		objects: map[string]object{
			"/site/index.html":                  {body: "home"},
			"/site/a b/c+d/(x)!*'@,;=$&ü~.html": {body: "weird"},
		},
	}
	h := newTestHandler(t, fake, config.Site{})

	for target, want := range map[string]string{
		"/":                                   "home",
		"/a%20b/c+d/(x)!*'@,;=$&%C3%BC~.html": "weird",
		"/a%20b/c%2Bd/%28x%29!*'@,;=$&%C3%BC~.html": "weird",
	} {
		rec := do(h, "GET", "http://example.com"+target, map[string]string{"Range": "bytes=0-"})
		if rec.Code != 206 || rec.Body.String() != want {
			t.Errorf("%s: %d %q", target, rec.Code, rec.Body.String())
		}
	}
}

func TestStreamsLargeBody(t *testing.T) {
	big := strings.Repeat("x", 4<<20)
	fake := &fakeS3{objects: map[string]object{"/site/big.bin": {body: big, contentType: "application/zip"}}}
	h := newTestHandler(t, fake, config.Site{})

	rec := do(h, "GET", "http://example.com/big.bin", nil)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), []byte(big)) {
		t.Fatalf("status %d, %d bytes", rec.Code, rec.Body.Len())
	}
}
