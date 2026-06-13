package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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

// newReverseProxy builds an httputil.ReverseProxy targeting backendIP:port over
// the given scheme. When scheme is "https" the proxy transport skips TLS
// verification — backends typically present self-signed certs.
//
// onError, if non-nil, is invoked with a short reason classification ("timeout",
// "connection-refused", "other") whenever the upstream call fails. The HTTP
// response is still a 502, exactly as before.
//
// When injectNode is true the proxy resolves the connecting peer via whois and,
// for tagged nodes, injects the Tailscale-Node-Tags and Tailscale-Node-Name
// headers. Those headers are always stripped from the inbound request first, so
// a client cannot spoof them regardless of whois resolution. whois may be nil:
// the headers are still stripped, but none are injected.
func newReverseProxy(scheme, backendIP string, port uint16, logger *slog.Logger, onError func(reason string), injectNode bool, whois whoisFunc) *httputil.ReverseProxy {
	target := &url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(backendIP, strconv.Itoa(int(port))),
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
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
			pr.Out.Host = target.Host

			if injectNode {
				injectNodeHeaders(pr, logger, whois)
			}
		},
	}

	if scheme == "https" {
		rp.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Warn("backend proxy error", "target", target.String(), "err", err)
		if onError != nil {
			onError(classifyProxyError(err))
		}
		http.Error(w, fmt.Sprintf("bad gateway: %v", err), http.StatusBadGateway)
	}
	return rp
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
