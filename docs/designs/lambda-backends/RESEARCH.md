# Lambda Backends Research

Tracking Issue: FZ-2447

## Overview

tsserve proxies each advertised Tailscale Service to one or more backends, every one of
which is a `host:port` that speaks HTTP: a Docker container IP or an ECS task/host address.
This research asks whether an AWS Lambda function can be a backend instead — the request
arriving at `https://svc.<tailnet>.ts.net` is delivered to the function rather than to a
socket — and, if so, whether such functions can be discovered from tags the way ECS tasks
are discovered from `dockerLabels`, or must be configured statically. The motivating case
is a small request/response HTTP service that is a candidate to move from a container to a
function.

**Short answer:** feasible, by either of two invocation paths, with tag-based discovery
possible; see Approach Options for the trade-offs and the Recommendation.

Omitted: User Stories (the card's questions are the use case); Target / Destination (n/a —
the work plugs into tsserve's own discovery and manager contract, covered under Current
System Analysis).

## Problem Statement

Everything downstream of discovery assumes a backend is a TCP address. `proxy.Manager.Register`
takes a `backendIP` and builds the pool member's address with `net.JoinHostPort`
(`proxy/manager.go:205-208`); `poolTransport.RoundTrip` sets `attempt.URL.Host` and
`attempt.Host` to that address and hands the request to a plain `http.Transport`
(`proxy/reverseproxy.go:138-179`). A Lambda function has no address. It is reached either
through an HTTPS endpoint AWS generates for it (a *function URL*), which is close to an
ordinary HTTP backend but needs every request signed, or through the Lambda `Invoke` API,
which takes a JSON event rather than an HTTP request.

On the discovery side, both watchers read `tsserve.*` labels from container metadata
(`labels.Parse`, `labels/labels.go:44-101`) and resolve an IP. A function has no labels and
no IP; it has AWS resource tags and an ARN.

## Domain Model and Terminology

Existing terms (SPEC.md "Service Manager"):

- **backend** — one discovered endpoint that can serve a service; identified by a
  *registration key* (Docker container ID, `taskArn#containerName` in ECS).
- **advertised service** — a Tailscale Service tsserve holds a `ListenService` listener for.
- **backend pool** — the ordered set of backends registered against an advertised service.
- **reader** — one ECS watcher bound to one AWS credential context (`ecs/account.go`).

Terms this feature would introduce (proposed; naming is for design to settle):

- **function backend** — a backend that is a Lambda function rather than a `host:port`.
  The stable identifier AWS offers for one is the function ARN.
- **function URL** — AWS's term for the per-function HTTPS endpoint
  (`https://<url-id>.lambda-url.<region>.on.aws`). The URL never changes once created.
- **qualifier** — AWS's term for the version or alias of a function to invoke. Tags apply
  at the function level, not to versions or aliases, so a tag cannot name one implicitly.

## Requirements

### Functional Requirements

Derived from the card and from what the existing discovery modes already promise:

1. A request to an advertised service reaches the function, and the function's response
   reaches the client, with the same status, headers and body a container backend would
   produce for an ordinary request/response exchange.
2. The identity headers a backend relies on today — the `Tailscale-User-*` headers the serve
   layer sets, plus `Tailscale-Node-Tags` / `Tailscale-Node-Name` when `tsserve.caps` is set
   (`proxy/reverseproxy.go:283-316`) — arrive at the function.
3. Functions become and cease to be backends without a tsserve restart, whether they are
   found by tag or named in configuration (the card asks which; see Approach Options).
4. Function backends can be served from the same tsserve process as Docker/ECS backends.

### Non-Functional Requirements

- Invoking through tsserve must not widen who can invoke the function: only tsserve's own
  AWS principal gains access. Whether the invocation may traverse the public internet at all
  is Scope Question 1.
- Bounded AWS API usage at rest: discovery polling stays well under the Lambda control-plane
  quota of 15 requests/second per region, which is shared across all control-plane calls and
  cannot be raised.
- Same failure reporting as other backends: the status page, `tsserve_service_backends`, and
  `tsserve_proxy_backend_errors_total{reason}` (`metrics/metrics.go:45-80`) stay meaningful.

### Out of Scope

- Making tsserve *create* functions, function URLs, or their resource policies. tsserve does
  not create Tailscale service definitions either (SPEC.md "Non-goals"); the same stance
  applies here.
- WebSocket or other connection-upgrade traffic to functions. Lambda has no such mode; see
  Complexity and Risk Areas.
- Fronting a function with API Gateway or an ALB. That yields an ordinary HTTP endpoint that
  the existing proxy path can already reach; the only gap is that nothing discovers it (see
  Approach Options, option 3).

## Current System Analysis

### Existing Data Models and Storage

tsserve holds no persistent data for this feature; everything is in-memory process state.

- `labels.ServiceDef` (`labels/labels.go:30-36`) — parsed label vocabulary: `Service`,
  `Port` (required, non-zero `uint16`, `labels/labels.go:62-68`), `Network` (default
  `bridge`), `Scheme` (`http`|`https`), `Caps`. `Port` and `Network` are meaningless for a
  function; `Scheme` is fixed by whichever invocation path is chosen.
- `proxy.ServiceDef` and `proxy.Origin` (`proxy/manager.go:174-192`) — what the manager
  receives, plus which reader (account/cluster/region) found the backend. `Origin` is
  displayed on the status page (`local/status.go:63-66,148-161`).
- `backend` (`proxy/pool.go:12-18`) — `key` and `addr` (`host:port`). `addr` is what the
  transport dials, what the status page shows, and what proxy-error logs name.
- ECS reader configuration: `ecs.Account` / `ecs.Defaults` / `ecs.WatcherSpec`
  (`ecs/account.go:18-59`) — per-account region, profile, config file, static keys, cluster.
  `TSSERVE_ECS_ACCOUNTS` is a JSON array of these in an env var; it is the existing precedent
  for structured configuration despite the SPEC's "no config file" non-goal.

### Existing Surfaces

Reconnaissance per `/pattern-discovery`; each candidate is something a function backend
would reuse or replace.

**Discovery contract.** Both watchers drive the same two-method `Registrar`
(`docker/watcher.go:20-25`): `Register(key, *labels.ServiceDef, backendIP) error` and
`Deregister(key)`. `main.go:309-399` selects exactly one discovery mode from
`TSSERVE_DISCOVERY` (`docker` | `ecs`); `registrarAdapter` (`main.go:279-305`) converts to
`proxy.ServiceDef` and stamps `Origin`. Verdict: **EXTEND** the loop shape, **REPLACE** the
registration boundary. The contract's `backendIP string` and the non-zero `Port` that
`labels.Parse` requires (`labels/labels.go:62-68`) are joined into a dial address the moment
`Manager.Register` runs (`proxy/manager.go:208`); neither has a value for a function, so a
Lambda reader cannot register anything usable through the interface as it stands. Either
its signature changes or a parallel registration path exists — a boundary change, not a
third implementor.

**ECS reader as the sibling implementation.** `ecs.Watcher.cycle` (`ecs/watcher.go:169-222`)
lists, describes, parses labels, resolves an address, then `removeMissing` deregisters
anything not seen. Healthy/unhealthy cadence (`ecs/watcher.go:137-142`; defaults 10s / 2m),
one reader per account built by `buildECSWatchers` (`main.go:414`, called from `:354`), per-reader AWS config
via `awsConfigForSpec` (`main.go:562-586`: adaptive retry, 5 attempts, region/profile/config
file/static keys), and `ecs.Registry` reader status for the status page
(`ecs/registry.go:50-70`). Verdict: **EXTEND** — the poll/diff/retry/multi-account shape and
the AWS config loading transfer directly; only the list/resolve steps differ.

**Request path.** One `httputil.ReverseProxy` per advertised service
(`proxy/reverseproxy.go:69-118`) with `Rewrite` setting scheme and `X-Forwarded-*`
(`:78-100`), `poolTransport` choosing a member per attempt and retrying on connect errors
for bodiless requests (`:138-179`), `newBackendTransport` (`:217-281`; 5s dial budget that
also bounds the TLS handshake, **`InsecureSkipVerify: true` for `https` backends at
`:247`**), node-header
injection via in-process WhoIs (`:283-316`), and `classifyProxyError` mapping `net` errors
to metric reasons (`:340-360`). Verdict: **EXTEND** the proxy, retry and metrics layers
for a function-URL backend, but **not the `https` transport as it stands**: skip-verify is
tolerable today because every backend is addressed by IP (`Register` takes a `backendIP`),
so there is no hostname a certificate could be checked against. A function URL is addressed
by hostname and presents an AWS-issued certificate; sending signed requests through the
current transport would bypass validation against an internet-facing endpoint — a security
regression, so the transport needs a verifying path before option 1 can use it.
**REPLACE** the transport for an Invoke-API backend (the request must become a JSON event,
so `http.Transport` is not involved).

**Manager.** `Register` (`proxy/manager.go:205-267`) is idempotent on key, refuses
scheme/caps conflicts, and opens the listener on the first member; `Deregister`
(`:470-520`) withdraws the service when the pool empties. Copy-on-write pool with
round-robin rotation (`proxy/pool.go:32-77`). Verdict: **EXTEND** the pool, listener and
conflict logic, which only need a key and an `addr`; the `Register` entry point is part of
the boundary above, since it is where `host:port` is assumed.

**AWS SDK dependencies.** `go.mod:6-12` already carries `aws-sdk-go-v2` core, `config`,
`credentials`, `ec2`, `ecs`, `s3`, `sts`; `aws/protocol/eventstream` is present indirectly
(`go.mod:24`), and the SigV4 signer is a package of the core module already present. Only
`service/lambda` and `service/resourcegroupstaggingapi` would be new. Verdict: **EXTEND**
the dependency set.

**Metrics and status.** Per-service metrics are keyed by service name only; the backend
gauge counts pool size (`metrics/metrics.go:78-80`); the status page shows each backend's
target address and origin (`local/status.go:148-161`). Verdict: **EXTEND** — a function
backend needs a displayable target in place of `host:port`.

Reusing the ECS reader shape, the AWS config loader, and the manager's pool and listener
logic; the `Registrar` boundary and the `https` transport do not fit as they stand, and no
existing code fits the transport for an Invoke-API backend, because it is not HTTP.

## Approach Options

The decision has two independent axes: **how a request reaches the function** (options 1-3)
and **how functions are found** (options A-B). Each option is tagged with what kind of
question decides it: *system-fact* (the code or AWS settles it), *product-intent* (a human
choice), or *design-phase* (design owns it).

### Invocation path

**1. Function URL with `AWS_IAM` auth, tsserve signs each request (SigV4).** *[system-fact
for feasibility; product-intent for acceptability]*

The function gets a fixed HTTPS endpoint; tsserve treats it as an `https` backend whose
host is the URL's hostname, adding a signing step before `RoundTrip`. Lambda converts the
HTTP request to a payload-format-2.0 event and the function's response back to HTTP,
including `cookies` → `set-cookie` and base64 for binary bodies; a bare string or JSON
return is inferred as a 200 `application/json` response. Invoking a function URL requires
`lambda:InvokeFunctionUrl` **and** (for URLs created from October 2025) `lambda:InvokeFunction`;
cross-account needs both an identity policy and a resource-based policy on the function.

- For: smallest request-path change — reuses the reverse proxy, retry and metrics paths;
  Lambda does the HTTP↔event translation, so fidelity is AWS's problem; response
  streaming (`InvokeMode=RESPONSE_STREAM`) arrives as ordinary chunked HTTP, so SSE and
  large responses (up to 200 MB) work without special handling; function timeout up to 15
  minutes applies directly.
- Against: **function URLs are reachable only over the public internet** — PrivateLink
  covers the Lambda API, not function URLs, and AWS support has confirmed there is no
  VPC-only option. IAM is the only gate. Signing covers the SHA-256 of the body and
  "Lambda doesn't support unsigned payloads" (AWS CloudFront guide), so the whole request
  body is in memory before it is sent and streaming request bodies are impossible. The
  `https` transport cannot be reused unchanged: it skips certificate verification
  (`proxy/reverseproxy.go:247`), which is a regression against a hostname-addressed public
  endpoint (see Existing Surfaces, request path), so option 1 carries a transport change
  as well as the signing step. Discovery must also fetch each function's URL
  (`GetFunctionUrlConfig`, inside the 15 rps control-plane bucket).

**2. Lambda `Invoke` API through a custom `http.RoundTripper`.** *[system-fact for
feasibility; design-phase for translation rules]*

tsserve builds the payload-format-2.0 event itself, calls `Invoke` (or
`InvokeWithResponseStream`) with `InvocationType=RequestResponse`, and turns the JSON
response back into an `*http.Response`. Because `httputil.ReverseProxy` only needs a
`RoundTripper`, the rewrite/header-injection/metrics wrapping stays as is.

- For: the Lambda API is reachable through an interface VPC endpoint, the same mechanism
  SPEC.md "Network requirements" already uses for the `ecs` / `ec2` APIs, so traffic can
  stay on AWS networking; the only permission is `lambda:InvokeFunction`, scopable with an
  `aws:ResourceTag` condition;
  cross-account works through the standard function resource policy; no public endpoint to
  create or govern; no per-function URL to discover; alias/version selection is a request
  parameter (`Qualifier`).
- Against: tsserve owns the translation. That means: 6 MB request and response limit for
  buffered invocations, 1 MB for request line plus headers, `RequestTooLargeException` (413)
  on overrun; the event format carries multi-value headers as one comma-joined string,
  cookies as a separate array, and bodies as text or base64 with an `isBase64Encoded`
  flag, none of which an `*http.Request` expresses the same way; the whole request body is
  read before the call. A function that throws is reported as an API-level HTTP 200 with an
  `X-Amz-Function-Error` header and an error document as the body. Streaming responses
  come back as an event stream with a metadata prelude rather than as HTTP. `Invoke` is
  subject to the 10 requests/second per execution environment rule of any synchronous
  invocation, and the SDK's standard retryer treats 429 and 5xx responses alike as
  retryable.

**3. Ordinary HTTP endpoint in front of the function (ALB or API Gateway), no new tsserve
code path.** *[product-intent]*

tsserve proxies to the ALB/API Gateway hostname as an `http`/`https` backend. Nothing to
build in the request path; discovery is the only gap (the hostname is not tag-discoverable
from the function, so this needs option B). Adds an AWS component per service and that
component's limits: API Gateway's 29 s default integration timeout (raisable for REST
APIs; a hard 30 s for HTTP APIs) and 10 MB payload cap; ALB's 1 MB request and response
body caps for Lambda targets, with no response streaming. Listed for completeness — it is
the "do nothing in tsserve" baseline.

### Discovery

**A. Tag-based discovery via the Resource Groups Tagging API.** *[system-fact]*

`tag:GetResources` with `ResourceTypeFilters=["lambda:function"]` and a `TagFilter` on
a chosen opt-in key returns matching function ARNs *with their tags* in one paginated call
(100 per page; 15 calls/second; regional; untagged resources are never returned). One call
per poll cycle per region-and-account is enough for hundreds of functions — cheaper than
ECS's `ListTasks` + `DescribeTasks` + `DescribeTaskDefinition`. AWS tag keys and values
permit `.`, `:` and `/`, so `tsserve.service=svc:api` is a legal tag. The alternative,
`lambda:ListFunctions` (50 per page) plus per-function `ListTags`, shares the
non-raisable 15 rps control-plane bucket and scales linearly with function count; it is
not attractive. Tag reads need `lambda:ListTags` only when going through the Lambda API;
`tag:GetResources` needs its own action.

Caveats that hold regardless: tags are function-level, so nothing in AWS metadata names
the version or alias to invoke; tags are read only when the reader polls, so a change is
visible one cycle later at the earliest; `tsserve.port` and `tsserve.network` have no
meaning for a function, yet `labels.Parse` requires the former.

**B. Static configuration.** *[product-intent]*

A service-to-function mapping supplied in tsserve's configuration (today all
configuration is environment variables; `TSSERVE_ECS_ACCOUNTS` is the precedent for a
structured value). No discovery calls, no tag conventions, no lag; but every new service is
a proxy-host redeploy, which is the workflow the label vocabulary was introduced to avoid
(SPEC.md "Goals": label-driven discovery).

### Recommendation

**Option 2 (Invoke API RoundTripper) with discovery A (tags via `GetResources`).**
Confidence: **medium**.

Reasoning: the deployment topology tsserve exists for keeps backends off the public
internet, and option 1 cannot honour that — an IAM-gated public URL is a smaller change but
a different security posture, and it also buffers every body and needs a verifying `https`
transport, so its "reuse everything" advantage is partly illusory. Option 2 keeps
invocation on the VPC-endpoint pattern SPEC.md already documents, needs one IAM action,
and the translation it demands is bounded and specified by AWS (payload format 2.0, which
the Lambda Web Adapter accepts). The motivating workload is small request/response traffic,
so the buffered-invocation limits are not constraints for it. Confidence is medium rather
than high because option 1 becomes the better answer if a public, IAM-gated endpoint is
acceptable to the operators — that is Scope Question 1.

## Integration Points

- **Multiple backends per service** (`docs/designs/multiple-backends-per-service/`, merged in
  `e059200`): a function backend is one pool member keyed by ARN; several functions tagged
  with the same service would round-robin. The conflict rule compares scheme and caps only,
  so mixing a function with container backends in one pool is not refused today — whether
  that is desirable is a design question.
- **ECS multi-account readers** (`ecs/account.go`, `main.go:354-372,562-586`): the Lambda
  reader would adopt the same per-account config, retry-mode, and registry-status handling.
  A single `TSSERVE_DISCOVERY` value selects one mode today (`main.go:396-397`); running
  Lambda discovery alongside ECS discovery changes that contract.
- **Status page and metrics** (`local/status.go`, `metrics/metrics.go`): rows and error
  reasons assume a dialled address; Lambda failure modes (throttled, function error,
  payload too large) do not map onto `timeout` / `connection-refused` / `dns` / `eof`.
- **Node identity headers** (`proxy/reverseproxy.go:283-316`): injected in `Rewrite`, so
  they are on the outbound request before any transport sees it — either invocation path
  carries them to the function unchanged.

## Complexity and Risk Areas

1. **Fidelity of the HTTP↔event translation (option 2).** Header case-folding, multi-value
   headers, cookies, binary detection, query-string re-encoding, and status inference are
   each places where a function written against API Gateway or a function URL may behave
   differently under tsserve.
2. **Limits that containers do not have** (the payload, header, timeout and upgrade limits
   listed under options 1 and 2). A client that streams a large upload today would fail.
3. **Retry semantics.** `poolTransport` retries only connect-class errors for bodiless
   requests, on the premise that a connect failure means the backend never saw the request.
   Lambda's failures are HTTP-level responses, and the SDK's retryer sits below tsserve's
   with no view of whether the function ran.
4. **Concurrency and cold starts.** Each execution environment serves 10 requests/second;
   default account concurrency is 1,000; a cold start adds latency of a kind the 5-second
   dial timeout does not govern.
5. **Discovery drift.** A function deleted or re-tagged between polls stays in the pool
   until the next cycle; a function whose invoke permission is revoked stays registered and
   fails per request.
6. **Discovery-mode exclusivity.** `TSSERVE_DISCOVERY` selects one mode; the motivating
   deployment presumably wants ECS and Lambda backends from one proxy host.
7. **Registration boundary.** `Registrar`, `labels.Parse` (`tsserve.port` required,
   `tsserve.network` defaulted) and `Manager.Register` all assume an IP and a port; a
   function has neither, so the boundary named under Existing Surfaces changes whichever
   invocation path is chosen.
8. **Versions and aliases.** Tags cannot name one; invoking `$LATEST` by default changes
   deployment semantics for teams that publish versions.

## Scope Questions

Load-bearing — recorded on the card for the reviewer, with the assumption the document
proceeds on:

1. **Is a public, IAM-gated function URL acceptable, or must invocation stay on AWS private
   networking?** Decides option 1 vs 2. *Working assumption: private networking required,
   matching the ECS topology in SPEC.md.*
2. **Do functions live in accounts other than the proxy host's?** Decides whether the
   multi-account reader shape is needed on day one. *Working assumption: yes, same as ECS.*
3. **Must Lambda discovery run alongside ECS discovery in one process?** Decides whether
   `TSSERVE_DISCOVERY` stays single-valued. *Working assumption: yes.*

Minor, non-blocking:

4. Does any candidate workload need streaming responses (SSE) or bodies over 6 MB?
   *Working assumption: no; the motivating service is small request/response.*
5. When nothing names a version or alias, should `$LATEST` be invoked or the function
   refused? *Working assumption: `$LATEST`, mirroring how a container's current image is
   used.*

## References

- [Invoking Lambda function URLs](https://docs.aws.amazon.com/lambda/latest/dg/urls-invocation.html) — request/response payload format, SigV4 requirement
- [Control access to Lambda function URLs](https://docs.aws.amazon.com/lambda/latest/dg/urls-auth.html) — `AWS_IAM` vs `NONE`, cross-account policy, `lambda:InvokedViaFunctionUrl`
- [Lambda quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html) — 6 MB / 200 MB / 1 MB header quota, 15 rps control plane, 10 rps per execution environment
- [Response streaming for Lambda functions](https://docs.aws.amazon.com/lambda/latest/dg/configuration-response-streaming.html) — supported runtimes, bandwidth cap, "function URLs do not support response streaming within a VPC environment"
- [InvokeWithResponseStream API](https://docs.aws.amazon.com/lambda/latest/api/API_InvokeWithResponseStream.html) — event-stream response shape and error codes
- [Introducing AWS Lambda response streaming](https://aws.amazon.com/blogs/compute/introducing-aws-lambda-response-streaming) — `HttpResponseStream` metadata prelude
- [Connecting inbound interface VPC endpoints for Lambda](https://docs.aws.amazon.com/lambda/latest/dg/configuration-vpc-endpoints.html) — PrivateLink covers the Lambda API, not function URLs
- [Make Lambda Function Urls accessible within the VPC only (AWS re:Post)](https://repost.aws/questions/QUnGvBkWeQSlibCq6Fy5HBkw/make-lambda-function-urls-to-be-accessible-within-the-vpc-only) — AWS confirmation that no private function-URL mode exists
- [Using tags on Lambda functions](https://docs.aws.amazon.com/lambda/latest/dg/tagging.html) — function-level only, `lambda:ListTags`, `GetResources` filtering
- [Using attribute-based access control in Lambda](https://docs.aws.amazon.com/lambda/latest/dg/attribute-based-access-control.html) — `aws:ResourceTag` conditions
- [Resource Groups Tagging API — GetResources](https://docs.aws.amazon.com/resourcegroupstagging/latest/APIReference/API_GetResources.html) and [endpoints and quotas](https://docs.aws.amazon.com/general/latest/gr/arg.html) — 100 per page, 15 calls/second, regional
- [Tag naming limits and requirements](https://docs.aws.amazon.com/tag-editor/latest/userguide/tagging.html#tag-conventions) — permitted characters
- [AWS Lambda Web Adapter](https://github.com/awslabs/aws-lambda-web-adapter) — accepts API Gateway v1/v2, ALB and function-URL events; `AWS_LWA_INVOKE_MODE=response_stream`
- [Restrict access to an AWS Lambda function URL origin (CloudFront)](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-restricting-access-to-lambda.html) — "Lambda doesn't support unsigned payloads"
- [Lambda functions as ALB targets](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/lambda-functions.html) — 1 MB request/response body limits
- [aws-sdk-go-v2 retry package](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/aws/retry) — default retryable HTTP status codes
