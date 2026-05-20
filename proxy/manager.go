package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"tailscale.com/tsnet"
)

// FatalError is returned by [Manager.Register] when a configuration problem
// makes further service registration pointless (for example HTTPS or MagicDNS
// being disabled in the admin console). Callers should propagate it and exit.
type FatalError struct{ err error }

func (e *FatalError) Error() string { return e.err.Error() }
func (e *FatalError) Unwrap() error { return e.err }

// Manager owns the tsnet.Server and the live set of Tailscale Service
// listeners and reverse proxies. It is safe for concurrent use.
type Manager struct {
	srv    *tsnet.Server
	logger *slog.Logger

	mu       sync.Mutex
	byCID    map[string]*activeService // containerID -> service
	byName   map[string]string         // serviceName -> containerID (winner)
	shutdown bool
}

type activeService struct {
	def     defSnapshot
	cancel  context.CancelFunc
	closeFn func() error
}

// defSnapshot is the subset of docker.ServiceDef we keep on the active service.
// We avoid the cross-package import by copying only the fields we need.
type defSnapshot struct {
	Service string
	Caps    []string
}

// NewManager wraps a tsnet.Server that has already started successfully.
func NewManager(srv *tsnet.Server, logger *slog.Logger) *Manager {
	return &Manager{
		srv:    srv,
		logger: logger,
		byCID:  map[string]*activeService{},
		byName: map[string]string{},
	}
}

// ServiceDef mirrors docker.ServiceDef so the proxy package does not import
// the docker package directly. The watcher converts before calling.
type ServiceDef struct {
	Service string
	Port    uint16
	Network string
	Scheme  string
	Caps    []string
}

// Register provisions a Tailscale Service listener for def and begins serving
// reverse-proxied traffic to backendIP:def.Port. It is idempotent over the
// (containerID, serviceName) pair: callers may safely call it repeatedly.
func (m *Manager) Register(containerID string, def *ServiceDef, backendIP string) error {
	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		return errors.New("manager is shutting down")
	}
	if _, exists := m.byCID[containerID]; exists {
		m.mu.Unlock()
		return nil // already registered
	}
	if owner, taken := m.byName[def.Service]; taken {
		m.mu.Unlock()
		m.logger.Warn("duplicate service name; ignoring",
			"service", def.Service,
			"holder", short(owner),
			"duplicate", short(containerID))
		return nil
	}
	m.mu.Unlock()

	mode := tsnet.ServiceModeHTTP{
		HTTPS: true,
		Port:  443,
	}
	if len(def.Caps) > 0 {
		mode.AcceptAppCaps = map[string][]string{"/": def.Caps}
	}

	ln, err := m.srv.ListenService(def.Service, mode)
	if err != nil {
		if isMagicDNSError(err) {
			return &FatalError{err: fmt.Errorf(
				"ListenService(%s) failed: %w: enable HTTPS Certificates and MagicDNS under DNS in the Tailscale admin console, and define the service under Services on TCP port 443",
				def.Service, err)}
		}
		m.logger.Error("ListenService failed; skipping container",
			"service", def.Service, "container", short(containerID), "err", err)
		return nil
	}

	var handler http.Handler = newReverseProxy(def.Scheme, backendIP, def.Port, m.logger)

	httpSrv := &http.Server{Handler: handler}
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// http.Serve on the tsnet ServiceListener: TLS is already terminated
		// inside the listener, so this is plain http.Serve.
		err := httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
			m.logger.Warn("service http.Serve returned", "service", def.Service, "err", err)
		}
	}()

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		cancel()
		_ = httpSrv.Close()
		_ = ln.Close()
		return errors.New("manager is shutting down")
	}
	m.byCID[containerID] = &activeService{
		def:    defSnapshot{Service: def.Service, Caps: def.Caps},
		cancel: cancel,
		closeFn: func() error {
			_ = httpSrv.Close()
			return ln.Close()
		},
	}
	m.byName[def.Service] = containerID
	m.mu.Unlock()

	m.logger.Info("service registered",
		"service", def.Service,
		"backend", fmt.Sprintf("%s://%s:%d", def.Scheme, backendIP, def.Port),
		"caps", def.Caps,
		"container", short(containerID))
	return nil
}

// Deregister tears down the listener and reverse proxy for the given
// container, if one was registered. Calls with an unknown containerID are a
// no-op.
func (m *Manager) Deregister(containerID string) {
	m.mu.Lock()
	svc, ok := m.byCID[containerID]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.byCID, containerID)
	if owner, taken := m.byName[svc.def.Service]; taken && owner == containerID {
		delete(m.byName, svc.def.Service)
	}
	m.mu.Unlock()

	svc.cancel()
	if err := svc.closeFn(); err != nil {
		m.logger.Warn("error closing service listener", "service", svc.def.Service, "err", err)
	}
	m.logger.Info("service deregistered", "service", svc.def.Service, "container", short(containerID))
}

// Close tears down all active services. It is safe to call once.
func (m *Manager) Close() {
	m.mu.Lock()
	m.shutdown = true
	services := make([]*activeService, 0, len(m.byCID))
	for _, s := range m.byCID {
		services = append(services, s)
	}
	m.byCID = map[string]*activeService{}
	m.byName = map[string]string{}
	m.mu.Unlock()

	for _, s := range services {
		s.cancel()
		if err := s.closeFn(); err != nil {
			m.logger.Warn("error closing service on shutdown", "service", s.def.Service, "err", err)
		}
	}
}

func isMagicDNSError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "magicdns") ||
		strings.Contains(s, "https certificates") ||
		strings.Contains(s, "https is not enabled") ||
		strings.Contains(s, "https not enabled")
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
