package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
	rp := newReverseProxy("http", host, port, discardLogger(), nil, false, nil)

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
	// proxy.Rewrite sets pr.Out.Host = target.Host, so the backend sees the
	// IP:port, not the original svc:... hostname.
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
	rp := newReverseProxy("http", host, port, discardLogger(), nil, false, nil)

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
	rp := newReverseProxy("http", host, port, discardLogger(), nil, false, nil)

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
	rp := newReverseProxy("http", host, port, discardLogger(), func(reason string) {
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
	rp := newReverseProxy("https", host, port, discardLogger(), nil, false, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "secured" {
		t.Errorf("body = %q", rr.Body.String())
	}

	// Sanity: an http-scheme transport against the same backend would fail TLS.
	tr, ok := rp.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("https transport not configured for skip-verify: %+v", rp.Transport)
	}
	// Avoid "tls unused" import warnings if the assertion ever weakens.
	_ = tls.VersionTLS12
}

func TestReverseProxy_OnErrorNilIsSafe(t *testing.T) {
	// onError == nil must not panic.
	rp := newReverseProxy("http", "127.0.0.1", 1, discardLogger(), nil, false, nil)
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

	rp := newReverseProxy("http", host, port, discardLogger(), nil, true,
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
	rp := newReverseProxy("http", host, port, discardLogger(), nil, true, untagged)

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
	rp := newReverseProxy("http", host, port, discardLogger(), nil, true, failing)

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

	rp := newReverseProxy("http", host, port, discardLogger(), nil, false,
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

	rp := newReverseProxy("http", host, port, discardLogger(), nil, true, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.42")
	req.Header.Set(headerNodeTags, "tag:admin")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	if gotTags, gotName := got(); gotTags != "" || gotName != "" {
		t.Errorf("node headers present with nil whois: tags=%q name=%q", gotTags, gotName)
	}
}
