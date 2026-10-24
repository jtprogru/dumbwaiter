// Package s3 is a minimal read-only S3 client: GET and HEAD of a single
// object, signed with SigV4 when credentials are configured.
package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
)

// SHA-256 of an empty payload. GET and HEAD never carry a body.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type Client struct {
	endpoint  *url.URL
	region    string
	pathStyle bool
	creds     *aws.Credentials
	signer    *v4.Signer
	http      *http.Client
	metrics   *metrics.Metrics
}

func New(cfg config.S3, m *metrics.Metrics) (*Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse s3 endpoint: %w", err)
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
		// Objects stored with Content-Encoding: gzip must reach the client
		// as is. With compression enabled the transport would unpack them.
		DisableCompression: true,
	}

	c := &Client{
		endpoint:  u,
		region:    cfg.Region,
		pathStyle: cfg.PathStyle,
		signer:    v4.NewSigner(),
		metrics:   m,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	if cfg.AccessKeyID != "" {
		c.creds = &aws.Credentials{
			AccessKeyID:     cfg.AccessKeyID,
			SecretAccessKey: cfg.SecretAccessKey,
		}
	}
	return c, nil
}

// Do sends a GET or HEAD for bucket/key. Headers from hdr are copied into
// the request before signing. The caller must close the response body.
func (c *Client) Do(ctx context.Context, method, bucket, key string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.objectURL(bucket, key), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}

	if c.creds != nil {
		req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
		err := c.signer.SignHTTP(ctx, *c.creds, req, emptyPayloadHash, "s3", c.region, time.Now(),
			func(o *v4.SignerOptions) {
				// S3 wants the path encoded once, objectURL already did it.
				o.DisableURIPathEscaping = true
			})
		if err != nil {
			return nil, fmt.Errorf("sign request: %w", err)
		}
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	c.metrics.S3Request(bucket, method, resultCode(ctx, resp, err), time.Since(start))
	return resp, err
}

// resultCode is the code label of a request: the response status, or why
// there is no response.
func resultCode(ctx context.Context, resp *http.Response, err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return strconv.Itoa(resp.StatusCode)
	case ctx.Err() != nil:
		return metrics.S3Canceled
	case errors.As(err, &ne) && ne.Timeout():
		return metrics.S3Timeout
	}
	return metrics.S3Error
}

func (c *Client) objectURL(bucket, key string) string {
	u := *c.endpoint
	base := strings.TrimSuffix(u.Path, "/")
	if c.pathStyle {
		u.Path = base + "/" + bucket + "/" + key
	} else {
		u.Host = bucket + "." + u.Host
		u.Path = base + "/" + key
	}
	u.RawPath = escapePath(u.Path)
	u.RawQuery = ""
	return u.String()
}

// escapePath applies the S3 URI encoding: everything except unreserved
// characters and the slash is percent-encoded.
func escapePath(p string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(p) * 3 / 2)
	for i := range len(p) {
		ch := p[i]
		if ch == '/' || isUnreserved(ch) {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[ch>>4])
		b.WriteByte(hex[ch&0x0f])
	}
	return b.String()
}

func isUnreserved(ch byte) bool {
	return 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z' || '0' <= ch && ch <= '9' ||
		ch == '-' || ch == '_' || ch == '.' || ch == '~'
}

// ErrorCode reads the S3 XML error code from a failed response, such as
// NoSuchKey or AccessDenied. HEAD responses have no body, so it returns "".
func ErrorCode(resp *http.Response) string {
	var body struct {
		Code string `xml:"Code"`
	}
	err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return body.Code
}
