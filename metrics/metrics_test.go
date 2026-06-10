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

// fakeConn is a minimal net.Conn whose Read drains a fixed payload once and
// whose Write discards, used to drive countingConn byte accounting in tests.
type fakeConn struct {
	net.Conn
	toRead []byte
}

func (f *fakeConn) Read(b []byte) (int, error) {
	if len(f.toRead) == 0 {
		return 0, io.EOF
	}
	n := copy(b, f.toRead)
	f.toRead = f.toRead[n:]
	return n, nil
}

func (f *fakeConn) Write(b []byte) (int, error) { return len(b), nil }

// hijackableRecorder is an httptest.ResponseRecorder that also implements
// http.Hijacker, standing in for the real net/http server connection.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
	conn     net.Conn
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return h.conn, nil, nil
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

	rec := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder(), conn: &fakeConn{}}
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !sawHijacker {
		t.Fatal("countingWriter does not implement http.Hijacker")
	}
	if !rec.hijacked {
		t.Fatal("Hijack was not forwarded to the underlying ResponseWriter")
	}
}

// TestMiddleware_HijackRecordsUpgradeNotLatency verifies that a hijacked
// (WebSocket-style) request is recorded as a 101, that no request-duration
// sample is emitted (which would pollute the latency histogram with the whole
// connection lifetime), and that bytes flowing over the hijacked conn are
// still accounted for in both directions.
func TestMiddleware_HijackRecordsUpgradeNotLatency(t *testing.T) {
	c := New()

	var openDuringConn float64
	h := c.Middleware("svc:ws", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		// While the connection is live, the open-websocket gauge should read 1.
		openDuringConn = testutil.ToFloat64(c.WSOpen.WithLabelValues("svc:ws"))
		// Drain the client (request bytes) and write back (response bytes).
		_, _ = io.Copy(io.Discard, conn)
		_, _ = conn.Write([]byte("response-frame"))
	}))

	rec := &hijackableRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		conn:             &fakeConn{toRead: []byte("client-frame")},
	}
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := testutil.ToFloat64(c.Requests.WithLabelValues("svc:ws", "GET", "101")); got != 1 {
		t.Errorf("expected one 101 request, got %v", got)
	}
	if got := testutil.CollectAndCount(c.Duration); got != 0 {
		t.Errorf("duration histogram should have no samples for a hijacked conn, got %d", got)
	}
	if got := testutil.ToFloat64(c.ReqBytes.WithLabelValues("svc:ws")); got != float64(len("client-frame")) {
		t.Errorf("request bytes = %v, want %d", got, len("client-frame"))
	}
	if got := testutil.ToFloat64(c.RespBytes.WithLabelValues("svc:ws")); got != float64(len("response-frame")) {
		t.Errorf("response bytes = %v, want %d", got, len("response-frame"))
	}
	if got := testutil.ToFloat64(c.InFlight.WithLabelValues("svc:ws")); got != 0 {
		t.Errorf("in-flight gauge = %v, want 0 after the connection closes", got)
	}
	if openDuringConn != 1 {
		t.Errorf("open-websocket gauge during connection = %v, want 1", openDuringConn)
	}
	if got := testutil.ToFloat64(c.WSOpen.WithLabelValues("svc:ws")); got != 0 {
		t.Errorf("open-websocket gauge = %v, want 0 after the connection closes", got)
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
