package local

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mkmba.nz/tsserve/proxy"
)

type fakeSnapshotter struct{ services []proxy.ServiceView }

func (f *fakeSnapshotter) Snapshot() []proxy.ServiceView { return f.services }

type fakeReaderSource struct{ readers []ReaderStatus }

func (f *fakeReaderSource) Readers() []ReaderStatus { return f.readers }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "just now"},
		{time.Second, "1s"},
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{61 * time.Second, "1m1s"},
		{time.Hour + 3*time.Minute, "1h3m"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d0h"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := humanDuration(tc.d); got != tc.want {
				t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

func TestShortID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"abcdef", "abcdef"},
		{"abcdef123456", "abcdef123456"},
		{"abcdef1234567890", "abcdef123456"},
		// ECS task ARN (old format, no cluster in path)
		{"arn:aws:ecs:us-east-1:123456789012:task/abcdef123456789", "abcdef123456"},
		// ECS task ARN (new format, with cluster in path)
		{"arn:aws:ecs:us-east-1:123456789012:task/MyCluster/abcdef123456789", "abcdef123456"},
		// Composite ARN#container key (per SPEC.md for multi-container task defs)
		{"arn:aws:ecs:us-east-1:123:task/cluster/abc123#whoami", "abc123#whoam"},
		// A function ARN has no '/', so shortID alone would render every
		// function as its constant prefix; rowsFor uses functionName instead.
		{"arn:aws:lambda:us-east-1:123456789012:function:example-fn", "arn:aws:lamb"},
	} {
		if got := shortID(tc.in); got != tc.want {
			t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFunctionName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"arn:aws:lambda:us-east-1:123456789012:function:example-fn", "example-fn"},
		{"arn:aws:lambda:us-east-1:123456789012:function:a-function-name-well-over-twelve", "a-function-name-well-over-twelve"},
	} {
		if got := functionName(tc.in); got != tc.want {
			t.Errorf("functionName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A function backend's key cell shows the function name, its Backend cell the
// lambda:// target, and its Cluster is empty (rendered as a dash).
func TestRowsFor_FunctionBackend(t *testing.T) {
	const arn = "arn:aws:lambda:ap-southeast-2:333333333333:function:example-fn"
	rows := rowsFor([]proxy.ServiceView{{
		Service: "svc:fn",
		Scheme:  proxy.SchemeLambda,
		Backends: []proxy.BackendView{{
			Backend:      "lambda://" + arn + ":live",
			Key:          arn,
			RegisteredAt: time.Now(),
			Origin:       proxy.Origin{Account: "333333333333", Region: "ap-southeast-2"},
		}},
	}})
	if len(rows) != 1 || len(rows[0].Backends) != 1 {
		t.Fatalf("rows = %+v, want 1 service with 1 backend", rows)
	}
	b := rows[0].Backends[0]
	if b.KeyShort != "example-fn" {
		t.Errorf("KeyShort = %q, want %q", b.KeyShort, "example-fn")
	}
	if b.Backend != "lambda://"+arn+":live" {
		t.Errorf("Backend = %q", b.Backend)
	}
	if b.Account != "333333333333" || b.Cluster != "" {
		t.Errorf("origin = account %q cluster %q, want 333333333333 and empty", b.Account, b.Cluster)
	}
}

func TestRowsFor_MapsServiceFields(t *testing.T) {
	now := time.Now()
	rows := rowsFor([]proxy.ServiceView{
		{
			Service: "svc:web",
			Scheme:  "http",
			Caps:    []string{"example.com/cap/read"},
			Backends: []proxy.BackendView{{
				Backend:      "http://10.0.0.1:80",
				Key:          "abcdef1234567890",
				RegisteredAt: now.Add(-5 * time.Second),
			}},
		},
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Service != "svc:web" {
		t.Errorf("row = %+v", r)
	}
	if len(r.Caps) != 1 || r.Caps[0] != "example.com/cap/read" {
		t.Errorf("caps wrong: %+v", r)
	}
	if len(r.Backends) != 1 {
		t.Fatalf("backends = %d, want 1: %+v", len(r.Backends), r.Backends)
	}
	b := r.Backends[0]
	if b.Backend != "http://10.0.0.1:80" {
		t.Errorf("Backend = %q", b.Backend)
	}
	if b.KeyShort != "abcdef123456" {
		t.Errorf("KeyShort = %q", b.KeyShort)
	}
	if !strings.HasSuffix(b.RegisteredAgo, " ago") {
		t.Errorf("RegisteredAgo = %q, want suffix ' ago'", b.RegisteredAgo)
	}
}

// Account and Cluster come from each backend's own Origin, so a pool whose
// members were found by different readers reports them correctly per row.
func TestRowsFor_IncludesOriginPerBackend(t *testing.T) {
	rows := rowsFor([]proxy.ServiceView{{
		Service: "svc:web",
		Scheme:  "http",
		Backends: []proxy.BackendView{
			{
				Backend:      "http://10.0.0.1:80",
				Key:          "abc",
				RegisteredAt: time.Now(),
				Origin:       proxy.Origin{Account: "111111111111", Cluster: "cluster-a"},
			},
			{
				Backend:      "http://10.1.0.1:80",
				Key:          "def",
				RegisteredAt: time.Now(),
				Origin:       proxy.Origin{Account: "222222222222", Cluster: "cluster-b"},
			},
			{
				// Docker discovery leaves Origin zero.
				Backend:      "http://172.17.0.2:80",
				Key:          "ghi",
				RegisteredAt: time.Now(),
			},
		},
	}})
	if len(rows) != 1 || len(rows[0].Backends) != 3 {
		t.Fatalf("rows = %+v, want 1 service with 3 backends", rows)
	}
	got := rows[0].Backends
	if got[0].Account != "111111111111" || got[0].Cluster != "cluster-a" {
		t.Errorf("backend[0] origin = %+v", got[0])
	}
	if got[1].Account != "222222222222" || got[1].Cluster != "cluster-b" {
		t.Errorf("backend[1] origin = %+v", got[1])
	}
	if got[2].Account != "" || got[2].Cluster != "" {
		t.Errorf("backend[2] origin = %+v, want empty for Docker discovery", got[2])
	}
}

func TestReaderRowsFor_States(t *testing.T) {
	now := time.Now()
	rows := readerRowsFor(&fakeReaderSource{readers: []ReaderStatus{
		{Name: "healthy", Cluster: "c1", PollInterval: 10 * time.Second, Healthy: true, LastPollOK: now.Add(-5 * time.Second)},
		{Name: "broken", Cluster: "c2", PollInterval: 10 * time.Second, LastError: "AccessDenied"},
		{Name: "fresh", Cluster: "c3", PollInterval: 10 * time.Second},
	}})
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].State != "healthy" || rows[0].LastPolled == "never" {
		t.Errorf("healthy row wrong: %+v", rows[0])
	}
	if rows[1].State != "error" || rows[1].LastError != "AccessDenied" {
		t.Errorf("broken row wrong: %+v", rows[1])
	}
	if rows[2].State != "pending" || rows[2].LastPolled != "never" {
		t.Errorf("fresh row wrong: %+v", rows[2])
	}
}

func TestReaderRowsFor_NilSource(t *testing.T) {
	if got := readerRowsFor(nil); got != nil {
		t.Errorf("nil source should yield nil rows, got %v", got)
	}
}

func TestStatusHandler_RendersReadersTable(t *testing.T) {
	s := newTestServer(&fakeSnapshotter{})
	s.DiscoveryMode = "ecs"
	s.Readers = &fakeReaderSource{readers: []ReaderStatus{
		{Mode: "ecs", Name: "border", Account: "077542728448", Cluster: "border-ecs", Region: "ap-southeast-2",
			PollInterval: 10 * time.Second, LastError: "AccessDenied: not authorized to perform: sts:AssumeRole"},
		{Mode: "lambda", Name: "functions", Account: "333333333333", Region: "us-west-2",
			PollInterval: 30 * time.Second, Healthy: true, LastPollOK: time.Now()},
	}}

	rr := httptest.NewRecorder()
	s.statusHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()

	for _, want := range []string{
		"Readers (2)",
		"<th>Mode</th>",
		`<td class="mono">border</td>
        <td class="mono">ecs</td>`,
		"border-ecs",
		"077542728448",
		"ap-southeast-2",
		"AccessDenied",
		// The Lambda reader: its mode, account, region, and a dash for cluster.
		`<td class="mono">functions</td>
        <td class="mono">lambda</td>
        <td class="mono">333333333333</td>
        <td class="mono"><span class="empty">—</span></td>
        <td class="mono">us-west-2</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestStatusHandler_NoReadersTableInDockerMode(t *testing.T) {
	s := newTestServer(&fakeSnapshotter{}) // Readers is nil
	rr := httptest.NewRecorder()
	s.statusHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rr.Body.String(), "<h2>Readers") {
		t.Errorf("readers table should be hidden when no reader source is wired")
	}
}

func newTestServer(snap Snapshotter) *Server {
	return &Server{
		Logger:        discardLogger(),
		Manager:       snap,
		LocalClient:   nil, // -> readTailnet returns "no local client"
		DiscoveryMode: "docker",
		MetricsAddr:   "127.0.0.1:9090",
		TraefikPort:   0,
		Started:       time.Now().Add(-90 * time.Second),
	}
}

func TestStatusHandler_EmptyServices(t *testing.T) {
	s := newTestServer(&fakeSnapshotter{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.statusHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Services (0)") {
		t.Errorf("body missing 'Services (0)':\n%s", body)
	}
	if !strings.Contains(body, "No services registered") {
		t.Errorf("body missing empty-services hint:\n%s", body)
	}
	if !strings.Contains(body, "docker discovery") {
		t.Errorf("body missing discovery mode pill: %s", body)
	}
	// LocalClient is nil so the tailnet block should render the unavailable state.
	if !strings.Contains(body, "Local tailnet status could not be read") {
		t.Errorf("body missing tailnet-unavailable message:\n%s", body)
	}
}

// Two services — one with a single backend, one with two backends found by
// different readers — render as a Services (2) heading, every backend address,
// and both readers' accounts.
func TestStatusHandler_RendersServicesTable(t *testing.T) {
	now := time.Now()
	snap := &fakeSnapshotter{services: []proxy.ServiceView{
		{
			Service: "svc:web",
			Scheme:  "http",
			Backends: []proxy.BackendView{{
				Backend:      "http://10.0.0.1:80",
				Key:          "abcdef1234567890",
				RegisteredAt: now.Add(-30 * time.Second),
			}},
		},
		{
			Service: "svc:api",
			Scheme:  "https",
			Caps:    []string{"example.com/cap/admin"},
			Backends: []proxy.BackendView{
				{
					Backend:      "https://10.0.0.2:3000",
					Key:          "deadbeef0000",
					RegisteredAt: now.Add(-90 * time.Second),
					Origin:       proxy.Origin{Account: "111111111111", Cluster: "cluster-a"},
				},
				{
					Backend:      "https://10.9.0.7:3000",
					Key:          "feedface1111",
					RegisteredAt: now.Add(-20 * time.Second),
					Origin:       proxy.Origin{Account: "222222222222", Cluster: "cluster-b"},
				},
			},
		},
		{
			Service: "svc:fn",
			Scheme:  proxy.SchemeLambda,
			Backends: []proxy.BackendView{{
				Backend:      "lambda://arn:aws:lambda:us-west-2:333333333333:function:example-fn",
				Key:          "arn:aws:lambda:us-west-2:333333333333:function:example-fn",
				RegisteredAt: now.Add(-10 * time.Second),
				Origin:       proxy.Origin{Account: "333333333333", Region: "us-west-2"},
			}},
		},
	}}
	s := newTestServer(snap)

	rr := httptest.NewRecorder()
	s.statusHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()

	for _, want := range []string{
		"svc:web",
		"svc:api",
		"http://10.0.0.1:80",
		"https://10.0.0.2:3000",
		"https://10.9.0.7:3000",
		"example.com/cap/admin",
		"abcdef123456", // shortID truncates
		"deadbeef0000",
		"feedface1111",
		"111111111111",
		"222222222222",
		"Services (3)", // the heading counts advertised services, not backends
		"<th>Key</th>",
		// The function backend: lambda:// target, account, a dash for cluster,
		// and the function name in the key cell.
		`<td class="mono">lambda://arn:aws:lambda:us-west-2:333333333333:function:example-fn</td>`,
		`<td class="mono">333333333333</td>
        <td class="mono"><span class="empty">—</span></td>
        <td class="mono">example-fn</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// The service and caps cells span their backend group, so a two-backend
	// service names its service once.
	if n := strings.Count(body, ">svc:api<"); n != 1 {
		t.Errorf("svc:api rendered %d times, want 1 spanning cell", n)
	}
	if n := strings.Count(body, `rowspan="2"`); n != 2 {
		t.Errorf("rowspan=\"2\" appears %d times, want 2 (Service and Caps span the pool)\n--- body ---\n%s", n, body)
	}
	if n := strings.Count(body, `rowspan="1"`); n != 4 {
		t.Errorf("rowspan=\"1\" appears %d times, want 4 for the two single-backend services", n)
	}
}

func TestStatusHandler_TraefikEnabledLink(t *testing.T) {
	s := newTestServer(&fakeSnapshotter{})
	s.TraefikPort = 8081

	rr := httptest.NewRecorder()
	s.statusHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "http://localhost:8081") {
		t.Errorf("expected traefik forward link, body:\n%s", body)
	}
	if strings.Contains(body, "disabled (set") {
		t.Errorf("traefik should not show as disabled when port set")
	}
}

func TestStatusHandler_TraefikDisabled(t *testing.T) {
	s := newTestServer(&fakeSnapshotter{})
	s.TraefikPort = 0

	rr := httptest.NewRecorder()
	s.statusHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "disabled") {
		t.Errorf("expected traefik disabled message, body:\n%s", body)
	}
}

func TestStatusHandler_CacheControlNoStore(t *testing.T) {
	s := newTestServer(&fakeSnapshotter{})
	rr := httptest.NewRecorder()
	s.statusHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
