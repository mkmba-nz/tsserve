# Multiple Backends Per Service

Tracking Issue: FZ-2399

Research: [RESEARCH.md](RESEARCH.md). Approved: A1 (a per-service record owns the listener and
holds its backend pool) + B1 (round-robin with retry on connection failure). The card owner's
answers 1A, 2A, 3C, 4A, 5A, 6A, 7A refer to the numbered questions in the research hand-off
comment on the card (Docker scope, failing backends, divergent labels, metrics and status page,
watcher reconciliation gap, terminology, retry with body). That numbering differs from
RESEARCH.md's Scope Questions; "hand-off Qn" below always means the card numbering.

## Introduction

tsserve advertises each Tailscale Service from exactly one discovered backend. A second
container or task carrying the same `tsserve.service` label is logged as a duplicate and
dropped, and when the winning backend disappears the service goes dark until the loser is
itself replaced (RESEARCH.md, Problem Statement).

This feature separates *advertising a service* from *registering a backend*. An advertised
service holds a backend pool; requests are served round-robin across the pool and, for
body-less requests, retried against another member when a connection cannot be established.
The service stays advertised while at least one backend remains.

## Terminology

Carried forward from RESEARCH.md (hand-off Q6 approved). These terms are used verbatim in the
stories below and are the only sanctioned names for code identifiers, log lines, and SPEC.md.

| Term | Meaning |
|---|---|
| **Backend** | One discovered endpoint — host and port — that can serve a service. It is reached using the advertised service's scheme. |
| **Registration key** | The watcher-supplied identifier for one backend: a Docker container ID, or `taskArn#containerName` in ECS. |
| **Advertised service** | A Tailscale Service for which tsserve holds an open `ListenService` listener. Its scheme and caps are properties of the service, not of any backend. |
| **Backend pool** | The ordered set (registration order) of backends currently registered against one advertised service. |
| **Origin** | Which discovery source found a backend (ECS account / cluster / region; zero for Docker). |

"First wins" and "duplicate service name" are retired: a second registration for a service
name is a second pool member, not a duplicate.

## Goals

1. A body-less request to an advertised service reaches a live backend whenever at least one
   member of its backend pool accepts connections; a request that carries a body reaches one
   whenever the member selected for it accepts connections (US-001, US-002).
2. Every backend carrying a `tsserve.service` label whose scheme and caps agree with the
   advertised service serves that service regardless of provenance — several tasks of one ECS
   service, tasks of different ECS services, backends from different readers, and several
   Docker containers (US-001, US-003, US-007).
3. Adding or removing a backend never tears down and re-opens the service listener while other
   members remain (US-001).
4. A backend is never recorded as active by a watcher while nothing is serving it (US-004).
5. Divergent labels across pool members have defined, logged behaviour (US-003).
6. Operators can see every backend of a service, with its Origin, on the status page and can
   count services and backends from metrics (US-005, US-006).
7. SPEC.md describes the pooled behaviour for both discovery modes and no longer lists replica
   fan-out as unsupported (US-007).

## User Stories

Stories are ordered by dependency. Each story's ACs are the authoritative specification of its
behaviour; later sections refer to them rather than restating them.

### US-001: An advertised service holds a backend pool

**Description:** As an operator running replicas of a labelled task, I want every replica
registered against the one advertised service so that the service survives any single replica
leaving.

**Acceptance Criteria:**

- [ ] Registering a backend for a service name that is already advertised adds it to that
      service's backend pool. `ListenService` is called once per advertised service, not once
      per backend, and the "duplicate service name; ignoring" outcome (`proxy/manager.go:138-145`)
      no longer exists. `proxy.Manager` keeps one per-service record per advertised service,
      owning the listener, the `http.Server`, the reverse proxy, and the pool; the `byName`
      winner map is gone.
- [ ] Backends join the same pool regardless of where they came from: distinct registration
      keys from one ECS service, from different ECS services, from different readers (different
      `Origin`), and from Docker container IDs are all pool members of equal standing.
- [ ] Re-registering an existing registration key remains a no-op that returns `nil`
      (`proxy/manager.go:134-137`).
- [ ] The advertised service's scheme and caps are fixed by the registration that opened its
      listener (`AcceptAppCaps` is a listener property, `proxy/manager.go:148-156`). Handling of
      later registrations that disagree is US-003.
- [ ] Deregistering one backend removes only that backend from the pool. The listener, the
      `http.Server`, and in-flight requests to other members are untouched. **Trap:** today
      `Deregister` closes the `http.Server` (`proxy/manager.go:217-219`); that close belongs to
      service teardown only.
- [ ] Deregistering the last backend tears the service down exactly as today's single-backend
      path does (cancel, close server, close listener), and the service name is free for a fresh
      registration afterwards.
- [ ] `Close` tears each advertised service down once, not once per backend.
- [ ] The decision "pool is now empty, tear down" and the removal of the record from the
      service index happen atomically under the manager lock. **Trap (multi-reader):** two ECS
      readers can `Register` and `Deregister` the same service name concurrently. A `Register`
      that arrives while the last member's teardown is in progress must end with a backend in
      the pool of an open listener — either it joins before the record leaves the index or it
      creates a fresh record afterwards. It must never be added to a record whose listener is
      being closed.
- [ ] **Trap (multi-reader):** two concurrent `Register` calls for a service name that is not
      yet advertised produce one listener, with the second registration joining the first's
      pool. Today `ListenService` runs outside the lock with no in-flight marker
      (`proxy/manager.go:146-156`), so a naive port of that code opens two listeners for one
      name. The manager needs a per-name "advertising in progress" state (or equivalent) that
      the second caller waits on or joins.
- [ ] Serving a request never waits for a `ListenService` call or a listener teardown in
      progress for any service. A brief wait on the lock that guards pool mutation is
      acceptable; a wait that spans the `ListenService` network call
      (`proxy/manager.go:146-156`) is not.
- [ ] A request whose handler is already running when the last pool member leaves (the handler
      passed `Accept` before `Server.Close` ran, and `Server.Close` does not wait for running
      handlers) sees an empty pool and receives a 502. **Trap:** a naive `pool[counter % len]`
      divides by zero here.
- [ ] `Snapshot` returns one `ServiceView` per advertised service, sorted by service name,
      carrying the service-level fields (`Service`, `Scheme`, `Caps`) and a list of backend
      views in pool order, each with the `Backend` address string (`scheme://ip:port`),
      registration key, `RegisteredAt`, and `Origin`. `BackendHost` and `Port` have no consumer
      (RESEARCH.md, Existing Data Models) and are dropped.
- [ ] Log lines distinguish advertising a service (first backend, listener opened) from a
      backend joining or leaving an existing pool, and pool-change lines include the pool size
      after the change.
- [ ] The implementation agent replaces, rather than deletes, the tests pinned to the old shape:
      `TestManager_DuplicateServiceNameFirstWins` (`proxy/manager_test.go:206-235`) becomes a
      proof that a second key for the same name produces one listener and a two-member pool;
      `TestManager_DeregisterClosesListenerAndClearsState` (`proxy/manager_test.go:248-277`)
      keeps its single-backend proof and gains a companion proving that removing one of two
      backends leaves the listener open and removing the second closes it.
      `TestManager_DuplicateContainerIDIsNoOp` (`proxy/manager_test.go:183-204`) concerns
      duplicate *keys* and stays valid. The two multi-reader traps above are each proven with
      real goroutines racing `Register` against `Deregister` (and `Register` against `Register`)
      on one service name.

### US-002: Requests are served round-robin with retry on connection failure

**Description:** As a client of an advertised service, I want my request to reach any live
backend so that a replica that has died but not yet been discovered as gone does not fail my
request.

**Acceptance Criteria:**

- [ ] Each advertised service has exactly one reverse proxy and one transport, built when the
      service is first advertised (`newReverseProxy`, `proxy/reverseproxy.go:49-92`, no longer
      closes over a single fixed target). Backend selection happens per request inside it; there
      is no proxy per backend and no proxy per request.
- [ ] Successive requests to a service with N pool members are distributed round-robin in pool
      order: over N consecutive successful requests, each member receives exactly one. The
      rotation position advances once per request, not once per attempt; a retried request
      walks the pool from the position it was assigned.
- [ ] Pool changes take effect on the next request without rebuilding the proxy: a backend added
      to the pool starts receiving requests; a backend removed from it receives no further
      requests.
- [ ] **Retry rule.** The service's transport (the component that performs `RoundTrip`)
      attempts the request against the remaining pool members in rotation order, at most once
      per member, when *both* hold: the attempt failed before the request was written to the
      backend connection (dial refused, dial timeout, host unreachable / no route, name
      resolution failure, TLS handshake failure), and the request carries no body. The client
      receives the first successful response. In every other case — a failure after the request
      was written, or a request that carries a body — there is no retry and the client receives
      a 502 as today. The retry loop also stops as soon as the client's request context is
      done.
- [ ] When every attempt fails, the client receives a 502 carrying the last error, and the
      "backend proxy error" log line for the 502 names the last backend tried (today it names
      the fixed target, `proxy/reverseproxy.go:85`).
- [ ] An attempt against a member whose host does not answer fails within a bounded dial
      timeout of a few seconds (5s suggested), the same for `http` and `https` services.
      **Trap:** the `http` path uses `http.DefaultTransport` today (`rp.Transport` is left nil,
      `proxy/reverseproxy.go:78-82`), whose dial timeout is 30s, and the `https` transport sets
      none at all — a blackholed member (terminated host) would otherwise cost the client 30s or
      more per attempt before the retry happens.
- [ ] **Trap:** the retry must be performed at a point where nothing has yet been written to the
      client. `ErrorHandler` is not that point: it is also reached after a backend has returned
      `101 Switching Protocols` and, in two cases, after the client connection has been hijacked
      (RESEARCH.md, Request path). The transport `RoundTrip` boundary is such a point — a
      `RoundTrip` that returns an error has written nothing to the client.
- [ ] **Trap:** "carries no body" means the outbound request's body is `nil`, which
      `httputil.ReverseProxy` sets whenever the inbound `ContentLength` is zero (Go 1.26
      `net/http/httputil/reverseproxy.go:436-437`); checking for `http.NoBody` alone misses
      this. `Request.GetBody` is `nil` for server requests, so a body can never be rewound.
- [ ] **Trap:** the outbound URL host and the outbound `Host` header (`pr.Out.Host`,
      `proxy/reverseproxy.go:70`) name the backend that actually receives each attempt,
      including retried attempts — not the first candidate. The `RoundTripper` contract forbids
      modifying the request it is given, so each attempt uses its own clone of the outbound
      request.
- [ ] **Trap:** whether an attempt failed before the request was written is decided from the
      error's type and phase, not from `classifyProxyError`'s string matching
      (`proxy/reverseproxy.go:151-168`), which cannot tell a dial timeout from a read timeout
      after the request was sent. `classifyProxyError` keeps its role of labelling the metric.
- [ ] `tsserve_proxy_backend_errors_total` increments exactly once per failed attempt, with the
      attempt's classified reason, and each attempt's "backend proxy error" log line names the
      backend it targeted. **Trap:** `ErrorHandler` also increments this counter today
      (`proxy/reverseproxy.go:84-90`); when all attempts fail, the last attempt must not be
      counted a second time by `ErrorHandler`, while errors that never pass through `RoundTrip`
      (the protocol-upgrade paths) are still counted once. A request that succeeds on a later
      attempt is recorded in `tsserve_proxy_requests_total` with its actual response code, not
      as a 502.
- [ ] Round-robin selection is safe under concurrent requests from several goroutines.
- [ ] A hijacked (protocol-upgrade) connection stays bound to the backend that accepted it for
      its lifetime. Removing that backend from the pool does not actively close the connection;
      it ends when the backend closes it.
- [ ] For `tsserve.scheme=https` services the transport still skips TLS verification
      (`proxy/reverseproxy.go:78-82`) and is shared by all pool members.
- [ ] Tests prove: two live backends alternate over four requests; a pool of {closed port, live
      backend} serves a body-less GET with a 200 and one backend error; the same pool returns a
      502 for a POST with a body without contacting the second backend; a backend that returns a
      500 is not retried; a backend added after the proxy was built receives requests; requests
      issued concurrently from several goroutines are all served.

### US-003: Backends that conflict with the advertised service are refused

**Description:** As an operator, I want a backend whose labels disagree with the service that is
already advertised to be refused and logged, so that pool members are interchangeable and
misconfiguration is visible rather than silent.

**Acceptance Criteria:**

- [ ] The manager refuses a registration whose `tsserve.scheme` or `tsserve.caps` differs from
      the advertised service's values: it is not added to the pool, it is not recorded anywhere
      in the manager, existing members keep serving, and `Register` returns an error a caller can
      distinguish from a listen failure (US-004). The manager compares caps as sets.
- [ ] The returned error carries the service, the refused registration key, the field that
      differs, and both values. The watcher that called `Register` logs it; US-004 governs the
      level and repetition.
- [ ] **Trap:** the backend port is *not* compared. In ECS bridge mode each task is assigned a
      dynamic host port and the manager receives the resolved host port, not the labelled
      `tsserve.port` (`ecs/watcher.go:262-265`); comparing ports would refuse exactly the
      replicas this feature exists for. The port is part of the backend address, not a service
      property. This narrows hand-off answer 3C, which named `tsserve.port`; Open Questions 1
      asks the card owner to confirm the narrowing.
- [ ] Once the pool empties and the service is torn down, the next registration for that name —
      including a previously refused one — advertises the service with its own scheme and caps.
- [ ] Refused backends do not appear in `Snapshot` or on the status page; the watcher's log
      line is their only surface.

### US-004: A watcher only records a backend as active when it is serving

**Description:** As an operator, I want a backend that could not be put into service to be
retried by discovery instead of being silently recorded as active, so that a transient listener
failure or a resolved conflict does not leave a backend permanently dark.

**Acceptance Criteria:**

- [ ] `Register` returns `nil` only when the backend is a member of the pool of a service with an
      open listener. A non-fatal `ListenService` failure returns an error instead of logging and
      returning `nil` (`proxy/manager.go:157-165`); a refused registration returns the US-003
      error; `FatalError` behaviour is unchanged and `registrarAdapter` (`main.go`) still
      propagates it to the fatal channel and returns every other error to the watcher unchanged.
      `TestManager_RegisterListenServiceErrorIsSkippedNotFatal` (`proxy/manager_test.go:279`)
      pins the old `nil` return and is rewritten to prove the new contract.
- [ ] The ECS watcher continues not to record a registration key in `active` when `Register`
      returned an error (`ecs/watcher.go:267-271`), so the `prev == backendAddr` short-circuit
      (`ecs/watcher.go:255-257`) cannot strand it, and the key is retried on every subsequent
      cycle while the task is still seen. A key that succeeds on a later cycle joins the pool.
- [ ] A registration that keeps failing or being refused on consecutive ECS cycles produces one
      Warn-level log line when it first fails and no further Warn- or Error-level lines while the
      same outcome persists. The existing per-cycle `Register failed` Error line
      (`ecs/watcher.go:267-270`) is what would otherwise fire every poll interval. **Trap:** the
      per-key state this needs is dropped when the key succeeds or leaves the `seen` set;
      otherwise it leaks one entry per task that ever failed.
- [ ] Docker mode keeps its event-driven contract: a failed or refused `Register` is logged at
      Warn (`docker/watcher.go:138-141`, Error today) and retried on the container's next start
      event. No reconciliation loop is added (see Non-Goals).
- [ ] Tests prove: an ECS key whose `Register` fails is absent from `active` and is registered
      again on the next cycle; the same key failing twice logs at Warn once; a key that fails
      then succeeds is in `active` after the successful cycle.

### US-005: The status page lists every backend of a service

**Description:** As an operator, I want to see every backend behind a service, with its account
and cluster, so that I can tell which replicas are registered and where they came from.

**Acceptance Criteria:**

- [ ] The `Services (N)` heading counts advertised services (`local/status.html:86`).
- [ ] Each advertised service occupies one group of table rows: one row per pool member in pool
      order, with the Service and Caps cells spanning the group (`rowspan`), and the per-backend
      cells — Backend, Account, Cluster, Container, Registered — as ordinary table cells. This
      is the "one row per service with a nested backend list" layout of hand-off answer 4A,
      expressed so that alignment does not depend on cell content width (a long registration key
      or a wrapped address cannot mis-align a backend's entries). A service with one backend
      renders as it does today.
- [ ] Account and Cluster are correct per backend when pool members have different `Origin`
      values; a zero `Origin` (Docker) renders as "—" as today.
- [ ] `rowsFor` (`local/status.go:136-151`) maps the new `ServiceView` shape; `serviceRow`
      carries a backend list rather than a single backend.
- [ ] Tests prove: two services, one with one backend and one with two backends from different
      readers, render a `Services (2)` heading, all three backend addresses, and both readers'
      accounts. `TestStatusHandler_RendersServicesTable` (`local/status_test.go:213-251`),
      `TestRowsFor_MapsServiceFields` (`local/status_test.go:69`) and `TestRowsFor_IncludesOrigin`
      (`local/status_test.go:98`) read the old `ServiceView` shape and are the tests to extend.
- [ ] Verify in browser: render the page with a multi-backend snapshot and confirm the grouped
      rows read correctly in light and dark schemes and the single-backend case looks as it
      does today.

### US-006: Metrics count services and backends separately

**Description:** As an operator, I want `tsserve_services_active` to keep meaning "advertised
services" and a separate series for pool size, so that dashboards built on the existing gauge
stay correct.

**Acceptance Criteria:**

- [ ] `tsserve_services_active` counts advertised services: it increments when a service's
      listener opens, decrements when the service is torn down, is unchanged by backends joining
      or leaving an existing pool, and is zero after `Close`. Today's increment and decrement
      sites (`proxy/manager.go:226`, `proxy/manager.go:258`) are per registration and move.
- [ ] A new gauge `tsserve_service_backends`, labelled by `service`, reports the pool size of
      each advertised service and is updated on every join and leave.
- [ ] **Trap:** the manager deletes the `tsserve_service_backends` series for a service when the
      service is torn down and clears all of them on `Close`; otherwise the exposition keeps a
      stale value for services that no longer exist.
- [ ] The help text of `tsserve_proxy_backend_errors_total` reflects the per-attempt semantics
      defined in US-002.
- [ ] Tests prove: registering two backends for one service leaves `tsserve_services_active` at
      1 and `tsserve_service_backends{service}` at 2; deregistering both returns the former to 0
      and removes the latter's series.

### US-007: SPEC.md documents pooled behaviour for both discovery modes

**Description:** As a reader of SPEC.md, I want the document to describe the behaviour tsserve
actually has, so that I do not plan around a limitation that no longer exists.

**Acceptance Criteria:**

- [ ] The "Replica fan-out is not supported" entry is removed from Known v1 limitations
      (`SPEC.md:535`), including its stale `proxy/manager.go:86` reference, and the "ECS replica
      fan-out" bullet is removed from Future Extensions (`SPEC.md:979`).
- [ ] The Service Manager section (`SPEC.md:577-597`) describes the advertised service, its
      backend pool, teardown on last removal, the conflict rule (US-003) and the `Register`
      return contract (US-004) in its Responsibilities list (`SPEC.md:581-592`); the Reverse
      Proxy section (`SPEC.md:599-613`) states the round-robin and retry rule from US-002,
      including the dial timeout.
- [ ] The Error Handling table row for duplicate `tsserve.service` (`SPEC.md:675`) is replaced
      by one stating that every container or task carrying the label joins the pool, and the
      "Backend unreachable" row (`SPEC.md:666`) describes the retry rule. A new row states that
      when the last member leaves before its replacement is discovered, the service is torn down
      and re-advertised (Non-Goals, listener grace period).
- [ ] The Docker Watcher section states that two Docker containers sharing a `tsserve.service`
      label both serve it (hand-off answer 1A) and that a refused or failed container is retried
      only on its next start event, and the ECS section states pooling for tasks of one or
      several ECS services and for tasks found by different readers.
- [ ] The metrics table (`SPEC.md:816-817`) reflects US-006, including the new
      `tsserve_service_backends` series.
- [ ] No occurrence of "first wins", "first one wins", "first task wins", or "duplicate service
      name" remains in SPEC.md or in non-test code. **Carve-out:** duplicate *registration keys*
      remain a documented no-op and may still be described as such.

## Non-Goals

- Active health checks, probing, or passive ejection of failing backends (hand-off answer 2A).
  A backend stays in the pool until discovery removes it; per-request retry covers the gap.
- Weighted, least-connections, or latency-aware selection.
- Retrying requests that carry a body, or retrying after the request was written to a backend
  (US-002 retry rule). The card owner's position on hand-off Q7 is that clients needing
  reliability for such requests retry themselves.
- A grace period that holds the listener open after the last pool member leaves. If the last
  old backend leaves before its replacement is discovered — a single-task ECS service being
  replaced, or a rolling deployment that drains to zero — the service is torn down and
  re-advertised, re-provisioning its certificate. Within one ECS poll cycle the watcher
  registers new tasks before it deregisters missing ones (`ecs/watcher.go:208-212`), so a
  replacement that is already RUNNING joins the pool before the old member leaves.
- A reconciliation loop in Docker mode for transient `ListenService` failures or refused
  conflicts; Docker mode stays event-driven (US-004). Consequence: a Docker container refused
  for a conflict does not join the pool when the conflicting member later dies; it joins on its
  own next start event.
- Showing refused backends on the status page (US-003).
- Per-backend `tsserve.scheme`; scheme is a property of the advertised service (US-003).
- Multiple services per container, EventBridge-driven ECS updates, Fargate support, and
  multiple tsserve nodes advertising one service (RESEARCH.md B4) — separate Future Extensions.
- Draining a backend gracefully before removal; in-flight requests to a leaving backend complete
  or fail on their own.

## Design Considerations

The only UI surface is the status page (US-005). The grouped-rows layout was chosen (hand-off
answer 4A) so that the `Services (N)` heading keeps counting services and so a single-backend
service renders exactly as it does today. The project defines no UI-prototype convention; no
prototype is attached.

## Technical Considerations

**Retry decision (hand-off Q7).** The card allowed "no retries at all" if safe retry adds
meaningful complexity. Retries are in scope, restricted to the US-002 retry rule. Rationale:
performed at the `RoundTrip` boundary the retry is pre-commit by construction (nothing has been
written to the client when `RoundTrip` returns an error), and a failure before the request was
written means nothing can be double-applied. The mechanism is a loop over the pool inside one
round-tripper — a few dozen lines with no new state beyond the round-robin position — and it is
testable with one closed port and one `httptest` backend. Without it, every request that lands
on a dead-but-not-yet-deregistered member fails for up to one ECS poll interval (default 10s)
or until Docker's `die` event, which is exactly the window the card's hard requirement is
about. `http.Transport`'s own replay (`isReplayable`) is unaffected: it only re-sends over a
fresh connection to the *same* host after a pooled connection dies, and never fires on a dial
failure.

**Phasing.** US-001 changes the `Snapshot` contract that `local/status.go` consumes, so the
implementation agent lands US-001 and US-005 in one commit (or keeps a compatible `Snapshot`
shape until US-005). US-002 and US-003 depend only on US-001; US-004 depends on US-003's error;
US-006 depends on US-001. The implementation agent writes US-007 last, once the observed
behaviour is fixed, and may deliver it alongside the code stories in one PR.

**Read path.** Discovery writes the pool at most once per task or container change; the request
path reads it on every request. US-001's lock AC is the binding constraint; a copy-on-write pool
snapshot or a read lock that `Register` does not hold across `ListenService` both satisfy it.

**Integration points.** Multi-reader ECS is what makes cross-account pools reachable; each
reader's `registrarAdapter` stamps its own `Origin` (`main.go`, per-reader `reg.origin`) and that
is the only thing distinguishing otherwise identical pool members on the status page. The
`Registrar` interfaces in `docker/watcher.go:22-25` and `ecs/watcher.go:23-26` do not change
shape; only the meaning of a non-nil `Register` error does (US-004).

**Testing strategy.** The project has no `docs/standards/`; follow the existing test
conventions. Manager behaviour is proven with the in-memory listener factory and `pipeListener`
in `proxy/manager_test.go`; request-path behaviour with `httptest` backends as in
`proxy/reverseproxy_test.go`; watcher behaviour with `recordingReg` and the fake ECS API in
`ecs/watcher_test.go`; status rendering with `newTestServer` in `local/status_test.go`.
Concurrency ACs (US-001 multi-reader traps, US-002 concurrent requests) are proven with real
goroutines, not mocks; the Makefile's `test` target is plain `go test ./...`, so the
implementation agent also runs the `proxy` package under `go test -race` while developing.
Feature-specific coverage each story must prove is listed in that story's ACs.

## Success Metrics

- An ECS service with `desiredCount: 2` produces one advertised service with two pool members
  on the status page, and consecutive body-less requests alternate between them.
- Stopping one task leaves the service advertised; body-less requests keep succeeding
  throughout the discovery window; `tsserve_services_active` stays at 1 and
  `tsserve_service_backends` drops to 1 when discovery catches up.
- Two Docker containers sharing a `tsserve.service` label both receive traffic.
- `go test ./...` and `go vet ./...` pass.

## Open Questions

1. **Port divergence.** Hand-off answer 3C ("refuse the divergent backend") was given for
   `tsserve.scheme` and `tsserve.port`. US-003 refuses on scheme and caps only, because the port
   the manager sees in ECS bridge mode is the dynamic host port, so a port comparison would
   refuse legitimate replicas. Confirm this reading, or ask for the labelled `tsserve.port` to
   be carried through to the manager and compared separately.
