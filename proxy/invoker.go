package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"
)

// LambdaAPI is the subset of the Lambda client a function backend's invoker
// uses. *lambda.Client satisfies it; tests supply a fake.
type LambdaAPI interface {
	Invoke(ctx context.Context, in *lambda.InvokeInput, opts ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

// maxFunctionBody caps the request body a function backend accepts. Lambda
// refuses a synchronous payload over 6 MB, and a body that has to be sent
// base64-encoded grows by a third, so 4 MiB of raw body keeps the event under
// the limit whatever the encoding. A var only so tests can shorten it.
var maxFunctionBody int64 = 4 << 20

// FunctionInvoker is the invoker of one function backend: an http.RoundTripper
// that performs each request as one synchronous Invoke of the function, with
// the request translated into a payload-format-2.0 (function URL) event and
// the function's return value translated back into a response.
type FunctionInvoker struct {
	api       LambdaAPI
	arn       string
	qualifier string
}

// NewFunctionInvoker returns the invoker for the function arn (unqualified),
// invoking qualifier (a version or alias) when it is non-empty and $LATEST
// otherwise. api should be built with [NewLambdaClient] so that Invoke is
// never retried by the SDK.
func NewFunctionInvoker(api LambdaAPI, arn, qualifier string) *FunctionInvoker {
	return &FunctionInvoker{api: api, arn: arn, qualifier: qualifier}
}

// Target renders the function backend's target,
// "lambda://<function ARN>[:<qualifier>]".
func (f *FunctionInvoker) Target() string {
	t := SchemeLambda + "://" + f.arn
	if f.qualifier != "" {
		t += ":" + f.qualifier
	}
	return t
}

// RoundTrip performs req as one Invoke. A request body over the cap is
// answered with 413 without invoking the function. An error return means no
// usable response: it is a *connectError when the function certainly did not
// run (the Lambda endpoint could not be reached, or the API refused the call
// with a throttle, a missing function or a denied permission), so the pool may
// try another member, and a *functionError when the function ran and failed.
func (f *FunctionInvoker) RoundTrip(req *http.Request) (*http.Response, error) {
	body, tooLarge, err := readBody(req)
	if err != nil {
		return nil, err
	}
	if tooLarge {
		return textResponse(req, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds the %d byte limit of a function backend\n", maxFunctionBody)), nil
	}

	payload, err := json.Marshal(newFunctionEvent(req, body))
	if err != nil {
		return nil, fmt.Errorf("encode function event: %w", err)
	}

	in := &lambda.InvokeInput{
		FunctionName:   aws.String(f.arn),
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
		Payload:        payload,
	}
	if f.qualifier != "" {
		in.Qualifier = aws.String(f.qualifier)
	}
	out, err := f.api.Invoke(req.Context(), in)
	if err != nil {
		if functionDidNotRun(err) {
			return nil, &connectError{err: err}
		}
		return nil, err
	}
	if out.FunctionError != nil {
		return nil, newFunctionError(aws.ToString(out.FunctionError), out.Payload)
	}
	return functionResponse(req, out.Payload)
}

// readBody reads and closes the request body, bounded by maxFunctionBody.
// tooLarge reports a body over the cap, in which case the body is discarded.
func readBody(req *http.Request) (body []byte, tooLarge bool, err error) {
	if req.Body == nil {
		return nil, false, nil
	}
	defer req.Body.Close()
	body, err = io.ReadAll(io.LimitReader(req.Body, maxFunctionBody+1))
	if err != nil {
		return nil, false, fmt.Errorf("read request body: %w", err)
	}
	if int64(len(body)) > maxFunctionBody {
		return nil, true, nil
	}
	return body, false, nil
}

// functionEvent is the payload-format-2.0 event a function URL delivers, with
// the fields this translation fills. The rest are left out, as the function
// URL format permits.
type functionEvent struct {
	Version               string            `json:"version"`
	RouteKey              string            `json:"routeKey"`
	RawPath               string            `json:"rawPath"`
	RawQueryString        string            `json:"rawQueryString"`
	Cookies               []string          `json:"cookies,omitempty"`
	Headers               map[string]string `json:"headers"`
	QueryStringParameters map[string]string `json:"queryStringParameters,omitempty"`
	RequestContext        eventContext      `json:"requestContext"`
	Body                  string            `json:"body"`
	IsBase64Encoded       bool              `json:"isBase64Encoded"`
}

type eventContext struct {
	DomainName string    `json:"domainName"`
	Stage      string    `json:"stage"`
	HTTP       eventHTTP `json:"http"`
}

type eventHTTP struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Protocol  string `json:"protocol"`
	SourceIP  string `json:"sourceIp"`
	UserAgent string `json:"userAgent"`
}

func newFunctionEvent(req *http.Request, body []byte) *functionEvent {
	headers := make(map[string]string, len(req.Header)+1)
	var cookies []string
	for name, values := range req.Header {
		if strings.EqualFold(name, "Cookie") {
			for _, v := range values {
				for _, c := range strings.Split(v, ";") {
					if c = strings.TrimSpace(c); c != "" {
						cookies = append(cookies, c)
					}
				}
			}
			continue
		}
		// ReverseProxy sends an empty User-Agent to suppress Go's default;
		// a function URL event carries no header for it.
		if v := strings.Join(values, ","); v != "" {
			headers[strings.ToLower(name)] = v
		}
	}
	// Go keeps Host out of the header map; the function sees the host the
	// client addressed, as a function URL would.
	headers["host"] = req.Host

	var query map[string]string
	if q := req.URL.Query(); len(q) > 0 {
		query = make(map[string]string, len(q))
		for name, values := range q {
			query[name] = strings.Join(values, ",")
		}
	}

	path := req.URL.EscapedPath()
	ev := &functionEvent{
		Version:               "2.0",
		RouteKey:              "$default",
		RawPath:               path,
		RawQueryString:        req.URL.RawQuery,
		Cookies:               cookies,
		Headers:               headers,
		QueryStringParameters: query,
		RequestContext: eventContext{
			DomainName: req.Host,
			Stage:      "$default",
			HTTP: eventHTTP{
				Method:    req.Method,
				Path:      path,
				Protocol:  req.Proto,
				SourceIP:  firstForwardedFor(req.Header.Get("X-Forwarded-For")),
				UserAgent: req.UserAgent(),
			},
		},
	}
	if len(body) > 0 {
		if isTextual(req.Header.Get("Content-Type")) {
			ev.Body = string(body)
		} else {
			ev.Body = base64.StdEncoding.EncodeToString(body)
			ev.IsBase64Encoded = true
		}
	}
	return ev
}

// isTextual reports whether a body of this Content-Type is sent to the function
// as text rather than base64.
func isTextual(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "text/"),
		mt == "application/json",
		mt == "application/x-www-form-urlencoded",
		mt == "application/xml",
		strings.HasSuffix(mt, "+json"),
		strings.HasSuffix(mt, "+xml"):
		return true
	}
	return false
}

// functionResult is a structured function return value.
type functionResult struct {
	StatusCode      int               `json:"statusCode"`
	Headers         map[string]string `json:"headers"`
	Cookies         []string          `json:"cookies"`
	Body            string            `json:"body"`
	IsBase64Encoded bool              `json:"isBase64Encoded"`
}

// functionResponse translates a function's return value into a response by
// the function URL rule: a JSON object with statusCode is a structured result;
// anything else is a 200 application/json with the payload as the body.
func functionResponse(req *http.Request, payload []byte) (*http.Response, error) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) != nil || probe["statusCode"] == nil {
		resp := newResponse(req, http.StatusOK, payload)
		resp.Header.Set("Content-Type", "application/json")
		return resp, nil
	}

	var res functionResult
	if err := json.Unmarshal(payload, &res); err != nil {
		return nil, fmt.Errorf("decode function result: %w", err)
	}
	if res.StatusCode < 100 || res.StatusCode > 999 {
		return nil, fmt.Errorf("function returned invalid statusCode %d", res.StatusCode)
	}
	body := []byte(res.Body)
	if res.IsBase64Encoded {
		var err error
		if body, err = base64.StdEncoding.DecodeString(res.Body); err != nil {
			return nil, fmt.Errorf("decode function result body: %w", err)
		}
	}
	resp := newResponse(req, res.StatusCode, body)
	for name, v := range res.Headers {
		// The body is complete and its length known; a stale length or a
		// chunked encoding copied from the function would corrupt the response.
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Transfer-Encoding") {
			continue
		}
		resp.Header.Set(name, v)
	}
	for _, c := range res.Cookies {
		resp.Header.Add("Set-Cookie", c)
	}
	return resp, nil
}

func newResponse(req *http.Request, code int, body []byte) *http.Response {
	return &http.Response{
		Status:        strconv.Itoa(code) + " " + http.StatusText(code),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Length": {strconv.Itoa(len(body))}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func textResponse(req *http.Request, code int, msg string) *http.Response {
	resp := newResponse(req, code, []byte(msg))
	resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
	return resp
}

// functionError is an Invoke whose function ran and failed: it threw, timed
// out, or returned a response too large for Lambda to deliver.
type functionError struct {
	kind      string // the Invoke output's FunctionError ("Unhandled", ...)
	errorType string
	message   string
}

func newFunctionError(kind string, payload []byte) *functionError {
	var doc struct {
		ErrorType    string `json:"errorType"`
		ErrorMessage string `json:"errorMessage"`
	}
	_ = json.Unmarshal(payload, &doc)
	return &functionError{kind: kind, errorType: doc.ErrorType, message: doc.ErrorMessage}
}

func (e *functionError) Error() string {
	return fmt.Sprintf("function error (%s): errorType=%q errorMessage=%q", e.kind, e.errorType, e.message)
}

// Lambda API error codes that mean the function did not run for this call.
const (
	codeThrottled    = "TooManyRequestsException"
	codeNotFound     = "ResourceNotFoundException"
	codeAccessDenied = "AccessDeniedException"
)

func apiErrorCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

func isThrottle(err error) bool { return apiErrorCode(err) == codeThrottled }

// functionDidNotRun reports an Invoke error the Lambda API returned without
// running the function. Connection failures to the endpoint are already
// connectErrors, from the dialer NewLambdaClient installs.
func functionDidNotRun(err error) bool {
	switch apiErrorCode(err) {
	case codeThrottled, codeNotFound, codeAccessDenied:
		return true
	}
	return false
}

// NewLambdaClient builds the Lambda client function backends invoke through,
// from cfg's region and credentials. Its SDK retries are off — the SDK would
// re-send on a connection error or a 5xx with no knowledge of whether the
// function ran, and would hide throttling — and its connection establishment
// to the endpoint is bounded by the same dial budget as a container backend,
// with every failure to establish one marked as a connectError. No response
// timeout is set: the function's own timeout governs.
//
// optFns may adjust other options, such as the endpoint. They run first, so
// neither guarantee above can be overridden: Retryer and HTTPClient are always
// set last.
func NewLambdaClient(cfg aws.Config, optFns ...func(*lambda.Options)) *lambda.Client {
	opts := append(slices.Clone(optFns), func(o *lambda.Options) {
		o.Retryer = aws.NopRetryer{}
		o.HTTPClient = &http.Client{Transport: newInvokeTransport()}
	})
	return lambda.NewFromConfig(cfg, opts...)
}

// newInvokeTransport is the transport of a Lambda client. Unlike an https
// container backend's it verifies the endpoint's certificate. A direct
// (non-proxied) connection handshakes inside the dial budget, so a failed
// handshake is a connectError too.
func newInvokeTransport() *http.Transport {
	dial := connectDialer()
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dial,
		DialTLSContext:      dialTLSWithin(dial, &tls.Config{MinVersion: tls.VersionTLS12}),
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: tlsHandshakeTimeout,
	}
}
