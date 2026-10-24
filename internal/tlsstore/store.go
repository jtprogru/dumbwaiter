// Package tlsstore keeps TLS certificates loaded from files, picks one by
// SNI and reloads them when the files change, e.g. after cert-manager
// renews a Kubernetes Secret.
package tlsstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/metrics"
)

type Store struct {
	pairs   []config.Certificate
	log     *slog.Logger
	metrics *metrics.TLS

	certs atomic.Pointer[[]*tls.Certificate]
	sum   []byte
}

// Load reads all certificates. Unlike reloads, a failure here is fatal.
func Load(pairs []config.Certificate, m *metrics.Metrics, log *slog.Logger) (*Store, error) {
	s := &Store{pairs: pairs, log: log, metrics: m.TLS()}
	if _, err := s.reload(); err != nil {
		return nil, err
	}
	// reload keeps the order of pairs, so index i always points to the
	// certificate from pairs[i], old or renewed.
	for i, p := range pairs {
		s.metrics.Certificate(p.Cert, func() time.Time {
			return (*s.certs.Load())[i].Leaf.NotAfter
		})
	}
	return s, nil
}

// GetCertificate implements tls.Config.GetCertificate. Without a matching
// certificate, or without SNI at all, the first one is returned.
func (s *Store) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	certs := *s.certs.Load()
	for _, c := range certs {
		if hello.SupportsCertificate(c) == nil {
			return c, nil
		}
	}
	return certs[0], nil
}

// Watch rereads the files every interval until ctx is done.
func (s *Store) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed, err := s.reload()
			switch {
			case err != nil:
				s.metrics.Failed()
				s.log.Warn("tls certificates reload failed, keeping the old ones", "err", err)
			case changed:
				s.metrics.Reloaded()
				s.log.Info("tls certificates reloaded", "count", len(s.pairs))
			}
		}
	}
}

// reload is not safe for concurrent use; only Load and Watch call it.
func (s *Store) reload() (bool, error) {
	h := sha256.New()
	raw := make([][2][]byte, len(s.pairs))
	for i, p := range s.pairs {
		crt, err := os.ReadFile(p.Cert)
		if err != nil {
			return false, err
		}
		key, err := os.ReadFile(p.Key)
		if err != nil {
			return false, err
		}
		h.Write(crt)
		h.Write(key)
		raw[i] = [2][]byte{crt, key}
	}
	sum := h.Sum(nil)
	if bytes.Equal(sum, s.sum) {
		return false, nil
	}

	certs := make([]*tls.Certificate, 0, len(raw))
	for i, pair := range raw {
		c, err := tls.X509KeyPair(pair[0], pair[1])
		if err != nil {
			return false, fmt.Errorf("%s: %w", s.pairs[i].Cert, err)
		}
		certs = append(certs, &c)
	}
	if len(certs) == 0 {
		return false, errors.New("no certificates configured")
	}

	s.certs.Store(&certs)
	s.sum = sum
	return true, nil
}
