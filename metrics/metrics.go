// Package metrics defines the Prometheus collectors used by tsserve and a
// middleware that records per-service request stats. Collectors live on a
// dedicated registry to keep tsserve's exposition free of unrelated metrics
// pulled in by transitive dependencies.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Collector groups the prometheus collectors tsserve exposes. All collectors
// register against an embedded Registry obtained from Registry().
type Collector struct {
	reg *prometheus.Registry

	Requests  *prometheus.CounterVec
	Duration  *prometheus.HistogramVec
	ReqBytes  *prometheus.CounterVec
	RespBytes *prometheus.CounterVec
	InFlight  *prometheus.GaugeVec
	Errors    *prometheus.CounterVec
	Active    prometheus.Gauge
}

// New builds a Collector with a fresh registry, the standard go/process
// collectors, and a tsserve_build_info gauge populated from debug.BuildInfo.
func New() *Collector {
	reg := prometheus.NewRegistry()

	c := &Collector{
		reg: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tsserve_proxy_requests_total",
			Help: "Total proxied HTTP requests, labelled by service, method, and response status code.",
		}, []string{"service", "method", "code"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "tsserve_proxy_request_duration_seconds",
			Help:    "Proxied HTTP request handler duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"service", "method"}),
		ReqBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tsserve_proxy_request_bytes_total",
			Help: "Total bytes read from proxied HTTP request bodies, by service.",
		}, []string{"service"}),
		RespBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tsserve_proxy_response_bytes_total",
			Help: "Total bytes written to proxied HTTP response bodies, by service.",
		}, []string{"service"}),
		InFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tsserve_proxy_in_flight_requests",
			Help: "Current in-flight proxied HTTP requests, by service.",
		}, []string{"service"}),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tsserve_proxy_backend_errors_total",
			Help: "Backend errors reported by the reverse proxy, by service and reason.",
		}, []string{"service", "reason"}),
		Active: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tsserve_services_active",
			Help: "Number of Tailscale Services currently registered and serving.",
		}),
	}

	reg.MustRegister(
		c.Requests, c.Duration, c.ReqBytes, c.RespBytes,
		c.InFlight, c.Errors, c.Active,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)
	return c
}

// Registry returns the prometheus Gatherer/Registerer pair as a Gatherer for
// use with promhttp.HandlerFor.
func (c *Collector) Registry() *prometheus.Registry { return c.reg }

// Middleware wraps next with per-request accounting against the named service.
// Safe to call with a nil Collector receiver: it returns next unchanged.
func (c *Collector) Middleware(service string, next http.Handler) http.Handler {
	if c == nil {
		return next
	}
	reqs := c.Requests
	dur := c.Duration
	reqB := c.ReqBytes.WithLabelValues(service)
	respB := c.RespBytes.WithLabelValues(service)
	inflight := c.InFlight.WithLabelValues(service)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Inc()
		defer inflight.Dec()

		start := time.Now()
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &countingReadCloser{ReadCloser: r.Body, n: reqB}
		}
		cw := &countingWriter{ResponseWriter: w, status: http.StatusOK, bytes: respB}
		next.ServeHTTP(cw, r)

		dur.WithLabelValues(service, r.Method).Observe(time.Since(start).Seconds())
		reqs.WithLabelValues(service, r.Method, strconv.Itoa(cw.status)).Inc()
	})
}

type countingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       prometheus.Counter
}

func (w *countingWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		w.bytes.Add(float64(n))
	}
	return n, err
}

// Flush forwards to the underlying ResponseWriter when supported so that
// streaming responses (e.g. from the reverse proxy) still flush.
func (w *countingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter when supported so that
// protocol upgrades (e.g. WebSocket 101 Switching Protocols) handled by the
// reverse proxy can take over the connection. Without this, the embedded
// ResponseWriter interface does not promote Hijack, and the proxy fails with
// "can't switch protocols using non-Hijacker ResponseWriter".
func (w *countingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("metrics: underlying ResponseWriter %T does not implement http.Hijacker", w.ResponseWriter)
	}
	return hj.Hijack()
}

type countingReadCloser struct {
	io.ReadCloser
	n prometheus.Counter
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.n.Add(float64(n))
	}
	return n, err
}

func buildInfo() prometheus.Collector {
	version := "unknown"
	revision := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				revision = s.Value
			}
		}
	}
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tsserve_build_info",
		Help: "tsserve build information; the value is always 1.",
		ConstLabels: prometheus.Labels{
			"version":    version,
			"revision":   revision,
			"go_version": runtime.Version(),
		},
	})
	g.Set(1)
	return g
}
