package tlsstore

import (
	"crypto/tls"
	"log/slog"
	"testing"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
	"github.com/jtprogru/dumbwaiter/internal/testcert"
)

func TestSNIAndReload(t *testing.T) {
	dir := t.TempDir()
	aCrt, aKey, aCert := testcert.Write(t, dir, "a", "a.test", "www.a.test")
	bCrt, bKey, bCert := testcert.Write(t, dir, "b", "b.test")

	s, err := Load([]config.Certificate{{Cert: aCrt, Key: aKey}, {Cert: bCrt, Key: bKey}}, metrics.New(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	pick := func(sni string) string {
		c, err := s.GetCertificate(&tls.ClientHelloInfo{
			ServerName:        sni,
			SupportedVersions: []uint16{tls.VersionTLS13},
			SignatureSchemes:  []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			SupportedCurves:   []tls.CurveID{tls.CurveP256},
			CipherSuites:      []uint16{tls.TLS_AES_128_GCM_SHA256},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(c.Leaf.Raw)
	}

	if pick("www.a.test") != string(aCert.Raw) {
		t.Error("www.a.test did not get certificate a")
	}
	if pick("b.test") != string(bCert.Raw) {
		t.Error("b.test did not get certificate b")
	}
	if pick("") != string(aCert.Raw) {
		t.Error("no SNI must fall back to the first certificate")
	}

	if changed, err := s.reload(); err != nil || changed {
		t.Errorf("reload of unchanged files: changed=%v err=%v", changed, err)
	}

	// cert-manager renewed b.test.
	_, _, newB := testcert.Write(t, dir, "b", "b.test")
	if changed, err := s.reload(); err != nil || !changed {
		t.Fatalf("reload after renewal: changed=%v err=%v", changed, err)
	}
	if pick("b.test") != string(newB.Raw) {
		t.Error("b.test still gets the old certificate")
	}
}

func TestBrokenReloadKeepsOldCertificates(t *testing.T) {
	dir := t.TempDir()
	crt, key, cert := testcert.Write(t, dir, "a", "a.test")
	s, err := Load([]config.Certificate{{Cert: crt, Key: key}}, metrics.New(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	// The key no longer matches the certificate.
	_, otherKey, _ := testcert.Write(t, dir, "other", "a.test")
	s.pairs[0].Key = otherKey
	if _, err := s.reload(); err == nil {
		t.Fatal("expected error for mismatched key")
	}

	c, _ := s.GetCertificate(&tls.ClientHelloInfo{ServerName: "a.test"})
	if string(c.Leaf.Raw) != string(cert.Raw) {
		t.Error("old certificate was replaced by a broken one")
	}
}
