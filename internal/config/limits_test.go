package config

import (
	"strings"
	"testing"
	"time"
)

func TestServingLimits(t *testing.T) {
	for _, tt := range []struct {
		name, server, site, wantError string
		global, perSite               int
		timeout                       time.Duration
		redirect                      bool
	}{
		{name: "defaults", global: 256, perSite: 64, timeout: 30 * time.Second, redirect: true},
		{name: "zero uses defaults", server: "max_in_flight: 0", site: "max_in_flight: 0, lookup_timeout: 0s", global: 256, perSite: 64, timeout: 30 * time.Second, redirect: true},
		{name: "explicit", server: "max_in_flight: 8", site: "max_in_flight: 2, lookup_timeout: 3s, directory_redirect: false", global: 8, perSite: 2, timeout: 3 * time.Second},
		{name: "negative global", server: "max_in_flight: -1", wantError: "server.max_in_flight"},
		{name: "negative site", site: "max_in_flight: -1", wantError: "sites[0].max_in_flight"},
		{name: "negative lookup", site: "lookup_timeout: -1s", wantError: "sites[0].lookup_timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := "server: {" + tt.server + "}\ns3: {endpoint: http://storage.test}\nsites: [{hosts: [a.test], bucket: site, " + tt.site + "}]"
			cfg, err := Parse([]byte(raw))
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("want %q, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			site := cfg.Sites[0]
			if cfg.Server.MaxInFlight != tt.global || site.MaxInFlight != tt.perSite || site.LookupTimeout != tt.timeout || site.DirectoryRedirectEnabled() != tt.redirect {
				t.Fatalf("unexpected limits: server=%+v site=%+v", cfg.Server, site)
			}
		})
	}
}
