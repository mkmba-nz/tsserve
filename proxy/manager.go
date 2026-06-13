package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"mkmba.nz/tsserve/metrics"
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
	// listenSvc opens a Tailscale Service listener. In production this calls
	// (*tsnet.Server).ListenService; tests swap it for an in-memory listener
	// factory.
	listenSvc func(name string, mode tsnet.ServiceMode) (net.Listener, error)
	// whois resolves a connecting peer's identity for node-header injection. In
	// production this is (*local.Client).WhoIs; tests inject a stub. It may be
	// nil, in which case no node headers are injected (they are still stripped).
	whois   whoisFunc
	logger  *slog.Logger
	metrics *metrics.Collector

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

// defSnapshot is the immutable per-service state we need to render the status
// page and clean up at deregistration time.
type defSnapshot struct {
	Service      string
	Backend      string // "scheme://ip:port"
	Scheme       string
	BackendHost  string
	Port         uint16
	Caps         []string
	ContainerID  string
	RegisteredAt time.Time
}

// ServiceView is a defensive copy of an active service's state, returned by
// [Manager.Snapshot] for read-only consumers (e.g. the status page).
type ServiceView struct {
	Service      string
	Backend      string
	Scheme       string
	BackendHost  string
	Port         uint16
	Caps         []string
	ContainerID  string
	RegisteredAt time.Time
}

// NewManager wraps a tsnet.Server that has already started successfully.
// mc may be nil; when nil no metrics are recorded. lc is the tsnet LocalClient
// used to resolve connecting peers for node-header injection; it may be nil, in
// which case no node headers are injected.
func NewManager(srv *tsnet.Server, lc *local.Client, logger *slog.Logger, mc *metrics.Collector) *Manager {
	m := &Manager{
		listenSvc: func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
			return srv.ListenService(name, mode)
		},
		logger:  logger,
		metrics: mc,
		byCID:   map[string]*activeService{},
		byName:  map[string]string{},
	}
	if lc != nil {
		m.whois = lc.WhoIs
	}
	return m
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

	ln, err := m.listenSvc(def.Service, mode)
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

	onError := func(reason string) {}
	if m.metrics != nil {
		onError = func(reason string) {
			m.metrics.Errors.WithLabelValues(def.Service, reason).Inc()
		}
	}

	// Node-header injection rides on the same opt-in as app capabilities: when a
	// service requests caps, tsserve also resolves the connecting peer and
	// injects Tailscale-Node-Tags/Name for tagged nodes.
	injectNode := len(def.Caps) > 0
	var handler http.Handler = newReverseProxy(def.Scheme, backendIP, def.Port, m.logger, onError, injectNode, m.whois)
	handler = m.metrics.Middleware(def.Service, handler)

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

	snap := defSnapshot{
		Service:      def.Service,
		Backend:      fmt.Sprintf("%s://%s:%d", def.Scheme, backendIP, def.Port),
		Scheme:       def.Scheme,
		BackendHost:  backendIP,
		Port:         def.Port,
		Caps:         append([]string(nil), def.Caps...),
		ContainerID:  containerID,
		RegisteredAt: time.Now(),
	}

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		cancel()
		_ = httpSrv.Close()
		_ = ln.Close()
		return errors.New("manager is shutting down")
	}
	m.byCID[containerID] = &activeService{
		def:    snap,
		cancel: cancel,
		closeFn: func() error {
			_ = httpSrv.Close()
			return ln.Close()
		},
	}
	m.byName[def.Service] = containerID
	m.mu.Unlock()

	if m.metrics != nil {
		m.metrics.Active.Inc()
	}

	m.logger.Info("service registered",
		"service", def.Service,
		"backend", snap.Backend,
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
	if m.metrics != nil {
		m.metrics.Active.Dec()
	}
	m.logger.Info("service deregistered", "service", svc.def.Service, "container", short(containerID))
}

// Snapshot returns a defensive copy of the active service set, sorted by
// service name. Intended for the status page and other read-only consumers.
func (m *Manager) Snapshot() []ServiceView {
	m.mu.Lock()
	out := make([]ServiceView, 0, len(m.byCID))
	for _, s := range m.byCID {
		out = append(out, ServiceView{
			Service:      s.def.Service,
			Backend:      s.def.Backend,
			Scheme:       s.def.Scheme,
			BackendHost:  s.def.BackendHost,
			Port:         s.def.Port,
			Caps:         append([]string(nil), s.def.Caps...),
			ContainerID:  s.def.ContainerID,
			RegisteredAt: s.def.RegisteredAt,
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
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
	if m.metrics != nil {
		m.metrics.Active.Set(0)
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
	// Strip ARN-style path prefix so ECS task ARNs log their task UUID
	// rather than the constant "arn:aws:ecs:" prefix.
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
