package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndEnv(t *testing.T) {
	t.Setenv("TEST_S3_KEY", "AKIA123")
	t.Setenv("TEST_S3_SECRET", "s3cr$t")

	cfg, err := Parse([]byte(`
s3:
  endpoint: https://storage.yandexcloud.net/
  region: ru-central1
  access_key_id: ${TEST_S3_KEY}
  secret_access_key: ${TEST_S3_SECRET}
sites:
  - hosts: [Example.COM., "www.example.com:8080"]
    bucket: site
    prefix: /public/
    headers:
      x-frame-options: DENY
      x-price: $5
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.HTTP.Addr != ":8080" || cfg.Server.Admin.Addr != ":9090" {
		t.Errorf("listener defaults not applied: %+v", cfg.Server)
	}
	if cfg.Server.ShutdownDelay != 5*time.Second {
		t.Errorf("shutdown_delay = %v", cfg.Server.ShutdownDelay)
	}
	if cfg.S3.AccessKeyID != "AKIA123" || cfg.S3.SecretAccessKey != "s3cr$t" {
		t.Errorf("env not expanded: %+v", cfg.S3)
	}
	if cfg.S3.Endpoint != "https://storage.yandexcloud.net" {
		t.Errorf("endpoint = %q", cfg.S3.Endpoint)
	}

	site := cfg.Sites[0]
	if got := strings.Join(site.Hosts, ","); got != "example.com,www.example.com" {
		t.Errorf("hosts = %s", got)
	}
	if site.Prefix != "public/" || site.Index != "index.html" {
		t.Errorf("prefix=%q index=%q", site.Prefix, site.Index)
	}
	if site.Headers["x-price"] != "$5" {
		t.Errorf("bare $ must survive expansion, got %q", site.Headers["x-price"])
	}
}

func TestEnvExpansionScope(t *testing.T) {
	t.Setenv("TEST_PORT", "8443")
	t.Setenv("TEST_BUCKET", "from-env")

	cfg, err := Parse([]byte(`
# ${NOT_SET_ANYWHERE} in a comment must not break the startup.
server:
  https:
    public_port: ${TEST_PORT}   # plain scalar: becomes an int
    certificates: [{cert: a, key: b}]
log:
  access: false
s3: {endpoint: "http://x"}
sites:
  - hosts: [a.test]
    bucket: "${TEST_BUCKET}"
    headers: {X-Port: '${TEST_PORT}'}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.HTTPS.PublicPort != 8443 {
		t.Errorf("public_port = %d", cfg.Server.HTTPS.PublicPort)
	}
	if cfg.Sites[0].Bucket != "from-env" || cfg.Sites[0].Headers["X-Port"] != "8443" {
		t.Errorf("quoted values not expanded: %+v", cfg.Sites[0])
	}
	if cfg.Log.AccessEnabled() {
		t.Error("log.access: false ignored")
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "at least one site") {
		t.Fatalf("got %v", err)
	}
}

func TestParseHTTPSDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
server:
  https:
    http3: true
    certificates:
      - {cert: /tls/tls.crt, key: /tls/tls.key}
s3: {endpoint: "http://minio:9000"}
sites: [{hosts: [a.test], bucket: a}]
`))
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Server.HTTPS
	if h.Addr != ":8443" || h.PublicPort != 443 || h.ReloadInterval != time.Minute {
		t.Errorf("https defaults not applied: %+v", h)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]struct {
		yaml string
		want string
	}{
		"unset env": {
			yaml: `s3: {endpoint: "${dumbwaiter_TEST_UNSET}"}`,
			want: "dumbwaiter_TEST_UNSET",
		},
		"unknown field": {
			yaml: "s3: {endpoint: http://x}\nsites: [{hosts: [a.test], bucket: a, buckt: b}]",
			want: "buckt",
		},
		"no sites": {
			yaml: "s3: {endpoint: http://x}",
			want: "at least one site",
		},
		"duplicate host": {
			yaml: "s3: {endpoint: http://x}\nsites: [{hosts: [a.test], bucket: a}, {hosts: [A.test], bucket: b}]",
			want: `"a.test" is already used by sites[0]`,
		},
		"wildcard host": {
			yaml: "s3: {endpoint: http://x}\nsites: [{hosts: ['*.a.test'], bucket: a}]",
			want: "invalid host",
		},
		"missing bucket": {
			yaml: "s3: {endpoint: http://x}\nsites: [{hosts: [a.test]}]",
			want: "bucket: required",
		},
		"missing endpoint": {
			yaml: "sites: [{hosts: [a.test], bucket: a}]",
			want: "s3: endpoint is required",
		},
		"half credentials": {
			yaml: "s3: {endpoint: http://x, access_key_id: k}\nsites: [{hosts: [a.test], bucket: a}]",
			want: "must be set together",
		},
		"site s3 override is validated": {
			yaml: "s3: {endpoint: http://x}\nsites: [{hosts: [a.test], bucket: a, s3: {endpoint: 'ftp://y'}}]",
			want: "sites[0].s3: endpoint",
		},
		"redirect without https": {
			yaml: "server: {http: {redirect_to_https: true}}\ns3: {endpoint: http://x}\nsites: [{hosts: [a.test], bucket: a}]",
			want: "requires server.https",
		},
		"https without certificates": {
			yaml: "server: {https: {}}\ns3: {endpoint: http://x}\nsites: [{hosts: [a.test], bucket: a}]",
			want: "at least one certificate",
		},
		"bad log level": {
			yaml: "log: {level: loud}\ns3: {endpoint: http://x}\nsites: [{hosts: [a.test], bucket: a}]",
			want: "unknown level",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Example.com", "example.com"},
		{"example.com:443", "example.com"},
		{"example.com.", "example.com"},
		{"[::1]:8080", "[::1]"},
		{"[::1]", "[::1]"},
		{"  WWW.Example.Com", "www.example.com"},
	}
	for _, tt := range tests {
		if got := NormalizeHost(tt.in); got != tt.want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
