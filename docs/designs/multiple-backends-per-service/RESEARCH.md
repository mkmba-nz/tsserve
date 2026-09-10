# Multiple Backends Per Service Research

Tracking Issue: FZ-2399

## Overview

tsserve advertises each Tailscale Service from exactly one discovered backend. When a second
container or task carries the same `tsserve.service` label, it is logged as a duplicate and
dropped. This research covers what it would take for one advertised service to be backed by a
set of backends — several tasks of one ECS service, tasks of different ECS services, or
backends found by different readers — so a request reaches a live backend whenever at least
one is registered.

Omitted: Target / Destination (no framework is being plugged into — this reshapes existing
components); User Stories (the card's three requirements already state them).

## Problem Statement

`proxy.Manager` conflates *advertising a service* with *registering a backend*. One
`activeService` owns one listener, one `http.Server`, and one backend address, and `byName`
holds a single winning registration per service name. Everything downstream inherits that
shape.

The consequence is worse than "only one replica gets traffic":

- **A registration can be dropped rather than queued, and `nil` does not mean "serving".**
  `Register` returns `nil` on two paths that leave nothing serving: the duplicate-name branch,
  which returns *before* recording anything in `byCID` (`proxy/manager.go:138-145`), and a
  non-fatal `ListenService` failure, which logs and returns `nil` with nothing registered
  (`proxy/manager.go:157-165` — a MagicDNS misconfiguration is separated out as a `FatalError`,
  but every other listen error takes this branch). The ECS watcher reads either `nil` as
  success and records the key in its own `active` map (`ecs/watcher.go:267-271`).
- **When the winner disappears, the service goes dark and stays dark.** The winner's task
  vanishing removes it from `seen`, so `removeMissing` deregisters it and closes the listener
  (`ecs/watcher.go:277-284`). The loser is still in `seen` and still in `w.active` with an
  unchanged backend address, so `applyTask` short-circuits at the `prev == backendAddr` check
  (`ecs/watcher.go:255-257`) and never calls `Register` again. The service stays down until
  that task is itself replaced. SPEC.md:675 records the symptom ("If the first stops, the
  second does NOT auto-register") but frames it as an ECS-poll timing quirk rather than a
  reconciliation gap. The same short-circuit strands the listen-failure path: a *transient*
  `ListenService` error leaves the backend recorded as active with no listener, and it is never
  retried.
- **Docker mode has the same hole** by a different route: `docker/watcher.go` is event-driven
  with no reconciliation map at all, so a losing container only re-registers if it restarts.

So the card's hard requirement — "an incoming request must reach a task if at least one is
registered" — is not merely unmet for replicas. The current design can leave a service with a
live backend the watcher believes is registered and no listener at all.

## Domain Model and Terminology

This repository has no glossary file. The terms below are mostly already in use in SPEC.md and
in code; this section pins them down. New or newly-precise terms need sign-off before they
enter code identifiers or SPEC.md (Scope Questions Q6).

| Term | Meaning | Status |
|---|---|---|
| **Backend** | One discovered endpoint — scheme, host, port — that can serve a service. | Existing. `defSnapshot.Backend`/`BackendHost` (`proxy/manager.go:57-67`), `tsserve_proxy_backend_errors_total`. |
| **Registration key** | The watcher-supplied identifier for one backend: a Docker container ID, or `taskArn#containerName` in ECS. | Existing. SPEC.md:371, SPEC.md:574; `ecs/watcher.go:312-314`. |
| **Advertised service** | A Tailscale Service for which tsserve holds an open `ListenService` listener. | New (precision). Today indistinguishable from "backend". |
| **Backend pool** | The set of backends currently registered against one advertised service. | New. SPEC.md:979 already writes "per-service backend pool". |
| **Origin** | Which discovery source found a backend (ECS account / cluster / region; zero for Docker). | Existing. `proxy.Origin` (`proxy/manager.go:119-123`). |

If a pool is adopted, **"first wins"** and **"duplicate service name"** (`proxy/manager.go:140`,
SPEC.md:535, SPEC.md:675) stop being accurate: a second registration is a second pool member,
not a duplicate.

## Requirements

Stated as observable outcomes. How they are met is the design phase's call — see Approach
Options.

### Functional Requirements

- A request to an advertised service reaches a live backend whenever at least one registered
  backend is reachable.
- Every backend carrying a given `tsserve.service` label serves that service, regardless of
  provenance: multiple tasks of one ECS service, tasks of different ECS services, and backends
  found by different readers with different `Origin` values.
- Removing one backend does not interrupt the service while others remain. The service stops
  being advertised only when its last backend goes away.
- Backends can be added and removed without the service advertisement or its TLS certificate
  being torn down and re-established.
- Which backend serves a given request is specified and documented, rather than incidental.
- Behaviour is defined when backends of one service carry divergent `tsserve.scheme`,
  `tsserve.port`, or `tsserve.caps`.
- An operator can see every backend of a service, and its account and cluster, on the status
  page.
- SPEC.md no longer states that replica fan-out is unsupported, and its Service Manager,
  Reverse Proxy, and Error Handling sections describe the observed behaviour (line references
  in Existing Surfaces below).

### Non-Functional Requirements

- Reads of the backend set happen on every proxied request; writes happen only when discovery
  observes a change. The read path must not serialise behind discovery.
- `tsserve_services_active` keeps well-defined, documented semantics (Q4).
- `go test ./...` and `go vet ./...` pass (`Makefile:30-34`).

### Out of Scope

- **Active health checking.** SPEC.md:975 keeps "Health checks — verify backend is reachable
  before advertising the service" as its own Future Extension. Working assumption: no probing
  and no passive ejection in this card (Q2).
- **Weighted, least-connections, or latency-aware balancing.** The card names low-volume admin
  instances and internal APIs; distribution quality is explicitly not a concern.
- **Multiple services per container** (SPEC.md:976) — a different axis of the same label
  vocabulary.

## Current System Analysis

### Existing Data Models and Storage

`proxy.Manager` (`proxy/manager.go:31-47`) holds `byCID map[string]*activeService`
(registration key → the one service it owns) and `byName map[string]string` (service name →
the registration key that won it), under a single `mu sync.Mutex` held only around map
mutation. `activeService` (`proxy/manager.go:49-53`) carries one listener, one `http.Server`,
and one `defSnapshot`; `defSnapshot` (`proxy/manager.go:57-67`) carries a single `Backend`
string plus `Scheme`, `BackendHost`, `Port`, `Caps`, `ContainerID`, `RegisteredAt`, `Origin`.

`ServiceView` (`proxy/manager.go:71-81`) is the read-only copy returned by `Snapshot()`, which
iterates `byCID` and sorts by service name (`proxy/manager.go:265-284`). `Scheme` and
`BackendHost` are carried on both structs but grep finds no consumer for either — only the
pre-formatted `Backend` string reaches the status page.

`AcceptAppCaps` is set from `def.Caps` at `ListenService` time (`proxy/manager.go:148-156`).
It is a property of the listener, so it cannot vary between backends of one service.
`injectNode` (`proxy/manager.go:178`) and the metrics middleware (`proxy/manager.go:180`) are
likewise bound once per service.

### Existing Surfaces

Reuse reconnaissance across `go.mod` and the tree found nothing that supplies backend pooling:
`tailscale.com v1.98.2` provides service listeners only; `httputil.ReverseProxy` has no retry
hook (below); the AWS SDK's adaptive retry (`main.go:220-221`) covers API calls, not the proxy
path. The relevant surfaces are all in-repo.

**Discovery — needs little or no change.** Both watchers already register at backend
granularity with distinct keys, so they can already emit two registrations for one service
name:

| Surface | Today |
|---|---|
| `docker/watcher.go:22-25` | `Registrar{Register(containerID, def, backendIP) error; Deregister(containerID)}`. Key is the raw container ID (`docker/watcher.go:138`). Event-driven; no reconciliation map. |
| `ecs/watcher.go:23-26` | Structurally identical `Registrar`, declared separately so `ecs` need not import `docker`. Key is `taskArn + "#" + containerName` (`ecs/watcher.go:312-314`). |
| `ecs/watcher.go:69,185-284` | `active map[string]string` (key → `host:port`) reconciled against a per-cycle `seen` set. Assumes one registration per `(taskArn, containerName)` — never per service name. Note the `prev == backendAddr` short-circuit (`ecs/watcher.go:255-257`): in steady state a poll cycle calls neither `Register` nor `Deregister`, so registration churn tracks *task* churn, not the 10s poll interval. |
| `main.go:287-295,466-471` | `registrarAdapter` converts `labels.ServiceDef` → `proxy.ServiceDef` and stamps a per-reader `proxy.Origin`. One shared `proxy.Manager` serves every reader. |

**Request path.**

- `newReverseProxy` (`proxy/reverseproxy.go:49-92`) closes over a single fixed
  `target *url.URL`, used by `Rewrite` (`pr.SetURL(target)`, `pr.Out.Host = target.Host`), by
  the `https` transport branch, and by the `ErrorHandler` log line.
- `httputil.ReverseProxy` exposes no retry. In the Go 1.26 source
  (`net/http/httputil/reverseproxy.go:564-569`) the transport error path is
  `res, err := transport.RoundTrip(outreq)` … `if err != nil { p.getErrorHandler()(rw, outreq, err) }`.
  Nothing has been written to the `ResponseWriter` on *that* path, which makes `ErrorHandler`
  the one existing seam where a different backend could still be tried.
- **`ErrorHandler` is also reached after the response is committed.** Of its eleven call sites
  in that file, seven are in the protocol-upgrade handler
  (`net/http/httputil/reverseproxy.go:824-872`), which only runs once the backend has returned
  `101 Switching Protocols`; the last two (`:868`, `:872`) run after the `ResponseWriter` has
  been hijacked. So entry into `ErrorHandler` does not by itself imply the request is
  un-served or the response uncommitted. A mid-stream body-copy failure is *not* one of these
  cases: `copyResponse` errors panic with `http.ErrAbortHandler` and never reach `ErrorHandler`
  (`net/http/httputil/reverseproxy.go:603-614`).
- `http.Transport` *does* retry internally, but only within one target: a request is replayed
  on a fresh connection when a pooled connection dies, and only if `isReplayable`
  (`net/http/request.go:1534`) holds — no body, or an idempotent method. It never crosses to a
  different host.
- **`Request.GetBody` is nil here.** Go documents it as "For server requests, it is unused"
  (`net/http/request.go:195`). An inbound server request body is a non-rewindable stream, and
  `ErrorHandler` receives `outreq`, whose body the failed transport may already have partially
  consumed. Any cross-backend replay of a request with a body therefore needs the body buffered
  first — this constraint, not the choice of selection algorithm, is where the cost sits.
- `classifyProxyError` (`proxy/reverseproxy.go:151-168`) already separates
  `connection-refused` / `dns` / `timeout` / `eof` / `other`.

**Status page.** `rowsFor` (`local/status.go:136-151`) maps one `ServiceView` to one row,
passing `Backend` and `Caps` through and deriving `ContainerShort`, `RegisteredAgo`, `Account`,
`Cluster`. `local/status.html:86-108` renders seven columns — Service, Backend, Caps, Account,
Cluster, Container, Registered — under a `Services ({{len .Services}})` heading. There is no
grouping: today the heading counts services because registrations and services are 1:1.

**Metrics.** `metrics.Collector` (`metrics/metrics.go:24-35`) labels every proxy series by
`service` alone (plus `method`/`code`/`reason`). `Active` is an unlabelled gauge
(`metrics/metrics.go:73-76`, "Number of Tailscale Services currently registered and serving")
incremented once per successful `Register` (`proxy/manager.go:226`), decremented per
`Deregister` (`proxy/manager.go:258`), zeroed on `Close` (`proxy/manager.go:305`). Because
registrations and services are 1:1 today, the gauge is accidentally correct; that stops being
true once a service can hold several registrations. `Middleware` is applied once per service
(`proxy/manager.go:180`), so per-request series stay per-service unchanged.

**SPEC.md sections that assert the current shape.** SPEC.md:535 (Known v1 limitations —
"Replica fan-out is not supported"; note its `proxy/manager.go:86` citation is stale, line 86
is part of the doc comment on `NewManager` and the branch is at `proxy/manager.go:138`),
SPEC.md:675 (Error Handling — duplicate service names), SPEC.md:577-597 (Service Manager),
SPEC.md:599-613 (Reverse Proxy), SPEC.md:979 (Future Extensions — ECS replica fan-out).

**House patterns worth matching.** `ecs/registry.go:70` returns immutable per-entity status
snapshots (built from the `ReaderStatus` at `ecs/registry.go:11`, mutated via
`PollSucceeded`/`PollFailed` at `ecs/registry.go:103-130`) — the closest precedent for exposing
per-member state to the status page. `ecs/watcher.go:129-134` switches between a healthy and a
degraded cadence. `ecs/cache.go:97` drops a cache entry on failure so the next call retries.
`ecs/cache.go:146` with `ecs/resolve.go:54,95,116` is the sentinel "not resolvable yet, retry
next cycle" idiom.

**Tests encoding the current shape.** `TestManager_DuplicateServiceNameFirstWins`
(`proxy/manager_test.go:206-235`) asserts a one-entry snapshot and that the first container
wins. `TestManager_DuplicateContainerIDIsNoOp` (`proxy/manager_test.go:183-204`) asserts a
re-`Register` does not overwrite the backend. `TestManager_DeregisterClosesListenerAndClearsState`
(`proxy/manager_test.go:248-277`) registers one backend, deregisters it, and asserts the
listener closed and the name freed — it does not exercise several backends, so it pins
teardown-on-removal only for the single-backend case.
`TestStatusHandler_RendersServicesTable` (`local/status_test.go:213-251`) feeds two
`ServiceView`s and asserts the heading reads `Services (2)`, tying the displayed count to
`len(Snapshot())`.

## Approach Options

Two decisions are separable. Each option is tagged with what settles it.

### Axis 1 — where the backend set lives

**A1. A per-service record owning the listener, holding its backends** *(design-phase)* —
Separate the service-scoped state (listener, `http.Server`, caps, cancel) from the
backend-scoped state, so a service outlives any one of its backends and teardown is driven by
the set emptying.
*Trade-off:* touches every field of `Manager` and changes `Snapshot`'s shape, so the status
page and its tests move with it. It is the only option here that satisfies "the service stops
being advertised only when its last backend goes away" without special cases.

**A2. Keep the flat registration map; group by service name on demand** *(design-phase)* —
Leave one entry per registration and derive the set when serving or snapshotting.
*Trade-off:* smallest diff to the data model, but the listener still hangs off one arbitrary
entry, so removing that entry either tears the service down or requires transferring ownership
to a sibling. Reintroduces the failure mode being fixed.

**A3. A separate pool type inside `proxy`, with Manager delegating to it** *(design-phase)* —
The same separation as A1, with the backend set as its own independently testable type.
*Trade-off:* the set still has to be mutated under the same lock as the service index, so the
seam buys less isolation than it appears to; largely a matter of taste against A1.

### Axis 2 — which backend serves a request

Listed cheapest first. Note that B0, B1 and B2 all need the same cross-backend retry machinery;
they differ only in how the *first* candidate is chosen.

**B0. Pool with no selection logic — always the first live member** *(product-intent)* — The
service holds all its backends but there is no rotation and no counter. Requests go to a
stable member; the others exist so the service survives that member leaving and so a failed
request can be retried against them. This is the shape the card is describing when it says it
is "not super concerned about load-balancing".
*Trade-off:* meets the hard requirement with the least new state. All traffic and all
long-lived connections concentrate on one backend, and during a rolling deployment every
connection moves at once when that backend is replaced. No distribution benefit at all.

**B1. Round-robin selection with retry on connect failure** *(product-intent)* — A counter over
the backend set picks the starting member; on a connect-time transport failure, the remaining
members are tried before returning 502.
*Trade-off:* differs from B0 by a counter and its test. Spreads both requests and long-lived
connections. Makes "which backend served this?" non-obvious from outside, which matters for
debugging a service whose members are not actually interchangeable.

**B2. Single active backend, promoted on failure** *(product-intent)* — Like B0, but a
persistent failure moves the designated member rather than being retried per request.
*Trade-off:* adds failure-tracking state that B0 does not need, in exchange for not re-trying
the same dead member on every request. Sits between B0 and active health checking, and starts
to overlap the Future Extension at SPEC.md:975.

**B3. External NLB/ALB shim** *(system-fact — available today)* — SPEC.md:535 documents this:
run an internal load balancer in the ECS VPC and label a single shim task pointing at it.
*Trade-off:* no tsserve code, and it is the right answer for a genuinely high-volume service.
But it requires per-service AWS infrastructure for exactly the low-volume admin endpoints the
card is about, and it leaves the "winner disappears, service stays dark" defect in place for
every service that does not adopt it.

**B4. Multiple tsserve nodes advertising one service** *(system-fact — orthogonal)* — Tailscale
Services support several hosts advertising one service; the KB describes states in terms of
"at least one host is actively advertising", a drain operation for removing a host from
rotation, and optional in-region load balancing via Regional Routing.
*Trade-off:* this is redundancy for the *proxy*, not fan-out for the *backends* — it does not
let one tsserve node reach N tasks. The KB documents no selection algorithm, no primary/backup
model, and no timing figures for unplanned host loss, so its behaviour cannot be relied on as a
substitute for in-process retry. Relevant as complementary deployment advice.

### Recommendation

**A1 + B1**, confidence **medium**.

A1 is the only Axis-1 option that meets the listener-lifetime requirement without an
ownership-transfer special case; A3 is an acceptable variant if the design phase prefers a
separately testable type.

**On the card's explicit question — is load balancing worth the complexity?** The honest
answer is that the complexity the card is worried about is not in the load balancing. Every
Axis-2 option that satisfies the hard requirement — including B0, the do-nothing-clever option
— has the same cost profile: reshape `Manager` (A1), add cross-backend retry in `ErrorHandler`,
and decide the request-body question. Measured in surface touched, B0 and B1 are identical:
`proxy/manager.go`, `proxy/reverseproxy.go`, `local/status.go`, `local/status.html`,
`metrics/metrics.go`, SPEC.md, and their tests. B1's entire marginal cost over B0 is a counter
and a test that two successive requests hit different members. Given that, take the
round-robin — B0's concentration of every long-lived connection on one task is a real
operational cost for no saving.

Retry is required no matter which option is chosen, because a backend can be dead while still
registered: ECS discovery reacts on a poll cycle (default 10s, `TSSERVE_ECS_POLL_INTERVAL`),
and Docker discovery reacts to a `die` event, so there is always a window where the pool
contains a task that will refuse connections. The alternative to retry is closing that window
— faster deregistration via EventBridge (SPEC.md:978) or active health checks (SPEC.md:975) —
but both are separate Future Extensions, both are strictly larger than a retry, and neither
closes the window completely.

Confidence is medium because two sub-decisions the recommendation depends on are open: whether
requests carrying a body are retried at all (see Complexity and Risk Areas), and Q2's
assumption that no ejection is needed. If Q2 comes back the other way, B2 becomes the better
Axis-2 choice.

## Integration Points

- **ECS multi-reader work** (`ecs/registry.go`, `main.go:466-509`) is what makes cross-account
  pools reachable, and its per-reader `Origin` stamping is the only thing that will distinguish
  otherwise identical members on the status page.
- **Fargate launch-type support** (SPEC.md:980) shares the awsvpc resolution path and would
  arrive in pools with no extra work.
- **Health checks** (SPEC.md:975) and **EventBridge-driven updates** (SPEC.md:978) are the two
  adjacent Future Extensions whose scope this card's answer to Q2 constrains; both are weighed
  in the Recommendation.

## Complexity and Risk Areas

- **Retrying a request that has a body.** `ErrorHandler` receives `outreq` after a failed
  `RoundTrip`; `GetBody` is nil for server requests and the transport may already have read
  part of the body. Replaying against a second backend can send a truncated body, and for a
  non-idempotent method can double-apply an effect.
- **Not every `ErrorHandler` call describes an unserved request.** The seam is only
  pre-commit for `RoundTrip` failures. The upgrade paths above enter it after a backend has
  accepted a `101`, and in two cases after the connection is hijacked, so "`ErrorHandler` was
  entered" is not by itself a safe trigger for trying another backend.
- **Divergent labels across backends of one service.** `tsserve.caps` feeds `AcceptAppCaps` at
  `ListenService` time (`proxy/manager.go:148-156`) and is a listener property, so it cannot
  vary per backend at all. `tsserve.scheme` and `tsserve.port` are per-registration in the
  label vocabulary, so divergence becomes observable as inconsistent responses rather than as
  an error.
- **Long-lived connections.** `tsserve_proxy_open_websockets` exists as a metric, so hijacked
  protocol upgrades are an anticipated traffic shape. Such a connection binds to whichever
  backend served it and drops when that backend leaves.
- **Listener churn during rolling replacement.** The last old backend can deregister in the
  same window a new one registers. If teardown and setup interleave, the service advertisement
  and its TLS certificate are re-established.
- **Lock granularity.** `Manager.mu` is currently held only around map mutation and never on
  the request path. Introducing a per-request read of manager-owned state changes that.
- **The watcher-side reconciliation gap.** `ecs/watcher.go:267-271` records as active any
  registration for which `Register` returned `nil`, and there is no path back. `nil` currently
  covers three outcomes the watcher cannot tell apart: registered and serving; declined as a
  duplicate name (`proxy/manager.go:138-145`); and failed to listen
  (`proxy/manager.go:157-165`). Pooling removes the duplicate-name trigger, but the
  listen-failure one survives it unchanged (Q5).
- **`tsserve_services_active` changes meaning.** The increment site (`proxy/manager.go:226`) is
  per registration; once a service can hold several, the gauge no longer counts services.
  Scrapers and dashboards read this series.

## Scope Questions

Each carries an explicitly-labelled working assumption where one was needed to proceed. The
load-bearing ones are repeated in the review hand-off so a one-line reply resolves them.

1. **Does this card cover Docker discovery, or ECS only?** The card is framed around ECS, but
   `docker/watcher.go` drives the same `proxy.Manager`, so pooling changes Docker behaviour
   whether or not it is documented. *Working assumption:* both, documented for both — two
   Docker containers sharing a `tsserve.service` label both serve it. The Functional
   Requirements above are written on that assumption.
2. **Are failing backends ejected, or retried forever?** SPEC.md:975 keeps health checks as a
   separate Future Extension. *Working assumption:* no probing and no ejection; a backend stays
   until discovery removes it. This assumption is what makes B1 preferable to B2.
3. **What happens when backends of one service disagree on `tsserve.scheme` or `tsserve.port`?**
   Candidates: honour each per backend, first-registration-wins with a warning, or refuse the
   divergent backend. `tsserve.caps` has no choice — the listener owns it. *No working
   assumption;* this is genuinely open.
4. **What should `tsserve_services_active` count, and what should the status page show?** These
   travel together. The gauge can keep counting advertised services (needing its increment site
   moved) or switch to counting backends (a breaking change to an exported series, possibly
   alongside a new backend-count series). The status page can render N rows sharing a service
   name, or one row per service with a nested backend list — and the `Services ({{len
   .Services}})` heading (`local/status.html:86`, pinned by `local/status_test.go:245`) has to
   pick one meaning either way. *No working assumption;* both are user-visible.
5. **Is the watcher-side reconciliation gap fixed by this card, and does `Register`'s return
   contract change with it?** Pooling removes the duplicate-name trigger but not the
   listen-failure one, so afterwards a `nil` return still conflates "registered and serving"
   with "not serving, try again later" — and the watcher's short-circuit makes the latter
   permanent. Answering this decides whether `Register` keeps signalling both with `nil`; the
   repo already has a sentinel-for-retry idiom (`ecs/cache.go:146` with
   `ecs/resolve.go:54,95,116`) if it does not. *No working assumption:* the hard requirement is
   not met while a backend can be recorded as active with nothing serving it, which argues for
   in scope — but fixing it reaches past pooling into the watcher contract, which is a scope
   call rather than a technical one.
6. **New terminology sign-off.** "Backend pool" and "advertised service" need approval before
   entering code identifiers or SPEC.md. *Working assumption:* acceptable, since SPEC.md:979
   already writes "per-service backend pool".
7. **Should `Scheme` and `BackendHost` stay on `ServiceView`?** No consumer reads them today.
   Minor and non-blocking; design's call.

## References

- Tailscale Services — service hosts, service states, draining, and Regional Routing
  in-region load balancing: https://tailscale.com/kb/1552/tailscale-services
- Tailscale Services beta announcement (TailVIP, endpoint model, multiple service hosts):
  https://tailscale.com/blog/services-beta
- Tailscale Regional Routing: https://tailscale.com/blog/regional-routing
- Tailscale high availability for subnet routers and app connectors — the documented
  primary-selection and failover model for *routes*, cited here only as the adjacent mechanism
  and not as Services behaviour: https://tailscale.com/docs/how-to/set-up-high-availability
- `tsnet.Server.ListenService` and `tsnet.ServiceModeHTTP` (`AcceptAppCaps`, `Port`,
  `PROXYProtocol`): https://pkg.go.dev/tailscale.com/tsnet
- Go `net/http/httputil.ReverseProxy` (`Rewrite`, `ErrorHandler`, no cross-target retry):
  https://pkg.go.dev/net/http/httputil#ReverseProxy
- Go `net/http.Request.GetBody` ("For server requests, it is unused"):
  https://pkg.go.dev/net/http#Request
