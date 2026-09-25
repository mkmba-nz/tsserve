# Lambda Function Backends

Tracking Issue: FZ-2447

Research: [RESEARCH.md](RESEARCH.md). Approved: invocation option 2 (the Lambda `Invoke` API
behind a custom `RoundTripper`) with discovery option A (tags, read through the Resource Groups
Tagging API). The card owner's answers 1A, 2A, 3A, 4A, 5A refer to the numbered questions in the
research hand-off comment on the card: invocation stays on AWS private networking; discovery is
tag-based; functions live in other accounts from day one; Lambda discovery runs alongside ECS
discovery in one process; proceed to design. The card owner also directed that the HTTP-to-Invoke
translation stay simple: the functions this is for need basic request/response handling only.

## Introduction

Every tsserve backend is a `host:port`. `proxy.Manager.Register` joins an IP and a port into a
dial address (`proxy/manager.go:205-208`), and the service's transport dials it. A Lambda function
has no address: it is reached through the `Invoke` API with a JSON event, and it is found by AWS
resource tags rather than container labels (RESEARCH.md, Problem Statement).

This feature adds a second kind of backend. A **function backend** is registered against an
advertised service exactly as a container is — it joins the service's backend pool, is served
round-robin, and withdraws the service when it is the last member — but each request to it is
translated into a function-URL-format event, delivered with one `Invoke` call, and the function's
response is translated back. Functions are discovered by a **Lambda reader** that polls the
Tagging API for functions tagged `tsserve.enable=true`, one reader per AWS credential context, in
the same process as ECS or Docker discovery.

The workload this is designed for is small request/response functions: a single route, `GET` or
`POST`, a bearer token in the `Authorization` header, an optional small JSON or form body, and a
JSON response of a few kilobytes with `Content-Type` and `Cache-Control` headers. Some of them
make authorisation decisions from the `Tailscale-User-*`, `Tailscale-Node-*` and
`Tailscale-App-Capabilities` headers tsserve forwards, so those must arrive intact. None need
streaming, cookies, binary bodies, or bodies anywhere near Lambda's limits; where the design has
a choice, it takes the simpler option and lists the richer one under Non-Goals.

## Terminology

Carried forward from RESEARCH.md and from `docs/designs/multiple-backends-per-service/`. These
terms are used verbatim in the stories below and are the only sanctioned names for code
identifiers, log lines, and SPEC.md.

| Term | Meaning |
|---|---|
| **Backend** | One discovered endpoint that can serve a service: a container backend or a function backend. |
| **Container backend** | A backend that is a host and port (a Docker container or an ECS task container), reached with the advertised service's scheme. What every backend was before this feature. |
| **Function backend** | A backend that is a Lambda function. Its registration key is the function's unqualified ARN; its target is `lambda://<function ARN>[:<qualifier>]`. |
| **Target** | The string that names where a backend's requests go, shown on the status page and in logs: `scheme://host:port` for a container backend, `lambda://...` for a function backend. |
| **Invoker** | The per-function-backend component that turns one outbound request into one `Invoke` call and the result back into a response. |
| **Qualifier** | AWS's term for the version or alias of a function to invoke. Named by the `tsserve.qualifier` function tag; when absent the unqualified function (`$LATEST`) is invoked. |
| **Function tags** | The `tsserve.*` AWS resource tags on a function: the counterpart of the `tsserve.*` container labels. |
| **Reader** | One watcher bound to one AWS credential context. An ECS reader polls one cluster; a Lambda reader polls one region for tagged functions. |
| **Discovery mode** | One of `docker`, `ecs`, `lambda`. `TSSERVE_DISCOVERY` names one or more. |
| **Registration key**, **Advertised service**, **Backend pool**, **Origin** | Unchanged from the multiple-backends PRD. A function backend's Origin carries the reader's account and region; its cluster is empty. |

"Function URL" appears only under Non-Goals; the approved design does not use one.

## Goals

1. A request to an advertised service backed by a function reaches the function, and the client
   receives the status, headers and body the function returned (US-002).
2. The identity headers a container backend receives today — `Tailscale-User-*`,
   `Tailscale-App-Capabilities`, and `Tailscale-Node-*` when `tsserve.caps` is set — reach the
   function (US-002).
3. A function tagged `tsserve.enable=true` becomes a backend, and a function that is deleted or
   untagged ceases to be one, without a tsserve restart, whether it lives in the proxy host's
   account or another (US-003).
4. Lambda discovery runs alongside ECS or Docker discovery in one tsserve process (US-004).
5. Invocation uses only the regional Lambda API — reachable over AWS private networking through
   an interface endpoint — and one IAM action on tsserve's own principal; no function URL or
   other public endpoint is created or required (US-002, US-006).
6. Operators can see function backends and Lambda readers on the status page and count function
   failures by reason in metrics (US-001, US-002, US-005).
7. SPEC.md, README.md and the systemd env example describe function backends: tags, IAM,
   networking, limits, and configuration (US-006).

## User Stories

Stories are ordered by dependency. Each story's ACs are the authoritative specification of its
behaviour; later sections refer to them rather than restating them.

### US-001: The manager accepts function backends

**Description:** As an operator, I want a function registered against an advertised service so
that the pool, listener, conflict and teardown logic the manager already has serves it.

**Acceptance Criteria:**

- [ ] A function backend enters the manager through a registration entry distinct from
      `Register(key, def, backendIP)` (`proxy/manager.go:205`). It carries the registration key
      (the unqualified function ARN), the service-level fields (service, caps, Origin), the
      target string, and the invoker (US-002) that performs the backend's requests.
      `docker.Registrar` (`docker/watcher.go:22-25`), `ecs.Registrar` (`ecs/watcher.go:23-26`),
      `registrarAdapter.Register` (`main.go:287-303`) and the container path through
      `Manager.Register` are unchanged. **Trap:** `Manager.Register` joins `backendIP` and
      `def.Port` into a dial address (`proxy/manager.go:208`); a function has neither, so
      function backends do not go through that path with a placeholder address.
- [ ] A function backend's target renders as `lambda://<function ARN>` with `:<qualifier>`
      appended when one is set, wherever a backend address renders today: `Snapshot`'s
      `BackendView.Backend` (`proxy/manager.go:143`), and the "service advertised", "backend
      joined service" and "backend left service" log lines.
- [ ] An advertised service whose first backend is a function has the scheme `lambda`. Pools are
      homogeneous: the manager refuses a container backend offered to a function-backed
      service, or a function offered to a container-backed service, through the existing
      scheme comparison in `conflictWith` (`proxy/manager.go:428`) with a `ConflictError`, and
      the watcher that offered it logs and retries as for any refused backend
      (multiple-backends US-003/US-004). No new conflict field is introduced.
- [ ] **Trap:** `lambda` is an internal scheme, never a label value. `labels.Parse` keeps
      rejecting `tsserve.scheme=lambda` (`labels/labels.go:80`), and the Lambda reader ignores a
      `tsserve.scheme` tag (US-003), so no path lets a container register with the function
      scheme or a function register with `http`/`https`.
- [ ] The transport of a function-backed service dispatches each attempt to the chosen pool
      member's invoker; container-backed services keep the shared `http.Transport` per
      service. **Trap:** `newReverseProxy` builds `newBackendTransport(scheme)` unconditionally
      (`proxy/reverseproxy.go:72`); a function-backed service has no use for one, so that
      construction is bypassed for the `lambda` scheme rather than left as a dead transport
      holding a dialer.
- [ ] **Trap:** `poolTransport.RoundTrip` sets `attempt.URL.Host` and `attempt.Host` to the
      member's address on its clone of the request (`proxy/reverseproxy.go:160-161`). A
      function has no host of its own, so for a function member that rewrite does not happen:
      the invoker receives the client-addressed host unchanged (see the host AC in US-002).
- [ ] Everything else about the pool is unchanged for function backends: registration is
      idempotent on key; several functions tagged with the same service round-robin; caps are
      fixed by the first registration; deregistering the last function tears the service down
      and frees the name; `tsserve_services_active` and `tsserve_service_backends` count
      function-backed services and function backends exactly as container ones.
- [ ] `Snapshot` reports a function backend's `Key` as the function ARN and its `Origin` as the
      reader stamps it — the same identity ECS readers stamp (`main.go:471`: the resolved
      account ID, an explicit `name`, or `default` in single-reader configuration) plus the
      region, with an empty cluster — and the service's `Scheme` as `lambda`, which is what
      lets the status page (US-005) tell a function backend from a container backend.
- [ ] Tests prove: registering a function backend advertises the service and it appears in
      `Snapshot` with the `lambda://` target; a second function joins the pool; a container
      backend offered to a function-backed service is refused, and a function offered to an
      `http` service is refused, with existing members untouched; deregistering the last
      function closes the listener. The in-memory listener factory in `proxy/manager_test.go`
      is the harness.

### US-002: Requests reach a function through the Invoke API

**Description:** As a client of a function-backed service, I want my request delivered to the
function and its response returned to me so that the function behaves like a container backend
for ordinary request/response traffic.

**Acceptance Criteria:**

- [ ] Each function backend's invoker is an `http.RoundTripper` that performs one `Invoke`
      (`InvocationType=RequestResponse`, the backend's function ARN and qualifier) per attempt
      using the Lambda client of the reader that discovered the function, and returns an
      `*http.Response`. `httputil.ReverseProxy`, `Rewrite`, node-header injection
      (`proxy/reverseproxy.go:69-118`) and the metrics middleware are unchanged and run before
      the invoker sees the request, so the outbound headers are the ones a container would
      receive.
- [ ] **Event format.** The `Invoke` payload is a function-URL / API Gateway payload-format-2.0
      event: `version` `"2.0"`, `routeKey` `"$default"`, `rawPath` is the request path,
      `rawQueryString` is the outbound request's query string (Go's `ReverseProxy` has already
      dropped unparsable parameters before `Rewrite`), `queryStringParameters` is the decoded
      query (several values of one name joined with `,`), `headers` holds every outbound request
      header with its name lowercased and several values of one name joined with `,`,
      `requestContext.http` carries `method`, `path`, `protocol`, `sourceIp` and `userAgent`,
      and `requestContext.stage` is `"$default"`. `rawPath` and `requestContext.http.path`
      carry the identical value: the request path as sent, percent-encoding preserved, neither
      decoded. `sourceIp` is the peer's tailnet IP: the
      leftmost `X-Forwarded-For` entry, which the serve layer sets (`proxy/reverseproxy.go:92-93`).
      Other `requestContext` fields may be left at their zero values.
- [ ] `headers.host` and `requestContext.domainName` are the host the client addressed (the
      service FQDN). Go's `ReverseProxy` clones the inbound request for the outbound one, so
      `Host` still carries that value when the transport receives it; the only thing that
      would overwrite it is the per-member rewrite in `poolTransport`, which US-001 excludes
      for function members. `X-Forwarded-Host`, which `Rewrite` sets from `pr.In.Host`
      (`proxy/reverseproxy.go:95`), carries the same value.
- [ ] The identity headers are in `headers`: `tailscale-user-login`, `tailscale-user-name`,
      `tailscale-user-profile-pic`, `tailscale-app-capabilities` (when granted), and
      `tailscale-node-tags` / `tailscale-node-name` (when the service has caps and the peer is
      tagged), plus `x-forwarded-for`, `x-forwarded-host`, `x-forwarded-proto`.
- [ ] A `Cookie` request header is delivered as the `cookies` array (one entry per `;`-separated
      pair, trimmed) and not repeated in `headers`, matching the function-URL event format.
- [ ] **Body.** The invoker reads the request body in full before `Invoke`. It is sent as text with
      `isBase64Encoded: false` when the request `Content-Type` is textual — `text/*`,
      `application/json`, `application/x-www-form-urlencoded`, `application/xml`, or a
      `+json` / `+xml` suffix — and base64-encoded with `isBase64Encoded: true` otherwise. A
      request with no body sends an empty `body` and `isBase64Encoded: false`.
- [ ] **Body cap.** The invoker bounds the read so the event stays under Lambda's 6 MB
      synchronous payload maximum even when the body is base64-encoded (about 4 MiB of raw
      body). One cap applies whatever the encoding; the headroom a text body leaves unused is
      the price of a single limit. A request over the cap receives a `413 Payload Too Large`
      from the invoker as a response, not an error: the function is not invoked, the client
      does not get a 502, `tsserve_proxy_backend_errors_total` is not incremented, and the
      metrics middleware records it in `tsserve_proxy_requests_total` with code `413` as it
      records any response.
- [ ] **Response format.** When the function's return value is a JSON object containing
      `statusCode`, the response status is that value, each entry of `headers` becomes a
      response header, each entry of `cookies` becomes one `Set-Cookie` header, the body is
      `body` (base64-decoded when `isBase64Encoded` is true), and `Content-Length` is the
      decoded length. **Trap:** a `content-length` or `transfer-encoding` entry in the
      function's `headers` is dropped rather than copied — the body is already complete and
      its length known, and a stale or chunked value would produce a malformed response. Any
      other return value — a bare string, number, array, `null`, or an
      object without `statusCode` — is a `200` with `Content-Type: application/json` and the
      JSON payload verbatim as the body. This is the function-URL inference rule and is the
      whole of it; nothing else is inferred.
- [ ] **Function error.** An `Invoke` output with `FunctionError` set fails the attempt: the
      client receives a 502 through the existing `ErrorHandler`, the "backend proxy error" log
      line names the target and the error document's `errorType` and `errorMessage`, and the
      attempt is counted with reason `function-error`. The pool does not retry it against
      another member: the function ran. An oversized response reaches tsserve as a function
      error and is covered by this rule.
- [ ] **Retry rule.** The existing pool retry (multiple-backends US-002) applies unchanged:
      remaining members are tried, for body-less requests only, when the attempt failed in a
      way that means the function did not run. The invoker marks these with the same type-based
      marker the pool recognises (`connectError`, `proxy/reverseproxy.go:193`): a connection
      failure to the Lambda endpoint (dial, DNS, TLS), `TooManyRequestsException` (429),
      `ResourceNotFoundException`, and `AccessDeniedException`. Everything else — a function
      error, a 5xx from the API, a failure after the request was sent — ends with a 502 and no
      further attempt. When a throttle is account-wide every member fails alike and each
      attempt counts as `throttled`; that is accepted, the same as a pool of dead containers
      today. **Trap:** `classifyProxyError` (`proxy/reverseproxy.go:340`) is string-matching
      for the metric label only; the retry decision is by type, as today.
- [ ] **Trap — SDK retries.** The Lambda client used for `Invoke` has SDK retries disabled (one
      attempt per call). The SDK's standard and adaptive retryers — `awsConfigForSpec`
      (`main.go:562`) configures adaptive with five attempts — re-send on connection errors and
      5xx responses with no knowledge of whether the function ran, which would run a `POST`
      twice, and an SDK-level retry would also hide throttling from the `throttled` reason.
      The reader's discovery clients keep their retries (US-003).
- [ ] The `Invoke` client bounds connection establishment to the Lambda endpoint by the same
      few-second budget as container dials (`dialTimeout`, `proxy/reverseproxy.go:40`), so a
      blackholed endpoint — a missing VPC endpoint route — costs one dial timeout rather than
      the SDK client's 30 s default. No response timeout is added: the function's own timeout
      governs, and a cold start is not a connection failure. Cancelling the client request
      cancels the `Invoke` in flight.
- [ ] `tsserve_proxy_backend_errors_total{reason}` gains two values, `function-error` and
      `throttled` (429); connection failures to the Lambda endpoint use the existing reasons.
      The help text lists the new values. Per-service request metrics
      (`tsserve_proxy_requests_total` with the function's status code, duration, bytes) apply
      unchanged.
- [ ] Tests prove, with a fake Lambda API in the shape of `ecs/fakes_test.go` and the
      `httptest` harness of `proxy/reverseproxy_test.go`: a `GET` with a query string and
      headers arrives as a 2.0 event with lowercased headers, the client-addressed host, the
      peer IP as `sourceIp`, and the identity headers; a JSON `POST` body arrives as text with
      `isBase64Encoded` false and a binary body arrives base64-encoded; a structured response's
      status, headers, cookies and base64 body reach the client with the right
      `Content-Length`; a bare-JSON return becomes a 200 `application/json`; a function error
      yields a 502, reason `function-error`, and no second attempt; a throttled body-less
      request is retried against the second pool member and a throttled `POST` is not; a body
      over the cap yields a 413 with no `Invoke` call; the `Invoke` client makes exactly one
      HTTP attempt when the endpoint fails.

### US-003: A Lambda reader discovers tagged functions

**Description:** As an operator, I want a function tagged `tsserve.enable=true` to be advertised
without configuring tsserve, so that functions are discovered the way ECS tasks are.

**Acceptance Criteria:**

- [ ] A new package (`lambda/`, following `ecs/`) implements the Lambda reader with the same
      cycle contract as `ecs.Watcher`: an initial cycle at start, the healthy poll cadence and
      the slower unhealthy retry cadence (`ecs/watcher.go:137-142`), `active` and `failed`
      bookkeeping with warn-once on a repeated failure (`ecs/watcher.go:298-306`),
      `removeMissing` at the end of every cycle (`ecs/watcher.go:310-322`), and the reader
      records a key active only once `Register` reports it serving.
- [ ] One cycle is one paginated `GetResources` call with `ResourceTypeFilters`
      `["lambda:function"]` and a `TagFilters` entry for key `tsserve.enable` with value `true`.
      Tag values come from the same response; the reader makes no `ListFunctions`, `ListTags`
      or `GetFunction` calls. **Trap:** pagination ends when the response's `PaginationToken`
      is empty, not absent; the reader consumes every page before it runs `removeMissing`, or
      a function on page two is torn down every cycle.
- [ ] **Function tags.** `tsserve.enable` must be `true` (the server-side filter guarantees
      it); `tsserve.service` is required and must start with `svc:`; `tsserve.caps` is an
      optional comma-separated list; `tsserve.qualifier` is an optional version number or alias
      name, appended to the target and passed as the `Invoke` `Qualifier`. `tsserve.port`,
      `tsserve.network` and `tsserve.scheme` are ignored. The service and caps rules are the
      ones `labels.Parse` applies (`labels/labels.go:54-60`, `labels/labels.go:85-92`); the
      implementation agent may share that code by extracting it. **Trap:** `labels.Parse`
      itself requires `tsserve.port` (`labels/labels.go:63`) and would reject every function,
      so the reader does not call it.
- [ ] A function whose tags fail validation is logged once at Warn naming the ARN and the
      reason (quiet while the same failure persists, via `failed`), skipped, and re-examined
      every cycle. It is not treated as gone: if it was serving under a previous valid tag set
      it stays registered, the same carve-out `docker.Watcher.reconcile` makes for a container
      whose labels fail to parse (`docker/watcher.go:156-160`).
- [ ] The registration key is the function ARN as `GetResources` returns it (unqualified).
- [ ] **Trap — tags change in place.** An ECS task's definition is immutable, so `ecs.Watcher`
      records only the address of an active key (`ecs/watcher.go:69`) and detects a move by
      address alone. Function tags can change on a live function, and `Register` is idempotent
      on key, so the Lambda reader's record of an active key covers the service name, caps and
      target (ARN plus qualifier); the reader handles a change in any of them as the backend
      having moved — deregister and forget the record first (`ecs/watcher.go:266-272`), then
      register, recording the key active only on success (`ecs/watcher.go:279-288`) — so the
      pool reflects the new tags by the next cycle.
- [ ] **Multi-account.** `TSSERVE_LAMBDA_ACCOUNTS` is a JSON array with the same per-entry
      fields as `TSSERVE_ECS_ACCOUNTS` minus `cluster` (`name`, `region`, `profile`,
      `configFile`, `accessKeyID` + `secretAccessKey`) and the same blank-inherits-defaults
      rule, from `AWS_REGION`, `TSSERVE_LAMBDA_AWS_PROFILE` and
      `TSSERVE_LAMBDA_AWS_CONFIG_FILE`. Unset, it yields one reader from those defaults.
      `TSSERVE_LAMBDA_POLL_INTERVAL` defaults to `30s`; `TSSERVE_LAMBDA_RETRY_INTERVAL` defaults
      to `2m` and never runs faster than the poll interval. Explicit names must be unique;
      static credentials are all-or-nothing (`ecs.Plan`, `ecs/account.go:80-136`). **Trap:**
      a Lambda reader is per region, and the obvious multi-region layout is two entries for
      one account with different `region`s; their STS identities are equal, so — as SPEC.md
      already says for ECS — such entries carry explicit, distinct `name`s, and US-006
      documents that beside the Lambda example.
- [ ] The `lambda` start path in `main.go` builds readers with the resilience contract of
      `buildECSWatchers` (`main.go:414`): an AWS config that will not load disables that reader
      and logs it; an unassumable role leaves the reader unhealthy and retrying at the retry
      cadence, with the reader resolving its identity lazily; the reader resolves an account ID
      via STS only when accounts are explicitly configured and no `name` is given; two readers
      resolving to one identity are logged at Warn and both kept running, as ECS does
      (`main.go:496-500`); and the other readers start and serve regardless. Each reader's
      registrar stamps its Origin with the reader's identity and region.
- [ ] Each reader loads its AWS config through the same loader as ECS readers
      (`awsConfigForSpec`: region, profile, config file, static keys, adaptive retry) for its
      Tagging and STS clients, and derives from the same credentials the `Invoke` client with
      retries disabled and the dial budget of US-002. The implementation agent generalises the
      spec type `awsConfigForSpec` takes rather than duplicating the loader.
- [ ] Lambda readers register in the same reader registry as ECS readers (`ecs.Registry`,
      `ecs/registry.go`), reporting poll outcomes through the same `Reader` handle, so the
      status page shows them (US-005). Whether the registry moves to a shared package or
      `lambda` imports `ecs` for it is the implementation agent's choice.
- [ ] Tests prove, with a fake Tagging API in the shape of `ecs/fakes_test.go` and the
      `recordingReg` pattern of `ecs/watcher_test.go`: a cycle registers each tagged function
      with its service, caps, qualifier, target and Origin; a function absent from the next
      cycle is deregistered; a qualifier change and a service-name change each deregister and
      re-register; invalid tags warn once, are retried, and leave a previously serving
      registration in place; two pages are consumed before anything is deregistered; a failed
      `Register` is not recorded active and is retried next cycle; `TSSERVE_LAMBDA_ACCOUNTS`
      planning inherits blank fields from the defaults, rejects a duplicate explicit name and a
      lone static credential, and yields one default reader when unset.

### US-004: Discovery modes combine

**Description:** As an operator running ECS-backed and function-backed services from one proxy
host, I want to enable both discovery modes so that one tsserve process serves both.

**Acceptance Criteria:**

- [ ] `TSSERVE_DISCOVERY` accepts a comma-separated list of discovery modes drawn from `docker`,
      `ecs` and `lambda` (surrounding whitespace tolerated, matched case-insensitively as
      today). A single value behaves exactly as today and the default stays `docker` (an
      unset or blank variable, via `envDefault`). An unknown entry, an empty list element
      (`ecs,,lambda`) or a repeated entry is a startup error naming the offending value.
- [ ] `lambda` starts the readers of US-003. Every listed mode starts independently, all share
      the one manager, and each reader's registrar stamps its own Origin (`main.go:279-305`).
      ECS-only settings (`TSSERVE_ECS_CLUSTER`) are required only when `ecs` is listed, and
      `TSSERVE_LAMBDA_*` settings are read only when `lambda` is; each mode's validation stays
      inside its own start path as `startWatcher` does today (`main.go:309-394`).
- [ ] **Trap:** `startWatcher` reports on one `watchErr` channel and `awaitTerminal`
      (`main.go:185`) exits the process on the first value it receives, a `nil` included. With
      several modes, a fan-in forwards the first genuine error at once and sends `nil` only when
      every mode has finished; a mode that starts zero readers sends nothing, as the ECS path
      already does (`main.go:366-371`).
- [ ] A terminal failure in any one mode ends the process, as a single mode's failure does
      today; US-006 documents it.
- [ ] The status page's discovery value shows the configured list.
- [ ] Tests prove: a single mode, a list, and a list with whitespace are accepted, and an
      unknown, empty or duplicate entry is rejected with the value named; the fan-in forwards
      the first error and sends `nil` only after all modes finish (real goroutines, no mocks).

### US-005: The status page shows function backends and Lambda readers

**Description:** As an operator, I want function backends and Lambda readers visible on the
status page so that I can see what is advertised from where and whether discovery is healthy.

**Acceptance Criteria:**

- [ ] In the Services table a function backend's Backend cell shows its `lambda://` target, its
      Account cell the reader's account, and its Cluster cell `—`; the grouped-rows layout is
      unchanged.
- [ ] The per-backend key cell shows the function name (the last `:`-separated segment of the
      ARN) for a function backend, which `rowsFor` (`local/status.go:144-160`) recognises by
      the service's `Scheme` being `lambda` (US-001). **Trap:** `shortID` (`local/status.go:204`)
      strips to the last `/` and truncates to twelve characters, which renders every function
      ARN as `arn:aws:lamb`. The column header changes from "Container" to "Key"
      (`local/status.html:90`), since the cell shows the registration key's short form for a
      container, a task, or a function; the Docker and ECS renderings of the cell are
      unchanged.
- [ ] The Readers table lists Lambda readers alongside ECS readers with a column naming the
      reader's discovery mode (`ecs` or `lambda`); a Lambda reader's Cluster cell is `—`. Health
      states, last-polled and last-error rendering are as for ECS readers. **Trap:** the
      reader status type is mirrored in `ecs.ReaderStatus`, `local.ReaderStatus`, and
      `readerSource` (`main.go:257-277`); the mode must be carried through all three.
- [ ] The Readers table remains content-driven (`local/status.html:61` renders it when the
      registry holds any reader): it appears when at least one ECS or Lambda reader is
      configured and stays hidden in Docker-only mode.
- [ ] Tests prove: `TestRowsFor_*`, `TestStatusHandler_RendersServicesTable`,
      `TestStatusHandler_RendersReadersTable` and `TestShortID` (`local/status_test.go`) are
      extended so that a snapshot containing a function backend and a Lambda reader renders the
      `lambda://` target, the function name, `—` for cluster, and the reader's mode.
- [ ] Verify in browser: render the page with a container-backed service, a function-backed
      service, an ECS reader and a Lambda reader, and confirm the tables read correctly in
      light and dark schemes and that Docker-only and ECS-only renderings look as they do today.

### US-006: SPEC.md, README.md and the env example document function backends

**Description:** As a reader of the project documentation, I want function backends described
where the other discovery modes are, so that I can deploy one without reading the code.

**Acceptance Criteria:**

- [ ] SPEC.md gains a "Lambda function backends" section beside "ECS Cluster Mode" covering:
      when to use it; the topology (the proxy host calls the Lambda, Tagging and STS APIs
      through internet egress or the `com.amazonaws.<region>.lambda`, `.tagging` and `.sts`
      interface endpoints; the function needs no VPC and no public endpoint); the function-tag
      vocabulary table (US-003) including `tsserve.qualifier` and the ignored keys; the poll
      model, default interval and the Tagging API quota (one call per 100 functions per cycle
      per reader against a 15 calls/second regional limit); the multi-account configuration and
      `TSSERVE_LAMBDA_*` variables; an IAM table — `tag:GetResources` (not resource-scopable),
      `lambda:InvokeFunction` on the functions (scopable with an
      `aws:ResourceTag/tsserve.enable` condition), `sts:GetCallerIdentity` when `name` is
      omitted; the event and response format summary and the inference rule (US-002); the
      limits containers do not have (buffered request and response, the body cap and 413, no
      WebSocket or streaming, the function's own timeout governs); the retry rule for function
      backends and the disabled SDK retries; homogeneous pools (US-001); and an operator
      warning that a function which trusts the forwarded identity headers must be invokable by
      tsserve's principal only — any other invocation path (a function URL, a public API) lets
      a caller supply those headers itself.
- [ ] SPEC.md's other sections are updated where they assume one mode or an address: the
      "Flexible discovery" goal (`SPEC.md:14`), the "(Docker labels only)" non-goal
      (`SPEC.md:19`), the `TSSERVE_DISCOVERY` row (`SPEC.md:224`) and the new variables in the
      Configuration table, the Core Components preamble that says exactly one watcher is active
      (`SPEC.md:537`), a "1c. Lambda Watcher (`lambda`)" subsection beside "1a. Docker Watcher"
      and "1b. ECS Watcher" (`SPEC.md:539,558`), the backend definition in Service
      Manager (`SPEC.md:583`), the Reverse Proxy section (the invoker), Startup step 5
      (`SPEC.md:638`), the Error Handling table (function error, throttled, body over cap,
      function deleted between polls, a discovery mode failing terminally), the status page
      description and the metric inventory row for `tsserve_proxy_backend_errors_total`
      (`SPEC.md:831`), Project Structure (`lambda/`), Dependencies (`service/lambda`,
      `service/resourcegroupstaggingapi`), and Future Extensions (function URLs, response
      streaming, mixed pools).
- [ ] README.md's discovery, labels and environment-variable sections mention function tags,
      `TSSERVE_DISCOVERY` lists and the `TSSERVE_LAMBDA_*` variables; `systemd/tsserve.env.example`
      gains annotated entries for them beside the ECS ones (`systemd/tsserve.env.example:27-56`).
- [ ] No sentence in SPEC.md or README.md still states or implies that every backend is a host
      and port, that only one discovery mode runs, or that `tsserve.*` configuration lives only
      in container labels. **Carve-out:** the ECS backend-resolution sections legitimately
      describe `host:port` resolution for tasks and stay as they are.

## Non-Goals

- Function URLs, whether IAM-gated or public (research option 1), and ALB or API Gateway in
  front of a function (option 3). Hand-off answer 1A.
- Static configuration of functions in environment variables (research option B). Hand-off
  answer 2A.
- Response streaming (`InvokeWithResponseStream`), server-sent events, request or response
  bodies over the buffered limits, and WebSocket or other protocol upgrades. A function that
  returns `101` produces a 502, as any non-switchable 101 does today.
- Event formats other than payload format 2.0 (API Gateway v1, ALB) and any inference beyond
  the US-002 rule.
- Mixed pools: a service backed by both containers and functions (US-001). Cutover from a
  container to a function is done by retagging; the pool is torn down and re-advertised.
- Retrying the same function after a throttle or a 5xx, backoff, or any concurrency
  management; the SDK's retries are deliberately off for `Invoke` (US-002).
- Detecting tag changes faster than the poll interval (no EventBridge or CloudTrail path).
- Creating or modifying functions, aliases, tags, resource policies or IAM; tsserve reads
  and invokes only.
- `tsserve.port`, `tsserve.network` or `tsserve.scheme` semantics for functions.
- Any change to how Docker or ECS discovery finds, resolves or registers container backends.
  (The shared startup path, the readers table and the reader-config loader do change — US-003,
  US-004, US-005.)
- Active health checks, weighted selection, and the other standing non-goals of the
  multiple-backends PRD.

## Design Considerations

The only UI surface is the status page (US-005). The grouped-rows layout and the Readers table
from the multiple-backends and multi-account work are reused; the changes are two cells per
function backend, one column rename, and one new Readers column. The project defines no
UI-prototype convention; no prototype is attached.

## Technical Considerations

**Simplicity of the translation (card owner's direction).** US-002 specifies the one event
format, the one inference rule, and buffered bodies. Every richer behaviour a function written
against API Gateway or a function URL might expect is listed under Non-Goals so the
implementation agent does not add it speculatively.

**Where the invoker lives.** The invoker must mark function-did-not-run failures with the
`connectError` type that `poolTransport` recognises and share the `dialTimeout` budget
(US-002); both are unexported in `proxy`. Either the invoker lives in `proxy` behind a small
Lambda API interface (an `Invoke` method, in the style of `ecs.ECSAPI`) with the `lambda`
reader supplying the client, or `proxy` exports the marker and the budget; the implementation
agent chooses. Per-member dispatch in `poolTransport` — the shared transport for container
backends, the member's invoker for function backends — is the mechanism US-001's transport AC
binds.

**AWS clients.** A reader holds Tagging and STS clients with the retry configuration
`awsConfigForSpec` applies today, and an `Invoke` client built from the same credentials with
retries disabled and the dial budget of US-002. Two new SDK modules: `service/lambda` and
`service/resourcegroupstaggingapi`. `awsConfigForSpec` currently takes `ecs.WatcherSpec`; US-003
asks for the spec type to be generalised rather than the loader duplicated.

**Quotas.** `GetResources` allows 15 calls/second per region; one reader makes one call per 100
functions per cycle, so the 30 s default keeps even hundreds of functions per region at a
fraction of a call per second. `Invoke`'s 10 requests/second per execution environment and
account concurrency limits are the function owner's concern; tsserve surfaces throttling as
`throttled`.

**Phasing.** US-001 and US-002 are testable without the reader and may land together; US-003
depends on both (it builds invokers); US-004 wires `lambda` into `TSSERVE_DISCOVERY` and is the
first point at which the mode is reachable; US-005 depends on US-001 and US-003; the
implementation agent writes US-006 last, once the observed behaviour is fixed, and may deliver
it with the code stories in one PR.

**Testing strategy.** The project has no `docs/standards/`; follow the existing test
conventions. Fake AWS APIs as in `ecs/fakes_test.go` (a fake Tagging API and a fake Lambda
`Invoke`); the manager with the in-memory listener factory in `proxy/manager_test.go`; the
request path with the `httptest` harness in `proxy/reverseproxy_test.go`; the reader with
`recordingReg` in `ecs/watcher_test.go`; status rendering with `newTestServer` in
`local/status_test.go`. Concurrency ACs (the US-004 fan-in) are proven with real goroutines.
The Makefile's `test` target is `go test ./...`; the implementation agent also runs `proxy` and
the new package under `go test -race`. Feature-specific coverage each story must prove is listed
in that story's ACs.

## Success Metrics

- A function tagged `tsserve.enable=true` and `tsserve.service=svc:example` appears on the
  status page within one poll interval, and `curl https://example.<tailnet>.ts.net/` returns
  the function's status, headers and body.
- The function's logs show the `Tailscale-User-*` headers (and the node headers when caps are
  set) for a request from the tailnet.
- Removing the `tsserve.enable` tag withdraws the service within one poll interval;
  restoring it re-advertises the service.
- One tsserve process with `TSSERVE_DISCOVERY=ecs,lambda` serves an ECS-backed and a
  function-backed service at once, and both readers show as healthy.
- `go test ./...` and `go vet ./...` pass.

## Open Questions

The card owner answers these on the card; the feature designer edits the affected story if an
answer differs from the default written here.

1. **Homogeneous pools.** US-001 refuses mixing containers and functions in one pool. Keep, or
   allow mixed pools (the cutover case) as a follow-up?
2. **Poll interval default.** US-003 proposes `30s` for Lambda readers (ECS defaults to `10s`)
   because functions change rarely and the Tagging API is regional and shared. Keep, or match
   ECS at `10s`?
3. **`tsserve.qualifier`.** US-003 adds this optional tag so teams that publish versions or
   aliases are not forced onto `$LATEST`. Keep, or drop it and always invoke `$LATEST`?
