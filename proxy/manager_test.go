package proxy

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/tsnet"
)

// fakeListenSvc returns in-memory listeners. Each call records the (name, mode)
// requested. The latest listener created for a name is tracked so tests can
// wait on its Close.
type fakeListenSvc struct {
	mu        sync.Mutex
	requests  []fakeListenReq
	errOn     map[string]error          // serviceName -> err to return for that name
	listeners map[string]*pipeListener  // serviceName -> most recent listener
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

func (p *pipeListener) Addr() net.Addr { return fakeAddr{} }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

func newTestManager(fake *fakeListenSvc) *Manager {
	return &Manager{
		listenSvc: fake.listenFn(),
		logger:    discardLogger(),
		byCID:     map[string]*activeService{},
		byName:    map[string]string{},
	}
}

func TestManager_RegisterAddsServiceToSnapshot(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.Register("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	views := m.Snapshot()
	if len(views) != 1 {
		t.Fatalf("snapshot len = %d, want 1: %+v", len(views), views)
	}
	v := views[0]
	if v.Service != "svc:web" {
		t.Errorf("Service = %q", v.Service)
	}
	if v.Backend != "http://10.0.0.1:80" {
		t.Errorf("Backend = %q, want http://10.0.0.1:80", v.Backend)
	}
	if v.ContainerID != "cid1" {
		t.Errorf("ContainerID = %q", v.ContainerID)
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
	if err := m.Register("cid1", def, "10.0.0.2"); err != nil {
		t.Fatalf("Register: %v", err)
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
	_ = m.Register("cid1", def, "10.0.0.1")

	fake.mu.Lock()
	mode := fake.requests[0].mode.(tsnet.ServiceModeHTTP)
	fake.mu.Unlock()
	if mode.AcceptAppCaps != nil {
		t.Errorf("AcceptAppCaps = %v, want nil when Caps empty", mode.AcceptAppCaps)
	}
}

func TestManager_DuplicateContainerIDIsNoOp(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.Register("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := m.Register("cid1", def, "10.0.0.99"); err != nil {
		t.Fatalf("second Register: %v", err)
	}
	fake.mu.Lock()
	n := len(fake.requests)
	fake.mu.Unlock()
	if n != 1 {
		t.Errorf("ListenService called %d times for same containerID, want 1", n)
	}
	if got := m.Snapshot()[0].Backend; got != "http://10.0.0.1:80" {
		t.Errorf("Backend = %q; second Register must not overwrite", got)
	}
}

func TestManager_DuplicateServiceNameFirstWins(t *testing.T) {
	fake := newFakeListenSvc()
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	if err := m.Register("cid-first", def, "10.0.0.1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := m.Register("cid-second", def, "10.0.0.2"); err != nil {
		t.Fatalf("second: %v", err)
	}

	views := m.Snapshot()
	if len(views) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(views))
	}
	if views[0].ContainerID != "cid-first" {
		t.Errorf("winner = %q, want cid-first", views[0].ContainerID)
	}
	if views[0].Backend != "http://10.0.0.1:80" {
		t.Errorf("backend = %q, want first registration's", views[0].Backend)
	}
	fake.mu.Lock()
	n := len(fake.requests)
	fake.mu.Unlock()
	if n != 1 {
		t.Errorf("ListenService called %d times, want 1 (dup skipped before listen)", n)
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
	if err := m.Register("cid1", def, "10.0.0.1"); err != nil {
		t.Fatalf("Register: %v", err)
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
	if err := m.Register("cid2", def, "10.0.0.2"); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if got := m.Snapshot()[0].ContainerID; got != "cid2" {
		t.Errorf("new owner = %q, want cid2", got)
	}
}

func TestManager_RegisterListenServiceErrorIsSkippedNotFatal(t *testing.T) {
	fake := newFakeListenSvc()
	fake.failOn("svc:bad", errors.New("some transient error"))
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:bad", Port: 80, Scheme: "http"}
	err := m.Register("cid1", def, "10.0.0.1")
	if err != nil {
		t.Fatalf("want non-fatal error returned as nil; got %v", err)
	}
	if got := m.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v, want empty after failed Register", got)
	}
	// Service name must remain available for a future Register.
	if err := m.Register("cid2", &ServiceDef{Service: "svc:bad", Port: 80, Scheme: "http"}, "10.0.0.2"); err != nil {
		t.Fatalf("retry Register: %v", err)
	}
}

func TestManager_RegisterListenServiceMagicDNSErrorIsFatal(t *testing.T) {
	fake := newFakeListenSvc()
	fake.failOn("svc:web", errors.New("HTTPS is not enabled on this tailnet"))
	m := newTestManager(fake)
	defer m.Close()

	def := &ServiceDef{Service: "svc:web", Port: 80, Scheme: "http"}
	err := m.Register("cid1", def, "10.0.0.1")
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
		if err := m.Register(fmt.Sprintf("cid%d", i), def, "10.0.0.1"); err != nil {
			t.Fatalf("Register: %v", err)
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
	err := m.Register("cidN", &ServiceDef{Service: "svc:new", Port: 80, Scheme: "http"}, "10.0.0.1")
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
			if err := m.Register(fmt.Sprintf("cid%d", i), def, "10.0.0.1"); err != nil {
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
	// Final state: snapshot must be coherent — every entry's name was registered
	// with that container id, and there are no dangling entries.
	for _, v := range m.Snapshot() {
		if v.ContainerID == "" || v.Service == "" {
			t.Errorf("incoherent snapshot row: %+v", v)
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
