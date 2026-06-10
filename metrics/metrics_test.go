package metrics

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// hijackableRecorder is an httptest.ResponseRecorder that also implements
// http.Hijacker, standing in for the real net/http server connection.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

// TestMiddleware_PreservesHijacker guards against the reverse proxy failing
// WebSocket/upgrade requests with "can't switch protocols using non-Hijacker
// ResponseWriter": the countingWriter must forward Hijack to the underlying
// writer when it supports it.
func TestMiddleware_PreservesHijacker(t *testing.T) {
	c := New()

	var sawHijacker bool
	h := c.Middleware("svc:ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		sawHijacker = ok
		if ok {
			_, _, _ = hj.Hijack()
		}
	}))

	rec := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !sawHijacker {
		t.Fatal("countingWriter does not implement http.Hijacker")
	}
	if !rec.hijacked {
		t.Fatal("Hijack was not forwarded to the underlying ResponseWriter")
	}
}

func TestMiddleware_RecordsRequestAndDuration(t *testing.T) {
	c := New()

	h := c.Middleware("svc:web", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("hello"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}

	if got := testutil.ToFloat64(c.Requests.WithLabelValues("svc:web", "POST", "418")); got != 1 {
		t.Errorf("requests counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.ReqBytes.WithLabelValues("svc:web")); got != 5 {
		t.Errorf("request bytes counter = %v, want 5", got)
	}
	if got := testutil.ToFloat64(c.RespBytes.WithLabelValues("svc:web")); got != 2 {
		t.Errorf("response bytes counter = %v, want 2", got)
	}
	if got := testutil.CollectAndCount(c.Duration); got != 1 {
		t.Errorf("duration sample count = %d, want 1", got)
	}
}

func TestMiddleware_DefaultStatusIs200(t *testing.T) {
	c := New()
	h := c.Middleware("svc:api", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := testutil.ToFloat64(c.Requests.WithLabelValues("svc:api", "GET", "200")); got != 1 {
		t.Errorf("expected one 200 request, got %v", got)
	}
}

func TestMiddleware_SeesBackend502FromInnerHandler(t *testing.T) {
	c := New()
	h := c.Middleware("svc:api", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := testutil.ToFloat64(c.Requests.WithLabelValues("svc:api", "GET", "502")); got != 1 {
		t.Errorf("expected one 502 request, got %v", got)
	}
}

func TestMiddleware_InFlightSettlesToZero(t *testing.T) {
	c := New()
	h := c.Middleware("svc:web", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := testutil.ToFloat64(c.InFlight.WithLabelValues("svc:web")); got != 0 {
		t.Errorf("in-flight gauge = %v, want 0", got)
	}
}

func TestMiddleware_NilCollectorIsNoop(t *testing.T) {
	var c *Collector
	called := false
	h := c.Middleware("ignored", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatal("inner handler not invoked through nil-collector middleware")
	}
}
