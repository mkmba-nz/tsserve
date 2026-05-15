# tsserve — Specification

A single-binary Docker-to-Tailscale-Services proxy built on `tsnet`.

## Overview

`tsserve` watches Docker for labeled containers and exposes them as **Tailscale Services** (not machines) using the `tsnet` Go library's `ListenService` API. It runs as a single container or binary, registers once on the tailnet as a single node, and reverse-proxies traffic to backend containers. When containers start or stop, it creates or tears down the corresponding service listeners automatically.

## Goals

- **Single binary/container** — no external `tailscaled` daemon required. `tsnet` is compiled in.
- **Tailscale Services, not machines** — uses `tsnet.Server.ListenService`, so the tailnet device list stays clean. Each backend gets its own `svc:` entry, not a machine.
- **HTTPS by default** — all services are exposed over HTTPS with automatic TLS certificates from Let's Encrypt via Tailscale's built-in cert provisioning. Clients access services at `https://servicename.tailnet.ts.net`. This requires MagicDNS and HTTPS to be enabled in the Tailscale admin console.
- **Minimal scope** — Docker label discovery, reverse proxying, lifecycle management. Nothing else.

## Non-goals

- TOML/YAML config file provider (Docker labels only).
- Headscale support (Tailscale Services require the Tailscale control plane).
- Tailscale Funnel support.
- Automatic service definition creation via the Tailscale API (users pre-create services in the admin console or configure ACL auto-approvers).
- TCP/UDP proxying (HTTP/HTTPS only for v1).
- Metrics, tracing, or observability endpoints.

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                    Docker Host                      │
│                                                     │
│  ┌───────────────────────────────────────────────┐  │
│  │              tsserve (single binary)           │  │
│  │                                               │  │
│  │  ┌─────────────┐    ┌──────────────────────┐  │  │
│  │  │ Docker      │    │ tsnet.Server          │  │  │
│  │  │ Watcher     │    │ (single tailnet node) │  │  │
│  │  │             │    │                       │  │  │
│  │  │ Monitors    │───▶│ ListenService("svc:a")│──┼──┼──▶ tailnet
│  │  │ container   │    │ ListenService("svc:b")│──┼──┼──▶ tailnet
│  │  │ start/stop  │    │ ListenService("svc:c")│──┼──┼──▶ tailnet
│  │  └──────┬──────┘    └──────────┬───────────┘  │  │
│  │         │                      │              │  │
│  │         │         ┌────────────┘              │  │
│  │         │         │  httputil.ReverseProxy    │  │
│  │         │         │  per service              │  │
│  └─────────┼─────────┼──────────────────────────┘  │
│            │         │                              │
│            ▼         ▼                              │
│  ┌──────┐ ┌──────┐ ┌──────┐                        │
│  │ :80  │ │ :3000│ │ :8080│  (container IPs)       │
│  │ nginx│ │ app  │ │ api  │                        │
│  └──────┘ └──────┘ └──────┘                        │
└─────────────────────────────────────────────────────┘
```

A single `tsnet.Server` instance registers on the tailnet. For each discovered container, `ListenService` is called to advertise a service, and a Go `httputil.ReverseProxy` handles the actual traffic forwarding to the container's IP on the Docker bridge network.

---

## Docker Labels

All labels are prefixed with `tsserve.`.

### Required

| Label | Description |
|---|---|
| `tsserve.enable` | Set to `"true"` to enable. |
| `tsserve.service` | The Tailscale service name, e.g. `svc:myapp`. Must match a service defined in the Tailscale admin console. |
| `tsserve.port` | The container port to proxy to (the port the app listens on inside the container). |

### Optional

| Label | Default | Description |
|---|---|---|
| `tsserve.network` | `bridge` | Docker network to use when resolving the container's IP address. |
| `tsserve.scheme` | `http` | Scheme to use when connecting to the backend: `http` or `https`. Use `https` if the backend terminates TLS itself (rare — most containers serve plain HTTP internally). |
| `tsserve.caps` | _(none)_ | App capabilities to accept and forward. Comma-separated list of capability names, mounted at `/`. See [App Capabilities](#app-capabilities). |

The Tailscale-facing side is always HTTPS on port 443 with automatic TLS certificates. This is not configurable — it is the only sensible default for Tailscale Services and matches how `tsnet.ListenService` with `ServiceModeHTTP{HTTPS: true, Port: 443}` works.

### Example

```yaml
services:
  tsserve:
    image: ghcr.io/you/tsserve:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - tsserve-state:/var/lib/tsserve
    environment:
      - TS_AUTHKEY=${TS_AUTHKEY}  # For local dev. Use TS_CLIENT_ID + TS_AUDIENCE for ECS.

  nginx:
    image: nginx:latest
    labels:
      - "tsserve.enable=true"
      - "tsserve.service=svc:web"
      - "tsserve.port=80"

  api:
    image: myapi:latest
    labels:
      - "tsserve.enable=true"
      - "tsserve.service=svc:api"
      - "tsserve.port=3000"
      - "tsserve.caps=example.com/cap/read,example.com/cap/admin"

  metrics:
    image: prometheus:latest
    labels:
      - "tsserve.enable=true"
      - "tsserve.service=svc:metrics"
      - "tsserve.port=9090"

volumes:
  tsserve-state:
```

After `docker compose up`, `svc:web`, `svc:api`, and `svc:metrics` are reachable on the tailnet at their respective service FQDNs.

### App Capabilities

Tailscale Services support [app capabilities](https://tailscale.com/kb/1537/grants-app-capabilities), which allow ACL grants to attach arbitrary capability metadata to a connection. When a client connects to a service, the capabilities granted to that client (by ACL policy) are forwarded as headers to the backend.

To opt in, set `tsserve.caps` to a comma-separated list of capability names:

```yaml
labels:
  - "tsserve.enable=true"
  - "tsserve.service=svc:api"
  - "tsserve.port=3000"
  - "tsserve.caps=example.com/cap/read,example.com/cap/admin"
```

This configures the `ServiceModeHTTP.AcceptAppCaps` field with all listed capabilities mounted at `/` (the root path):

```go
tsnet.ServiceModeHTTP{
    HTTPS: true,
    Port:  443,
    AcceptAppCaps: map[string][]string{
        "/": {"example.com/cap/read", "example.com/cap/admin"},
    },
}
```

The Tailscale proxy layer then forwards the granted capabilities to the backend as headers. The backend can inspect these to make authorization decisions. See the [Tailscale app capabilities documentation](https://tailscale.com/kb/1537/grants-app-capabilities) for details on the header format and how to define grants in ACL policy.

If `tsserve.caps` is not set, `AcceptAppCaps` is left nil and no capability headers are forwarded.

---

## Configuration

tsserve is configured entirely via environment variables. There is no config file.

### Authentication

tsserve supports three authentication modes. The first matching mode wins, checked in this order:

**1. Workload Identity Federation (recommended for ECS / cloud)** — Zero static credentials. tsserve exchanges a cloud-provider OIDC token for a short-lived Tailscale auth key at startup. Set `TS_CLIENT_ID` and either `TS_AUDIENCE` (for automatic token discovery on supported platforms like AWS ECS) or `TS_ID_TOKEN` (to supply a token directly). `TSSERVE_TAGS` is required.

**2. OAuth Client** — Set `TS_CLIENT_ID` and `TS_CLIENT_SECRET`. Useful for long-lived infrastructure where OIDC isn't available. `TSSERVE_TAGS` is required.

**3. Auth Key (local dev / testing)** — Set `TS_AUTHKEY` to a Tailscale auth key (`tskey-auth-...`). The key must be tagged. Simplest option for local Docker Compose testing.

### Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| **Authentication** | | | |
| `TS_AUTHKEY` | ✱ | — | Tailscale auth key. Must be for a tagged node. |
| `TS_CLIENT_ID` | ✱ | — | Client ID for OAuth or workload identity federation. Obtain from the Tailscale admin console (OAuth clients or federated identities). |
| `TS_CLIENT_SECRET` | ✱ | — | OAuth client secret. Used with `TS_CLIENT_ID` for OAuth auth. |
| `TS_ID_TOKEN` | ✱ | — | OIDC token for workload identity federation. Used with `TS_CLIENT_ID`. Mutually exclusive with `TS_AUDIENCE`. |
| `TS_AUDIENCE` | ✱ | — | Audience for automatic OIDC token discovery (e.g. on AWS ECS, GCP, GitHub Actions). Used with `TS_CLIENT_ID`. Mutually exclusive with `TS_ID_TOKEN`. |
| `TSSERVE_TAGS` | ✱✱ | — | Comma-separated ACL tags to advertise (e.g. `tag:tsserve`). **Required** for OAuth and OIDC auth. Maps to `tsnet.Server.AdvertiseTags`. Not needed if using `TS_AUTHKEY` with a pre-tagged key. |
| **General** | | | |
| `TSSERVE_HOSTNAME` | No | `tsserve` | The hostname for the tsnet node on the tailnet. |
| `TSSERVE_STATE_DIR` | No | `/var/lib/tsserve` | Directory for tsnet state persistence across restarts. |
| `TSSERVE_LOG_LEVEL` | No | `info` | Log level: `debug`, `info`, `warn`, `error`. |

✱ One authentication method must be provided. ✱✱ Required for OAuth and OIDC modes.

### ECS Deployment Example

For AWS ECS with workload identity federation:

1. In the Tailscale admin console, create a **federated identity** trusting your AWS OIDC issuer (the ECS cluster's identity endpoint). Configure the subject claim to match your ECS task role ARN. Grant `auth_keys` write scope and assign `tag:tsserve`.
2. Copy the **Client ID** and **Audience** from the federated identity configuration.
3. Configure your ECS task definition:

```json
{
  "environment": [
    {"name": "TS_CLIENT_ID", "value": "<client-id-from-step-2>"},
    {"name": "TS_AUDIENCE", "value": "<audience-from-step-2>"},
    {"name": "TSSERVE_TAGS", "value": "tag:tsserve"}
  ]
}
```

No secrets to store or rotate. The ECS task's IAM role identity is exchanged for a short-lived Tailscale auth key at startup.

### Local Development Example

For local Docker Compose testing with an auth key:

```yaml
services:
  tsserve:
    image: ghcr.io/you/tsserve:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - tsserve-state:/var/lib/tsserve
    environment:
      - TS_AUTHKEY=${TS_AUTHKEY}  # tskey-auth-... tagged with tag:tsserve
```

### How Authentication Maps to tsnet.Server

The startup code maps environment variables to `tsnet.Server` fields:

```go
srv := &tsnet.Server{
    Hostname:      os.Getenv("TSSERVE_HOSTNAME"),
    Dir:           os.Getenv("TSSERVE_STATE_DIR"),
    AuthKey:       os.Getenv("TS_AUTHKEY"),
    ClientID:      os.Getenv("TS_CLIENT_ID"),
    ClientSecret:  os.Getenv("TS_CLIENT_SECRET"),
    IDToken:       os.Getenv("TS_ID_TOKEN"),
    Audience:      os.Getenv("TS_AUDIENCE"),
    AdvertiseTags: splitTags(os.Getenv("TSSERVE_TAGS")),
}
```

tsnet handles the token exchange, key generation, and node registration internally. tsserve does not need to implement any OIDC logic.

---

## Core Components

### 1. Docker Watcher

Connects to the Docker daemon via the Docker socket and monitors container lifecycle events.

**Responsibilities:**
- On startup, list all running containers and register any with valid `tsserve.*` labels.
- Subscribe to Docker events. React to `start`, `die`, and `stop` events.
- For each qualifying container, resolve its IP on the configured Docker network.
- Pass discovered service definitions to the Service Manager.

**Implementation notes:**
- Use the `github.com/docker/docker/client` Go SDK.
- Filter events to container types only.
- When a container starts, wait briefly (e.g. 1s) for its network to be ready before resolving its IP.

### 2. Service Manager

Owns the `tsnet.Server` instance and manages the mapping from Tailscale services to active listeners.

**Responsibilities:**
- Maintain a map of `containerID → *activeService` where `activeService` holds the `ServiceListener`, a cancel function, and the running reverse proxy.
- When the Docker Watcher reports a new container:
  1. Build the `ServiceModeHTTP` config: always `HTTPS: true, Port: 443`. If `tsserve.caps` is set, parse the comma-separated capability names and set `AcceptAppCaps: map[string][]string{"/": caps}`.
  2. Call `tsnet.Server.ListenService(serviceName, mode)`. This tells tsnet to listen for HTTPS connections and automatically provision a TLS certificate from Let's Encrypt for the service's FQDN.
  3. Create an `httputil.ReverseProxy` targeting the container's IP and port.
  4. Serve HTTP on the returned `ServiceListener` in a goroutine. Note: despite calling `http.Serve` (not `http.ServeTLS`), TLS is handled by tsnet's listener — the `ServiceListener` already terminates TLS before handing the request to the handler.
  5. Store the active service in the map.
- When the Docker Watcher reports a container has stopped:
  1. Look up the active service by container ID.
  2. Close the `ServiceListener` (this stops accepting new connections).
  3. Remove the entry from the map.

**Implementation notes:**
- `ListenService` can be called multiple times on the same `tsnet.Server` for different services — this is the core of how one node hosts many services.
- Use a `sync.Mutex` or similar to protect the map since Docker events arrive asynchronously.
- Use `context.Context` cancellation for clean goroutine shutdown.

### 3. Reverse Proxy

A standard Go `net/http/httputil.ReverseProxy` per service.

**Responsibilities:**
- Forward requests to the backend container IP + port.
- Set `X-Forwarded-For`, `X-Forwarded-Proto` headers (Go's ReverseProxy does `X-Forwarded-For` by default).
- Log proxy errors.

**Implementation notes:**
- Use `httputil.NewSingleHostReverseProxy` as the base.
- Override the `ErrorHandler` to log errors and return 502.
- If `tsserve.scheme=https`, set the proxy target scheme to `https` and configure the transport to skip TLS verification (for self-signed backend certs).

---

## Lifecycle Handling

### Startup

1. Parse environment variables. Validate that at least one authentication method is configured.
2. Initialise `tsnet.Server` with all configured fields (`Hostname`, `Dir`, `AuthKey`, `ClientID`, `ClientSecret`, `IDToken`, `Audience`, `AdvertiseTags`). Call `srv.Start()`. tsnet handles authentication internally — OIDC token exchange, OAuth key generation, or auth key login.
3. Wait for tsnet to be ready (connected to tailnet).
4. Obtain `LocalClient` via `srv.LocalClient()` for the status-page tailnet view.
5. Start the Docker Watcher. List existing containers and register services.
6. Begin listening for Docker events.

### Container Start

1. Docker Watcher receives `start` event.
2. Inspect container for `tsserve.*` labels. Ignore if missing or `tsserve.enable != "true"`.
3. Resolve container IP on the specified Docker network.
4. Pass service definition to Service Manager.
5. Service Manager calls `ListenService`, starts reverse proxy goroutine.
6. Log: `"service registered: svc:web -> 172.17.0.3:80"`.

### Container Stop

1. Docker Watcher receives `die` or `stop` event.
2. Look up container ID in the Service Manager's active map.
3. Close the `ServiceListener`. In-flight requests will complete; new connections are refused.
4. Remove from map.
5. Log: `"service deregistered: svc:web"`.

### Container IP Change (Restart)

When a container restarts, Docker emits `die` then `start`. The stop handler tears down the old listener, and the start handler creates a new one with the new container IP. No special handling needed.

### tsserve Shutdown

1. Receive `SIGINT` or `SIGTERM`.
2. Close all active `ServiceListener`s.
3. Call `tsnet.Server.Close()`.
4. Exit.

---

## Error Handling

| Scenario | Behaviour |
|---|---|
| `ListenService` fails (e.g. service not defined, node untagged) | Log error with service name. Do not crash. Skip this container. |
| `ListenService` fails because HTTPS/MagicDNS not enabled | Fatal error on first occurrence. Exit with a clear message telling the user to enable HTTPS and MagicDNS in the admin console. |
| Container IP cannot be resolved | Log warning. Skip this container. Retry on next Docker event for this container. |
| Backend unreachable (container crashed but event not yet received) | Reverse proxy returns 502 to the client. Normal behaviour. |
| Docker socket unavailable at startup | Fatal error. Exit with message. |
| `tsnet.Server.Start()` fails (e.g. bad auth key, OIDC token exchange failure, expired credentials) | Fatal error. Exit with message. For OIDC failures, suggest checking the federated identity configuration in the Tailscale admin console. |
| Duplicate `tsserve.service` on two containers | First one wins. Log a warning for the duplicate. If the first stops, the second does NOT auto-register (it would need to be restarted). |

---

## Project Structure

```
tsserve/
├── main.go              # Entry point, signal handling, wiring
├── docker/
│   ├── watcher.go       # Docker event subscription and container inspection
│   └── labels.go        # Label parsing and validation
├── proxy/
│   ├── manager.go       # Service Manager: tsnet.Server + active service map
│   └── reverseproxy.go  # Reverse proxy factory with error handling
├── Dockerfile
├── go.mod
├── go.sum
└── README.md
```

---

## Dependencies

| Dependency | Purpose |
|---|---|
| `tailscale.com/tsnet` | Embedded Tailscale node, `ListenService` API, `LocalClient` for the status page tailnet view |
| `tailscale.com/client/tailscale` | `LocalClient` type used by the status page |
| `github.com/docker/docker/client` | Docker daemon API client |
| `github.com/docker/docker/api/types` | Docker API types for events and container inspection |
| Go stdlib `net/http/httputil` | Reverse proxy |
| Go stdlib `log/slog` | Structured logging |

Note: `tailscale.com/client/tailscale` is pulled in transitively by `tailscale.com/tsnet` — it's not an additional module dependency, just an additional import path within the same module.

---

## Dockerfile

```dockerfile
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags='-w -s' -o tsserve .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /build/tsserve /usr/local/bin/tsserve
ENTRYPOINT ["tsserve"]
```

No Tailscale binary needed in the image — `tsnet` is compiled into the Go binary.

---

## Tailscale Admin Setup (User Prerequisite)

Users must configure their tailnet before using tsserve:

### 0. Enable HTTPS and MagicDNS

Go to the Tailscale admin console → DNS and ensure both **MagicDNS** and **HTTPS Certificates** are enabled. These are required for tsnet to provision TLS certificates. Without them, `ListenService` with `HTTPS: true` will return an error.

### 1. Define tags and auto-approvers in ACLs

```jsonc
{
  "tagOwners": {
    "tag:tsserve": ["autogroup:admin"]
  },
  "autoApprovers": {
    "services": {
      // list each service, or use a wildcard pattern if supported
      "svc:web": ["tag:tsserve"],
      "svc:api": ["tag:tsserve"]
    }
  },
  "grants": [{
    "src": ["autogroup:member"],
    "dst": ["svc:web", "svc:api"],
    "ip": ["*"]
  }]
}
```

If using app capabilities, add capability grants to the policy. For example, to grant read access to all members and admin access to a specific group:

```jsonc
{
  "grants": [
    {
      "src": ["autogroup:member"],
      "dst": ["svc:api"],
      "ip": ["*"],
      "app": {
        "example.com/cap/read": [{}]
      }
    },
    {
      "src": ["group:admins"],
      "dst": ["svc:api"],
      "ip": ["*"],
      "app": {
        "example.com/cap/read": [{}],
        "example.com/cap/admin": [{}]
      }
    }
  ]
}
```

The capability names in the ACL grants must match those listed in the container's `tsserve.caps` label.

### 2. Create services in the admin console

Go to the Tailscale admin console → Services and create each service (`svc:web`, `svc:api`, etc.) before starting tsserve. Define each to listen on **TCP port 443**. tsserve always advertises HTTPS on 443 — clients will access services at URLs like `https://web.tailnet.ts.net` with valid auto-provisioned TLS certificates.

### 3. Configure authentication

**For ECS / cloud (recommended):** Create a federated identity in the Tailscale admin console that trusts your cloud provider's OIDC issuer. Assign `tag:tsserve` and grant `auth_keys` write scope. See [Tailscale workload identity federation docs](https://tailscale.com/docs/features/workload-identity-federation) for provider-specific setup.

**For local development:** Create an auth key tagged with `tag:tsserve` and pass it as `TS_AUTHKEY`.

---

## Future Extensions (Out of Scope for v1)

These are explicitly not part of the initial implementation but noted for future consideration:

- **TCP proxying** — add a `ServiceModeTCP` path for databases, etc.
- **Automatic service definition creation** — call the Tailscale API to create `svc:` entries automatically when containers appear, removing the manual admin console step.
- **Funnel support** — expose selected services to the public internet.
- **Health checks** — verify backend is reachable before advertising the service.
- **Multiple services per container** — indexed labels like `tsserve.1.service`, `tsserve.1.port`.
- **Path-scoped app capabilities** — allow per-path capability mounts (e.g. `tsserve.caps./foo=example.com/cap/foo`) instead of the current all-at-root approach.

---

## References and Background

This project is directly inspired by two existing tools and one Tailscale API, combining the best aspects of each.

### tsbridge — https://github.com/jtdowney/tsbridge

A single Go binary reverse proxy that uses the `tsnet` library to expose multiple backend services on a tailnet. tsbridge embeds Tailscale via tsnet (no external daemon), supports both TOML config files and Docker label discovery (Traefik-style), and runs its own Go reverse proxy per service. Each service gets its own `tsnet.Server` instance, which means **each service registers as a separate machine/device** on the tailnet. This is architecturally clean but clutters the tailnet device list when running many services.

tsserve borrows tsbridge's architecture of a single self-contained Go binary with an embedded tsnet node and Docker label discovery, but avoids the machine-per-service problem.

### DockTail — https://github.com/marvinvr/docktail

A Docker container that watches for labeled containers and exposes them as **Tailscale Services** (not machines) using the `tailscale serve` CLI. DockTail requires an external `tailscaled` daemon (either on the host or as a sidecar container) and shells out to the `tailscale` CLI to advertise services. The Tailscale daemon does the actual proxying — DockTail is purely a discovery/configuration agent.

tsserve borrows DockTail's use of the Tailscale Services feature (no machine entries, services appear in the Services tab of the admin console), but replaces the external daemon dependency with tsnet compiled into the binary.

### tsnet `ListenService` API — https://pkg.go.dev/tailscale.com/tsnet

The `tsnet.Server.ListenService` method (added in Tailscale v1.86+) is what makes this project possible. It allows a tsnet node to advertise itself as a Tailscale Service host and return a `ServiceListener` that accepts connections for that service. Combined with `ServiceModeHTTP{HTTPS: true}`, it provides automatic TLS certificate provisioning.

Key API references:
- **ListenService**: https://pkg.go.dev/tailscale.com/tsnet#Server.ListenService
- **ServiceModeHTTP** (including `AcceptAppCaps`): https://pkg.go.dev/tailscale.com/tsnet#ServiceModeHTTP
- **Official example** (tsnet-services): https://github.com/tailscale/tailscale/tree/main/tsnet/example/tsnet-services
- **Reverse proxy example**: search for `ExampleServer_ListenService_reverseProxy` at https://pkg.go.dev/tailscale.com/tsnet
- **How-to guide**: https://tailscale.com/docs/features/tsnet/how-to/register-service
- **Tailscale Services docs**: https://tailscale.com/docs/features/tailscale-services

### Workload Identity Federation — https://tailscale.com/docs/features/workload-identity-federation

tsnet supports OIDC-based workload identity federation, allowing cloud workloads (ECS, GKE, GitHub Actions, etc.) to authenticate to a tailnet without static credentials. The tsnet.Server fields `ClientID`, `IDToken`, `Audience`, and `AdvertiseTags` handle the token exchange internally.

- **GA announcement**: https://tailscale.com/blog/workload-identity-ga
- **tsnet.Server API** (auth fields): https://tailscale.com/docs/reference/tsnet-server-api

### Also considered

- **tsdproxy** (https://github.com/almeidapaulopt/tsdproxy) — the original inspiration for the Docker-to-Tailscale proxy pattern. Creates machine entries per service using embedded Tailscale instances. tsserve aims to solve the same problem but with the newer Services API.
- **Tailscale sidecar containers** (https://github.com/tailscale-dev/ScaleTail) — Tailscale's recommended approach of one sidecar per service. Works well but operationally heavy for many services.
