package proxy

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// newReverseProxy builds an httputil.ReverseProxy targeting backendIP:port over
// the given scheme. When scheme is "https" the proxy transport skips TLS
// verification — backends typically present self-signed certs.
//
// onError, if non-nil, is invoked with a short reason classification ("timeout",
// "connection-refused", "other") whenever the upstream call fails. The HTTP
// response is still a 502, exactly as before.
func newReverseProxy(scheme, backendIP string, port uint16, logger *slog.Logger, onError func(reason string)) *httputil.ReverseProxy {
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
