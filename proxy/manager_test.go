package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"tailscale.com/tsnet"

	"mkmba.nz/tsserve/metrics"
)

// fakeListenSvc returns in-memory listeners. Each call records the (name, mode)
// requested. The latest listener created for a name is tracked so tests can
// wait on its Close.
type fakeListenSvc struct {
	mu        sync.Mutex
	requests  []fakeListenReq
	errOn     map[string]error         // serviceName -> err to return for that name
	listeners map[string]*pipeListener // serviceName -> most recent listener
}

type fakeListenReq struct {
	name string
	mode tsnet.ServiceMode
}

func newFakeListenSvc() *fakeListenSvc {
	return &fakeListenSvc{
		errOn:     map[string]error{},
		listeners: map[string]*pipeListener{},
	}
}

func (f *fakeListenSvc) failOn(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errOn[name] = err
}

// waitClose returns a channel that's closed when the most recent listener for
// the given service name is closed. If no listener has been created yet, it
// returns a channel that will never fire — call after Register.
func (f *fakeListenSvc) waitClose(name string) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ln, ok := f.listeners[name]; ok {
		return ln.closed
	}
	never := make(chan struct{})
	return never
}

func (f *fakeListenSvc) listenFn() func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
	return func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
		f.mu.Lock()
		f.requests = append(f.requests, fakeListenReq{name: name, mode: mode})
		err := f.errOn[name]
		// tsnet refuses a second handler for a service's port while the previous
		// one is still in the serve config, i.e. until the outgoing
		// ServiceListener has been closed. Model that here: a fake that always
		// succeeds would hide any code path that opens a listener before the
		// one it replaces has finished closing.
		if prev, ok := f.listeners[name]; ok && err == nil && !prev.isClosed() {
			err = errServiceHandlerExists
		}
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		ln := &pipeListener{closed: make(chan struct{})}
		f.mu.Lock()
		f.listeners[name] = ln
		f.mu.Unlock()
		return ln, nil
	}
}

// errServiceHandlerExists mirrors the error tsnet's ListenService returns when
// a handler for the service's port is still registered.
var errServiceHandlerExists = errors.New("a Service handler already exists for this port")

// pipeListener: minimal net.Listener that blocks Accept until Close. Each
// instance owns its own close channel; closing multiple times is safe.
type pipeListener struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func (p *pipeListener) Accept() (net.Conn, error) {
	<-p.closed
	return nil, net.ErrClosed
}

func (p *pipeListener) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func (p *pipeListener) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

func (p *pipeListener) Addr() net.Addr { return fakeAddr{} }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

func newTestManager(fake *fakeListenSvc) *Manager {
	return &Manager{
		listenSvc: fake.listenFn(),
		logger:    discardLogger(),
		services:  map[string]*advertisedService{},
		byKey:     map[string]string{},
	}
}

// listenCount reports how many times ListenService was called for a name.
func (f *fakeListenSvc) listenCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.name == name {
			n++
		}
	}
	return n
}

// backendsOf returns the "scheme://host:port" of each pool member of the named
// service, in pool order.
func backendsOf(t *testing.T, m *Manager, service string) []string {
	t.Helper()
	for _, v := range m.Snapshot() {
		if v.Service != service {
			continue
		}
		out := make([]string, 0, len(v.Backends))
		for _, b := range v.Backends {
			out = append(out, b.Backend)
		}
		return out
	}
	return nil
}

func TestManager_RegisterAddsServiceToSnapshot(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort: %v", err)
	}

	views := m.Snapshot()
	if len(views) != 1 {
		t.Fatalf("snapshot len = %d, want 1: %+v", len(views), views)
	}
	v := views[0]
	if v.Service != "svc:web" {
		t.Errorf("Service = %q", v.Service)
	}
	if v.Scheme != "http" {
		t.Errorf("Scheme = %q, want http", v.Scheme)
	}
	if len(v.Backends) != 1 {
		t.Fatalf("backends = %d, want 1: %+v", len(v.Backends), v.Backends)
	}
	if v.Backends[0].Backend != "http://10.0.0.1:80" {
		t.Errorf("Backend = %q, want http://10.0.0.1:80", v.Backends[0].Backend)
	}
	if v.Backends[0].Key != "cid1" {
		t.Errorf("Key = %q, want cid1", v.Backends[0].Key)
	}
}

func TestManager_RegisterPassesAppCapsToListenService(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{
		Service: "svc:api",
		Port:    3000,
		Scheme:  "http",
		Caps:    []string{"example.com/cap/read", "example.com/cap/admin"},
	}
	if err := m.RegisterHostPort("cid1", def, "10.0.0.2"); err != nil {
		t.Fatalf("RegisterHostPort: %v", err)
	}

	fake.mu.Lock()
	req := fake.requests[0]
	fake.mu.Unlock()

	mode, ok := req.mode.(tsnet.ServiceModeHTTP)
	if !ok {
		t.Fatalf("mode type = %T, want ServiceModeHTTP", req.mode)
	}
	if !mode.HTTPS || mode.Port != 443 {
		t.Errorf("mode HTTPS=%v Port=%d, want true / 443", mode.HTTPS, mode.Port)
	}
	caps, ok := mode.AcceptAppCaps["/"]
	if !ok {
		t.Fatalf("AcceptAppCaps[\"/\"] missing: %+v", mode.AcceptAppCaps)
	}
	if len(caps) != 2 || caps[0] != "example.com/cap/read" || caps[1] != "example.com/cap/admin" {
		t.Errorf("caps = %v", caps)
	}
}

func TestManager_RegisterWithoutCapsLeavesAcceptAppCapsNil(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	_ = m.RegisterHostPort("cid1", def, "10.0.0.1")

	fake.mu.Lock()
	mode := fake.requests[0].mode.(tsnet.ServiceModeHTTP)
	fake.mu.Unlock()
	if mode.AcceptAppCaps != nil {
		t.Errorf("AcceptAppCaps = %v, want nil when Caps empty", mode.AcceptAppCaps)
	}
}

func TestManager_DuplicateRegistrationKeyIsNoOp(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("first RegisterHostPort: %v", err)
	}
	if err := m.RegisterHostPort("cid1", def, "10.0.0.99"); err != nil {
		t.Fatalf("second RegisterHostPort: %v", err)
	}
	if n := fake.listenCount("svc:web"); n != 1 {
		t.Errorf("ListenService called %d times for the same registration key, want 1", n)
	}
	if got := backendsOf(t, m, "svc:web"); len(got) != 1 || got[0] != "http://10.0.0.1:80" {
		t.Errorf("pool = %v; a repeat of the same key must not add or overwrite a backend", got)
	}
}

// A second registration key for a name that is already advertised joins that
// service's pool: one listener, two backends of equal standing.
func TestManager_SecondKeyForServiceJoinsPool(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid-first", def, "10.0.0.1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := m.RegisterHostPort("cid-second", def, "10.0.0.2"); err != nil {
		t.Fatalf("second: %v", err)
	}

	views := m.Snapshot()
	if len(views) != 1 {
		t.Fatalf("snapshot len = %d, want 1 advertised service: %+v", len(views), views)
	}
	if n := fake.listenCount("svc:web"); n != 1 {
		t.Errorf("ListenService called %d times, want 1 (one listener per advertised service)", n)
	}
	got := backendsOf(t, m, "svc:web")
	want := []string{"http://10.0.0.1:80", "http://10.0.0.2:80"}
	if len(got) != len(want) {
		t.Fatalf("pool = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pool[%d] = %q, want %q (registration order)", i, got[i], want[i])
		}
	}
	keys := []string{views[0].Backends[0].Key, views[0].Backends[1].Key}
	if keys[0] != "cid-first" || keys[1] != "cid-second" {
		t.Errorf("pool keys = %v, want [cid-first cid-second]", keys)
	}
}

// Backends join the same pool regardless of provenance: different readers
// (different Origin) and different ports are all pool members.
func TestManager_BackendsFromDifferentOriginsSharePool(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	readerADef := &ServiceDef{Service: "svc:api", Port: 32768, Scheme: "http",
		Origin: Origin{Account: "111111111111", Cluster: "cluster-a"}}
	readerBDef := &ServiceDef{Service: "svc:api", Port: 49153, Scheme: "http",
		Origin: Origin{Account: "222222222222", Cluster: "cluster-b"}}

	if err := m.RegisterHostPort("arn:aws:ecs:x:111111111111:task/cluster-a/a#api", readerADef, "10.0.0.1"); err != nil {
		t.Fatalf("reader A: %v", err)
	}
	if err := m.RegisterHostPort("arn:aws:ecs:x:222222222222:task/cluster-b/b#api", readerBDef, "10.1.0.1"); err != nil {
		t.Fatalf("reader B: %v", err)
	}

	views := m.Snapshot()
	if len(views) != 1 || len(views[0].Backends) != 2 {
		t.Fatalf("want 1 service with 2 backends, got %+v", views)
	}
	if n := fake.listenCount("svc:api"); n != 1 {
		t.Errorf("ListenService called %d times, want 1", n)
	}
	if got := views[0].Backends[0].Origin.Account; got != "111111111111" {
		t.Errorf("backend[0] account = %q", got)
	}
	if got := views[0].Backends[1].Origin.Account; got != "222222222222" {
		t.Errorf("backend[1] account = %q", got)
	}
	if got := views[0].Backends[1].Backend; got != "http://10.1.0.1:49153" {
		t.Errorf("backend[1] = %q, want http://10.1.0.1:49153 (divergent ports are accepted)", got)
	}
}

// Two concurrent Registers for a name that is not yet advertised must produce
// one listener, with the loser joining the winner's pool rather than opening a
// second listener for the same service.
func TestManager_ConcurrentRegisterOpensOneListener(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	// Hold the first ListenService call open until both goroutines are inside
	// Register, so the second necessarily races the first's listen.
	inner := fake.listenFn()
	release := make(chan struct{})
	var listens atomic.Int32
	m.listenSvc = func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
		if listens.Add(1) == 1 {
			<-release
		}
		return inner(name, mode)
	}

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	started := make(chan struct{}, 2)
	for i, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			errs[i] = m.RegisterHostPort(fmt.Sprintf("cid%d", i), def, ip)
		}()
	}
	// Both goroutines have entered Register, and the first is parked inside
	// ListenService. Give the second a moment to reach the point where it either
	// joins the first's claim or wrongly opens its own listener, then let the
	// first finish.
	<-started
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("RegisterHostPort %d: %v", i, err)
		}
	}
	if got := listens.Load(); got != 1 {
		t.Errorf("ListenService called %d times, want 1", got)
	}
	if got := backendsOf(t, m, "svc:web"); len(got) != 2 {
		t.Errorf("pool = %v, want 2 members", got)
	}
}

// A Register racing the teardown of the last pool member must end with a
// backend in the pool of an open listener — never attached to a record whose
// listener is being closed.
func TestManager_RegisterRacingLastDeregisterEndsServing(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	for round := 0; round < 100; round++ {
		if err := m.RegisterHostPort("cid-old", def, "10.0.0.1"); err != nil {
			t.Fatalf("seed Register: %v", err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		var regErr error
		go func() { defer wg.Done(); m.Deregister("cid-old") }()
		go func() { defer wg.Done(); regErr = m.RegisterHostPort("cid-new", def, "10.0.0.2") }()
		wg.Wait()
		if regErr != nil {
			t.Fatalf("round %d: Register: %v", round, regErr)
		}

		views := m.Snapshot()
		if len(views) != 1 {
			t.Fatalf("round %d: snapshot = %+v, want the service still advertised", round, views)
		}
		if len(views[0].Backends) != 1 || views[0].Backends[0].Key != "cid-new" {
			t.Fatalf("round %d: pool = %+v, want only cid-new", round, views[0].Backends)
		}
		// The listener the surviving backend is attached to must be open: a
		// request served through it would otherwise go nowhere.
		select {
		case <-fake.waitClose("svc:web"):
			t.Fatalf("round %d: surviving backend is attached to a closed listener", round)
		default:
		}
		m.Deregister("cid-new")
	}
}

func TestManager_DeregisterUnknownIsNoOp(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	m.Deregister("nonexistent") // must not panic
	if len(m.Snapshot()) != 0 {
		t.Errorf("snapshot not empty")
	}
}

func TestManager_DeregisterClosesListenerAndClearsState(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort: %v", err)
	}

	closeCh := fake.waitClose("svc:web")
	m.Deregister("cid1")
	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("listener was not closed within 1s")
	}

	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v, want empty", got)
	}

	// After deregistration, the service name should be free again.
	if err := m.RegisterHostPort("cid2", def, "10.0.0.2"); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if got := m.Snapshot()[0].Backends[0].Key; got != "cid2" {
		t.Errorf("new backend = %q, want cid2", got)
	}
}

// Removing one of two backends leaves the listener open and the service
// serving; removing the second closes it.
func TestManager_DeregisterClosesListenerOnlyOnLastBackend(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort cid1: %v", err)
	}
	if err := m.RegisterHostPort("cid2", def, "10.0.0.2"); err != nil {
		t.Fatalf("RegisterHostPort cid2: %v", err)
	}
	closeCh := fake.waitClose("svc:web")

	m.Deregister("cid1")
	select {
	case <-closeCh:
		t.Fatal("listener closed while a backend remained in the pool")
	case <-time.After(50 * time.Millisecond):
	}
	got := backendsOf(t, m, "svc:web")
	if len(got) != 1 || got[0] != "http://10.0.0.2:80" {
		t.Errorf("pool = %v, want only http://10.0.0.2:80", got)
	}

	m.Deregister("cid2")
	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("listener was not closed after the last backend left")
	}
	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v, want empty", got)
	}
}

// Close tears each advertised service down once, not once per backend.
func TestManager_CloseTearsDownPooledServiceOnce(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	for i, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if err := m.RegisterHostPort(fmt.Sprintf("cid%d", i), def, ip); err != nil {
			t.Fatalf("RegisterHostPort %d: %v", i, err)
		}
	}
	closeCh := fake.waitClose("svc:web")
	m.Close()
	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("listener not closed by Close")
	}
	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v, want empty after Close", got)
	}
}

// A non-fatal ListenService failure returns an error so the caller does not
// record the backend as active with nothing serving it.
func TestManager_RegisterListenServiceErrorIsReturnedNotFatal(t *testing.T) {
	fake := newFakeListenSvc()
	fake.failOn("svc:bad", errors.New("some transient error"))
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:bad", Port: 80, Scheme: "http"}
	err := m.RegisterHostPort("cid1", def, "10.0.0.1")
	if err == nil {
		t.Fatalf("want an error when ListenService fails, got nil")
	}
	var fe *FatalError
	if errors.As(err, &fe) {
		t.Fatalf("transient listen failure must not be fatal: %v", err)
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		t.Fatalf("listen failure must be distinguishable from a conflict: %v", err)
	}
	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v, want empty after failed Register", got)
	}
	// Service name must remain available for a future Register.
	fake.mu.Lock()
	delete(fake.errOn, "svc:bad")
	fake.mu.Unlock()
	if err := m.RegisterHostPort("cid2", &ServiceDef{Service: "svc:bad", Port: 80, Scheme: "http"}, "10.0.0.2"); err != nil {
		t.Fatalf("retry Register: %v", err)
	}
	if got := backendsOf(t, m, "svc:bad"); len(got) != 1 || got[0] != "http://10.0.0.2:80" {
		t.Errorf("pool = %v, want the retried backend serving", got)
	}
}

// A backend whose scheme or caps disagree with the advertised service is
// refused: it joins no pool, is recorded nowhere, and the existing members keep
// serving.
func TestManager_RefusesConflictingBackend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		def   *ServiceDef
		field string
	}{
		{"scheme", &ServiceDef{Service: "svc:web", Port: 80, Scheme: "https", Caps: []string{"example.com/cap/read"}}, "scheme"},
		{"caps", &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http", Caps: []string{"example.com/cap/admin"}}, "caps"},
		{"caps-missing", &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}, "caps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeListenSvc()
			m := newTestManager(fake)
			defer m.Close()

			advertised := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http", Caps: []string{"example.com/cap/read"}}
			if err := m.RegisterHostPort("cid1", advertised, "10.0.0.1"); err != nil {
				t.Fatalf("RegisterHostPort: %v", err)
			}

			err := m.RegisterHostPort("cid2", tc.def, "10.0.0.2")
			var ce *ConflictError
			if !errors.As(err, &ce) {
				t.Fatalf("error = %v (%T), want *ConflictError", err, err)
			}
			if ce.Service != "svc:web" || ce.Key != "cid2" || ce.Field != tc.field {
				t.Errorf("conflict = %+v, want service svc:web, key cid2, field %s", ce, tc.field)
			}
			if ce.Advertised == ce.Offered {
				t.Errorf("conflict must report both values, got %q vs %q", ce.Advertised, ce.Offered)
			}
			if got := backendsOf(t, m, "svc:web"); len(got) != 1 || got[0] != "http://10.0.0.1:80" {
				t.Errorf("pool = %v, want only the advertising backend", got)
			}
			// A refused key is recorded nowhere, so Deregister cannot disturb the
			// surviving member.
			m.Deregister("cid2")
			if got := backendsOf(t, m, "svc:web"); len(got) != 1 {
				t.Errorf("pool = %v after deregistering a refused key, want 1 member", got)
			}
		})
	}
}

// Caps are compared as sets: order and repetition do not make two backends of
// one service incompatible.
func TestManager_CapsCompareAsSets(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	first := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http",
		Caps: []string{"example.com/cap/read", "example.com/cap/admin"}}
	second := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http",
		Caps: []string{"example.com/cap/admin", "example.com/cap/read", "example.com/cap/read"}}

	if err := m.RegisterHostPort("cid1", first, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort cid1: %v", err)
	}
	if err := m.RegisterHostPort("cid2", second, "10.0.0.2"); err != nil {
		t.Fatalf("RegisterHostPort cid2: %v", err)
	}
	if got := backendsOf(t, m, "svc:web"); len(got) != 2 {
		t.Errorf("pool = %v, want 2 members", got)
	}
}

// Once the pool empties, the next registration advertises the service afresh
// with its own scheme and caps — including one refused earlier.
func TestManager_RefusedBackendCanAdvertiseAfterTeardown(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	http1 := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	https2 := &ServiceDef{Service: "svc:web", Port: 443, Scheme: "https"}

	if err := m.RegisterHostPort("cid1", http1, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort cid1: %v", err)
	}
	var ce *ConflictError
	if err := m.RegisterHostPort("cid2", https2, "10.0.0.2"); !errors.As(err, &ce) {
		t.Fatalf("RegisterHostPort cid2 = %v, want *ConflictError", err)
	}
	m.Deregister("cid1")

	if err := m.RegisterHostPort("cid2", https2, "10.0.0.2"); err != nil {
		t.Fatalf("re-Register cid2: %v", err)
	}
	views := m.Snapshot()
	if len(views) != 1 || views[0].Scheme != "https" {
		t.Fatalf("views = %+v, want svc:web re-advertised as https", views)
	}
	if got := views[0].Backends[0].Backend; got != "https://10.0.0.2:443" {
		t.Errorf("backend = %q, want https://10.0.0.2:443", got)
	}
}

func TestManager_RegisterListenServiceMagicDNSErrorIsFatal(t *testing.T) {
	fake := newFakeListenSvc()
	fake.failOn("svc:web", errors.New("HTTPS is not enabled on this tailnet"))
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	err := m.RegisterHostPort("cid1", def, "10.0.0.1")
	if err == nil {
		t.Fatalf("want FatalError, got nil")
	}
	var fe *FatalError
	if !errors.As(err, &fe) {
		t.Fatalf("error type = %T, want *FatalError; msg=%v", err, err)
	}
}

func TestManager_CloseTearsDownAllAndRefusesNewRegistrations(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)

	for i := 0; i < 3; i++ {
		def := &ServiceDef{Service: fmt.Sprintf("svc:s%d", i), Port: 80, Scheme: "http"}
		if err := m.RegisterHostPort(fmt.Sprintf("cid%d", i), def, "10.0.0.1"); err != nil {
			t.Fatalf("RegisterHostPort: %v", err)
		}
	}

	chs := []<-chan struct{}{
		fake.waitClose("svc:s0"),
		fake.waitClose("svc:s1"),
		fake.waitClose("svc:s2"),
	}
	m.Close()
	for i, ch := range chs {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("listener s%d not closed within 1s", i)
		}
	}

	// Register after Close must fail.
	err := m.RegisterHostPort("cidN", &ServiceDef{Service: "svc:new", Port: 80, Scheme: "http"}, "10.0.0.1")
	if err == nil || err.Error() != "manager is shutting down" {
		t.Errorf("Register after Close: %v, want 'manager is shutting down'", err)
	}
}

func TestManager_ConcurrentRegisterDeregisterDoesNotPanic(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	const N = 50
	var wg sync.WaitGroup
	var failures atomic.Int32

	for i := 0; i < N; i++ {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			def := &ServiceDef{Service: fmt.Sprintf("svc:s%d", i), Port: 80, Scheme: "http"}
			if err := m.RegisterHostPort(fmt.Sprintf("cid%d", i), def, "10.0.0.1"); err != nil {
				failures.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			// Some of these will run before Register, hitting the unknown-cid branch.
			m.Deregister(fmt.Sprintf("cid%d", i))
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Errorf("Register failures: %d", failures.Load())
	}
	// Final state: snapshot must be coherent — every advertised service has a
	// name and at least one backend, and there are no dangling entries.
	for _, v := range m.Snapshot() {
		if v.Service == "" || len(v.Backends) == 0 {
			t.Errorf("incoherent snapshot row: %+v", v)
		}
		for _, b := range v.Backends {
			if b.Key == "" || b.Backend == "" {
				t.Errorf("incoherent backend on %s: %+v", v.Service, b)
			}
		}
	}
}

func TestManager_RegisterIsMagicDNSError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"MagicDNS is required", true},
		{"HTTPS Certificates not enabled", true},
		{"HTTPS is not enabled", true},
		{"https not enabled on this tailnet", true},
		{"some random network error", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.msg, func(t *testing.T) {
			var err error
			if tc.msg != "" {
				err = errors.New(tc.msg)
			}
			if got := isMagicDNSError(err); got != tc.want {
				t.Errorf("isMagicDNSError(%q) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// tsserve_services_active counts advertised services and tsserve_service_backends
// the pool size, so a second backend for one service moves only the latter.
func TestManager_MetricsCountServicesAndBackendsSeparately(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	m.metrics = metrics.New()
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort cid1: %v", err)
	}
	if got := testutil.ToFloat64(m.metrics.Active); got != 1 {
		t.Errorf("tsserve_services_active = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.metrics.Backends.WithLabelValues("svc:web")); got != 1 {
		t.Errorf("tsserve_service_backends{svc:web} = %v, want 1", got)
	}

	if err := m.RegisterHostPort("cid2", def, "10.0.0.2"); err != nil {
		t.Fatalf("RegisterHostPort cid2: %v", err)
	}
	if got := testutil.ToFloat64(m.metrics.Active); got != 1 {
		t.Errorf("tsserve_services_active = %v after a second backend, want 1", got)
	}
	if got := testutil.ToFloat64(m.metrics.Backends.WithLabelValues("svc:web")); got != 2 {
		t.Errorf("tsserve_service_backends{svc:web} = %v, want 2", got)
	}

	m.Deregister("cid1")
	if got := testutil.ToFloat64(m.metrics.Active); got != 1 {
		t.Errorf("tsserve_services_active = %v while a backend remains, want 1", got)
	}
	if got := testutil.ToFloat64(m.metrics.Backends.WithLabelValues("svc:web")); got != 1 {
		t.Errorf("tsserve_service_backends{svc:web} = %v, want 1", got)
	}

	m.Deregister("cid2")
	if got := testutil.ToFloat64(m.metrics.Active); got != 0 {
		t.Errorf("tsserve_services_active = %v after the last backend left, want 0", got)
	}
	// The series is deleted with the service, so the exposition does not keep a
	// stale value for a service that no longer exists.
	if got := testutil.CollectAndCount(m.metrics.Backends); got != 0 {
		t.Errorf("tsserve_service_backends series = %d after teardown, want 0", got)
	}
}

// Close clears both metrics, leaving no stale per-service series behind.
func TestManager_CloseClearsMetrics(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	m.metrics = metrics.New()

	for i, name := range []string{"svc:a", "svc:b"} {
		def := &ServiceDef{Service: name, Port: 80, Scheme: "http"}
		if err := m.RegisterHostPort(fmt.Sprintf("cid%d", i), def, "10.0.0.1"); err != nil {
			t.Fatalf("RegisterHostPort %s: %v", name, err)
		}
	}
	if got := testutil.CollectAndCount(m.metrics.Backends); got != 2 {
		t.Fatalf("tsserve_service_backends series = %d, want 2", got)
	}

	m.Close()
	if got := testutil.ToFloat64(m.metrics.Active); got != 0 {
		t.Errorf("tsserve_services_active = %v after Close, want 0", got)
	}
	if got := testutil.CollectAndCount(m.metrics.Backends); got != 0 {
		t.Errorf("tsserve_service_backends series = %d after Close, want 0", got)
	}
}

// gatedListener blocks in Close until released, so a test can hold a service's
// teardown open and drive another registration into the window.
type gatedListener struct {
	net.Listener
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedListener) Close() error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.Listener.Close()
}

// A Register that arrives while the last backend's listener is still closing
// must not call ListenService: tsnet refuses a second handler for the service's
// port until the outgoing one has left the serve config. It must wait for the
// teardown and then advertise the service afresh.
func TestManager_RegisterDuringTeardownWaitsForListenerToClose(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	closeEntered := make(chan struct{})
	release := make(chan struct{})
	inner := fake.listenFn()
	var opened atomic.Int32
	m.listenSvc = func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
		ln, err := inner(name, mode)
		if err != nil || opened.Add(1) != 1 {
			return ln, err
		}
		// Gate only the first listener; its Close is the teardown we hold open.
		return &gatedListener{Listener: ln, entered: closeEntered, release: release}, nil
	}

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid-old", def, "10.0.0.1"); err != nil {
		t.Fatalf("seed Register: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.Deregister("cid-old")
	}()

	// The teardown is now parked inside the listener's Close.
	select {
	case <-closeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown did not reach the listener close")
	}

	started := make(chan struct{})
	var regErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		regErr = m.RegisterHostPort("cid-new", def, "10.0.0.2")
	}()
	<-started
	// Give the registration time to reach ListenService if it is going to. It
	// must not: the outgoing listener is still open, so the call would be
	// refused. A correct implementation is parked waiting for the teardown.
	time.Sleep(100 * time.Millisecond)
	if n := opened.Load(); n != 1 {
		t.Errorf("ListenService called %d times while the previous listener was still closing, want 1", n)
	}

	close(release)
	wg.Wait()

	if regErr != nil {
		t.Fatalf("RegisterHostPort racing teardown: %v", regErr)
	}
	views := m.Snapshot()
	if len(views) != 1 || len(views[0].Backends) != 1 || views[0].Backends[0].Key != "cid-new" {
		t.Fatalf("snapshot = %+v, want svc:web serving only cid-new", views)
	}
	if n := opened.Load(); n != 2 {
		t.Errorf("ListenService called %d times overall, want 2 (one per advertisement)", n)
	}
}

// Log lines distinguish advertising a service from a backend joining or
// leaving an existing pool, and every pool change carries the resulting size.
func TestManager_LogsDistinguishAdvertisingFromPoolChanges(t *testing.T) {
	fake := newFakeListenSvc()
	var buf bytes.Buffer
	m := newTestManager(fake)
	m.logger = slog.New(slog.NewTextHandler(&buf, nil))

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	for i, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		if err := m.RegisterHostPort(fmt.Sprintf("cid%d", i), def, ip); err != nil {
			t.Fatalf("RegisterHostPort %d: %v", i, err)
		}
	}
	m.Deregister("cid0")
	m.Deregister("cid1")

	out := buf.String()
	for _, want := range []string{
		`msg="service advertised"`,
		`msg="backend joined service"`,
		`msg="backend left service"`,
		`msg="service withdrawn; last backend left"`,
	} {
		if n := strings.Count(out, want); n != 1 {
			t.Errorf("%s appears %d times, want 1\n--- log ---\n%s", want, n, out)
		}
	}
	// One pool_size per pool change: 1 on advertise, 2 on join, 1 on leave, 0 on
	// withdrawal.
	for _, want := range []string{"pool_size=1", "pool_size=2", "pool_size=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
	if n := strings.Count(out, "pool_size="); n != 4 {
		t.Errorf("pool_size appears %d times, want 4 (one per pool change)\n--- log ---\n%s", n, out)
	}
}

// A service re-advertised while its predecessor is still tearing down must end
// with the gauges describing the live service, not the withdrawn one: the
// teardown's bookkeeping has to be finished before the replacement can take the
// name.
func TestManager_MetricsSurviveReadvertiseDuringTeardown(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	m.metrics = metrics.New()
	defer m.Close()

	closeEntered := make(chan struct{})
	release := make(chan struct{})
	inner := fake.listenFn()
	var opened atomic.Int32
	m.listenSvc = func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
		ln, err := inner(name, mode)
		if err != nil || opened.Add(1) != 1 {
			return ln, err
		}
		return &gatedListener{Listener: ln, entered: closeEntered, release: release}, nil
	}

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid-old", def, "10.0.0.1"); err != nil {
		t.Fatalf("seed Register: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.Deregister("cid-old") }()
	select {
	case <-closeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown did not reach the listener close")
	}

	started := make(chan struct{})
	var regErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		regErr = m.RegisterHostPort("cid-new", def, "10.0.0.2")
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if regErr != nil {
		t.Fatalf("RegisterHostPort racing teardown: %v", regErr)
	}
	if got := testutil.ToFloat64(m.metrics.Active); got != 1 {
		t.Errorf("tsserve_services_active = %v, want 1: the service is advertised", got)
	}
	if got := testutil.CollectAndCount(m.metrics.Backends); got != 1 {
		t.Errorf("tsserve_service_backends series = %d, want 1: the live service's series must not be deleted by the teardown it replaced", got)
	}
	if got := testutil.ToFloat64(m.metrics.Backends.WithLabelValues("svc:web")); got != 1 {
		t.Errorf("tsserve_service_backends{svc:web} = %v, want 1", got)
	}
}

// gateFirstListener makes the first listener the manager opens park inside
// Close until the returned release channel is closed, so a test can hold a
// service's withdrawal open and observe the manager mid-teardown. The handle
// is populated when that listener is opened, hence the atomic: it is written
// on whichever goroutine called Register.
func gateFirstListener(t *testing.T, m *Manager, fake *fakeListenSvc) (gated *atomic.Pointer[gatedListener], entered <-chan struct{}, release chan struct{}) {
	t.Helper()
	enteredCh := make(chan struct{})
	release = make(chan struct{})
	gated = &atomic.Pointer[gatedListener]{}
	inner := fake.listenFn()
	var opened atomic.Int32
	m.listenSvc = func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
		ln, err := inner(name, mode)
		if err != nil || opened.Add(1) != 1 {
			return ln, err
		}
		g := &gatedListener{Listener: ln, entered: enteredCh, release: release}
		gated.Store(g)
		return g, nil
	}
	return gated, enteredCh, release
}

// A service whose pool has emptied is kept in the index until its listener has
// finished closing, so that a racing Register waits rather than colliding with
// the outgoing handler. Snapshot must not show it during that window: every
// ServiceView is meant to carry at least one backend, and this one has none.
func TestManager_SnapshotOmitsServiceBeingWithdrawn(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()
	_, entered, release := gateFirstListener(t, m, fake)

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid-1", def, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort: %v", err)
	}
	if got := m.Snapshot(); len(got) != 1 || len(got[0].Backends) != 1 {
		t.Fatalf("Snapshot while serving = %+v, want 1 service with 1 backend", got)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Deregister("cid-1")
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("withdrawal did not reach the listener close")
	}

	// The record is still in the index here, in its withdrawing state.
	if got := m.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot during withdrawal = %+v, want no services", got)
	}

	close(release)
	<-done

	if got := m.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot after withdrawal = %+v, want no services", got)
	}
}

// Close must leave a service that is already being withdrawn to the Deregister
// that started it. Tearing it down a second time would run cancel and closeFn
// concurrently with the first teardown.
func TestManager_CloseSkipsServiceAlreadyBeingWithdrawn(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	m.metrics = metrics.New()
	_, entered, release := gateFirstListener(t, m, fake)

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid-1", def, "10.0.0.1"); err != nil {
		t.Fatalf("RegisterHostPort: %v", err)
	}

	// Count teardowns rather than listener closes: one teardown closes the
	// listener twice by design, once via the server and once directly.
	var teardowns atomic.Int32
	m.mu.Lock()
	rec := m.services["svc:web"]
	inner := rec.closeFn
	rec.closeFn = func() error {
		teardowns.Add(1)
		return inner()
	}
	m.mu.Unlock()

	deregistered := make(chan struct{})
	go func() {
		defer close(deregistered)
		m.Deregister("cid-1")
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("withdrawal did not reach the listener close")
	}

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		m.Close()
	}()

	// Close must not block on the parked withdrawal, and must not touch it.
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a service that was already being withdrawn")
	}

	close(release)
	<-deregistered

	if got := teardowns.Load(); got != 1 {
		t.Errorf("closeFn ran %d times, want 1 — the withdrawal that started owns the teardown", got)
	}
	// Close zeroes the gauges for the whole index. The withdrawal completing
	// afterwards must not decrement on top of that: the gauge is unsigned in
	// meaning, and -1 is the value a shutdown race used to leave behind.
	if got := testutil.ToFloat64(m.metrics.Active); got != 0 {
		t.Errorf("tsserve_services_active = %v after Close raced a withdrawal, want 0", got)
	}
	if got := testutil.CollectAndCount(m.metrics.Backends); got != 0 {
		t.Errorf("tsserve_service_backends series = %d after Close, want 0", got)
	}
}

// Advertising two different services at once must not put two ListenService
// calls in flight together: tsnet edits the node's serve config with a
// compare-and-swap over its ETag, so overlapping edits make one of them fail
// with an etag mismatch — which is what leaves a running, labelled backend
// unserved at start-up, when every discovered service advertises at once.
func TestManager_ConcurrentRegisterOfDifferentServicesDoesNotOverlapListen(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	inner := fake.listenFn()
	var inFlight, maxInFlight atomic.Int32
	m.listenSvc = func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			high := maxInFlight.Load()
			if n <= high || maxInFlight.CompareAndSwap(high, n) {
				break
			}
		}
		// Stay inside the call: an unserialised advertisement of the other
		// service arrives during this window and is counted above.
		time.Sleep(50 * time.Millisecond)
		return inner(name, mode)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"svc:web", "svc:api"} {
		def := &ServiceDef{Service: name, Port: 80, Scheme: "http"}
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = m.RegisterHostPort(fmt.Sprintf("cid%d", i), def, ip)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("RegisterHostPort %d: %v", i, err)
		}
	}
	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("%d ListenService calls were in flight at once, want 1: serve-config edits must not overlap", got)
	}
	if got := len(m.Snapshot()); got != 2 {
		t.Errorf("advertised services = %d, want 2", got)
	}
}

// A withdrawal edits the serve config too, so it is serialised against
// advertisement across service names: while one service's listener is closing,
// another service must not be inside ListenService.
func TestManager_AdvertiseWaitsForAnotherServicesTeardown(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	closeEntered := make(chan struct{})
	release := make(chan struct{})
	inner := fake.listenFn()
	var opened atomic.Int32
	m.listenSvc = func(name string, mode tsnet.ServiceMode) (net.Listener, error) {
		ln, err := inner(name, mode)
		if err != nil || opened.Add(1) != 1 {
			return ln, err
		}
		// Gate the first service's listener; its Close is the withdrawal this
		// test holds open.
		return &gatedListener{Listener: ln, entered: closeEntered, release: release}, nil
	}

	web := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	api := &ServiceDef{Service: "svc:api", Port: 80, Scheme: "http"}
	if err := m.RegisterHostPort("cid-web", web, "10.0.0.1"); err != nil {
		t.Fatalf("seed Register: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.Deregister("cid-web") }()
	select {
	case <-closeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("withdrawal did not reach the listener close")
	}

	var regErr error
	wg.Add(1)
	go func() { defer wg.Done(); regErr = m.RegisterHostPort("cid-api", api, "10.0.0.2") }()
	// Give the registration time to reach ListenService if nothing holds it
	// back. It must not: the other service's handler is still being removed
	// from the serve config.
	time.Sleep(100 * time.Millisecond)
	if n := opened.Load(); n != 1 {
		t.Errorf("ListenService calls = %d while another service's listener was closing, want 1 (the seed advertisement only)", n)
	}

	close(release)
	wg.Wait()

	if regErr != nil {
		t.Fatalf("RegisterHostPort racing another service's teardown: %v", regErr)
	}
	views := m.Snapshot()
	if len(views) != 1 || views[0].Service != "svc:api" || len(views[0].Backends) != 1 {
		t.Fatalf("snapshot = %+v, want svc:api serving one backend", views)
	}
	if n := opened.Load(); n != 2 {
		t.Errorf("ListenService calls = %d overall, want 2 (one per advertisement)", n)
	}
}

func functionDef(service, arn, qualifier string) *FunctionDef {
	return &FunctionDef{
		Service: service,
		Origin:  Origin{Account: "111122223333", Region: "us-east-1"},
		Invoker: NewFunctionInvoker(nil, arn, qualifier),
	}
}

const (
	fnHello = "arn:aws:lambda:us-east-1:111122223333:function:hello"
	fnHi    = "arn:aws:lambda:us-east-1:111122223333:function:hi"
)

func TestManager_RegisterFunctionAdvertisesLambdaService(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	if err := m.RegisterFunction(fnHello, functionDef("svc:hello", fnHello, "live")); err != nil {
		t.Fatalf("RegisterFunction: %v", err)
	}
	if n := fake.listenCount("svc:hello"); n != 1 {
		t.Fatalf("ListenService calls = %d, want 1", n)
	}
	views := m.Snapshot()
	if len(views) != 1 || views[0].Scheme != SchemeLambda || len(views[0].Backends) != 1 {
		t.Fatalf("snapshot = %+v, want one lambda service with one backend", views)
	}
	b := views[0].Backends[0]
	if b.Backend != "lambda://"+fnHello+":live" {
		t.Errorf("Backend = %q", b.Backend)
	}
	if b.Key != fnHello {
		t.Errorf("Key = %q, want the function ARN", b.Key)
	}
	if b.Origin != (Origin{Account: "111122223333", Region: "us-east-1"}) {
		t.Errorf("Origin = %+v", b.Origin)
	}

	// Registration is idempotent on key.
	if err := m.RegisterFunction(fnHello, functionDef("svc:hello", fnHello, "live")); err != nil {
		t.Fatalf("repeat RegisterFunction: %v", err)
	}
	if got := backendsOf(t, m, "svc:hello"); len(got) != 1 {
		t.Errorf("pool = %v after a repeat registration, want 1 member", got)
	}
}

func TestManager_SecondFunctionJoinsPoolAndLastLeavingWithdraws(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	m.metrics = metrics.New()
	defer m.Close()

	for _, arn := range []string{fnHello, fnHi} {
		if err := m.RegisterFunction(arn, functionDef("svc:hello", arn, "")); err != nil {
			t.Fatalf("RegisterFunction(%s): %v", arn, err)
		}
	}
	want := []string{"lambda://" + fnHello, "lambda://" + fnHi}
	if got := backendsOf(t, m, "svc:hello"); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pool = %v, want %v", got, want)
	}
	if n := fake.listenCount("svc:hello"); n != 1 {
		t.Errorf("ListenService calls = %d, want 1", n)
	}
	if got := testutil.ToFloat64(m.metrics.Backends.WithLabelValues("svc:hello")); got != 2 {
		t.Errorf("tsserve_service_backends = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.metrics.Active); got != 1 {
		t.Errorf("tsserve_services_active = %v, want 1", got)
	}

	closeCh := fake.waitClose("svc:hello")
	m.Deregister(fnHello)
	select {
	case <-closeCh:
		t.Fatal("listener closed while a function remained")
	default:
	}
	m.Deregister(fnHi)
	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("listener was not closed after the last function left")
	}
	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v, want empty", got)
	}
	if got := testutil.ToFloat64(m.metrics.Active); got != 0 {
		t.Errorf("tsserve_services_active = %v, want 0", got)
	}
}

// Pools are homogeneous: a container backend offered to a function-backed
// service is refused, as is a function offered to a container-backed one.
func TestManager_RefusesMixedPools(t *testing.T) {
	t.Run("container into function service", func(t *testing.T) {
		m := newTestManager(newFakeListenSvc())
		defer m.Close()
		if err := m.RegisterFunction(fnHello, functionDef("svc:hello", fnHello, "")); err != nil {
			t.Fatalf("RegisterFunction: %v", err)
		}
		err := m.RegisterHostPort("cid1", &ServiceDef{Service: "svc:hello", Port: 80, Scheme: "http"}, "10.0.0.1")
		var ce *ConflictError
		if !errors.As(err, &ce) || ce.Field != "scheme" || ce.Advertised != SchemeLambda || ce.Offered != "http" {
			t.Fatalf("error = %v, want a scheme ConflictError lambda vs http", err)
		}
		if got := backendsOf(t, m, "svc:hello"); len(got) != 1 || got[0] != "lambda://"+fnHello {
			t.Errorf("pool = %v, want only the function", got)
		}
	})
	t.Run("function into container service", func(t *testing.T) {
		m := newTestManager(newFakeListenSvc())
		defer m.Close()
		if err := m.RegisterHostPort("cid1", &ServiceDef{Service: "svc:hello", Port: 80, Scheme: "http"}, "10.0.0.1"); err != nil {
			t.Fatalf("RegisterHostPort: %v", err)
		}
		err := m.RegisterFunction(fnHello, functionDef("svc:hello", fnHello, ""))
		var ce *ConflictError
		if !errors.As(err, &ce) || ce.Field != "scheme" || ce.Advertised != "http" || ce.Offered != SchemeLambda {
			t.Fatalf("error = %v, want a scheme ConflictError http vs lambda", err)
		}
		if got := backendsOf(t, m, "svc:hello"); len(got) != 1 || got[0] != "http://10.0.0.1:80" {
			t.Errorf("pool = %v, want only the container", got)
		}
	})
}

// A function-backed service has no shared transport; a container-backed one
// keeps its own.
func TestReverseProxy_FunctionServiceHasNoSharedTransport(t *testing.T) {
	if base := newReverseProxy(SchemeLambda, newBackendPool(), discardLogger(), nil, nil).Transport.(*poolTransport).base; base != nil {
		t.Errorf("lambda service base transport = %T, want nil", base)
	}
	if base := newReverseProxy("http", newBackendPool(), discardLogger(), nil, nil).Transport.(*poolTransport).base; base == nil {
		t.Error("http service base transport = nil")
	}
}

func TestManager_RegisterFunctionWithoutInvokerIsRefused(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()
	if err := m.RegisterFunction(fnHello, &FunctionDef{Service: "svc:hello"}); err == nil {
		t.Fatal("RegisterFunction without an invoker succeeded")
	}
	if n := fake.listenCount("svc:hello"); n != 0 {
		t.Errorf("ListenService calls = %d, want 0", n)
	}
}

// Log lines for function backends name the function, so members of one pool
// can be told apart as they join and leave.
func TestManager_FunctionBackendLogsNameTheFunction(t *testing.T) {
	fake := newFakeListenSvc()
	var buf bytes.Buffer
	m := newTestManager(fake)
	m.logger = slog.New(slog.NewTextHandler(&buf, nil))

	for _, arn := range []string{fnHello, fnHi} {
		if err := m.RegisterFunction(arn, functionDef("svc:fn", arn, "")); err != nil {
			t.Fatalf("RegisterFunction %s: %v", arn, err)
		}
	}
	m.Deregister(fnHello)
	m.Deregister(fnHi)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for _, tc := range []struct{ msg, key, backend string }{
		{`msg="service advertised"`, "key=hello", "backend=lambda://" + fnHello},
		{`msg="backend joined service"`, "key=hi", "backend=lambda://" + fnHi},
		{`msg="backend left service"`, "key=hello", "backend=lambda://" + fnHello},
		{`msg="service withdrawn; last backend left"`, "key=hi", "backend=lambda://" + fnHi},
	} {
		found := false
		for _, l := range lines {
			if strings.Contains(l, tc.msg) {
				found = true
				if !strings.Contains(l, " "+tc.key+" ") || !strings.Contains(l, tc.backend+" ") {
					t.Errorf("%s line = %q, want %s and %s", tc.msg, l, tc.key, tc.backend)
				}
			}
		}
		if !found {
			t.Errorf("no %s line\n--- log ---\n%s", tc.msg, buf.String())
		}
	}
}

func TestShort(t *testing.T) {
	for in, want := range map[string]string{
		fnHello: "hello",
		"arn:aws:ecs:us-east-1:111122223333:task/cluster/0123456789abcdef": "0123456789ab",
		"0123456789abcdef0123": "0123456789ab",
		"cid0":                 "cid0",
	} {
		if got := short(in); got != want {
			t.Errorf("short(%q) = %q, want %q", in, got, want)
		}
	}
}
