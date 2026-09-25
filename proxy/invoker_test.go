package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"mkmba.nz/tsserve/metrics"
)

const testFnARN = "arn:aws:lambda:us-east-1:111122223333:function:hello"

// fakeLambda implements LambdaAPI: it records every Invoke and answers with
// out/err, or with respond when set.
type fakeLambda struct {
	mu      sync.Mutex
	inputs  []*lambda.InvokeInput
	out     *lambda.InvokeOutput
	err     error
	respond func(ctx context.Context) (*lambda.InvokeOutput, error)
}

func (f *fakeLambda) Invoke(ctx context.Context, in *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(ctx)
	}
	return f.out, f.err
}

func (f *fakeLambda) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

// event decodes the payload of the i'th Invoke.
func (f *fakeLambda) event(t *testing.T, i int) functionEvent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) <= i {
		t.Fatalf("Invoke called %d times, want more than %d", len(f.inputs), i)
	}
	var ev functionEvent
	if err := json.Unmarshal(f.inputs[i].Payload, &ev); err != nil {
		t.Fatalf("decode event: %v\n%s", err, f.inputs[i].Payload)
	}
	return ev
}

func returning(payload string) *fakeLambda {
	return &fakeLambda{out: &lambda.InvokeOutput{StatusCode: 200, Payload: []byte(payload)}}
}

// functionPoolOf builds a pool of function backends, one per fake, in order.
func functionPoolOf(fakes ...*fakeLambda) *backendPool {
	p := newBackendPool()
	for i, f := range fakes {
		inv := NewFunctionInvoker(f, fmt.Sprintf("%s-%d", testFnARN, i), "")
		p.add(&backend{key: fmt.Sprintf("fn%d", i), registeredAt: time.Now(), invoker: inv, target: inv.Target()})
	}
	return p
}

// reasonRecorder collects the reasons passed to onError.
type reasonRecorder struct {
	mu      sync.Mutex
	reasons []string
}

func (r *reasonRecorder) record(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

func (r *reasonRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reasons...)
}

func functionProxy(rec *reasonRecorder, whois whoisFunc, fakes ...*fakeLambda) *httputil.ReverseProxy {
	return newReverseProxy(SchemeLambda, functionPoolOf(fakes...), discardLogger(), rec.record, whois)
}

func throttleErr() error {
	return &smithy.GenericAPIError{Code: "TooManyRequestsException", Message: "Rate exceeded"}
}

func TestFunctionInvoker_Target(t *testing.T) {
	if got := NewFunctionInvoker(nil, testFnARN, "").Target(); got != "lambda://"+testFnARN {
		t.Errorf("Target = %q", got)
	}
	if got := NewFunctionInvoker(nil, testFnARN, "live").Target(); got != "lambda://"+testFnARN+":live" {
		t.Errorf("qualified Target = %q", got)
	}
}

// A GET arrives as a payload-format-2.0 event carrying the client-addressed
// host, the peer's IP, lowercased headers and the identity headers, invoked
// synchronously against the backend's function and qualifier.
func TestFunctionInvoker_GetBecomes20Event(t *testing.T) {
	fake := returning(`{"statusCode":204}`)
	pool := newBackendPool()
	inv := NewFunctionInvoker(fake, testFnARN, "live")
	pool.add(&backend{key: testFnARN, invoker: inv, target: inv.Target()})
	rp := newReverseProxy(SchemeLambda, pool, discardLogger(), nil, taggedWhois("runner-01", "tag:ci"))

	req := httptest.NewRequest(http.MethodGet, "/a%2Fb/c?x=1&x=2&y=z", nil)
	req.Host = "hello.example.ts.net"
	req.Header.Set("X-Forwarded-For", "100.64.0.7")
	req.Header.Set("Tailscale-User-Login", "alice@example.com")
	req.Header.Set("Tailscale-User-Name", "Alice")
	req.Header.Set("Tailscale-App-Capabilities", `{"example.com/cap/read":[{}]}`)
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Add("Accept", "text/plain")
	req.Header.Add("Accept", "application/json")
	req.Header.Set("User-Agent", "curl/8")
	req.Header.Set("Cookie", "a=1; b=2")
	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if fake.calls() != 1 {
		t.Fatalf("Invoke calls = %d, want 1", fake.calls())
	}
	in := fake.inputs[0]
	if aws.ToString(in.FunctionName) != testFnARN || aws.ToString(in.Qualifier) != "live" || in.InvocationType != "RequestResponse" {
		t.Errorf("input = function %q qualifier %q type %q", aws.ToString(in.FunctionName), aws.ToString(in.Qualifier), in.InvocationType)
	}

	ev := fake.event(t, 0)
	if ev.Version != "2.0" || ev.RouteKey != "$default" || ev.RequestContext.Stage != "$default" {
		t.Errorf("version/routeKey/stage = %q/%q/%q", ev.Version, ev.RouteKey, ev.RequestContext.Stage)
	}
	if ev.RawPath != "/a%2Fb/c" || ev.RequestContext.HTTP.Path != "/a%2Fb/c" {
		t.Errorf("rawPath = %q, http.path = %q, want /a%%2Fb/c for both", ev.RawPath, ev.RequestContext.HTTP.Path)
	}
	if ev.RawQueryString != "x=1&x=2&y=z" {
		t.Errorf("rawQueryString = %q", ev.RawQueryString)
	}
	if want := map[string]string{"x": "1,2", "y": "z"}; !equalMaps(ev.QueryStringParameters, want) {
		t.Errorf("queryStringParameters = %v, want %v", ev.QueryStringParameters, want)
	}
	h := ev.Headers
	wantHeaders := map[string]string{
		"host":                       "hello.example.ts.net",
		"x-forwarded-host":           "hello.example.ts.net",
		"x-forwarded-for":            "100.64.0.7",
		"x-forwarded-proto":          "https",
		"tailscale-user-login":       "alice@example.com",
		"tailscale-user-name":        "Alice",
		"tailscale-app-capabilities": `{"example.com/cap/read":[{}]}`,
		"tailscale-node-name":        "runner-01",
		"tailscale-node-tags":        "ci",
		"authorization":              "Bearer abc",
		"accept":                     "text/plain,application/json",
		"user-agent":                 "curl/8",
	}
	for k, v := range wantHeaders {
		if h[k] != v {
			t.Errorf("headers[%q] = %q, want %q", k, h[k], v)
		}
	}
	if _, ok := h["cookie"]; ok {
		t.Errorf("cookie repeated in headers: %q", h["cookie"])
	}
	if len(ev.Cookies) != 2 || ev.Cookies[0] != "a=1" || ev.Cookies[1] != "b=2" {
		t.Errorf("cookies = %q, want [a=1 b=2]", ev.Cookies)
	}
	c := ev.RequestContext
	if c.DomainName != "hello.example.ts.net" || c.HTTP.Method != "GET" || c.HTTP.SourceIP != "100.64.0.7" ||
		c.HTTP.UserAgent != "curl/8" || c.HTTP.Protocol != "HTTP/1.1" {
		t.Errorf("requestContext = %+v", c)
	}
	if ev.Body != "" || ev.IsBase64Encoded {
		t.Errorf("body = %q base64 %v, want empty and false", ev.Body, ev.IsBase64Encoded)
	}
}

// The node name of a user-owned caller reaches the function in the event's
// headers, with client-supplied identity headers stripped.
func TestFunctionInvoker_NodeNameForUntaggedPeer(t *testing.T) {
	fake := returning(`{}`)
	userNode := func(ctx context.Context, _ string) (*apitype.WhoIsResponse, error) {
		return &apitype.WhoIsResponse{Node: &tailcfg.Node{ComputedName: "alice-laptop"}}, nil
	}
	rp := functionProxy(&reasonRecorder{}, userNode, fake)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "100.64.0.8")
	req.Header.Set(headerNodeName, "spoofed")
	req.Header.Set(headerNodeTags, "tag:admin")
	rp.ServeHTTP(httptest.NewRecorder(), req)

	h := fake.event(t, 0).Headers
	if h["tailscale-node-name"] != "alice-laptop" {
		t.Errorf("tailscale-node-name = %q, want alice-laptop", h["tailscale-node-name"])
	}
	if v, ok := h["tailscale-node-tags"]; ok {
		t.Errorf("tailscale-node-tags = %q for an untagged node, want absent", v)
	}
}

func TestFunctionInvoker_RequestBodyEncoding(t *testing.T) {
	binary := []byte{0x00, 0xff, 0x10, 0x80}
	for _, tc := range []struct {
		name, contentType string
		body              []byte
		wantBody          string
		wantB64           bool
	}{
		{"json", "application/json; charset=utf-8", []byte(`{"a":1}`), `{"a":1}`, false},
		{"form", "application/x-www-form-urlencoded", []byte("a=1&b=2"), "a=1&b=2", false},
		{"text", "text/plain", []byte("hi"), "hi", false},
		{"suffix", "application/vnd.api+json", []byte(`{}`), `{}`, false},
		{"binary", "application/octet-stream", binary, base64.StdEncoding.EncodeToString(binary), true},
		{"no content type", "", binary, base64.StdEncoding.EncodeToString(binary), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := returning(`{"statusCode":200}`)
			rp := functionProxy(&reasonRecorder{}, nil, fake)
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rp.ServeHTTP(httptest.NewRecorder(), req)

			ev := fake.event(t, 0)
			if ev.Body != tc.wantBody || ev.IsBase64Encoded != tc.wantB64 {
				t.Errorf("body = %q base64 %v, want %q %v", ev.Body, ev.IsBase64Encoded, tc.wantBody, tc.wantB64)
			}
			if ev.RequestContext.HTTP.Method != http.MethodPost {
				t.Errorf("method = %q", ev.RequestContext.HTTP.Method)
			}
		})
	}
}

func TestFunctionInvoker_StructuredResponse(t *testing.T) {
	fake := returning(`{
		"statusCode": 201,
		"headers": {"Content-Type": "application/json", "cache-control": "no-store", "Content-Length": "999", "Transfer-Encoding": "chunked"},
		"cookies": ["s=1; Path=/", "t=2"],
		"body": "` + base64.StdEncoding.EncodeToString([]byte(`{"token":"x"}`)) + `",
		"isBase64Encoded": true
	}`)
	rp := functionProxy(&reasonRecorder{}, nil, fake)
	srv := httptest.NewServer(rp)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/token")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if body.String() != `{"token":"x"}` {
		t.Errorf("body = %q", body.String())
	}
	if resp.ContentLength != 13 {
		t.Errorf("Content-Length = %d, want 13", resp.ContentLength)
	}
	if len(resp.TransferEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none", resp.TransferEncoding)
	}
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", resp.Header)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != "s=1; Path=/" || got[1] != "t=2" {
		t.Errorf("Set-Cookie = %q", got)
	}
}

func TestFunctionInvoker_BareReturnIsJSON200(t *testing.T) {
	for _, payload := range []string{`"hello"`, `42`, `[1,2]`, `null`, `{"message":"no status"}`} {
		t.Run(payload, func(t *testing.T) {
			rp := functionProxy(&reasonRecorder{}, nil, returning(payload))
			rr := httptest.NewRecorder()
			rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			if rr.Body.String() != payload {
				t.Errorf("body = %q, want %q", rr.Body.String(), payload)
			}
		})
	}
}

// A function error is a 502, counted once as function-error, and not tried
// against another member: the function ran.
func TestFunctionInvoker_FunctionErrorIs502WithoutRetry(t *testing.T) {
	failing := &fakeLambda{out: &lambda.InvokeOutput{
		StatusCode:    200,
		FunctionError: aws.String("Unhandled"),
		Payload:       []byte(`{"errorType":"TypeError","errorMessage":"boom"}`),
	}}
	other := returning(`{"statusCode":200}`)
	rec := &reasonRecorder{}
	var logs bytes.Buffer
	rp := newReverseProxy(SchemeLambda, functionPoolOf(failing, other), slog.New(slog.NewTextHandler(&logs, nil)), rec.record, nil)

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if failing.calls() != 1 || other.calls() != 0 {
		t.Errorf("calls = %d/%d, want 1/0", failing.calls(), other.calls())
	}
	if got := rec.got(); len(got) != 1 || got[0] != "function-error" {
		t.Errorf("reasons = %v, want [function-error]", got)
	}
	for _, want := range []string{"backend proxy error", "lambda://" + testFnARN + "-0", "TypeError", "boom"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}
}

// A throttle means the function did not run, so a body-less request moves to
// the next member; a request with a body does not.
func TestFunctionInvoker_ThrottleRetryRule(t *testing.T) {
	t.Run("GET retried", func(t *testing.T) {
		throttled := &fakeLambda{err: throttleErr()}
		other := returning(`{"statusCode":200,"body":"ok"}`)
		rec := &reasonRecorder{}
		rp := functionProxy(rec, nil, throttled, other)

		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

		if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
			t.Errorf("response = %d %q, want 200 ok", rr.Code, rr.Body.String())
		}
		if throttled.calls() != 1 || other.calls() != 1 {
			t.Errorf("calls = %d/%d, want 1/1", throttled.calls(), other.calls())
		}
		if got := rec.got(); len(got) != 1 || got[0] != "throttled" {
			t.Errorf("reasons = %v, want [throttled]", got)
		}
	})
	t.Run("POST not retried", func(t *testing.T) {
		throttled := &fakeLambda{err: throttleErr()}
		other := returning(`{"statusCode":200}`)
		rec := &reasonRecorder{}
		rp := functionProxy(rec, nil, throttled, other)

		rr := httptest.NewRecorder()
		rp.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))

		if rr.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", rr.Code)
		}
		if throttled.calls() != 1 || other.calls() != 0 {
			t.Errorf("calls = %d/%d, want 1/0", throttled.calls(), other.calls())
		}
		if got := rec.got(); len(got) != 1 || got[0] != "throttled" {
			t.Errorf("reasons = %v, want [throttled]", got)
		}
	})
}

// When every member is throttled each attempt counts as throttled and the
// client receives a 502.
func TestFunctionInvoker_AllMembersThrottled(t *testing.T) {
	a, b := &fakeLambda{err: throttleErr()}, &fakeLambda{err: throttleErr()}
	rec := &reasonRecorder{}
	rr := httptest.NewRecorder()
	functionProxy(rec, nil, a, b).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rr.Code)
	}
	if a.calls() != 1 || b.calls() != 1 {
		t.Errorf("calls = %d/%d, want 1/1", a.calls(), b.calls())
	}
	if got := rec.got(); len(got) != 2 || got[0] != "throttled" || got[1] != "throttled" {
		t.Errorf("reasons = %v, want [throttled throttled]", got)
	}
}

// A request without a User-Agent carries none in the event, rather than the
// empty value ReverseProxy sets to suppress Go's default.
func TestFunctionInvoker_NoEmptyUserAgent(t *testing.T) {
	fake := returning(`{}`)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Del("User-Agent")
	functionProxy(&reasonRecorder{}, nil, fake).ServeHTTP(httptest.NewRecorder(), req)
	if v, ok := fake.event(t, 0).Headers["user-agent"]; ok {
		t.Errorf("headers[user-agent] = %q, want absent", v)
	}
}

// Which Invoke errors mean the function did not run, and so may move to
// another member.
func TestFunctionInvoker_DidNotRunClassification(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
	}{
		{"TooManyRequestsException", true},
		{"ResourceNotFoundException", true},
		{"AccessDeniedException", true},
		{"ServiceException", false},
		{"InvalidRequestContentException", false},
	} {
		inv := NewFunctionInvoker(&fakeLambda{err: &smithy.GenericAPIError{Code: tc.code}}, testFnARN, "")
		_, err := inv.RoundTrip(httptest.NewRequest(http.MethodGet, "/", nil))
		if got := isConnectError(err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// A body over the cap is answered 413 by the invoker without invoking the
// function, and is not a backend error.
func TestFunctionInvoker_BodyOverCapIs413(t *testing.T) {
	old := maxFunctionBody
	maxFunctionBody = 10
	t.Cleanup(func() { maxFunctionBody = old })

	fake := returning(`{"statusCode":200}`)
	rec := &reasonRecorder{}
	mc := metrics.New()
	rp := mc.Middleware("svc:hello", functionProxy(rec, nil, fake))

	rr := httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("01234567890")))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rr.Code)
	}
	if got := testutil.ToFloat64(mc.Requests.WithLabelValues("svc:hello", http.MethodPost, "413")); got != 1 {
		t.Errorf("tsserve_proxy_requests_total{code=413} = %v, want 1", got)
	}
	if fake.calls() != 0 {
		t.Errorf("Invoke calls = %d, want 0", fake.calls())
	}
	if got := rec.got(); len(got) != 0 {
		t.Errorf("reasons = %v, want none", got)
	}

	// Exactly at the cap is accepted.
	rr = httptest.NewRecorder()
	rp.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789")))
	if rr.Code != http.StatusOK || fake.calls() != 1 {
		t.Errorf("at cap: status = %d, calls = %d, want 200 and 1", rr.Code, fake.calls())
	}
}

// The default cap keeps a base64-encoded body inside Lambda's 6 MB payload.
func TestFunctionInvoker_DefaultCapFitsPayloadLimit(t *testing.T) {
	if enc := base64.StdEncoding.EncodedLen(int(maxFunctionBody)); enc >= 6*1024*1024-64*1024 {
		t.Errorf("encoded cap = %d bytes, leaves under 64 KiB for the rest of the event", enc)
	}
}

// Cancelling the client request cancels the Invoke in flight.
func TestFunctionInvoker_ClientCancelCancelsInvoke(t *testing.T) {
	cancelled := make(chan struct{})
	fake := &fakeLambda{respond: func(ctx context.Context) (*lambda.InvokeOutput, error) {
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}}
	rp := functionProxy(&reasonRecorder{}, nil, fake)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		rp.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	for fake.calls() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Invoke not cancelled")
	}
	<-done
}

// testLambdaClient builds a client from NewLambdaClient aimed at endpoint,
// from a config with the adaptive five-attempt retry mode the readers' shared
// AWS config loader sets.
func testLambdaClient(endpoint string) *lambda.Client {
	cfg := aws.Config{
		Region:           "us-east-1",
		Credentials:      aws.AnonymousCredentials{},
		RetryMode:        aws.RetryModeAdaptive,
		RetryMaxAttempts: 5,
	}
	return NewLambdaClient(cfg, func(o *lambda.Options) { o.BaseEndpoint = aws.String(endpoint) })
}

// The Invoke client makes exactly one HTTP attempt: the SDK's retries, which
// would re-send a request whose function may have run, are off.
func TestNewLambdaClient_OneAttemptOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		errorType string
		reason    string
		retryable bool
	}{
		{"server error", http.StatusInternalServerError, "ServiceException", "other", false},
		{"throttle", http.StatusTooManyRequests, "TooManyRequestsException", "throttled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("X-Amzn-ErrorType", tc.errorType)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
			}))
			defer srv.Close()

			inv := NewFunctionInvoker(testLambdaClient(srv.URL), testFnARN, "")
			_, err := inv.RoundTrip(httptest.NewRequest(http.MethodGet, "/", nil))
			if err == nil {
				t.Fatal("RoundTrip succeeded, want error")
			}
			if hits.Load() != 1 {
				t.Errorf("HTTP attempts = %d, want 1", hits.Load())
			}
			if got := classifyProxyError(err); got != tc.reason {
				t.Errorf("reason = %q, want %q (err %v)", got, tc.reason, err)
			}
			if isConnectError(err) != tc.retryable {
				t.Errorf("retryable = %v, want %v", isConnectError(err), tc.retryable)
			}
		})
	}
}

// A caller's option functions cannot re-enable SDK retries: the client's own
// Retryer and HTTPClient are applied after them.
func TestNewLambdaClient_OptionsCannotOverrideRetryer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-Amzn-ErrorType", "ServiceException")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}}
	client := NewLambdaClient(cfg, func(o *lambda.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.Retryer = retry.NewStandard(func(so *retry.StandardOptions) { so.MaxAttempts = 5 })
		o.HTTPClient = http.DefaultClient
	})
	_, err := NewFunctionInvoker(client, testFnARN, "").RoundTrip(httptest.NewRequest(http.MethodGet, "/", nil))
	if err == nil {
		t.Fatal("RoundTrip succeeded, want error")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("HTTP attempts = %d, want 1", n)
	}
}

// A Lambda endpoint that refuses connections is a connectError — the function
// did not run — reported with the existing connection-refused reason.
func TestNewLambdaClient_ConnectFailureIsRetryable(t *testing.T) {
	inv := NewFunctionInvoker(testLambdaClient("http://"+closedPort(t)), testFnARN, "")
	_, err := inv.RoundTrip(httptest.NewRequest(http.MethodGet, "/", nil))
	if !isConnectError(err) {
		t.Fatalf("err = %v, want a connectError", err)
	}
	if got := classifyProxyError(err); got != "connection-refused" {
		t.Errorf("reason = %q, want connection-refused", got)
	}
	var fe *functionError
	if errors.As(err, &fe) {
		t.Errorf("connect failure classified as function error")
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Connection establishment to the Lambda endpoint — dial and TLS handshake —
// is bounded by the dial budget, and a stalled handshake is a connectError.
func TestNewLambdaClient_StalledHandshakeFailsWithinDialBudget(t *testing.T) {
	orig := dialTimeout
	dialTimeout = 150 * time.Millisecond
	t.Cleanup(func() { dialTimeout = orig })

	// Accepts connections, then never speaks TLS.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			defer c.Close()
		}
	}()

	inv := NewFunctionInvoker(testLambdaClient("https://"+ln.Addr().String()), testFnARN, "")
	start := time.Now()
	_, err = inv.RoundTrip(httptest.NewRequest(http.MethodGet, "/", nil))
	elapsed := time.Since(start)

	if !isConnectError(err) {
		t.Fatalf("err = %v, want a connectError", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("stalled handshake took %v, want it bounded by the %v dial budget", elapsed, dialTimeout)
	}
	if n := accepted.Load(); n != 1 {
		t.Errorf("connections = %d, want exactly 1 (no SDK retry)", n)
	}
}
