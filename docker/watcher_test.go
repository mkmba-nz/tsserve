package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

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

	mu        sync.Mutex
	inspected []string // IDs passed to ContainerInspect, in order
}

func (f *fakeDocker) Ping(ctx context.Context) (types.Ping, error) {
	return types.Ping{}, f.pingErr
}
func (f *fakeDocker) ContainerList(ctx context.Context, _ container.ListOptions) ([]container.Summary, error) {
	return f.list, f.listErr
}
func (f *fakeDocker) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	f.mu.Lock()
	f.inspected = append(f.inspected, id)
	f.mu.Unlock()
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

func (f *fakeDocker) inspects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inspected...)
}

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
	w := newWatcher(api, reg, discardLogger())
	w.settleFn = func() {} // no sleep in tests
	return w
}

// labelledContainer is the fixture the sweep tests share: one running container
// labelled for svc:web, reachable on the default bridge network.
func labelledContainer() (id string, labelMap map[string]string) {
	return "cid1", map[string]string{
		"tsserve.enable":  "true",
		"tsserve.service": "svc:web",
		"tsserve.port":    "80",
	}
}

// sweepFixture wires that container into a fake Docker daemon and a watcher
// whose logs the caller can read back.
func sweepFixture(registerErr error) (*fakeDocker, *fakeRegistrar, *Watcher, *bytes.Buffer) {
	id, labelMap := labelledContainer()
	api := &fakeDocker{
		list: []container.Summary{{ID: id, Labels: labelMap}},
		inspectByID: map[string]container.InspectResponse{
			id: inspectWithIP(map[string]string{"bridge": "10.0.0.1"}, labelMap),
		},
	}
	reg := &fakeRegistrar{registerErr: registerErr}
	buf := &bytes.Buffer{}
	w := newTestWatcher(api, reg)
	w.logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return api, reg, w, buf
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

// --- reconcile ----------------------------------------------------------

func TestReconcile_RegistersOnlyLabelledContainers(t *testing.T) {
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

	if err := w.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
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

func TestReconcile_SkipsContainersWithInvalidLabels(t *testing.T) {
	// service without svc: prefix => Parse returns validation error;
	// reconcile should silently skip (no Register call).
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

	if err := w.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(reg.regs()); got != 0 {
		t.Errorf("regs = %d, want 0", got)
	}
}

func TestReconcile_ListErrorPropagates(t *testing.T) {
	api := &fakeDocker{listErr: errors.New("docker down")}
	w := newTestWatcher(api, &fakeRegistrar{})
	if err := w.reconcile(context.Background()); err == nil {
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

// A refused or failed Register in Docker mode is logged at Warn — not Error —
// and the container is registered again on its next start event as well as on
// the next sweep.
func TestHandleEvent_RegisterFailureWarnsAndRetriesOnNextStart(t *testing.T) {
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
	reg := &fakeRegistrar{registerErr: errors.New("backend cid1 refused for service svc:web: scheme conflict")}

	var buf bytes.Buffer
	w := newTestWatcher(api, reg)
	w.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	start := events.Message{Action: events.ActionStart, Actor: events.Actor{ID: "cid1"}}
	w.handleEvent(context.Background(), start)

	out := buf.String()
	if n := strings.Count(out, "level=ERROR"); n != 0 {
		t.Errorf("ERROR lines = %d, want 0\n--- log ---\n%s", n, out)
	}
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want 1\n--- log ---\n%s", n, out)
	}

	// The container's next start event registers it again, and the repeat of
	// the same failure is reported at Debug rather than warning afresh.
	w.handleEvent(context.Background(), start)
	if n := len(reg.regs()); n != 2 {
		t.Errorf("Register calls = %d, want 2 (retried on the next start event)", n)
	}
	out = buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("WARN lines after the repeat failure = %d, want 1\n--- log ---\n%s", n, out)
	}
	if n := strings.Count(out, "level=DEBUG"); n != 1 {
		t.Errorf("DEBUG lines after the repeat failure = %d, want 1\n--- log ---\n%s", n, out)
	}
}

// The sweep cadence Run uses is the watcher's own, and production builds get
// the 30-second default rather than whatever a test last set.
func TestNewWatcher_SweepsEveryThirtySeconds(t *testing.T) {
	w := newWatcher(&fakeDocker{}, &fakeRegistrar{}, discardLogger())
	if w.sweepInterval != 30*time.Second {
		t.Errorf("sweepInterval = %s, want 30s", w.sweepInterval)
	}
}

// --- sweep --------------------------------------------------------------

// The reported bug: a container is running and labelled, its registration
// failed once, and it stays unserved until tsserve is restarted. The sweep is
// what ends that — the same container, unchanged and with no new start event,
// is retried and registers once the cause clears. The repeat failure in between
// is logged at Debug so a persistent one does not warn every sweep.
func TestSweep_RetriesFailedRegistrationUntilItSucceeds(t *testing.T) {
	_, reg, w, buf := sweepFixture(errors.New("etag mismatch"))
	ctx := context.Background()

	if err := w.reconcile(ctx); err != nil { // initial scan: the registration fails
		t.Fatalf("initial scan: %v", err)
	}
	if err := w.reconcile(ctx); err != nil { // a sweep while it still fails
		t.Fatalf("sweep while still failing: %v", err)
	}
	if n := len(reg.regs()); n != 2 {
		t.Fatalf("Register calls while failing = %d, want 2 (one per cycle)", n)
	}

	// The cause clears. The next sweep registers the container, and the one
	// after that leaves it alone: it is serving.
	reg.mu.Lock()
	reg.registerErr = nil
	reg.mu.Unlock()
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("sweep after the cause cleared: %v", err)
	}
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("sweep once serving: %v", err)
	}

	regs := reg.regs()
	if len(regs) != 3 {
		t.Fatalf("Register calls = %d, want 3 (two failed, one that stuck)", len(regs))
	}
	last := regs[2]
	if last.containerID != "cid1" || last.ip != "10.0.0.1" || last.def.Service != "svc:web" {
		t.Errorf("final registration = %+v, want cid1 -> svc:web at 10.0.0.1", last)
	}
	if got := reg.deregs(); len(got) != 0 {
		t.Errorf("deregs = %v, want none: the container never went away", got)
	}

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want 1 (the first failure only)\n--- log ---\n%s", n, out)
	}
	if n := strings.Count(out, "level=DEBUG"); n != 1 {
		t.Errorf("DEBUG lines = %d, want 1 (the repeat of the same failure)\n--- log ---\n%s", n, out)
	}
}

// A container that is no longer listed is deregistered by the sweep, and its
// bookkeeping is dropped so it is not deregistered twice.
func TestSweep_DeregistersContainersThatAreGone(t *testing.T) {
	api, reg, w, _ := sweepFixture(nil)
	ctx := context.Background()

	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("initial scan: %v", err)
	}

	api.list = nil
	for i := 0; i < 2; i++ {
		if err := w.reconcile(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}

	if got := reg.deregs(); len(got) != 1 || got[0] != "cid1" {
		t.Errorf("deregs = %v, want [cid1] exactly once", got)
	}
	if n := len(reg.regs()); n != 1 {
		t.Errorf("Register calls = %d, want 1: a departed container is not re-registered", n)
	}
}

// A container whose registration failed and which then departs is forgotten
// rather than retried forever: it is not deregistered (it never served), and a
// container that comes back and fails again warns afresh rather than being
// treated as a continuing failure.
func TestSweep_DropsRetryStateForDepartedContainer(t *testing.T) {
	api, reg, w, buf := sweepFixture(errors.New("etag mismatch"))
	present := api.list
	ctx := context.Background()

	if err := w.reconcile(ctx); err != nil { // fails, and the failure is remembered
		t.Fatalf("initial scan: %v", err)
	}
	api.list = nil
	if err := w.reconcile(ctx); err != nil { // the container is gone
		t.Fatalf("sweep with the container gone: %v", err)
	}
	if n := len(w.failed); n != 0 {
		t.Errorf("remembered failures = %d, want 0 once the container is gone", n)
	}
	if got := reg.deregs(); len(got) != 0 {
		t.Errorf("deregs = %v, want none: the failed container never served", got)
	}

	api.list = present
	if err := w.reconcile(ctx); err != nil { // it is back, and fails again
		t.Fatalf("sweep after the container returned: %v", err)
	}

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Errorf("WARN lines = %d, want 2 (one per arrival)\n--- log ---\n%s", n, out)
	}
	if n := len(reg.regs()); n != 2 {
		t.Errorf("Register calls = %d, want 2", n)
	}
}

// A container whose IP cannot be resolved is retried by the sweep like any
// other unserved container, and warns once rather than on every sweep.
func TestSweep_RetriesUnresolvableIPQuietly(t *testing.T) {
	api, reg, w, buf := sweepFixture(nil)
	id, labelMap := labelledContainer()
	// The container is not attached to the network its labels name.
	api.inspectByID[id] = inspectWithIP(map[string]string{"other-net": "10.0.0.1"}, labelMap)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := w.reconcile(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if n := len(reg.regs()); n != 0 {
		t.Errorf("Register calls = %d, want 0 while the IP cannot be resolved", n)
	}

	// The container joins the network its labels name; the next sweep registers it.
	api.inspectByID[id] = inspectWithIP(map[string]string{"bridge": "10.0.0.1"}, labelMap)
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("sweep after the network was attached: %v", err)
	}
	regs := reg.regs()
	if len(regs) != 1 || regs[0].ip != "10.0.0.1" {
		t.Fatalf("regs = %+v, want one registration at 10.0.0.1", regs)
	}

	out := buf.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("WARN lines = %d, want 1 (the first failure only)\n--- log ---\n%s", n, out)
	}
	if n := strings.Count(out, "level=DEBUG"); n != 2 {
		t.Errorf("DEBUG lines = %d, want 2 (the two repeats)\n--- log ---\n%s", n, out)
	}
}

// A container that Docker reports has died is dropped from the watcher's own
// bookkeeping too, so the next sweep does not deregister it a second time.
func TestHandleEvent_DieDropsRegistrationState(t *testing.T) {
	api, reg, w, _ := sweepFixture(nil)
	ctx := context.Background()

	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	api.list = nil
	w.handleEvent(ctx, events.Message{Action: events.ActionDie, Actor: events.Actor{ID: "cid1"}})
	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("sweep after die: %v", err)
	}

	if got := reg.deregs(); len(got) != 1 || got[0] != "cid1" {
		t.Errorf("deregs = %v, want [cid1] exactly once", got)
	}
}

// A container that is already serving is left alone by later sweeps — no
// repeated registration, and no inspect call per sweep to discover that. A
// backend that does move is re-registered from its start event, with the stale
// record dropped first so nothing claims to serve the address that has gone.
func TestSweep_LeavesServingContainersAloneUntilTheyMove(t *testing.T) {
	api, reg, w, _ := sweepFixture(nil)
	id, labelMap := labelledContainer()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := w.reconcile(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if n := len(reg.regs()); n != 1 {
		t.Fatalf("Register calls = %d, want 1: a serving container is left alone", n)
	}
	if n := len(api.inspects()); n != 1 {
		t.Errorf("ContainerInspect calls = %d, want 1: sweeps do not re-inspect a serving container", n)
	}

	api.inspectByID[id] = inspectWithIP(map[string]string{"bridge": "10.0.0.2"}, labelMap)
	w.handleEvent(ctx, events.Message{Action: events.ActionStart, Actor: events.Actor{ID: id}})

	regs := reg.regs()
	if len(regs) != 2 || regs[1].ip != "10.0.0.2" {
		t.Fatalf("regs = %+v, want a second registration at 10.0.0.2", regs)
	}
	if got := reg.deregs(); len(got) != 1 || got[0] != "cid1" {
		t.Errorf("deregs = %v, want [cid1]: the stale address is dropped before re-registering", got)
	}
}

// A sweep judges a container present because Docker listed it, not because its
// labels parsed. The two can disagree: a container registered from a start
// event carries the labels ContainerInspect reported, and a list that surfaces
// different label data for it must not be read as the container having gone.
func TestSweep_KeepsServingContainerWhoseListedLabelsDoNotParse(t *testing.T) {
	api, reg, w, _ := sweepFixture(nil)
	id, labelMap := labelledContainer()
	ctx := context.Background()

	w.handleEvent(ctx, events.Message{Action: events.ActionStart, Actor: events.Actor{ID: id}})
	if n := len(reg.regs()); n != 1 {
		t.Fatalf("Register calls = %d, want 1 after the start event", n)
	}

	// The container is still listed and still running, but its listed labels
	// no longer parse.
	broken := map[string]string{}
	for k, v := range labelMap {
		broken[k] = v
	}
	broken["tsserve.port"] = "not-a-port"
	api.list = []container.Summary{{ID: id, Labels: broken}}

	if err := w.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if got := reg.deregs(); len(got) != 0 {
		t.Errorf("deregs = %v, want none: a listed container is still serving", got)
	}
	if _, serving := w.active[id]; !serving {
		t.Errorf("active = %v, want %s still registered", w.active, id)
	}
}

// Run sweeps on its own cadence: with no events at all, a container whose
// registration failed is retried again and again until it is cancelled.
func TestRun_SweepsUntilCancelled(t *testing.T) {
	api, reg, w, _ := sweepFixture(errors.New("etag mismatch"))
	api.events = make(chan events.Message)
	api.errs = make(chan error)
	w.sweepInterval = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// The initial scan is one attempt; each sweep adds another.
	const want = 4
	deadline := time.Now().Add(5 * time.Second)
	for len(reg.regs()) < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := len(reg.regs()); n < want {
		t.Fatalf("Register attempts = %d after 5s, want at least %d (one per sweep)", n, want)
	}
	for _, r := range reg.regs() {
		if r.containerID != "cid1" || r.ip != "10.0.0.1" {
			t.Fatalf("retry registered %+v, want cid1 at 10.0.0.1", r)
		}
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
