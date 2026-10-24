// Package config loads and validates the dumbwaiter YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Server Server `yaml:"server"`
	Log    Log    `yaml:"log"`
	// S3 is the default storage for every site that has no own s3 block.
	S3    S3     `yaml:"s3"`
	Sites []Site `yaml:"sites"`
}

type Server struct {
	HTTP  HTTP   `yaml:"http"`
	HTTPS *HTTPS `yaml:"https"`
	Admin Admin  `yaml:"admin"`

	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	// ShutdownDelay is how long the pod keeps serving after SIGTERM with
	// /readyz failing, so endpoints get updated before listeners close.
	ShutdownDelay   time.Duration `yaml:"shutdown_delay"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

type HTTP struct {
	Addr string `yaml:"addr"`
	// H2C enables HTTP/2 with prior knowledge on the plain listener.
	H2C bool `yaml:"h2c"`
	// RedirectToHTTPS answers every request on the plain listener with a
	// redirect to https. Requires server.https.
	RedirectToHTTPS bool `yaml:"redirect_to_https"`
}

type HTTPS struct {
	Addr string `yaml:"addr"`
	// PublicPort is the port clients connect to. It goes into Alt-Svc and
	// into http->https redirects, because in Kubernetes it usually differs
	// from the container port.
	PublicPort     int           `yaml:"public_port"`
	HTTP3          bool          `yaml:"http3"`
	Certificates   []Certificate `yaml:"certificates"`
	ReloadInterval time.Duration `yaml:"reload_interval"`
}

type Certificate struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

type Admin struct {
	Addr string `yaml:"addr"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// Access turns the per-request log on or off. On by default.
	Access *bool `yaml:"access"`
}

// AccessEnabled reports whether per-request logging is on.
func (l Log) AccessEnabled() bool { return l.Access == nil || *l.Access }

// S3 is comparable on purpose: sites with equal settings share one client.
type S3 struct {
	Endpoint        string        `yaml:"endpoint"`
	Region          string        `yaml:"region"`
	PathStyle       bool          `yaml:"path_style"`
	AccessKeyID     string        `yaml:"access_key_id"`
	SecretAccessKey string        `yaml:"secret_access_key"`
	Timeout         time.Duration `yaml:"timeout"`
}

type Site struct {
	Hosts  []string `yaml:"hosts"`
	Bucket string   `yaml:"bucket"`
	Prefix string   `yaml:"prefix"`
	Index  string   `yaml:"index"`
	// NotFound is an object served with status 404 when nothing else matched.
	NotFound string `yaml:"not_found"`
	// SPA serves the root index for missing paths without a file extension.
	SPA     bool              `yaml:"spa"`
	Headers map[string]string `yaml:"headers"`
	// S3 replaces the top-level s3 block for this site entirely.
	S3 *S3 `yaml:"s3"`
}

// Hosts returns all configured hosts, sorted. Handy for logs.
func (c *Config) Hosts() []string {
	var out []string
	for _, s := range c.Sites {
		out = append(out, s.Hosts...)
	}
	sort.Strings(out)
	return out
}

// Storage returns the effective S3 settings of the site.
func (s Site) Storage(def S3) S3 {
	if s.S3 != nil {
		return *s.S3
	}
	return def
}

// Load reads the file, expands ${VAR} references and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var cfg Config
	if root.Kind != 0 { // 0 means an empty file
		if err := expandEnv(&root); err != nil {
			return nil, err
		}
		// Node.Decode cannot reject unknown fields, so the expanded tree
		// goes through a strict decoder once more.
		expanded, err := yaml.Marshal(&root)
		if err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(expanded))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv substitutes ${VAR} in scalar values, never in comments. A bare
// $ stays as is, so header values with dollar signs survive. Unset
// variables are an error: an empty secret key is easier to catch here than
// as a 403 from S3.
func expandEnv(root *yaml.Node) error {
	var missing []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
			n.Value = envRef.ReplaceAllStringFunc(n.Value, func(m string) string {
				name := envRef.FindStringSubmatch(m)[1]
				v, ok := os.LookupEnv(name)
				if !ok {
					missing = append(missing, name)
				}
				return v
			})
			if n.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 {
				// Let a plain scalar resolve again: port: ${PORT} is an int.
				n.Tag = ""
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(root)
	if len(missing) > 0 {
		return fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) setDefaults() {
	s := &c.Server
	setDefault(&s.HTTP.Addr, ":8080")
	setDefault(&s.Admin.Addr, ":9090")
	setDefault(&s.ReadHeaderTimeout, 10*time.Second)
	setDefault(&s.IdleTimeout, 120*time.Second)
	setDefault(&s.ShutdownDelay, 5*time.Second)
	setDefault(&s.ShutdownTimeout, 30*time.Second)
	if s.HTTPS != nil {
		setDefault(&s.HTTPS.Addr, ":8443")
		setDefault(&s.HTTPS.PublicPort, 443)
		setDefault(&s.HTTPS.ReloadInterval, time.Minute)
	}

	setDefault(&c.Log.Level, "info")
	setDefault(&c.Log.Format, "json")

	c.S3.setDefaults()
	for i := range c.Sites {
		site := &c.Sites[i]
		setDefault(&site.Index, "index.html")
		site.Index = strings.TrimPrefix(site.Index, "/")
		site.NotFound = strings.TrimPrefix(site.NotFound, "/")
		site.Prefix = strings.Trim(site.Prefix, "/")
		if site.Prefix != "" {
			site.Prefix += "/"
		}
		for j, h := range site.Hosts {
			site.Hosts[j] = NormalizeHost(h)
		}
		if site.S3 != nil {
			site.S3.setDefaults()
		}
	}
}

func (s *S3) setDefaults() {
	setDefault(&s.Region, "us-east-1")
	setDefault(&s.Timeout, 30*time.Second)
	s.Endpoint = strings.TrimSuffix(s.Endpoint, "/")
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level: unknown level %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format: unknown format %q", c.Log.Format)
	}

	if https := c.Server.HTTPS; https != nil {
		if len(https.Certificates) == 0 {
			add("server.https.certificates: at least one certificate is required")
		}
		for i, crt := range https.Certificates {
			if crt.Cert == "" || crt.Key == "" {
				add("server.https.certificates[%d]: both cert and key are required", i)
			}
		}
		if https.PublicPort < 1 || https.PublicPort > 65535 {
			add("server.https.public_port: %d is out of range", https.PublicPort)
		}
	} else if c.Server.HTTP.RedirectToHTTPS {
		add("server.http.redirect_to_https: requires server.https")
	}

	if len(c.Sites) == 0 {
		add("sites: at least one site is required")
	}
	seen := map[string]int{}
	for i, site := range c.Sites {
		p := fmt.Sprintf("sites[%d]", i)
		if len(site.Hosts) == 0 {
			add("%s.hosts: at least one host is required", p)
		}
		for _, h := range site.Hosts {
			if h == "" || strings.ContainsAny(h, "/:* ") {
				add("%s.hosts: invalid host %q, expected a bare domain name", p, h)
				continue
			}
			if prev, dup := seen[h]; dup {
				add("%s.hosts: %q is already used by sites[%d]", p, h, prev)
			}
			seen[h] = i
		}
		if site.Bucket == "" {
			add("%s.bucket: required", p)
		}
		if site.Index == "" || strings.HasSuffix(site.Index, "/") {
			add("%s.index: must be an object name like index.html", p)
		}
		for name := range site.Headers {
			if name == "" || strings.ContainsAny(name, " :\r\n") {
				add("%s.headers: invalid header name %q", p, name)
			}
		}
		where := "s3"
		if site.S3 != nil {
			where = p + ".s3"
		}
		storage := site.Storage(c.S3)
		if err := storage.validate(); err != nil {
			add("%s: %w", where, err)
		}
	}

	return errors.Join(errs...)
}

func (s *S3) validate() error {
	if s.Endpoint == "" {
		return errors.New("endpoint is required")
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("endpoint %q must be an http(s) URL", s.Endpoint)
	}
	if (s.AccessKeyID == "") != (s.SecretAccessKey == "") {
		return errors.New("access_key_id and secret_access_key must be set together")
	}
	return nil
}

// NormalizeHost lowercases a host and drops the port and the trailing dot.
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndexByte(h, ':'); i != -1 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}
