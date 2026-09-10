package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"tailscale.com/client/tailscale/apitype"
)

// Node identity headers tsserve injects for tagged peers when a service opts in
// (i.e. when tsserve.caps is set). Both are stripped from inbound requests
// before forwarding so a client cannot spoof them.
const (
	headerNodeTags = "Tailscale-Node-Tags"
	headerNodeName = "Tailscale-Node-Name"
)

// whoisFunc resolves the owner of a tailnet address. It matches
// (*local.Client).WhoIs so the production path passes that method directly and
// tests can inject a stub.
type whoisFunc func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)

// whoisTimeout bounds the in-process WhoIs lookup so a slow local call cannot
// stall proxying indefinitely.
const whoisTimeout = 2 * time.Second

// dialTimeout bounds connection establishment to a backend. Without it a
// blackholed member (say a terminated EC2 host) would hold a request for the
// Go default of 30s before another pool member is tried.
// A var rather than a const only so tests can shorten it; nothing outside
// tests assigns to it.
var dialTimeout = 5 * time.Second

// tlsHandshakeTimeout bounds a handshake the Transport performs itself, which
// on the http path means the one to an https:// proxy from the environment.
// It matches the value http.DefaultTransport uses for that same handshake.
// https backends are not covered by it: DialTLSContext below handshakes them
// inside the dialTimeout budget instead.
const tlsHandshakeTimeout = 10 * time.Second

// errNoBackend is returned when a request arrives for a service whose pool is
// empty — the handler was already running when the last member left.
var errNoBackend = errors.New("no backend registered for service")

// newReverseProxy builds the single httputil.ReverseProxy for one advertised
// service. Backends are chosen from pool per request rather than fixed at
// construction time, so a backend joining or leaving takes effect on the next
// request without rebuilding anything. When scheme is "https" the proxy
// transport skips TLS verification — backends typically present self-signed
// certs — and is shared by every pool member.
//
// onError, if non-nil, is invoked with a short reason classification ("timeout",
// "connection-refused", "other") once per failed attempt. When every attempt
// fails the HTTP response is a 502, exactly as before.
//
// When injectNode is true the proxy resolves the connecting peer via whois and,
// for tagged nodes, injects the Tailscale-Node-Tags and Tailscale-Node-Name
// headers. Those headers are always stripped from the inbound request first, so
// a client cannot spoof them regardless of whois resolution. whois may be nil:
// the headers are still stripped, but none are injected.
func newReverseProxy(scheme string, pool *backendPool, logger *slog.Logger, onError func(reason string), injectNode bool, whois whoisFunc) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Transport: &poolTransport{
			base:    newBackendTransport(scheme),
			pool:    pool,
			scheme:  scheme,
			logger:  logger,
			onError: onError,
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Only the scheme is known here. The host is deliberately left
			// unset: which backend receives the request is decided per attempt
			// inside poolTransport, which sets both the URL host and the Host
			// header on its own copy.
			pr.Out.URL.Scheme = scheme
			// Tailscale's serve layer fronts our listener and sets
			// X-Forwarded-For to the peer's tailnet IP before forwarding to us
			// over a loopback socket. httputil.ReverseProxy in Rewrite mode
			// strips client-provided forwarding headers from pr.Out, so we
			// explicitly copy the upstream header from pr.In. We deliberately do
			// not call pr.SetXForwarded(): it would otherwise insert our local
			// RemoteAddr (127.0.0.1) into X-Forwarded-For, which is useless to
			// the backend.
			if xff := pr.In.Header.Get("X-Forwarded-For"); xff != "" {
				pr.Out.Header.Set("X-Forwarded-For", xff)
			}
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")

			if injectNode {
				injectNodeHeaders(pr, logger, whois)
			}
		},
	}

	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Attempts made by poolTransport are already logged and counted, once
		// each. Errors that never reach the transport — protocol upgrades that
		// fail after a 101, an empty pool — are logged and counted here.
		var attempted *attemptError
		if !errors.As(err, &attempted) {
			logger.Warn("backend proxy error", "err", err)
			if onError != nil {
				onError(classifyProxyError(err))
			}
		}
		http.Error(w, fmt.Sprintf("bad gateway: %v", err), http.StatusBadGateway)
	}
	return rp
}

// poolTransport is the single RoundTripper of one advertised service. It picks
// the backend for each request from the service's pool, round-robin, and — for
// requests it can safely replay — walks the remaining members when a connection
// cannot be established.
//
// The retry lives at the RoundTrip boundary rather than in ErrorHandler because
// a RoundTrip that returns an error has written nothing to the client, so the
// retry is pre-commit by construction. ErrorHandler is reached too late: it also
// runs after a backend has answered 101 Switching Protocols, and after the
// client connection has been hijacked.
type poolTransport struct {
	base    http.RoundTripper
	pool    *backendPool
	scheme  string
	logger  *slog.Logger
	onError func(reason string)
}

func (t *poolTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	members, start := t.pool.rotation()
	if len(members) == 0 {
		return nil, errNoBackend
	}

	// A request that carries a body cannot be replayed: server requests have a
	// nil GetBody, so the first attempt consumes it. httputil.ReverseProxy
	// leaves the outbound body nil whenever the inbound request has none, which
	// covers both a missing body and http.NoBody.
	replayable := req.Body == nil

	var lastErr error
	for i := range members {
		b := members[(start+i)%len(members)]

		// Each attempt must carry the host of the backend it actually reaches —
		// not that of the first candidate — and the RoundTripper contract
		// forbids modifying the request we were given, which ReverseProxy still
		// holds (it passes it to ErrorHandler and to the protocol-upgrade path).
		attempt := req.Clone(req.Context())
		attempt.URL.Scheme = t.scheme
		attempt.URL.Host = b.addr
		attempt.Host = b.addr

		resp, err := t.base.RoundTrip(attempt)
		if err == nil {
			return resp, nil
		}

		t.logger.Warn("backend proxy error", "target", backendURL(t.scheme, b.addr), "err", err)
		if t.onError != nil {
			t.onError(classifyProxyError(err))
		}
		lastErr = &attemptError{err: err}

		if !replayable || !isConnectError(err) || req.Context().Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// attemptError marks an error poolTransport has already logged and counted, so
// ErrorHandler does not count the last failed attempt a second time.
type attemptError struct{ err error }

func (e *attemptError) Error() string { return e.err.Error() }
func (e *attemptError) Unwrap() error { return e.err }

// connectError marks a failure to establish a connection to a backend. The
// request had not been written when it happened, so another pool member can be
// tried without any risk of the backend having acted on it. Recognising these
// by type rather than by matching the error text is what lets the retry tell a
// dial timeout from a read timeout on a request that was already sent.
type connectError struct{ err error }

func (e *connectError) Error() string { return e.err.Error() }
func (e *connectError) Unwrap() error { return e.err }

func isConnectError(err error) bool {
	var ce *connectError
	return errors.As(err, &ce)
}

// CloseIdleConnections drops the idle backend connections this service is
// holding. The transport belongs to one advertised service, so once that
// service is withdrawn nothing can reuse them. Named to match the interface
// http.Client uses for the same purpose.
func (t *poolTransport) CloseIdleConnections() {
	if tr, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		tr.CloseIdleConnections()
	}
}

// newBackendTransport builds the transport for one advertised service, shared
// by every member of its pool. Its dialer wraps every connection-establishment
// failure — dial refused, dial timeout, no route, DNS failure and, for https, a
// failed TLS handshake — in a connectError so poolTransport can recognise them.
func newBackendTransport(scheme string) *http.Transport {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, &connectError{err: err}
		}
		return c, nil
	}

	tr := &http.Transport{
		// Unchanged from the transports this replaced: the http path came from
		// http.DefaultTransport, which honours the proxy environment, and the
		// https path was a bare Transport, which does not.
		Proxy:           http.ProxyFromEnvironment,
		DialContext:     dial,
		MaxIdleConns:    100,
		IdleConnTimeout: 90 * time.Second,
		// A pooled service fans out over several hosts and runs several
		// requests against each, so the Go default of 2 idle connections per
		// host would force a fresh dial (and, for https, a fresh handshake) on
		// most requests.
		MaxIdleConnsPerHost:   32,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: time.Second,
	}
	if scheme != "https" {
		return tr
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	tr.TLSClientConfig = tlsCfg
	// An https backend is never reached through a proxy (Proxy is consulted
	// first and would bypass this hook), so handshaking here rather than leaving
	// it to the Transport reliably keeps the failure inside a connectError, and
	// keeps these backends on HTTP/1.1 as before.
	tr.Proxy = nil
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// One budget covers dial and handshake together, so an https attempt is
		// bounded by the same few seconds as a plain one even against a host
		// that accepts the connection and then stalls.
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()

		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		cfg := tlsCfg
		// Backends are addressed by IP today, which carries no SNI either way.
		// A hostname-addressed one should still get the name the Transport
		// would have sent.
		if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil && net.ParseIP(host) == nil {
			cfg = tlsCfg.Clone()
			cfg.ServerName = host
		}
		tc := tls.Client(c, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = c.Close()
			return nil, &connectError{err: err}
		}
		return tc, nil
	}
	return tr
}

// injectNodeHeaders strips any inbound node-identity headers (spoof prevention)
// and, for tagged peers, replaces them with values resolved via whois. The peer
// IP comes from X-Forwarded-For, which the Tailscale serve layer sets to the
// peer's tailnet IP; pr.In.RemoteAddr is the loopback socket and useless here.
func injectNodeHeaders(pr *httputil.ProxyRequest, logger *slog.Logger, whois whoisFunc) {
	// Always strip first so a client-supplied value never survives, even when
	// the node is untagged or whois fails.
	pr.Out.Header.Del(headerNodeTags)
	pr.Out.Header.Del(headerNodeName)

	if whois == nil {
		return
	}

	ip := firstForwardedFor(pr.In.Header.Get("X-Forwarded-For"))
	if ip == "" {
		return
	}

	ctx, cancel := context.WithTimeout(pr.In.Context(), whoisTimeout)
	defer cancel()

	resp, err := whois(ctx, ip)
	if err != nil {
		logger.Debug("whois lookup failed; not injecting node headers", "ip", ip, "err", err)
		return
	}
	if resp == nil || resp.Node == nil || !resp.Node.IsTagged() {
		return
	}

	pr.Out.Header.Set(headerNodeTags, formatNodeTags(resp.Node.Tags))
	pr.Out.Header.Set(headerNodeName, resp.Node.ComputedName)
}

// firstForwardedFor returns the leftmost (original client) entry of an
// X-Forwarded-For header value, trimmed. Returns "" when the header is empty.
func firstForwardedFor(xff string) string {
	if xff == "" {
		return ""
	}
	if i := strings.IndexByte(xff, ','); i >= 0 {
		xff = xff[:i]
	}
	return strings.TrimSpace(xff)
}

// formatNodeTags renders ACL tags as a comma-separated list with the "tag:"
// prefix stripped, e.g. ["tag:github-runner", "tag:prod"] -> "github-runner,prod".
func formatNodeTags(tags []string) string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		out = append(out, strings.TrimPrefix(t, "tag:"))
	}
	return strings.Join(out, ",")
}

func classifyProxyError(err error) string {
	if err == nil {
		return "other"
	}
	if errors.Is(err, errNoBackend) {
		return "no-backend"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"), strings.Contains(s, "i/o timeout"):
		return "timeout"
	case strings.Contains(s, "connection refused"):
		return "connection-refused"
	case strings.Contains(s, "no such host"):
		return "dns"
	case strings.Contains(s, "EOF"):
		return "eof"
	default:
		return "other"
	}
}

// backendURL renders a backend for logs and the status page. The scheme is a
// property of the advertised service; the address is the backend's own.
func backendURL(scheme, addr string) string { return scheme + "://" + addr }
