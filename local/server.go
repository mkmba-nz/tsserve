// Package local hosts the in-process control surface: a tailnet HTTPS server
// (status page + traefik dashboard proxy) and a loopback HTTP server exposing
// /metrics for Prometheus scrape.
//
// The split is intentional: status/traefik traffic flows over the tailnet (and
// is reachable from any tailnet peer that can resolve the node's hostname),
// while /metrics binds to a local interface so scrape traffic stays on the
// host. See SPEC.md for the user-facing description.
package local

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mkmba.nz/tsserve/metrics"
	"mkmba.nz/tsserve/proxy"
)

// Server owns both local listeners (tailnet HTTPS + loopback HTTP metrics).
type Server struct {
	Logger        *slog.Logger
	Manager       *proxy.Manager
	LocalClient   *local.Client
	Metrics       *metrics.Collector
	TraefikPort   int    // 0 = disabled
	MetricsAddr   string // e.g. "127.0.0.1:9090"; "" disables the metrics listener
	DiscoveryMode string // for display only
	TSNet         *tsnet.Server
	Started       time.Time
}

// Run starts both listeners and blocks until ctx is cancelled or one of them
// fails. Returns nil on clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	tlsLn, err := s.TSNet.ListenTLS("tcp", ":443")
	if err != nil {
		return fmt.Errorf("tailnet ListenTLS :443: %w", err)
	}
	tlsSrv := &http.Server{
		Handler:           s.tailnetMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { errCh <- runHTTP("tailnet", s.Logger, tlsSrv, tlsLn) }()
	s.Logger.Info("local tailnet listener ready", "addr", "https://"+tlsLn.Addr().String())

	var metricsSrv *http.Server
	if s.MetricsAddr != "" {
		mLn, err := net.Listen("tcp", s.MetricsAddr)
		if err != nil {
			_ = tlsSrv.Close()
			return fmt.Errorf("metrics listen %s: %w", s.MetricsAddr, err)
		}
		metricsSrv = &http.Server{
			Handler:           s.metricsMux(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() { errCh <- runHTTP("metrics", s.Logger, metricsSrv, mLn) }()
		s.Logger.Info("local metrics listener ready", "addr", "http://"+mLn.Addr().String()+"/metrics")
	}

	var firstErr error
	select {
	case <-ctx.Done():
	case firstErr = <-errCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tlsSrv.Shutdown(shutdownCtx)
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}

	return firstErr
}

func (s *Server) tailnetMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.statusHandler()(w, r)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		hint := "tsserve: /metrics is served on the loopback listener"
		if s.MetricsAddr != "" {
			hint += " (default 127.0.0.1:9090; override with TSSERVE_METRICS_ADDR).\nCurrent: " + s.MetricsAddr + "\n"
		} else {
			hint += " — currently disabled (TSSERVE_METRICS_ADDR is empty).\n"
		}
		http.Error(w, hint, http.StatusNotFound)
	})
	th := traefikHandler(s.TraefikPort)
	mux.Handle("/traefik", th)
	mux.Handle("/traefik/", th)
	return mux
}

func (s *Server) metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(s.Metrics.Registry(), promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "tsserve metrics endpoint — see /metrics")
	})
	return mux
}

func runHTTP(label string, logger *slog.Logger, srv *http.Server, ln net.Listener) error {
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	if err != nil {
		logger.Warn("local http listener exited", "listener", label, "err", err)
	}
	return err
}
