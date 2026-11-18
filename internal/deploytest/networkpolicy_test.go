// Package deploytest checks rendered deployment security boundaries.
package deploytest

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

//nolint:tagliatelle // Kubernetes API field names use camelCase.
type policy struct {
	Kind string `yaml:"kind"`
	Spec struct {
		PodSelector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"podSelector"`
		PolicyTypes []string      `yaml:"policyTypes"`
		Ingress     []ingressRule `yaml:"ingress"`
	} `yaml:"spec"`
}

//nolint:tagliatelle // Kubernetes API field names use camelCase.
type ingressRule struct {
	Ports []struct {
		Port     int    `yaml:"port"`
		Protocol string `yaml:"protocol"`
	} `yaml:"ports"`
	From []struct {
		NamespaceSelector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"namespaceSelector"`
	} `yaml:"from"`
}

func render(t *testing.T, tool string, args ...string) string {
	t.Helper()
	path, err := exec.LookPath(tool)
	if err != nil {
		if os.Getenv("REQUIRE_DEPLOY_TOOLS") == "1" {
			t.Fatal(err)
		}
		t.Skipf("%s is not installed; make check-network-policies requires it", tool)
	}
	cmd := exec.CommandContext(t.Context(), path, args...)
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, out)
	}
	return string(out)
}

func findPolicy(t *testing.T, manifest string) *policy {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		// Decode the kind before the typed policy: other resources can have
		// fields such as spec.ports.port with different representations.
		var doc yaml.Node
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			t.Fatal(err)
		}
		var header struct{ Kind string }
		if err := doc.Decode(&header); err != nil {
			t.Fatal(err)
		}
		if header.Kind != "NetworkPolicy" {
			continue
		}
		var p policy
		if err := doc.Decode(&p); err != nil {
			t.Fatal(err)
		}
		return &p
	}
}

func checkPolicy(t *testing.T, p *policy, adminPort, httpsPort int, adminNamespace string, wantUDP bool) {
	t.Helper()
	if p == nil || !slices.Contains(p.Spec.PolicyTypes, "Ingress") || len(p.Spec.PodSelector.MatchLabels) == 0 {
		t.Fatal("missing ingress policy or workload selector")
	}
	adminRules, udpRules := 0, 0
	for _, rule := range p.Spec.Ingress {
		if len(rule.Ports) == 0 {
			t.Fatal("rule without port restrictions could open admin")
		}
		for _, port := range rule.Ports {
			if port.Port == adminPort && port.Protocol == "TCP" {
				adminRules++
				if adminNamespace == "" || len(rule.From) != 1 || rule.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != adminNamespace {
					t.Fatalf("admin rule is not limited to expected namespace %q: %+v", adminNamespace, rule)
				}
			}
			if port.Port == httpsPort && port.Protocol == "UDP" {
				udpRules++
			}
		}
	}
	if (adminRules == 1) != (adminNamespace != "") || adminRules > 1 {
		t.Fatalf("unexpected admin rules: %d for namespace %q", adminRules, adminNamespace)
	}
	if (udpRules == 1) != wantUDP || udpRules > 1 {
		t.Fatalf("UDP rules = %d, want enabled=%v", udpRules, wantUDP)
	}
}

func TestHelmNetworkPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, namespace string
		args            []string
		udp             bool
		admin, https    int
	}{
		{name: "default denies admin", admin: 9090, https: 8443},
		{name: "http3 enabled", args: []string{"--set", "tls.enabled=true"}, udp: true, admin: 9090, https: 8443},
		{name: "http3 disabled", args: []string{"--set", "tls.enabled=true,tls.http3=false"}, admin: 9090, https: 8443},
		{name: "explicit scraper", args: []string{"-f", "deploy/helm/dumbwaiter/ci/networkpolicy-values.yaml"}, namespace: "monitoring", udp: true, admin: 9090, https: 8443},
		{name: "custom ports", args: []string{"--set", "tls.enabled=true,containerPorts.admin=9191,containerPorts.https=9443"}, udp: true, admin: 9191, https: 9443},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"template", "security-test", "deploy/helm/dumbwaiter"}, tt.args...)
			p := findPolicy(t, render(t, "helm", args...))
			checkPolicy(t, p, tt.admin, tt.https, tt.namespace, tt.udp)
		})
	}
	t.Run("explicit opt out", func(t *testing.T) {
		manifest := render(t, "helm", "template", "security-test", "deploy/helm/dumbwaiter", "--set", "networkPolicy.enabled=false")
		if findPolicy(t, manifest) != nil {
			t.Fatal("disabled policy was rendered")
		}
	})
}

func TestKustomizeNetworkPolicy(t *testing.T) {
	for _, overlay := range []string{"ingress", "gateway", "tls"} {
		t.Run(overlay, func(t *testing.T) {
			p := findPolicy(t, render(t, "kubectl", "kustomize", "deploy/kubernetes/overlays/"+overlay))
			checkPolicy(t, p, 9090, 8443, "monitoring", overlay == "tls")
		})
	}
}
