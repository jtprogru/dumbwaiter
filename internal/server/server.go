// Package server runs the listeners: plain HTTP, optional HTTPS with
// HTTP/2 and HTTP/3, and the admin port with health probes.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/jtprogru/dumbwaiter/internal/config"
	"github.com/jtprogru/dumbwaiter/internal/tlsstore"
)

// Sites is the request handler that also knows which hosts it serves.
type Sites interface {
	http.Handler
	HasHost(host string) bool
}

type Server struct {
	cfg   *config.Config
	log   *slog.Logger
	ready atomic.Bool

	certs *tlsstore.Store

	httpLn, httpsLn, adminLn net.Listener
	udpConn                  net.PacketConn

	httpSrv, httpsSrv, adminSrv *http.Server
	h3Srv                       *http3.Server
}

// New binds all listeners, so a busy port fails here and not later.
func New(ctx context.Context, cfg *config.Config, sites Sites, log *slog.Logger) (srv *Server, err error) {
	s := &Server{cfg: cfg, log: log}
	defer func() {
		if err != nil {
			s.closeListeners()
		}
	}()

	var lc net.ListenConfig
	sc := cfg.Server
	errorLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	logged := func(h http.Handler) http.Handler {
		if !cfg.Log.AccessEnabled() {
			return h
		}
		return accessLog(log, h)
	}
	newHTTPServer := func(h http.Handler, p *http.Protocols) *http.Server {
		return &http.Server{
			Handler:           h,
			Protocols:         p,
			ReadHeaderTimeout: sc.ReadHeaderTimeout,
			IdleTimeout:       sc.IdleTimeout,
			ErrorLog:          errorLog,
		}
	}

	if s.adminLn, err = lc.Listen(ctx, "tcp", sc.Admin.Addr); err != nil {
		return nil, fmt.Errorf("admin listener: %w", err)
	}
	s.adminSrv = newHTTPServer(s.adminHandler(), nil)

	if https := sc.HTTPS; https != nil {
		if s.certs, err = tlsstore.Load(https.Certificates, log); err != nil {
			return nil, fmt.Errorf("load tls certificates: %w", err)
		}
		tlsConf := &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: s.certs.GetCertificate,
		}

		var handler http.Handler = sites
		if https.HTTP3 {
			if s.udpConn, err = lc.ListenPacket(ctx, "udp", https.Addr); err != nil {
				return nil, fmt.Errorf("http3 listener: %w", err)
			}
			s.h3Srv = &http3.Server{
				Handler: logged(sites),
				// Serve sets the h3 ALPN itself. QUIC enforces TLS 1.3.
				TLSConfig:   tlsConf.Clone(),
				Port:        https.PublicPort,
				IdleTimeout: sc.IdleTimeout,
				Logger:      log,
			}
			handler = advertiseHTTP3(s.h3Srv, sites)
		}

		if s.httpsLn, err = lc.Listen(ctx, "tcp", https.Addr); err != nil {
			return nil, fmt.Errorf("https listener: %w", err)
		}
		var p http.Protocols
		p.SetHTTP1(true)
		p.SetHTTP2(true)
		s.httpsSrv = newHTTPServer(logged(handler), &p)
		s.httpsSrv.TLSConfig = tlsConf
	}

	if s.httpLn, err = lc.Listen(ctx, "tcp", sc.HTTP.Addr); err != nil {
		return nil, fmt.Errorf("http listener: %w", err)
	}
	var handler http.Handler = sites
	if sc.HTTP.RedirectToHTTPS {
		handler = redirectToHTTPS(sites, sc.HTTPS.PublicPort)
	}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(sc.HTTP.H2C)
	s.httpSrv = newHTTPServer(logged(handler), &p)

	return s, nil
}

// Run serves until ctx is cancelled or a listener fails, then shuts down
// gracefully.
func (s *Server) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 4)
	serve := func(name string, fn func() error) {
		go func() {
			err := fn()
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				errCh <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	serve("admin", func() error { return s.adminSrv.Serve(s.adminLn) })
	serve("http", func() error { return s.httpSrv.Serve(s.httpLn) })
	attrs := []any{"http", s.httpLn.Addr().String(), "admin", s.adminLn.Addr().String()}
	if s.httpsSrv != nil {
		serve("https", func() error { return s.httpsSrv.ServeTLS(s.httpsLn, "", "") })
		go s.certs.Watch(ctx, s.cfg.Server.HTTPS.ReloadInterval)
		attrs = append(attrs, "https", s.httpsLn.Addr().String())
	}
	if s.h3Srv != nil {
		serve("http3", func() error { return s.h3Srv.Serve(s.udpConn) })
		attrs = append(attrs, "http3", s.udpConn.LocalAddr().String())
	}

	s.ready.Store(true)
	s.log.Info("listening", attrs...)

	var runErr error
	select {
	case <-ctx.Done():
		s.log.Info("shutdown requested, draining", "delay", s.cfg.Server.ShutdownDelay)
		s.ready.Store(false)
		// Keep serving while Kubernetes removes the pod from endpoints.
		time.Sleep(s.cfg.Server.ShutdownDelay)
	case runErr = <-errCh:
		s.ready.Store(false)
		s.log.Error("listener failed, shutting down", "err", runErr)
	}

	// ctx is already cancelled here, so the deadline hangs off a detached copy.
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Server.ShutdownTimeout)
	defer done()

	var wg sync.WaitGroup
	shutdown := func(name string, fn func(context.Context) error) {
		wg.Go(func() {
			if err := fn(shutdownCtx); err != nil {
				s.log.Warn("shutdown", "listener", name, "err", err)
			}
		})
	}
	shutdown("http", s.httpSrv.Shutdown)
	if s.httpsSrv != nil {
		shutdown("https", s.httpsSrv.Shutdown)
	}
	if s.h3Srv != nil {
		shutdown("http3", s.h3Srv.Shutdown)
	}
	wg.Wait()
	if s.udpConn != nil {
		s.udpConn.Close()
	}
	_ = s.adminSrv.Shutdown(shutdownCtx)

	s.log.Info("stopped")
	return runErr
}

// Addresses of the bound listeners, mostly for tests that bind to port 0.
func (s *Server) HTTPAddr() string  { return s.httpLn.Addr().String() }
func (s *Server) AdminAddr() string { return s.adminLn.Addr().String() }
func (s *Server) HTTPSAddr() string { return s.httpsLn.Addr().String() }
func (s *Server) HTTP3Addr() string { return s.udpConn.LocalAddr().String() }

func (s *Server) closeListeners() {
	for _, ln := range []net.Listener{s.httpLn, s.httpsLn, s.adminLn} {
		if ln != nil {
			ln.Close()
		}
	}
	if s.udpConn != nil {
		s.udpConn.Close()
	}
}

func (s *Server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// advertiseHTTP3 adds Alt-Svc to responses over TCP, so browsers learn
// they can switch to HTTP/3.
func advertiseHTTP3(h3 *http3.Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = h3.SetQUICHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

func redirectToHTTPS(sites Sites, port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !sites.HasHost(r.Host) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		host := config.NormalizeHost(r.Host)
		if port != 443 {
			host = net.JoinHostPort(host, strconv.Itoa(port))
		}
		//nolint:gosec // Not an open redirect: the host was checked against the configured sites above.
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
}
