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
	} {
		if got := shortID(tc.in); got != tc.want {
			t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRowsFor_MapsServiceFields(t *testing.T) {
	now := time.Now()
	rows := rowsFor([]proxy.ServiceView{
		{
			Service:      "svc:web",
			Backend:      "http://10.0.0.1:80",
			Caps:         []string{"example.com/cap/read"},
			ContainerID:  "abcdef1234567890",
			RegisteredAt: now.Add(-5 * time.Second),
		},
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Service != "svc:web" || r.Backend != "http://10.0.0.1:80" {
		t.Errorf("row = %+v", r)
	}
	if len(r.Caps) != 1 || r.Caps[0] != "example.com/cap/read" {
		t.Errorf("caps wrong: %+v", r)
	}
	if r.ContainerShort != "abcdef123456" {
		t.Errorf("ContainerShort = %q", r.ContainerShort)
	}
	if !strings.HasSuffix(r.RegisteredAgo, " ago") {
		t.Errorf("RegisteredAgo = %q, want suffix ' ago'", r.RegisteredAgo)
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

func TestStatusHandler_RendersServicesTable(t *testing.T) {
	now := time.Now()
	snap := &fakeSnapshotter{services: []proxy.ServiceView{
		{
			Service:      "svc:web",
			Backend:      "http://10.0.0.1:80",
			ContainerID:  "abcdef1234567890",
			RegisteredAt: now.Add(-30 * time.Second),
		},
		{
			Service:      "svc:api",
			Backend:      "https://10.0.0.2:3000",
			Caps:         []string{"example.com/cap/admin"},
			ContainerID:  "deadbeef0000",
			RegisteredAt: now.Add(-90 * time.Second),
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
		"example.com/cap/admin",
		"abcdef123456", // shortID truncates
		"deadbeef0000",
		"Services (2)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
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
