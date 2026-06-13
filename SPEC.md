# tsserve — Specification

A single-binary Docker-to-Tailscale-Services proxy built on `tsnet`.

## Overview

`tsserve` watches Docker for labeled containers and exposes them as **Tailscale Services** (not machines) using the `tsnet` Go library's `ListenService` API. It runs as a single container or binary, registers once on the tailnet as a single node, and reverse-proxies traffic to backend containers. When containers start or stop, it creates or tears down the corresponding service listeners automatically.

## Goals

- **Single binary/container** — no external `tailscaled` daemon required. `tsnet` is compiled in.
- **Tailscale Services, not machines** — uses `tsnet.Server.ListenService`, so the tailnet device list stays clean. Each backend gets its own `svc:` entry, not a machine.
- **HTTPS by default** — all services are exposed over HTTPS with automatic TLS certificates from Let's Encrypt via Tailscale's built-in cert provisioning. Clients access services at `https://servicename.tailnet.ts.net`. This requires MagicDNS and HTTPS to be enabled in the Tailscale admin console.
- **Flexible discovery** — labelled containers can be discovered via a local Docker socket (single-host deployments) or via the AWS ECS API (multi-host clusters where Tailscale must not run on the container hosts). The `tsserve.*` label vocabulary is identical across modes.
- **Minimal scope** — label-driven discovery, reverse proxying, lifecycle management. Nothing else.

## Non-goals

- TOML/YAML config file provider (Docker labels only).
- Headscale support (Tailscale Services require the Tailscale control plane).
- Tailscale Funnel support.
- Automatic service definition creation via the Tailscale API (users pre-create services in the admin console or configure ACL auto-approvers).
- TCP/UDP proxying (HTTP/HTTPS only for v1).
- Distributed tracing.

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
| `tsserve.caps` | _(none)_ | App capabilities to accept and forward. Comma-separated list of capability names, mounted at `/`. Also enables [node identity headers](#node-identity-headers) (`Tailscale-Node-Tags`, `Tailscale-Node-Name`) for tagged peers. See [App Capabilities](#app-capabilities). |

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

#### Node identity headers

App capabilities carry the *granted capability values*, not the connecting node's own identity, and Tailscale's serve layer does not populate the `Tailscale-User-*` headers for tagged (non-user) nodes. A backend therefore cannot tell one tagged device (e.g. a CI runner) from another, and — unlike `tailscaled` — it cannot run a `whois` itself.

To fill this gap, **whenever `tsserve.caps` is set**, tsserve resolves the connecting peer in-process (`LocalClient.WhoIs`) and, for **tagged nodes only**, injects two extra headers before proxying to the backend:

| Header | Value |
| --- | --- |
| `Tailscale-Node-Tags` | The node's ACL tags, comma-separated, with the `tag:` prefix stripped (e.g. `github-runner,prod`). |
| `Tailscale-Node-Name` | The node's `ComputedName` (its MagicDNS base name / hostname). |

There is no separate opt-in: node-header injection rides on the same `tsserve.caps` opt-in as app capabilities. Untagged (user) nodes receive neither header. If the WhoIs lookup fails, no headers are injected.

Like the identity and app-capability headers Serve manages, **both node headers are stripped from the inbound request before forwarding**, so a client cannot spoof them. The peer IP used for the lookup is taken from the `X-Forwarded-For` header the serve layer sets to the peer's tailnet IP.

#### Upstream caveat — grants must target a node, not just the service

As of Tailscale `v1.98.x`, app capabilities granted to a Tailscale Service do not reach the backend when the ACL grant's `dst` is only the service name. Tailscale's serve layer resolves the connecting peer's capabilities against the hosting node's machine IP, but a grant compiled from `dst: ["svc:foo"]` registers the capability against the service's virtual IP — so the lookup misses and the `Tailscale-App-Capabilities` header is silently omitted. All other plumbing works: the request reaches the backend, identity headers arrive normally, and `tsserve.caps` is still passed correctly to `AcceptAppCaps`. Only the capability header is missing.

Tracked upstream as [tailscale/tailscale#19618](https://github.com/tailscale/tailscale/issues/19618). [PR #19702](https://github.com/tailscale/tailscale/pull/19702) landed helper functions (`PeerCapsForService`, `PeerCapsForIP`) but did not wire them into the serve-layer header injection.

**Workaround**: include the tsserve node's tag alongside the service in the grant's `dst`. That adds the node's machine IP to the cap rule's destination set so the lookup succeeds:

```jsonc
{
  "src": ["autogroup:member"],
  "dst": ["svc:api", "tag:tsserve"],
  "ip":  ["*"],
  "app": {
    "example.com/cap/admin": [{}]
  }
}
```

This widens the cap grant slightly — the peer now has the capability against the node generally, not just when accessing this service — but for typical deployments (one tsserve node fronting these specific services) it's equivalent in practice.

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
| **Discovery** | | | |
| `TSSERVE_DISCOVERY` | No | `docker` | Discovery backend. `docker` reads `/var/run/docker.sock`. `ecs` queries the AWS ECS API. See [ECS Cluster Mode](#ecs-cluster-mode). |
| `TSSERVE_ECS_CLUSTER` | ✱✱✱ | — | ECS cluster name or ARN to watch. Required when `TSSERVE_DISCOVERY=ecs`. |
| `TSSERVE_ECS_POLL_INTERVAL` | No | `10s` | How often to poll the ECS API for task changes. Go duration format. |
| `TSSERVE_ECS_AWS_PROFILE` | No | — | Shared-config profile to use **only** for the ECS discovery client. Set this instead of `AWS_PROFILE` so tsnet's WIF flow still resolves to the default (instance-role) identity Tailscale trusts. |
| `TSSERVE_ECS_AWS_CONFIG_FILE` | No | — | Shared-config file path for the ECS discovery client only. Set this instead of `AWS_CONFIG_FILE` to keep the WIF credential chain clean. |
| `AWS_REGION` | ✱✱✱ | — | Standard AWS env var. Required when `TSSERVE_DISCOVERY=ecs` if not derivable from instance metadata or IAM role. Safe to set process-wide — it does not affect identity. |
| **Observability** | | | |
| `TSSERVE_METRICS_ADDR` | No | `127.0.0.1:9090` | `host:port` for the loopback Prometheus listener that serves `/metrics`. Set to `""` to disable, or `0.0.0.0:9100` to expose to a remote scraper (the operator owns host-firewall enforcement in that case). |
| `TSSERVE_TRAEFIK_PORT` | No | — | If set, the tailnet HTTPS listener proxies `/traefik` to `http://localhost:<port>`. Intended for reaching a co-located Traefik dashboard. Omit to disable. |

✱ One authentication method must be provided. ✱✱ Required for OAuth and OIDC modes. ✱✱✱ Required when `TSSERVE_DISCOVERY=ecs`.

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

## ECS Cluster Mode

When `TSSERVE_DISCOVERY=ecs`, tsserve discovers backends by querying the AWS ECS API instead of a local Docker socket. This is the deployment topology for clusters where Tailscale must not run on the container hosts.

### When to use this mode

- tsserve runs on a dedicated proxy host (typically a small EC2 instance in a border VPC).
- Containers run on a separate, locked-down ECS cluster (EC2 launch type) in another VPC.
- ECS hosts have no internet egress and must not initiate any connection to the Tailscale control plane.
- One tsserve instance proxies for many ECS hosts and many tasks across the cluster.

The Docker discovery path is unchanged and remains the recommended choice for single-host deployments (local dev, Docker Compose, a single VM running both Docker and tsserve).

### Deployment topology

```
┌─────────────────────────────────────────────────────────────────────┐
│ Border VPC                                                          │
│                                                                     │
│   ┌────────────────────────────────────────────────────────────┐    │
│   │ Proxy host (EC2)                                           │    │
│   │   tsserve                                                  │    │
│   │     tsnet.Server ─────────────────────▶ Tailscale (HTTPS)  │    │
│   │     ecs.Watcher  ──────▶ AWS ECS / EC2 APIs                │    │
│   │     Manager + ReverseProxy ───┐                            │    │
│   └───────────────────────────────┼────────────────────────────┘    │
└───────────────────────────────────┼─────────────────────────────────┘
                                    │ VPC peering / Transit Gateway
                                    ▼
┌─────────────────────────────────────────────────────────────────────┐
│ ECS VPC (private; no internet egress; no Tailscale)                 │
│                                                                     │
│   ┌──────────────────────────┐   ┌──────────────────────────┐       │
│   │ EC2 host (ECS-managed)   │   │ EC2 host (ECS-managed)   │  ...  │
│   │   bridge tasks           │   │   awsvpc tasks           │       │
│   │   reachable at           │   │   reachable at           │       │
│   │   hostIP:hostPort        │   │   eniIP:containerPort    │       │
│   └──────────────────────────┘   └──────────────────────────┘       │
└─────────────────────────────────────────────────────────────────────┘
```

The ECS hosts are strictly passive: they accept inbound TCP from the proxy host's security group and originate nothing. All Tailscale traffic, all AWS API calls, and all reverse-proxying terminate on the proxy host.

### Network requirements

- **Cross-VPC reachability**: VPC peering, Transit Gateway, or PrivateLink between the border VPC and the ECS VPC. The proxy host must be able to open TCP connections to task IPs (awsvpc mode) or ECS host private IPs (bridge mode).
- **Security groups**: ECS task ENIs (awsvpc) or container instance SGs (bridge) must allow inbound TCP from the proxy host's SG on the labelled ports.
- **AWS API access**: the proxy host needs egress to the regional ECS and EC2 endpoints. Either via the border VPC's existing internet egress or via interface VPC endpoints (`com.amazonaws.<region>.ecs`, `com.amazonaws.<region>.ec2`) in the border VPC. No endpoints are required in the ECS VPC.

### IAM permissions

The proxy host's IAM role (or AWS credentials supplied via the standard SDK env vars) must permit:

| Action | Purpose |
|---|---|
| `ecs:ListTasks` | Enumerate running tasks in the configured cluster. |
| `ecs:DescribeTasks` | Read task metadata, network bindings, and ENI attachments. |
| `ecs:DescribeTaskDefinition` | Read `containerDefinitions[*].dockerLabels` — the source of truth for `tsserve.*` labels. |
| `ecs:DescribeContainerInstances` | Resolve `containerInstanceArn` → `ec2InstanceId` for bridge-mode tasks. |
| `ec2:DescribeInstances` | Resolve EC2 instance ID → private IPv4 for bridge-mode forwarding. |

For deployments running awsvpc tasks exclusively, the last two actions can be omitted.

Scope the IAM policy to the specific cluster ARN where possible; `ecs:Describe*` actions accept resource ARN conditions.

### Where labels live

Docker labels on ECS tasks are declared in the task definition under `containerDefinitions[*].dockerLabels`. The same `tsserve.*` vocabulary documented in [Docker Labels](#docker-labels) applies, with one exception:

- `tsserve.network` is **ignored in ECS mode**. The network mode is determined by the task definition (`networkMode: bridge` or `networkMode: awsvpc`), not by a label.

Example task definition:

```json
{
  "family": "myapi",
  "networkMode": "bridge",
  "containerDefinitions": [
    {
      "name": "api",
      "image": "myapi:latest",
      "portMappings": [{ "containerPort": 3000, "hostPort": 0, "protocol": "tcp" }],
      "dockerLabels": {
        "tsserve.enable": "true",
        "tsserve.service": "svc:api",
        "tsserve.port": "3000",
        "tsserve.caps": "example.com/cap/read,example.com/cap/admin"
      }
    }
  ]
}
```

`tsserve.port` is always the **container port** (the port the application listens on inside the container), in both bridge and awsvpc modes. tsserve handles the host-port lookup internally for bridge mode.

### Backend resolution

The ECS watcher branches on the task definition's `networkMode`:

**Bridge mode** (`networkMode: "bridge"`):
1. The task runs on a container instance. Container ports are exposed at dynamic host ports.
2. For the labelled container, find the entry in `task.containers[*].networkBindings` where `containerPort == tsserve.port`. Use its `hostPort`.
3. Resolve the host's private IP: `task.containerInstanceArn` → `DescribeContainerInstances` → `ec2InstanceId` → `DescribeInstances` → `privateIpAddress`.
4. Forward to `hostPrivateIP:hostPort`.

**awsvpc mode** (`networkMode: "awsvpc"`):
1. Each task has its own ENI. Find the attachment of type `ElasticNetworkInterface` in `task.attachments`.
2. Read `privateIPv4Address` from the attachment's `details`.
3. Forward to `eniPrivateIP:tsserve.port` (no port translation needed).

Either way, the reverse-proxy layer (`proxy.Manager`, `proxy.reverseproxy`) receives a `host:port` string and is unchanged from the Docker-mode case.

### Caching

To keep ECS API call volume bounded:

- **Task definitions** are immutable per ARN; cache `DescribeTaskDefinition` results indefinitely, keyed by ARN.
- **Container instance → EC2 host private IP** mappings are long-lived; cache for the lifetime of the host. Invalidate on lookup failure.

### Lifecycle (polling model)

v1 uses a poll-and-diff loop; no EventBridge integration:

1. Every `TSSERVE_ECS_POLL_INTERVAL` (default `10s`):
   - `ListTasks(cluster, desiredStatus=RUNNING)` → `DescribeTasks(taskArns)`.
   - Filter to tasks whose task definition has at least one container with `tsserve.enable=true`.
   - Compute diff against the current set of registered task ARNs.
2. For new task ARNs: resolve backend (per "Backend resolution" above) and call `Manager.Register(taskArn, def, backend)`.
3. For disappeared task ARNs: call `Manager.Deregister(taskArn)`.
4. Tasks with unchanged ARN are a no-op (task ARNs are stable; a redeploy produces new ARNs).

This is sufficient at typical proxy latencies (a ~10s lag between a task starting and becoming reachable is acceptable; the reverse proxy already returns 502 if the backend isn't up yet, which is identical to the Docker-mode behaviour). EventBridge-driven fast updates are listed under [Future Extensions](#future-extensions-out-of-scope-for-v1).

### API quota notes

`ListTasks` and `DescribeTasks` share a single account-wide, per-region token bucket — the "Cluster resource read actions" category — with a burst of 100 and a sustained rate of 20 requests/sec. Default 10s polling stays under 2 requests/sec for clusters up to ~1,000 tasks. Raise `TSSERVE_ECS_POLL_INTERVAL` (suggested 30s) for larger clusters, or adopt the EventBridge fast path. These rate limits are documented in the ECS API Reference but are **not** exposed as AWS Service Quotas codes — increases require an AWS Support ticket. The watcher uses the SDK's adaptive retry mode so a transient `ThrottlingException` is absorbed without crashing; the next poll cycle reconciles whatever was missed.

### Example: proxy task definition

The proxy itself can run as a small ECS task in the border VPC, using workload identity federation:

```json
{
  "family": "tsserve-proxy",
  "networkMode": "awsvpc",
  "taskRoleArn": "arn:aws:iam::123456789012:role/tsserve-proxy",
  "containerDefinitions": [
    {
      "name": "tsserve",
      "image": "ghcr.io/you/tsserve:latest",
      "essential": true,
      "environment": [
        {"name": "TSSERVE_DISCOVERY",     "value": "ecs"},
        {"name": "TSSERVE_ECS_CLUSTER",   "value": "prod-apps"},
        {"name": "AWS_REGION",            "value": "us-east-1"},
        {"name": "TS_CLIENT_ID",          "value": "<federated-identity-client-id>"},
        {"name": "TS_AUDIENCE",           "value": "<federated-identity-audience>"},
        {"name": "TSSERVE_TAGS",          "value": "tag:tsserve"}
      ],
      "mountPoints": [
        {"sourceVolume": "tsserve-state", "containerPath": "/var/lib/tsserve"}
      ]
    }
  ],
  "volumes": [
    {"name": "tsserve-state", "efsVolumeConfiguration": {"fileSystemId": "fs-..."}}
  ]
}
```

The proxy task role (`tsserve-proxy`) carries both the Tailscale workload identity federation subject claim and the AWS IAM permissions listed above. No static credentials anywhere.

### Known v1 limitations

- **Replica fan-out is not supported.** If an ECS service runs N replicas of a task definition that declares `tsserve.service=svc:api`, only the first task discovered will back the service; the others are logged as duplicates and ignored (consistent with `proxy.Manager`'s existing "first wins" behaviour — see `proxy/manager.go:86`). For services that need load-balanced fan-out, run an internal NLB or ALB in the ECS VPC and label a single "shim" task that forwards to it. See [Future Extensions](#future-extensions-out-of-scope-for-v1).

---

## Core Components

Exactly one watcher is active per tsserve process, chosen at startup by `TSSERVE_DISCOVERY`. Both watchers feed the same `Registrar` interface consumed by the Service Manager, so the manager and reverse proxy are unaware of the discovery backend.

### 1a. Docker Watcher (`TSSERVE_DISCOVERY=docker`)

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

### 1b. ECS Watcher (`TSSERVE_DISCOVERY=ecs`)

Queries the AWS ECS API on a fixed interval and drives the same `Registrar` interface as the Docker watcher. See [ECS Cluster Mode](#ecs-cluster-mode) for the user-facing description; this section covers implementation.

**Responsibilities:**
- On startup, list all running tasks in `TSSERVE_ECS_CLUSTER` and register any whose task definition declares `tsserve.enable=true` on at least one container.
- Every `TSSERVE_ECS_POLL_INTERVAL`, repeat the list/diff and apply additions and removals.
- For each labelled container in a task, resolve the backend `host:port` based on the task definition's `networkMode` (bridge or awsvpc).
- Pass discovered service definitions to the Service Manager.

**Implementation notes:**
- Use the AWS SDK for Go v2 (`github.com/aws/aws-sdk-go-v2/service/ecs` and `.../service/ec2`). Standard SDK config resolution (env, shared config, EC2 IMDS, ECS task role) handles credentials. The ECS client's config is loaded with `TSSERVE_ECS_AWS_PROFILE` / `TSSERVE_ECS_AWS_CONFIG_FILE` if set, rather than the process-wide `AWS_PROFILE` / `AWS_CONFIG_FILE`, so tsnet's WIF flow (which also calls `LoadDefaultConfig`) is not pulled onto a cross-account assumed role.
- Use task ARN (not container ID) as the registration key, since ECS has no container-ID equivalent for the lifetime of a task.
- Cache `DescribeTaskDefinition` results indefinitely by ARN (task defs are immutable per revision).
- Cache container-instance → EC2 instance ID → private IP mappings; refresh on miss.
- Skip tasks that lack `tsserve.enable=true`. Skip tasks that haven't reached `lastStatus=RUNNING`.
- When a task definition declares multiple containers with `tsserve.enable=true`, treat each (taskArn, containerName) pair as a separate registration. Use a composite registration key like `taskArn#containerName`.
- Handle ECS API throttling: SDK retries with exponential backoff are sufficient at expected scales (a few `Describe*` calls per poll cycle).

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
- When the service has opted into `tsserve.caps`, inject [node identity headers](#node-identity-headers) (`Tailscale-Node-Tags`, `Tailscale-Node-Name`) for tagged peers, resolved via `LocalClient.WhoIs`, and strip any inbound copies of those headers first to prevent spoofing.
- Log proxy errors.

**Implementation notes:**
- Use `httputil.NewSingleHostReverseProxy` as the base.
- Override the `ErrorHandler` to log errors and return 502.
- If `tsserve.scheme=https`, set the proxy target scheme to `https` and configure the transport to skip TLS verification (for self-signed backend certs).

---

## Lifecycle Handling

The "Container Start / Stop / Restart" subsections below describe Docker mode. The ECS mode lifecycle is poll-driven and is described in [ECS Cluster Mode → Lifecycle (polling model)](#lifecycle-polling-model). The shared parts — startup, shutdown, and how the Service Manager reacts to register/deregister calls — are documented here.

### Startup

1. Parse environment variables. Validate that at least one authentication method is configured.
2. Initialise `tsnet.Server` with all configured fields (`Hostname`, `Dir`, `AuthKey`, `ClientID`, `ClientSecret`, `IDToken`, `Audience`, `AdvertiseTags`). Call `srv.Start()`. tsnet handles authentication internally — OIDC token exchange, OAuth key generation, or auth key login.
3. Wait for tsnet to be ready (connected to tailnet).
4. Obtain `LocalClient` via `srv.LocalClient()` for the status-page tailnet view.
5. Select the watcher based on `TSSERVE_DISCOVERY` (`docker` or `ecs`).
6. Start the watcher. It performs an initial enumeration (Docker: `ContainerList` + event subscription; ECS: `ListTasks` + the first poll) and registers any qualifying backends.

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
| Docker socket unavailable at startup (Docker mode) | Fatal error. Exit with message. |
| ECS `DescribeTasks` returns `AccessDeniedException` (ECS mode) | Fatal error on first occurrence. Exit with message naming the IAM action that was denied. |
| ECS `TSSERVE_ECS_CLUSTER` does not exist (ECS mode) | Fatal error. Exit with message. |
| ECS API throttling (`ThrottlingException`, `RequestLimitExceeded`) | Rely on SDK retry-with-backoff. If a poll cycle still fails after retries, log a warning and continue; the next poll will catch up. Do not crash. |
| Bridge-mode task with no `networkBinding` matching `tsserve.port` | Log warning naming the task ARN and labelled port. Skip this container. Retry on next poll cycle. |
| awsvpc-mode task with no `ElasticNetworkInterface` attachment yet | Log debug message. Skip this poll. The next cycle will pick it up once the ENI is attached. |
| `ec2:DescribeInstances` fails for a container instance (ECS mode) | Log warning. Skip this container. Cached host IPs are invalidated on failure so the next poll retries cleanly. |
| `tsnet.Server.Start()` fails (e.g. bad auth key, OIDC token exchange failure, expired credentials) | Fatal error. Exit with message. For OIDC failures, suggest checking the federated identity configuration in the Tailscale admin console. |
| Duplicate `tsserve.service` on two containers (Docker mode) or two tasks (ECS mode) | First one wins. Log a warning for the duplicate. If the first stops, the second does NOT auto-register (it would need to be restarted, or ECS mode would need to rediscover it on the next poll cycle). |

---

## Observability and local endpoints

tsserve exposes a small in-process control surface on **two** local listeners. The split is deliberate: status and dashboards are tailnet-scoped (any peer that can resolve the node's hostname can reach them), while Prometheus metrics stay on the host loopback by default so scrape traffic never traverses the tailnet.

### Tailnet HTTPS listener — `:443`

Available at `https://<TSSERVE_HOSTNAME>.<tailnet>.ts.net/`. Uses `tsnet.Server.ListenTLS`, which provisions a TLS certificate for the node's own hostname automatically — the same MagicDNS/HTTPS Certificates prerequisites the service listeners already require. The listener is always on; no env var disables it.

| Path | Purpose |
|---|---|
| `/` | Single-page HTML status: tailnet identity (hostname, FQDN, tailnet name, MagicDNS suffix, node IPs, backend state), discovery mode, uptime, build info, and a table of currently registered services (service name → backend → caps → container → registered time). No JavaScript, no external assets. |
| `/traefik`, `/traefik/...` | Reverse proxy to `http://localhost:<TSSERVE_TRAEFIK_PORT>` with the `/traefik` prefix stripped before forwarding. Intended for reaching a Traefik dashboard on the same host. Returns 404 with a one-line hint when `TSSERVE_TRAEFIK_PORT` is not set. |
| `/metrics` | 404 with a hint pointing at the loopback metrics listener. |

**Traefik prefix-stripping caveat.** `StripPrefix("/traefik", ...)` works for assets Traefik serves at relative paths, but absolute-path links inside the dashboard bundle (e.g. `/api/...`) bypass the prefix and land back on tsserve's mux at `/api/`, where they will 404. Operators who need full fidelity should configure Traefik to serve the dashboard under a matching path prefix; tsserve does not rewrite response bodies.

### Loopback HTTP listener — `TSSERVE_METRICS_ADDR` (default `127.0.0.1:9090`)

| Path | Purpose |
|---|---|
| `/metrics` | Prometheus exposition served by `promhttp.HandlerFor` over a dedicated registry (no leakage from transitive deps). |
| `/` | One-line `text/plain` hint pointing at `/metrics`. |

Loopback binding is the security boundary; the endpoint has no authentication. Operators who set `TSSERVE_METRICS_ADDR=0.0.0.0:9090` to expose the endpoint to a remote scraper are responsible for host-firewall enforcement. Set `TSSERVE_METRICS_ADDR=""` to disable the listener entirely.

### Metric inventory

All counters and gauges are prefixed `tsserve_`. Histograms use the default Prometheus buckets.

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `tsserve_proxy_requests_total` | Counter | `service`, `method`, `code` | One increment per proxied HTTP request (the wrapped response status is the `code`, so backend 502s show up here). Hijacked protocol upgrades (WebSocket) are recorded as `101`. |
| `tsserve_proxy_request_duration_seconds` | Histogram | `service`, `method` | Wall-clock time from handler entry to exit. Hijacked (WebSocket) connections are excluded, since their lifetime is a session, not a request latency. |
| `tsserve_proxy_request_bytes_total` | Counter | `service` | Bytes read from request bodies. After a hijack, also counts bytes read from the client over the upgraded connection. |
| `tsserve_proxy_response_bytes_total` | Counter | `service` | Bytes written to response bodies (does not include response headers). After a hijack, also counts bytes written to the client over the upgraded connection. |
| `tsserve_proxy_in_flight_requests` | Gauge | `service` | Currently executing handlers, by service. Includes open WebSocket connections (they block in the handler for their lifetime). |
| `tsserve_proxy_open_websockets` | Gauge | `service` | Current open WebSocket (hijacked protocol-upgrade) connections, by service. |
| `tsserve_proxy_backend_errors_total` | Counter | `service`, `reason` | Backend errors observed by the reverse proxy's `ErrorHandler`. `reason` is a coarse classification: `timeout`, `connection-refused`, `dns`, `eof`, `other`. |
| `tsserve_services_active` | Gauge | — | Number of Tailscale Services currently registered. |
| `tsserve_build_info` | Gauge | `version`, `revision`, `go_version` (const) | Constant `1`. Useful for grouping in dashboards. |

Standard `go_*` and `process_*` collectors are also registered.

---

## Project Structure

```
tsserve/
├── main.go              # Entry point, signal handling, watcher selection, wiring
├── docker/
│   ├── watcher.go       # Docker event subscription and container inspection
│   └── labels.go        # Label parsing and validation (shared with ecs/)
├── ecs/
│   ├── watcher.go       # ECS poll-and-diff loop; drives the same Registrar
│   ├── resolve.go       # Backend resolution: bridge (host:hostPort) vs awsvpc (eni:containerPort)
│   └── cache.go         # Task-definition and container-instance → host-IP caches
├── proxy/
│   ├── manager.go       # Service Manager: tsnet.Server + active service map
│   └── reverseproxy.go  # Reverse proxy factory with error handling
├── metrics/
│   └── metrics.go       # Prometheus collectors + per-request middleware
├── local/
│   ├── server.go        # Tailnet HTTPS + loopback HTTP listeners
│   ├── status.go        # Status page handler
│   ├── status.html      # Embedded status page template
│   └── traefik.go       # /traefik reverse proxy + env-var parsing
├── Dockerfile
├── go.mod
├── go.sum
└── README.md
```

The `Registrar` interface declared in `docker/watcher.go` is the seam between discovery and proxying. The ECS watcher implements no new contract; it satisfies the same interface. Label parsing is shared — `docker/labels.go` operates on `map[string]string`, which is the shape of both Docker's `Config.Labels` and ECS's `containerDefinitions[*].dockerLabels`.

---

## Dependencies

| Dependency | Purpose |
|---|---|
| `tailscale.com/tsnet` | Embedded Tailscale node, `ListenService` API, `LocalClient` for the status page tailnet view |
| `tailscale.com/client/tailscale` | `LocalClient` type used by the status page |
| `github.com/docker/docker/client` | Docker daemon API client (Docker discovery mode) |
| `github.com/docker/docker/api/types` | Docker API types for events and container inspection |
| `github.com/aws/aws-sdk-go-v2/config` | AWS SDK config resolution (ECS discovery mode) |
| `github.com/aws/aws-sdk-go-v2/service/ecs` | ECS API client: `ListTasks`, `DescribeTasks`, `DescribeTaskDefinition`, `DescribeContainerInstances` |
| `github.com/aws/aws-sdk-go-v2/service/ec2` | EC2 API client: `DescribeInstances` for bridge-mode host IP resolution |
| `github.com/prometheus/client_golang` | Prometheus collectors and `promhttp` exposition handler |
| Go stdlib `net/http/httputil` | Reverse proxy |
| Go stdlib `html/template` | Status page rendering |
| Go stdlib `log/slog` | Structured logging |

Notes:
- `tailscale.com/client/tailscale` is pulled in transitively by `tailscale.com/tsnet` — it's not an additional module dependency, just an additional import path within the same module.
- The AWS SDK is only imported by the `ecs/` package. Builds intended for Docker-only deployments can use a build tag to exclude it if image size matters; the default build includes both watchers.

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
- **EventBridge-driven ECS updates** — subscribe to `ECS Task State Change` events via EventBridge → SQS for sub-second lifecycle reaction in cluster mode, instead of the v1 polling loop. Polling remains as a reconciliation fallback.
- **ECS replica fan-out** — when multiple tasks share a `tsserve.service` value (e.g. an ECS service with `desiredCount > 1`), advertise the service once and load-balance requests across all healthy task backends, rather than the current "first task wins" behaviour. Likely involves extending `proxy.Manager` to hold a small per-service backend pool.
- **Fargate launch-type support** — the ECS watcher's awsvpc code path already handles Fargate's task-IP shape, but Fargate has additional constraints (no container-instance ARN, no host-IP fallback, IAM via task role only) that need explicit testing and a documented setup path.

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
