package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"

	"mkmba.nz/tsserve/labels"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ---------------------------------------------------------------

type fakeRegistrar struct {
	mu          sync.Mutex
	registered  []registerCall
	deregistered []string
	registerErr error
}

type registerCall struct {
	containerID string
	def         *labels.ServiceDef
	ip          string
}

func (r *fakeRegistrar) Register(id string, def *labels.ServiceDef, ip string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registered = append(r.registered, registerCall{id, def, ip})
	return r.registerErr
}

func (r *fakeRegistrar) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deregistered = append(r.deregistered, id)
}

func (r *fakeRegistrar) regs() []registerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]registerCall(nil), r.registered...)
}

func (r *fakeRegistrar) deregs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deregistered...)
}

type fakeDocker struct {
	pingErr     error
	list        []container.Summary
	listErr     error
	inspectByID map[string]container.InspectResponse
	inspectErr  map[string]error
	events      chan events.Message
	errs        chan error
}

func (f *fakeDocker) Ping(ctx context.Context) (types.Ping, error) {
	return types.Ping{}, f.pingErr
}
func (f *fakeDocker) ContainerList(ctx context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return f.list, f.listErr
}
func (f *fakeDocker) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	if err, ok := f.inspectErr[id]; ok {
		return container.InspectResponse{}, err
	}
	if r, ok := f.inspectByID[id]; ok {
		return r, nil
	}
	return container.InspectResponse{}, errors.New("not found: " + id)
}
func (f *fakeDocker) Events(ctx context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	return f.events, f.errs
}
func (f *fakeDocker) Close() error { return nil }

// helpers ----------------------------------------------------------------

func inspectWithIP(networks map[string]string, labelMap map[string]string) container.InspectResponse {
	eps := map[string]*network.EndpointSettings{}
	for name, ip := range networks {
		eps[name] = &network.EndpointSettings{IPAddress: ip}
	}
	return container.InspectResponse{
		Config: &container.Config{Labels: labelMap},
		NetworkSettings: &container.NetworkSettings{
			Networks: eps,
		},
	}
}

func newTestWatcher(api dockerAPI, reg Registrar) *Watcher {
	return &Watcher{
		cli:      api,
		reg:      reg,
		logger:   discardLogger(),
		settleFn: func() {}, // no sleep in tests
	}
}

// --- resolveIP ----------------------------------------------------------

func TestResolveIP_FindsIPOnNamedNetwork(t *testing.T) {
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": inspectWithIP(map[string]string{"bridge": "172.17.0.5", "frontend": "10.0.0.1"}, nil),
		},
	}
	w := newTestWatcher(api, &fakeRegistrar{})

	ip, err := w.resolveIP(context.Background(), "cid1", "frontend")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ip != "10.0.0.1" {
		t.Errorf("ip = %q, want 10.0.0.1", ip)
	}
}

func TestResolveIP_DefaultBridge(t *testing.T) {
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": inspectWithIP(map[string]string{"bridge": "172.17.0.5"}, nil),
		},
	}
	w := newTestWatcher(api, &fakeRegistrar{})

	ip, err := w.resolveIP(context.Background(), "cid1", "bridge")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ip != "172.17.0.5" {
		t.Errorf("ip = %q, want 172.17.0.5", ip)
	}
}

func TestResolveIP_NetworkNotAttached(t *testing.T) {
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": inspectWithIP(map[string]string{"bridge": "172.17.0.5"}, nil),
		},
	}
	w := newTestWatcher(api, &fakeRegistrar{})

	if _, err := w.resolveIP(context.Background(), "cid1", "frontend"); err == nil {
		t.Fatal("expected error for unattached network, got nil")
	}
}

func TestResolveIP_EmptyIPAddress(t *testing.T) {
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": inspectWithIP(map[string]string{"bridge": ""}, nil),
		},
	}
	w := newTestWatcher(api, &fakeRegistrar{})

	if _, err := w.resolveIP(context.Background(), "cid1", "bridge"); err == nil {
		t.Fatal("expected error for empty IP, got nil")
	}
}

func TestResolveIP_NilNetworkSettings(t *testing.T) {
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": {Config: &container.Config{}},
		},
	}
	w := newTestWatcher(api, &fakeRegistrar{})

	if _, err := w.resolveIP(context.Background(), "cid1", "bridge"); err == nil {
		t.Fatal("expected error when NetworkSettings is nil")
	}
}

func TestResolveIP_InspectErrorPropagates(t *testing.T) {
	api := &fakeDocker{
		inspectErr: map[string]error{"cid1": errors.New("docker said no")},
	}
	w := newTestWatcher(api, &fakeRegistrar{})

	_, err := w.resolveIP(context.Background(), "cid1", "bridge")
	if err == nil || err.Error() != "docker said no" {
		t.Fatalf("err = %v, want 'docker said no'", err)
	}
}

// --- scanExisting -------------------------------------------------------

func TestScanExisting_RegistersOnlyLabelledContainers(t *testing.T) {
	enabled := map[string]string{
		"tsserve.enable":  "true",
		"tsserve.service": "svc:web",
		"tsserve.port":    "80",
	}
	disabled := map[string]string{
		"tsserve.enable":  "false",
		"tsserve.service": "svc:other",
		"tsserve.port":    "80",
	}
	unrelated := map[string]string{"foo": "bar"}

	api := &fakeDocker{
		list: []container.Summary{
			{ID: "cid-on", Labels: enabled},
			{ID: "cid-off", Labels: disabled},
			{ID: "cid-none", Labels: unrelated},
		},
		inspectByID: map[string]container.InspectResponse{
			"cid-on": inspectWithIP(map[string]string{"bridge": "172.17.0.5"}, enabled),
		},
	}
	reg := &fakeRegistrar{}
	w := newTestWatcher(api, reg)

	if err := w.scanExisting(context.Background()); err != nil {
		t.Fatalf("scanExisting: %v", err)
	}

	regs := reg.regs()
	if len(regs) != 1 {
		t.Fatalf("regs = %d, want 1: %+v", len(regs), regs)
	}
	if regs[0].containerID != "cid-on" {
		t.Errorf("got ID %q, want cid-on", regs[0].containerID)
	}
	if regs[0].def.Service != "svc:web" || regs[0].ip != "172.17.0.5" {
		t.Errorf("registered with svc=%q ip=%q", regs[0].def.Service, regs[0].ip)
	}
}

func TestScanExisting_SkipsContainersWithInvalidLabels(t *testing.T) {
	// service without svc: prefix => Parse returns validation error;
	// scanExisting should silently skip (no Register call).
	bad := map[string]string{
		"tsserve.enable":  "true",
		"tsserve.service": "no-prefix",
		"tsserve.port":    "80",
	}
	api := &fakeDocker{
		list: []container.Summary{{ID: "cid-bad", Labels: bad}},
	}
	reg := &fakeRegistrar{}
	w := newTestWatcher(api, reg)

	if err := w.scanExisting(context.Background()); err != nil {
		t.Fatalf("scanExisting: %v", err)
	}
	if got := len(reg.regs()); got != 0 {
		t.Errorf("regs = %d, want 0", got)
	}
}

func TestScanExisting_ListErrorPropagates(t *testing.T) {
	api := &fakeDocker{listErr: errors.New("docker down")}
	w := newTestWatcher(api, &fakeRegistrar{})
	if err := w.scanExisting(context.Background()); err == nil {
		t.Fatal("want error, got nil")
	}
}

// --- handleEvent --------------------------------------------------------

func TestHandleEvent_StartCallsRegisterAfterSettleAndInspect(t *testing.T) {
	enabled := map[string]string{
		"tsserve.enable":  "true",
		"tsserve.service": "svc:web",
		"tsserve.port":    "80",
	}
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": inspectWithIP(map[string]string{"bridge": "172.17.0.10"}, enabled),
		},
	}
	reg := &fakeRegistrar{}
	settled := false
	w := newTestWatcher(api, reg)
	w.settleFn = func() { settled = true }

	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "cid1"},
	})

	if !settled {
		t.Errorf("settleFn not called before inspect")
	}
	regs := reg.regs()
	if len(regs) != 1 || regs[0].containerID != "cid1" || regs[0].ip != "172.17.0.10" {
		t.Errorf("regs = %+v", regs)
	}
}

func TestHandleEvent_StartSkipsContainerWithoutTsserveEnable(t *testing.T) {
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": inspectWithIP(map[string]string{"bridge": "172.17.0.10"}, map[string]string{"foo": "bar"}),
		},
	}
	reg := &fakeRegistrar{}
	w := newTestWatcher(api, reg)

	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "cid1"},
	})

	if got := len(reg.regs()); got != 0 {
		t.Errorf("regs = %d, want 0 (unlabelled container)", got)
	}
}

func TestHandleEvent_StartSwallowsInspectError(t *testing.T) {
	api := &fakeDocker{
		inspectErr: map[string]error{"cid1": errors.New("gone")},
	}
	reg := &fakeRegistrar{}
	w := newTestWatcher(api, reg)

	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "cid1"},
	})

	if got := len(reg.regs()); got != 0 {
		t.Errorf("regs on inspect error = %d, want 0", got)
	}
}

func TestHandleEvent_StopActionsCallDeregister(t *testing.T) {
	for _, action := range []events.Action{events.ActionDie, events.ActionStop, events.ActionKill, events.ActionDestroy} {
		t.Run(string(action), func(t *testing.T) {
			reg := &fakeRegistrar{}
			w := newTestWatcher(&fakeDocker{}, reg)

			w.handleEvent(context.Background(), events.Message{
				Action: action,
				Actor:  events.Actor{ID: "cid1"},
			})

			if got := reg.deregs(); len(got) != 1 || got[0] != "cid1" {
				t.Errorf("deregs = %v, want [cid1]", got)
			}
		})
	}
}

func TestHandleEvent_IgnoresUnrelatedActions(t *testing.T) {
	reg := &fakeRegistrar{}
	w := newTestWatcher(&fakeDocker{}, reg)

	w.handleEvent(context.Background(), events.Message{
		Action: events.ActionExecCreate,
		Actor:  events.Actor{ID: "cid1"},
	})

	if got := len(reg.regs()) + len(reg.deregs()); got != 0 {
		t.Errorf("expected no-op on unrelated action; got %d total calls", got)
	}
}

// --- register (IP-resolve-failure path) ---------------------------------

func TestRegister_IPResolveFailureSkipsWithoutRegistrarCall(t *testing.T) {
	enabled := map[string]string{
		"tsserve.enable":  "true",
		"tsserve.service": "svc:web",
		"tsserve.port":    "80",
		"tsserve.network": "missing-net",
	}
	api := &fakeDocker{
		inspectByID: map[string]container.InspectResponse{
			"cid1": inspectWithIP(map[string]string{"bridge": "172.17.0.5"}, enabled),
		},
	}
	reg := &fakeRegistrar{}
	w := newTestWatcher(api, reg)

	def, err := labels.Parse(enabled)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	w.register(context.Background(), "cid1", def)

	if got := len(reg.regs()); got != 0 {
		t.Errorf("regs = %d, want 0 (network not attached)", got)
	}
}

// --- short --------------------------------------------------------------

func TestShort(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"abcdef123456", "abcdef123456"},
		{"abcdef1234567890", "abcdef123456"},
	} {
		if got := short(tc.in); got != tc.want {
			t.Errorf("short(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
