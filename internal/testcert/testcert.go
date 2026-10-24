// Package testcert writes self-signed certificates for tests.
package testcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Write creates <name>.crt and <name>.key in dir for the given hosts and
// returns their paths and the parsed certificate.
func Write(tb testing.TB, dir, name string, hosts ...string) (certPath, keyPath string, cert *x509.Certificate) {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0]},
		DNSNames:              hosts,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatal(err)
	}
	cert, _ = x509.ParseCertificate(der)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		tb.Fatal(err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	writePEM(tb, certPath, "CERTIFICATE", der)
	writePEM(tb, keyPath, "PRIVATE KEY", keyDER)
	return certPath, keyPath, cert
}

func writePEM(tb testing.TB, path, typ string, der []byte) {
	tb.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		tb.Fatal(err)
	}
}
