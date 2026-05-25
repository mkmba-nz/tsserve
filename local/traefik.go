package local

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

// ParseTraefikPort accepts "" (returns 0, meaning disabled) or a decimal port
// 1..65535. Anything else returns an error so misconfiguration fails fast at
// startup rather than producing a confused 404 later.
func ParseTraefikPort(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("TSSERVE_TRAEFIK_PORT=%q: not a number", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("TSSERVE_TRAEFIK_PORT=%d: must be 1..65535", n)
	}
	return n, nil
}

// traefikHandler returns an http.Handler mounted at /traefik that forwards
// to http://localhost:<port>. When port is 0 the handler returns a 404 with
// a one-line hint pointing at TSSERVE_TRAEFIK_PORT. Note: for this handler
// to work, traefik needs to be configured with api.basepath: /traefik/
func traefikHandler(port int) http.Handler {
	if port == 0 {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "tsserve: /traefik is disabled; set TSSERVE_TRAEFIK_PORT to enable.\n", http.StatusNotFound)
		})
	}
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, fmt.Sprintf("tsserve: traefik proxy error: %v\n", err), http.StatusBadGateway)
	}
	return rp
}
