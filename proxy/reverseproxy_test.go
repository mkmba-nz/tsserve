package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"mkmba.nz/tsserve/metrics"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// poolOf builds a backend pool from "host:port" addresses, in pool order.
func poolOf(addrs ...string) *backendPool {
	p := newBackendPool()
	for i, addr := range addrs {
		p.add(&backend{key: fmt.Sprintf("cid%d", i), addr: addr, registeredAt: time.Now()})
	}
	return p
}

// newSingleBackendProxy builds a reverse proxy for a service with exactly one
// backend, the shape most of these tests exercise.
func newSingleBackendProxy(scheme, backendIP string, port uint16, logger *slog.Logger, onError func(reason string), injectNode bool, whois whoisFunc) *httputil.ReverseProxy {
	return newReverseProxy(scheme, poolOf(net.JoinHostPort(backendIP, strconv.Itoa(int(port)))), logger, onError, injectNode, whois)
}

// closedPort binds and immediately releases a port, so connects to it are
// refused.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// countingBackend serves 200 OK and counts the requests it received.
func countingBackend(t *testing.T) (addr string, hits func() int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(r.Host))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	return u.Host, n.Load
}

// hostPort splits an httptest.Server URL into its host (IP) and port.
func hostPort(t *testing.T, s string) (string, uint16) {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	h, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %s: %v", u.Host, err)
	}
	pn, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port %s: %v", p, err)
	}
	return h, uint16(pn)
}

func TestReverseProxy_ForwardsRequestAndHeaders(t *testing.T) {
	var gotMethod, gotPath, gotXFFProto, gotXFFHost, gotHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotXFFProto = r.Header.Get("X-Forwarded-Proto")
		gotXFFHost = r.Header.Get("X-Forwarded-Host")
		gotHost = r.Host
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))
	defer backend.Close()

	host, port := hostPort(t, backend.URL)
	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, false, nil)

	req := httptest.NewRequest(http.MethodPost, "/foo/bar", strings.NewReader("body"))
	req.Host = "svc.example.ts.net"
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}
	if body := rr.Body.String(); body != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
	if gotMethod != http.MethodPost {
		t.Errorf("backend method = %q, want POST", gotMethod)
	}
	if gotPath != "/foo/bar" {
		t.Errorf("backend path = %q", gotPath)
	}
	if gotXFFProto != "https" {
		t.Errorf("X-Forwarded-Proto on backend = %q, want https", gotXFFProto)
	}
	if gotXFFHost != "svc.example.ts.net" {
		t.Errorf("X-Forwarded-Host on backend = %q, want svc.example.ts.net", gotXFFHost)
	}
	// poolTransport sets the outbound Host to the selected backend, so the
	// backend sees the IP:port, not the original svc:... hostname.
	if !strings.HasPrefix(gotHost, host+":") {
		t.Errorf("backend Host = %q, want prefix %q", gotHost, host+":")
	}
}

// In production, tsnet's serve layer fronts the listener and sets
// X-Forwarded-For to the peer's tailnet IP before our handler ever sees the
// request. Our Rewrite must NOT clobber that with the local loopback
// RemoteAddr that the inbound request carries on its way to us.
func TestReverseProxy_PreservesUpstreamXForwardedFor(t *testing.T) {
	var gotXFF string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
	}))
	defer backend.Close()

	host, port := hostPort(t, backend.URL)
	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.42") // simulated upstream peer IP
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotXFF != "100.64.0.42" {
		t.Errorf("X-Forwarded-For at backend = %q, want %q (must not be overwritten or appended with loopback)",
			gotXFF, "100.64.0.42")
	}
}

// When the inbound request has no X-Forwarded-For (e.g. a unit test, or a
// configuration where tsnet's serve layer didn't set it), we must NOT
// fabricate one from the loopback RemoteAddr.
func TestReverseProxy_DoesNotAddLoopbackXForwardedFor(t *testing.T) {
	var gotXFF string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
	}))
	defer backend.Close()

	host, port := hostPort(t, backend.URL)
	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, false, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No X-Forwarded-For on the inbound request. RemoteAddr is whatever
	// httptest uses (e.g. "192.0.2.1:1234") — a stand-in for loopback in
	// production.
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotXFF != "" {
		t.Errorf("X-Forwarded-For at backend = %q, want empty (must not synthesize from RemoteAddr)", gotXFF)
	}
}

func TestReverseProxy_ConnectionRefused_502AndErrorClassified(t *testing.T) {
	// Bind to a port, immediately release it: subsequent connects refuse.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, port := hostPort(t, "http://"+l.Addr().String())
	_ = l.Close()

	var calls atomic.Int32
	var lastReason atomic.Value
	rp := newSingleBackendProxy("http", host, port, discardLogger(), func(reason string) {
		calls.Add(1)
		lastReason.Store(reason)
	}, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("onError called %d times, want 1", calls.Load())
	}
	if got := lastReason.Load().(string); got != "connection-refused" {
		t.Errorf("reason = %q, want connection-refused", got)
	}
}

func TestReverseProxy_HTTPSBackendSkipsTLSVerify(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secured"))
	}))
	defer backend.Close()

	host, port := hostPort(t, backend.URL)
	rp := newSingleBackendProxy("https", host, port, discardLogger(), nil, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "secured" {
		t.Errorf("body = %q", rr.Body.String())
	}

	// Sanity: an http-scheme transport against the same backend would fail TLS.
	pt, ok := rp.Transport.(*poolTransport)
	if !ok {
		t.Fatalf("transport type = %T, want *poolTransport", rp.Transport)
	}
	tr, ok := pt.base.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("https transport not configured for skip-verify: %+v", pt.base)
	}
	// Avoid "tls unused" import warnings if the assertion ever weakens.
	_ = tls.VersionTLS12
}

func TestReverseProxy_OnErrorNilIsSafe(t *testing.T) {
	// onError == nil must not panic.
	rp := newSingleBackendProxy("http", "127.0.0.1", 1, discardLogger(), nil, false, nil)
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}

func TestClassifyProxyError(t *testing.T) {
	cases := []struct {
		msg  string
		want string
	}{
		{"dial tcp 1.2.3.4:80: i/o timeout", "timeout"},
		{"context deadline exceeded", "timeout"},
		{"dial tcp 127.0.0.1:1: connect: connection refused", "connection-refused"},
		{"dial tcp: lookup foo.bar: no such host", "dns"},
		{"unexpected EOF", "eof"},
		{"something else", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := classifyProxyError(stringError(tc.msg))
			if got != tc.want {
				t.Errorf("classify(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
	if got := classifyProxyError(nil); got != "other" {
		t.Errorf("classify(nil) = %q, want other", got)
	}
}

type stringError string

func (s stringError) Error() string { return string(s) }

// taggedWhois returns a whoisFunc that resolves any address to a tagged node
// with the given tags and computed name.
func taggedWhois(name string, tags ...string) whoisFunc {
	return func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
		return &apitype.WhoIsResponse{
			Node: &tailcfg.Node{Tags: tags, ComputedName: name},
		}, nil
	}
}

// nodeHeaderBackend spins up a backend that records the node-identity headers it
// receives and returns the test target host/port plus a getter.
func nodeHeaderBackend(t *testing.T) (host string, port uint16, got func() (tags, name string)) {
	t.Helper()
	var gotTags, gotName string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTags = r.Header.Get(headerNodeTags)
		gotName = r.Header.Get(headerNodeName)
	}))
	t.Cleanup(backend.Close)
	h, p := hostPort(t, backend.URL)
	return h, p, func() (string, string) { return gotTags, gotName }
}

// When injectNode is on and the peer resolves to a tagged node, the backend sees
// Tailscale-Node-Tags (tag: prefix stripped) and Tailscale-Node-Name.
func TestReverseProxy_InjectsNodeHeadersForTaggedPeer(t *testing.T) {
	host, port, got := nodeHeaderBackend(t)

	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, true,
		taggedWhois("runner-01", "tag:github-runner", "tag:prod"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.42")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	gotTags, gotName := got()
	if gotTags != "github-runner,prod" {
		t.Errorf("Tailscale-Node-Tags = %q, want %q", gotTags, "github-runner,prod")
	}
	if gotName != "runner-01" {
		t.Errorf("Tailscale-Node-Name = %q, want %q", gotName, "runner-01")
	}
}

// An untagged peer gets no node headers, and any client-supplied (spoofed) node
// headers are stripped before forwarding.
func TestReverseProxy_StripsSpoofedNodeHeadersForUntaggedPeer(t *testing.T) {
	host, port, got := nodeHeaderBackend(t)

	untagged := func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
		return &apitype.WhoIsResponse{Node: &tailcfg.Node{ComputedName: "laptop"}}, nil
	}
	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, true, untagged)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.42")
	req.Header.Set(headerNodeTags, "tag:admin") // spoof attempt
	req.Header.Set(headerNodeName, "evil")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotTags, gotName := got(); gotTags != "" || gotName != "" {
		t.Errorf("node headers leaked: tags=%q name=%q, want both empty", gotTags, gotName)
	}
}

// When whois fails (e.g. ErrPeerNotFound), the inbound spoofed headers are still
// stripped and nothing is injected.
func TestReverseProxy_StripsNodeHeadersWhenWhoisFails(t *testing.T) {
	host, port, got := nodeHeaderBackend(t)

	failing := func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error) {
		return nil, errors.New("peer not found")
	}
	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, true, failing)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.42")
	req.Header.Set(headerNodeTags, "tag:admin")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotTags, gotName := got(); gotTags != "" || gotName != "" {
		t.Errorf("node headers leaked on whois failure: tags=%q name=%q", gotTags, gotName)
	}
}

// With injectNode off, node headers pass through untouched: tsserve does not
// strip or inject them when a service has not opted in.
func TestReverseProxy_NoInjectionWhenDisabled(t *testing.T) {
	host, port, got := nodeHeaderBackend(t)

	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, false,
		taggedWhois("runner-01", "tag:github-runner"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.42")
	req.Header.Set(headerNodeTags, "passthrough")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotTags, _ := got(); gotTags != "passthrough" {
		t.Errorf("Tailscale-Node-Tags = %q, want passthrough (untouched when disabled)", gotTags)
	}
}

// injectNode on but a nil whois (no LocalClient): headers are still stripped, but
// none are injected.
func TestReverseProxy_NilWhoisStripsButDoesNotInject(t *testing.T) {
	host, port, got := nodeHeaderBackend(t)

	rp := newSingleBackendProxy("http", host, port, discardLogger(), nil, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.42")
	req.Header.Set(headerNodeTags, "tag:admin")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotTags, gotName := got(); gotTags != "" || gotName != "" {
		t.Errorf("node headers present with nil whois: tags=%q name=%q", gotTags, gotName)
	}
}

// Successive requests to a two-member pool alternate between the members: over
// four requests each backend receives exactly two.
func TestReverseProxy_RoundRobinsAcrossPool(t *testing.T) {
	addrA, hitsA := countingBackend(t)
	addrB, hitsB := countingBackend(t)

	rp := newReverseProxy("http", poolOf(addrA, addrB), discardLogger(), nil, false, nil)

	seen := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rr.Code)
		}
		// The backend echoes the Host header it received, proving each attempt
		// carries the address of the member that actually served it.
		seen = append(seen, rr.Body.String())
	}
	if hitsA() != 2 || hitsB() != 2 {
		t.Errorf("hits = %d/%d, want 2/2 over four requests", hitsA(), hitsB())
	}
	if seen[0] == seen[1] {
		t.Errorf("consecutive requests hit the same backend: %v", seen)
	}
	if seen[0] != seen[2] || seen[1] != seen[3] {
		t.Errorf("rotation is not in pool order: %v", seen)
	}
}

// A body-less GET against a pool of {closed port, live backend} succeeds on the
// second member, and the failed attempt is counted exactly once.
func TestReverseProxy_RetriesBodylessRequestOnConnectFailure(t *testing.T) {
	live, hits := countingBackend(t)
	dead := closedPort(t)

	var errCalls atomic.Int32
	var lastReason atomic.Value
	rp := newReverseProxy("http", poolOf(dead, live), discardLogger(), func(reason string) {
		errCalls.Add(1)
		lastReason.Store(reason)
	}, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (retry must reach the live backend); body=%q", rr.Code, rr.Body.String())
	}
	if hits() != 1 {
		t.Errorf("live backend hits = %d, want 1", hits())
	}
	if errCalls.Load() != 1 {
		t.Errorf("backend errors = %d, want exactly 1 (one failed attempt)", errCalls.Load())
	}
	if got, _ := lastReason.Load().(string); got != "connection-refused" {
		t.Errorf("reason = %q, want connection-refused", got)
	}
	if rr.Body.String() != live {
		t.Errorf("served Host = %q, want the retried backend %q", rr.Body.String(), live)
	}
}

// A request that carries a body is never replayed: it fails on the member it
// was assigned without contacting the second.
func TestReverseProxy_DoesNotRetryRequestWithBody(t *testing.T) {
	live, hits := countingBackend(t)
	dead := closedPort(t)

	var errCalls atomic.Int32
	rp := newReverseProxy("http", poolOf(dead, live), discardLogger(), func(string) {
		errCalls.Add(1)
	}, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload")))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if hits() != 0 {
		t.Errorf("live backend hits = %d, want 0 (a request with a body must not be replayed)", hits())
	}
	if errCalls.Load() != 1 {
		t.Errorf("backend errors = %d, want exactly 1", errCalls.Load())
	}
}

// A backend that answers with a 500 has received the request, so it is returned
// to the client rather than retried against another member.
func TestReverseProxy_DoesNotRetryBackendErrorResponse(t *testing.T) {
	var failHits atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failHits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	failURL, err := url.Parse(failing.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	live, liveHits := countingBackend(t)

	var errCalls atomic.Int32
	rp := newReverseProxy("http", poolOf(failURL.Host, live), discardLogger(), func(string) {
		errCalls.Add(1)
	}, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 passed through", rr.Code)
	}
	if failHits.Load() != 1 || liveHits() != 0 {
		t.Errorf("hits = %d/%d, want 1/0 (a 500 is a response, not a connect failure)", failHits.Load(), liveHits())
	}
	if errCalls.Load() != 0 {
		t.Errorf("backend errors = %d, want 0", errCalls.Load())
	}
}

// When every member refuses the connection the client gets a 502 and each
// attempt is counted exactly once — the last is not double-counted by
// ErrorHandler.
func TestReverseProxy_AllBackendsFail502CountsEachAttemptOnce(t *testing.T) {
	pool := poolOf(closedPort(t), closedPort(t))

	var errCalls atomic.Int32
	rp := newReverseProxy("http", pool, discardLogger(), func(string) {
		errCalls.Add(1)
	}, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if errCalls.Load() != 2 {
		t.Errorf("backend errors = %d, want 2 (one per attempt, no double count)", errCalls.Load())
	}
}

// Pool changes take effect on the next request without rebuilding the proxy.
func TestReverseProxy_PoolChangesTakeEffectPerRequest(t *testing.T) {
	first, firstHits := countingBackend(t)
	second, secondHits := countingBackend(t)

	pool := poolOf(first)
	rp := newReverseProxy("http", pool, discardLogger(), nil, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if firstHits() != 1 || secondHits() != 0 {
		t.Fatalf("hits = %d/%d, want 1/0 before the pool changed", firstHits(), secondHits())
	}

	pool.add(&backend{key: "cid-late", addr: second, registeredAt: time.Now()})
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
	}
	if secondHits() != 1 {
		t.Errorf("late backend hits = %d, want 1", secondHits())
	}

	pool.remove("cid-late")
	before := secondHits()
	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if got := secondHits(); got != before {
		t.Errorf("removed backend received %d further requests, want 0", got-before)
	}
}

// A request arriving after the last pool member left gets a 502 rather than
// dividing by zero on an empty pool.
func TestReverseProxy_EmptyPoolReturns502(t *testing.T) {
	var errCalls atomic.Int32
	var reason atomic.Value
	rp := newReverseProxy("http", newBackendPool(), discardLogger(), func(r string) {
		errCalls.Add(1)
		reason.Store(r)
	}, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if errCalls.Load() != 1 {
		t.Errorf("backend errors = %d, want 1", errCalls.Load())
	}
	if got, _ := reason.Load().(string); got != "no-backend" {
		t.Errorf("reason = %q, want no-backend", got)
	}
}

// Round-robin selection is safe under concurrent requests, and every request is
// served.
func TestReverseProxy_ConcurrentRequestsAllServed(t *testing.T) {
	addrA, hitsA := countingBackend(t)
	addrB, hitsB := countingBackend(t)
	rp := newReverseProxy("http", poolOf(addrA, addrB), discardLogger(), nil, false, nil)

	const N = 60
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := httptest.NewRecorder()
			rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
			if rr.Code == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()

	if ok.Load() != N {
		t.Errorf("%d/%d requests served with 200", ok.Load(), N)
	}
	if total := hitsA() + hitsB(); total != N {
		t.Errorf("backends received %d requests, want %d", total, N)
	}
	if hitsA() != N/2 || hitsB() != N/2 {
		t.Errorf("hits = %d/%d, want %d/%d — the rotation must advance exactly once per request",
			hitsA(), hitsB(), N/2, N/2)
	}
}

// The retry stops as soon as the client's request context is done.
func TestReverseProxy_StopsRetryingWhenClientContextDone(t *testing.T) {
	live, hits := countingBackend(t)
	dead := closedPort(t)

	var errCalls atomic.Int32
	var cancelOnFirstError context.CancelFunc
	rp := newReverseProxy("http", poolOf(dead, live), discardLogger(), func(string) {
		errCalls.Add(1)
		cancelOnFirstError()
	}, false, nil)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel from the first attempt's failure callback: the client goes away
	// between attempts, which is the case the retry loop has to notice.
	cancelOnFirstError = cancel
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if hits() != 0 {
		t.Errorf("live backend hits = %d, want 0 once the client has gone away", hits())
	}
	if errCalls.Load() != 1 {
		t.Errorf("backend errors = %d, want 1 (only the attempt that failed)", errCalls.Load())
	}
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
}

// A TLS handshake failure happens before the request is written, so it is
// retried against the next pool member just like a refused dial.
func TestReverseProxy_RetriesOnTLSHandshakeFailure(t *testing.T) {
	// A plain-HTTP listener in an https pool: the TCP connection is accepted and
	// the handshake then fails.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	plainURL, err := url.Parse(plain.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var served atomic.Int32
	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		_, _ = w.Write([]byte("secured"))
	}))
	defer tlsBackend.Close()
	tlsURL, err := url.Parse(tlsBackend.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var errCalls atomic.Int32
	rp := newReverseProxy("https", poolOf(plainURL.Host, tlsURL.Host), discardLogger(), func(string) {
		errCalls.Add(1)
	}, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "secured" {
		t.Errorf("body = %q, want the second backend's response", rr.Body.String())
	}
	if served.Load() != 1 {
		t.Errorf("tls backend served %d requests, want 1", served.Load())
	}
	if errCalls.Load() != 1 {
		t.Errorf("backend errors = %d, want 1 (the failed handshake)", errCalls.Load())
	}
}

// A request that succeeds on a retry is counted with the code it actually
// received, not as the 502 the first attempt would have produced.
func TestReverseProxy_RetriedRequestRecordsActualResponseCode(t *testing.T) {
	live, _ := countingBackend(t)
	mc := metrics.New()

	rp := newReverseProxy("http", poolOf(closedPort(t), live), discardLogger(), func(reason string) {
		mc.Errors.WithLabelValues("svc:web", reason).Inc()
	}, false, nil)
	handler := mc.Middleware("svc:web", rp)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	if got := testutil.ToFloat64(mc.Requests.WithLabelValues("svc:web", http.MethodGet, "200")); got != 1 {
		t.Errorf("tsserve_proxy_requests_total{code=200} = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(mc.Requests); got != 1 {
		t.Errorf("request series = %d, want 1: the retried request must not also be recorded as a 502", got)
	}
	if got := testutil.ToFloat64(mc.Errors.WithLabelValues("svc:web", "connection-refused")); got != 1 {
		t.Errorf("tsserve_proxy_backend_errors_total = %v, want 1", got)
	}
}

// Each attempt's error log names the backend it targeted, so the line for the
// 502 identifies the last backend tried.
func TestReverseProxy_ErrorLogNamesEachBackendTried(t *testing.T) {
	first, second := closedPort(t), closedPort(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	rp := newReverseProxy("http", poolOf(first, second), logger, nil, false, nil)
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}

	out := buf.String()
	for _, want := range []string{"target=http://" + first, "target=http://" + second} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
	if n := strings.Count(out, "backend proxy error"); n != 2 {
		t.Errorf("error lines = %d, want 2 (one per attempt, none added by ErrorHandler)\n--- log ---\n%s", n, out)
	}
}

// An https backend that completes the TCP connection and then stalls must fail
// within the dial budget rather than hang. This is the case the transport's own
// TLSHandshakeTimeout does not cover: DialTLSContext takes over the whole
// connect path for https, so the budget has to span dial and handshake
// together.
func TestBackendTransport_HTTPSStalledHandshakeFailsWithinDialBudget(t *testing.T) {
	orig := dialTimeout
	dialTimeout = 150 * time.Millisecond
	t.Cleanup(func() { dialTimeout = orig })

	// Accepts the connection, then never speaks TLS.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			defer c.Close()
		}
	}()

	tr := newBackendTransport("https")
	req, err := http.NewRequest("GET", "https://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := tr.RoundTrip(req)
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip succeeded against a backend that never completed the TLS handshake")
	}
	select {
	case <-accepted:
	default:
		t.Fatal("backend never accepted the connection; the test did not exercise the handshake stall")
	}
	// Generous ceiling: the point is that some budget applies. Without one the
	// handshake waits indefinitely and this test times out instead.
	if elapsed > 2*time.Second {
		t.Errorf("stalled handshake took %v to fail, want it bounded by the %v dial budget", elapsed, dialTimeout)
	}
	// A connect-phase failure is what makes the request retryable against the
	// remaining pool members.
	if !isConnectError(err) {
		t.Errorf("error %[1]T (%[1]v) is not a connect error, so a stalled handshake would not fail over", err)
	}
}

// A protocol upgrade (websocket and friends) is hijacked end to end and is
// therefore pinned to the backend that answered it. Removing that backend from
// the pool must not disturb the connection: pool membership decides where the
// *next* request goes, not where an established one is already talking.
func TestReverseProxy_UpgradedConnectionSurvivesBackendLeavingPool(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "expected an upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n"); err != nil {
			return
		}
		if err := buf.Flush(); err != nil {
			return
		}
		// Echo whatever arrives, for as long as the client keeps it open.
		for {
			line, err := buf.ReadString('\n')
			if err != nil {
				return
			}
			if _, err := buf.WriteString(line); err != nil {
				return
			}
			if err := buf.Flush(); err != nil {
				return
			}
		}
	}))
	defer backend.Close()

	pool := poolOf(backend.Listener.Addr().String())
	front := httptest.NewServer(newReverseProxy("http", pool, discardLogger(), nil, false, nil))
	defer front.Close()

	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	req := "GET / HTTP/1.1\r\nHost: example\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q, want 101 Switching Protocols", strings.TrimSpace(status))
	}
	// Drain the remaining response headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// The upgraded connection works before the pool changes.
	if got := echoOnce(t, conn, br, "before\n"); got != "before\n" {
		t.Fatalf("echo before removal = %q, want %q", got, "before\n")
	}

	// Take the backend out of the pool, exactly as a Deregister would.
	if size := pool.remove("cid0"); size != 0 {
		t.Fatalf("pool size after remove = %d, want 0", size)
	}

	// The established connection is unaffected.
	if got := echoOnce(t, conn, br, "after\n"); got != "after\n" {
		t.Errorf("echo after removal = %q, want %q — the hijacked connection must stay bound to its backend", got, "after\n")
	}
}

// echoOnce writes a line to an upgraded connection and reads the echoed line.
func echoOnce(t *testing.T, conn net.Conn, br *bufio.Reader, line string) string {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo of %q: %v", line, err)
	}
	return got
}
