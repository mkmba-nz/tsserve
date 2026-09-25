package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"mkmba.nz/tsserve/metrics"
)

// FatalError is returned by [Manager.RegisterHostPort] and [Manager.RegisterFunction] when a configuration problem
// makes further service registration pointless (for example HTTPS or MagicDNS
// being disabled in the admin console). Callers should propagate it and exit.
type FatalError struct{ err error }

func (e *FatalError) Error() string { return e.err.Error() }
func (e *FatalError) Unwrap() error { return e.err }

// ConflictError is returned by [Manager.RegisterHostPort] and [Manager.RegisterFunction] when a backend's
// service-level labels disagree with the service that is already advertised
// under that name. The backend is refused: it joins no pool and is recorded
// nowhere in the manager. Callers can distinguish it from a listen failure and
// log it as a misconfiguration.
//
// Scheme and caps are properties of the advertised service — caps in
// particular are fixed when the listener opens, since tsnet takes them as part
// of the ServiceMode — so pool members must agree on both to be
// interchangeable. The backend port is deliberately not compared: in ECS
// bridge mode each task is assigned its own dynamic host port.
type ConflictError struct {
	Service string
	// Key is the refused registration key.
	Key string
	// Field names the label that differs ("scheme" or "caps").
	Field string
	// Advertised is the advertised service's value, Offered the refused
	// backend's.
	Advertised string
	Offered    string
}

func (e *ConflictError) Error() string {
	// The full key, not short(): an ECS key is "<taskArn>#<containerName>", and
	// shortening it keeps only a slice of the task ID, dropping the container
	// name that says which of a task's containers was refused.
	return fmt.Sprintf("backend %s refused for service %s: %s=%q conflicts with the advertised service's %s=%q",
		e.Key, e.Service, e.Field, e.Offered, e.Field, e.Advertised)
}

// errShuttingDown is returned by a registration once Close has run.
var errShuttingDown = errors.New("manager is shutting down")

// Manager owns the tsnet.Server and the live set of advertised Tailscale
// Services, each with its backend pool and reverse proxy. It is safe for
// concurrent use.
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

	// serveMu serialises every call that mutates the node's serve config:
	// opening a service listener, and closing one. tsnet applies those edits as
	// a read-modify-write guarded by an ETag, so two of them in flight at once —
	// two services advertising at start-up, an advertisement racing a withdrawal
	// — make one of them fail with an etag mismatch, leaving a running backend
	// unserved. Held strictly around the tsnet call and never while mu is held,
	// so there is no lock ordering to reason about.
	serveMu sync.Mutex

	mu       sync.Mutex
	services map[string]*advertisedService // service name -> advertised service
	byKey    map[string]string             // registration key -> service name
	shutdown bool
}

// advertisedService is one Tailscale Service tsserve holds a listener for,
// together with the pool of backends serving it. The listener, the HTTP server
// and the reverse proxy are created once, when the service is first
// advertised, and shared by every backend that joins afterwards.
type advertisedService struct {
	name string
	// scheme and caps are fixed by the registration that opened the listener.
	scheme string
	caps   []string

	pool *backendPool

	// A record is in exactly one of three states, all guarded by Manager.mu, and
	// a registration that finds the name occupied waits for whichever transition is
	// in flight rather than opening a second listener for it:
	//
	//	advertising  ready == false            -> wait on advertised
	//	serving      ready == true             -> join the pool
	//	withdrawing  withdrawing != nil        -> wait on withdrawing
	//
	// Both waits matter to correctness. tsnet refuses ListenService while a
	// handler for the service's port is still in the serve config, so a registration
	// that opened a listener before the outgoing one had finished closing would
	// fail outright, leaving the service dark until discovery came round again.
	ready bool
	// advertised is closed once the ListenService call has finished, whether it
	// succeeded or not.
	advertised chan struct{}
	// withdrawing is created when the last backend leaves and closed once the
	// listener is shut and the record has left the service index.
	withdrawing chan struct{}

	cancel  context.CancelFunc
	closeFn func() error
}

// ServiceView is a defensive copy of an advertised service's state, returned by
// [Manager.Snapshot] for read-only consumers (e.g. the status page).
type ServiceView struct {
	Service  string
	Scheme   string
	Caps     []string
	Backends []BackendView
}

// BackendView is one member of an advertised service's backend pool.
type BackendView struct {
	// Backend is the target a request is proxied to: "scheme://host:port" for
	// a container backend, "lambda://<function ARN>[:<qualifier>]" for a
	// function backend.
	Backend string
	// Key is the watcher-supplied registration key.
	Key          string
	RegisteredAt time.Time
	Origin       Origin
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
		logger:   logger,
		metrics:  mc,
		services: map[string]*advertisedService{},
		byKey:    map[string]string{},
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
	// Origin identifies the discovery source this service came from (which ECS
	// reader/account/cluster). It is zero for Docker discovery.
	Origin Origin
}

// Origin records which discovery source advertised a service. For ECS
// discovery it names the reader (account/cluster) the task was found in; for
// Docker discovery it is left zero.
type Origin struct {
	Account string
	Cluster string
	Region  string
}

// RegisterHostPort adds backendIP:def.Port to the backend pool of def.Service,
// advertising the service (opening its Tailscale Service listener) if this is
// its first backend. It is idempotent over the registration key: repeat calls
// for a key already in a pool are a no-op.
//
// A nil return means the backend is a member of the pool of a service with an
// open listener. Every other outcome returns an error: a *FatalError for a
// configuration problem the caller should exit on, a *ConflictError when the
// backend's labels disagree with the advertised service, and a plain error when
// the listener could not be opened. Callers are expected to retry the latter
// two on their next discovery cycle rather than record the backend as active.
func (m *Manager) RegisterHostPort(key string, def *ServiceDef, backendIP string) error {
	return m.register(key, def, &backend{
		key:          key,
		addr:         net.JoinHostPort(backendIP, strconv.Itoa(int(def.Port))),
		registeredAt: time.Now(),
		origin:       def.Origin,
	})
}

// SchemeLambda is the scheme of an advertised service backed by functions. It
// is internal: no container label or function tag can name it, so the scheme
// comparison in conflictWith keeps every pool homogeneous — a container backend
// is refused by a function-backed service and a function by a container-backed
// one.
const SchemeLambda = "lambda"

// FunctionDef describes a function backend: a Lambda function registered
// against an advertised service. It has no address; every request to it is
// performed by Invoker, whose Target names it in logs and on the status page.
type FunctionDef struct {
	Service string
	Caps    []string
	Origin  Origin
	Invoker *FunctionInvoker
}

// RegisterFunction adds a function backend to the pool of def.Service,
// advertising the service with the lambda scheme if this is its first backend.
// Its contract — idempotence over key, the errors returned, and what a nil
// return means — is RegisterHostPort's.
func (m *Manager) RegisterFunction(key string, def *FunctionDef) error {
	if def.Invoker == nil {
		return fmt.Errorf("function backend %s has no invoker", key)
	}
	return m.register(key, &ServiceDef{
		Service: def.Service,
		Scheme:  SchemeLambda,
		Caps:    def.Caps,
		Origin:  def.Origin,
	}, &backend{
		key:          key,
		registeredAt: time.Now(),
		origin:       def.Origin,
		invoker:      def.Invoker,
		target:       def.Invoker.Target(),
	})
}

// register adds b to the pool of def.Service; see RegisterHostPort. Only def's
// service-level fields (Service, Scheme, Caps) are read.
func (m *Manager) register(key string, def *ServiceDef, b *backend) error {
	for {
		m.mu.Lock()
		if m.shutdown {
			m.mu.Unlock()
			return errShuttingDown
		}
		if _, exists := m.byKey[key]; exists {
			m.mu.Unlock()
			return nil // already a pool member
		}

		rec, occupied := m.services[def.Service]
		if !occupied {
			// First backend for this name: claim it before releasing the lock so
			// a concurrent registration waits for our listener instead of opening a
			// second one.
			rec = &advertisedService{
				name:       def.Service,
				scheme:     def.Scheme,
				caps:       append([]string(nil), def.Caps...),
				pool:       newBackendPool(),
				advertised: make(chan struct{}),
			}
			m.services[def.Service] = rec
			m.mu.Unlock()
			return m.advertise(rec, key, b)
		}

		// The name is occupied by a transition in flight; wait for it and
		// re-evaluate. On a successful advertise we join the new pool; after a
		// failed advertise or a completed withdrawal the record is gone and we
		// become the advertiser.
		if wait := rec.transition(); wait != nil {
			m.mu.Unlock()
			<-wait
			continue
		}

		if err := rec.conflictWith(key, def); err != nil {
			m.mu.Unlock()
			return err
		}
		size := rec.pool.add(b)
		m.byKey[key] = rec.name
		m.setPoolSize(rec.name, size)
		m.mu.Unlock()

		m.logger.Info("backend joined service",
			"service", rec.name,
			"backend", b.targetURL(rec.scheme),
			"key", short(key),
			"pool_size", size)
		return nil
	}
}

// advertise opens the listener for a service claimed by this caller and starts
// serving it with b as its first backend. On failure the claim is released so a
// later registration can retry the name.
func (m *Manager) advertise(rec *advertisedService, key string, b *backend) error {
	mode := tsnet.ServiceModeHTTP{
		HTTPS: true,
		Port:  443,
	}
	if len(rec.caps) > 0 {
		mode.AcceptAppCaps = map[string][]string{"/": rec.caps}
	}

	m.serveMu.Lock()
	rawLn, err := m.listenSvc(rec.name, mode)
	m.serveMu.Unlock()
	if err != nil {
		m.abandon(rec)
		if isMagicDNSError(err) {
			return &FatalError{err: fmt.Errorf(
				"ListenService(%s) failed: %w: enable HTTPS Certificates and MagicDNS under DNS in the Tailscale admin console, and define the service under Services on TCP port 443",
				rec.name, err)}
		}
		return fmt.Errorf("ListenService(%s): %w", rec.name, err)
	}

	// Curry the service label once rather than resolving it per failed attempt,
	// and keep the closure free of any reference to the manager or the record.
	onError := func(reason string) {}
	if m.metrics != nil {
		errs := m.metrics.Errors.MustCurryWith(prometheus.Labels{"service": rec.name})
		onError = func(reason string) { errs.WithLabelValues(reason).Inc() }
	}

	// Scope the proxy's logger to this service: its error lines have no single
	// backend to name (an empty pool, a protocol upgrade that failed after the
	// response began), so the service is what makes them attributable.
	svcLogger := m.logger.With("service", rec.name)
	rp := newReverseProxy(rec.scheme, rec.pool, svcLogger, onError, m.whois)
	var handler http.Handler = rp
	handler = m.metrics.Middleware(rec.name, handler)

	// Every close of this listener — ours below, http.Server.Close's, and the
	// one http.Server.Serve makes on its way out — withdraws the service's
	// handler from the serve config, so the lock belongs on the listener rather
	// than on any one caller that remembers to take it.
	ln := &serialisedListener{Listener: rawLn, mu: &m.serveMu}

	httpSrv := &http.Server{Handler: handler}
	ctx, cancel := context.WithCancel(context.Background())
	closeFn := func() error {
		_ = httpSrv.Close()
		// This service's transport is its own, so its keep-alive connections
		// to the departed backends have no other user. Without this a service
		// that is withdrawn and re-advertised — a replaced task, a Docker
		// restart — leaves its idle sockets behind each time.
		closeIdleConns(rp)
		return ln.Close()
	}

	m.mu.Lock()
	if m.shutdown {
		m.releaseLocked(rec)
		m.mu.Unlock()
		cancel()
		_ = closeFn()
		return errShuttingDown
	}
	rec.cancel = cancel
	rec.closeFn = closeFn
	size := rec.pool.add(b)
	rec.ready = true
	close(rec.advertised)
	m.byKey[key] = rec.name
	m.serviceAdvertised(rec.name, size)
	m.mu.Unlock()

	go func() {
		// http.Serve on the tsnet ServiceListener: TLS is already terminated
		// inside the listener, so this is plain http.Serve.
		err := httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
			m.logger.Warn("service http.Serve returned", "service", rec.name, "err", err)
		}
	}()

	m.logger.Info("service advertised",
		"service", rec.name,
		"backend", b.targetURL(rec.scheme),
		"caps", rec.caps,
		"key", short(key),
		"pool_size", size)
	return nil
}

// serialisedListener is a service listener whose Close takes the serve-config
// lock. Closing a tsnet ServiceListener removes the service's handler from the
// node's serve config with the same ETag-guarded compare-and-swap ListenService
// uses, and the close arrives from more than one place: the manager's own
// teardown, http.Server.Close, and http.Server.Serve closing the listener it
// was handed when it returns. Wrapping the listener covers all of them, so no
// future caller has to remember the rule.
type serialisedListener struct {
	net.Listener
	mu *sync.Mutex
}

func (l *serialisedListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Listener.Close()
}

// abandon releases a claim on a service name whose listener could not be
// opened, waking any registration waiting on it.
func (m *Manager) abandon(rec *advertisedService) {
	m.mu.Lock()
	m.releaseLocked(rec)
	m.mu.Unlock()
}

// closeIdleConns releases the idle connections held by a proxy's transport,
// using the same interface check http.Client.CloseIdleConnections applies.
func closeIdleConns(rp *httputil.ReverseProxy) {
	if tr, ok := rp.Transport.(interface{ CloseIdleConnections() }); ok {
		tr.CloseIdleConnections()
	}
}

// releaseLocked drops rec from the service index (if it is still the record
// held there) and wakes its waiters. Callers hold m.mu.
func (m *Manager) releaseLocked(rec *advertisedService) {
	if cur, ok := m.services[rec.name]; ok && cur == rec {
		delete(m.services, rec.name)
	}
	if !rec.ready {
		close(rec.advertised)
	}
}

// transition returns the channel to wait on when this record is mid-advertise
// or mid-withdrawal, and nil when it is serving and can be joined. Callers hold
// m.mu.
func (rec *advertisedService) transition() <-chan struct{} {
	switch {
	case rec.withdrawing != nil:
		return rec.withdrawing
	case !rec.ready:
		return rec.advertised
	default:
		return nil
	}
}

// conflictWith reports whether def's service-level labels disagree with the
// already-advertised service. Callers hold m.mu.
func (rec *advertisedService) conflictWith(key string, def *ServiceDef) error {
	if def.Scheme != rec.scheme {
		return &ConflictError{
			Service: rec.name, Key: key, Field: "scheme",
			Advertised: rec.scheme, Offered: def.Scheme,
		}
	}
	if !sameCaps(rec.caps, def.Caps) {
		return &ConflictError{
			Service: rec.name, Key: key, Field: "caps",
			Advertised: strings.Join(sortedCaps(rec.caps), ","),
			Offered:    strings.Join(sortedCaps(def.Caps), ","),
		}
	}
	return nil
}

// sameCaps compares two capability lists as sets: order and repetition do not
// make two backends of one service incompatible. Capability lists have a
// handful of entries at most, so a scan beats building sets.
func sameCaps(a, b []string) bool {
	return capsSubset(a, b) && capsSubset(b, a)
}

func capsSubset(a, b []string) bool {
	for _, c := range a {
		if !slices.Contains(b, c) {
			return false
		}
	}
	return true
}

// sortedCaps renders a capability list in a stable order for error messages.
func sortedCaps(caps []string) []string {
	return slices.Sorted(slices.Values(caps))
}

// Deregister removes one backend from its service's pool. The listener and
// HTTP server stay up — along with in-flight requests to other members — while
// any backend remains; the service is torn down when its last backend leaves.
// Calls with an unknown registration key are a no-op.
func (m *Manager) Deregister(key string) {
	m.mu.Lock()
	name, ok := m.byKey[key]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.byKey, key)
	rec, ok := m.services[name]
	if !ok || !rec.ready {
		// A key only reaches byKey once its service is serving, so this is
		// unreachable; guard rather than trust it, since the teardown below
		// calls cancel/closeFn, which a record that never finished advertising
		// has not set.
		m.mu.Unlock()
		return
	}
	var target string
	for _, b := range rec.pool.list() {
		if b.key == key {
			target = b.targetURL(rec.scheme)
		}
	}
	size := rec.pool.remove(key)
	if size > 0 {
		m.setPoolSize(name, size)
		m.mu.Unlock()
		m.logger.Info("backend left service", "service", name, "backend", target, "key", short(key), "pool_size", size)
		return
	}
	// The pool is empty: mark the record withdrawing and keep it in the index
	// while the listener closes. A registration arriving now waits on that channel
	// rather than calling ListenService, which tsnet would refuse while this
	// service's handler is still in the serve config.
	rec.withdrawing = make(chan struct{})
	m.mu.Unlock()

	rec.cancel()
	if err := rec.closeFn(); err != nil {
		m.logger.Warn("error closing service listener", "service", name, "err", err)
	}

	m.mu.Lock()
	if cur, ok := m.services[name]; ok && cur == rec {
		delete(m.services, name)
		// The gauges belong to whoever drops the record. A Close that ran
		// while this teardown was in flight has already emptied the index and
		// zeroed both gauges, and decrementing on top of that would leave
		// tsserve_services_active at -1.
		m.serviceWithdrawn(name)
	}
	close(rec.withdrawing)
	m.mu.Unlock()

	m.logger.Info("service withdrawn; last backend left",
		"service", name, "backend", target, "key", short(key), "pool_size", 0)
}

// Snapshot returns a defensive copy of the advertised service set, sorted by
// service name, each with its backend pool in pool order. Intended for the
// status page and other read-only consumers.
func (m *Manager) Snapshot() []ServiceView {
	m.mu.Lock()
	out := make([]ServiceView, 0, len(m.services))
	for _, rec := range m.services {
		if !rec.ready || rec.withdrawing != nil {
			// Either the listener is still being opened, or the pool has
			// emptied and the record is only still here so a racing registration
			// waits for the listener to close. Neither has a backend to show,
			// and every ServiceView is meant to carry at least one.
			continue
		}
		members := rec.pool.list()
		backends := make([]BackendView, 0, len(members))
		for _, b := range members {
			backends = append(backends, BackendView{
				Backend:      b.targetURL(rec.scheme),
				Key:          b.key,
				RegisteredAt: b.registeredAt,
				Origin:       b.origin,
			})
		}
		out = append(out, ServiceView{
			Service:  rec.name,
			Scheme:   rec.scheme,
			Caps:     append([]string(nil), rec.caps...),
			Backends: backends,
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Close tears down every advertised service exactly once. It is safe to call
// once.
func (m *Manager) Close() {
	m.mu.Lock()
	m.shutdown = true
	recs := make([]*advertisedService, 0, len(m.services))
	for _, rec := range m.services {
		// Records still inside ListenService tear themselves down when they see
		// the shutdown flag, and a withdrawing one is already being torn down
		// by the Deregister that emptied its pool. Closing either here would
		// run cancel/closeFn a second time, concurrently with the first.
		if rec.ready && rec.withdrawing == nil {
			recs = append(recs, rec)
		}
	}
	m.services = map[string]*advertisedService{}
	m.byKey = map[string]string{}
	m.allServicesWithdrawn()
	m.mu.Unlock()

	for _, rec := range recs {
		rec.cancel()
		if err := rec.closeFn(); err != nil {
			m.logger.Warn("error closing service on shutdown", "service", rec.name, "err", err)
		}
	}
}

// The gauges below are pure functions of the advertised-service set. Every
// path that changes that set goes through exactly one of these, so a new path
// cannot leave a stale series behind. Each is a no-op when no collector is
// wired.

// setPoolSize publishes a service's current pool size.
func (m *Manager) setPoolSize(service string, n int) {
	if m.metrics != nil {
		m.metrics.Backends.WithLabelValues(service).Set(float64(n))
	}
}

// serviceAdvertised records a service whose listener has just opened, with its
// initial pool size.
func (m *Manager) serviceAdvertised(service string, n int) {
	if m.metrics != nil {
		m.metrics.Active.Inc()
	}
	m.setPoolSize(service, n)
}

// serviceWithdrawn records a service whose last backend has left. Its pool-size
// series is deleted rather than zeroed, so the exposition does not keep a value
// for a service that no longer exists.
func (m *Manager) serviceWithdrawn(service string) {
	if m.metrics != nil {
		m.metrics.Active.Dec()
		m.metrics.Backends.DeleteLabelValues(service)
	}
}

// allServicesWithdrawn clears both gauges on shutdown.
func (m *Manager) allServicesWithdrawn() {
	if m.metrics != nil {
		m.metrics.Active.Set(0)
		m.metrics.Backends.Reset()
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
	// A Lambda function ARN has no path: log the whole function name, which
	// is what tells two members of one pool apart, rather than the constant
	// "arn:aws:lamb" prefix.
	if _, fn, ok := strings.Cut(id, ":function:"); ok && strings.HasPrefix(id, "arn:") {
		return fn
	}
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
