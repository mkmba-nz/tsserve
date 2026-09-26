# tsserve — Specification

A single-binary proxy from Docker containers, ECS tasks and Lambda functions to Tailscale Services, built on `tsnet`.

## Overview

`tsserve` watches Docker for labeled containers — or the AWS ECS API for labeled tasks, or AWS for tagged Lambda functions — and exposes them as **Tailscale Services** (not machines) using the `tsnet` Go library's `ListenService` API. It runs as a single container or binary, registers once on the tailnet as a single node, and reverse-proxies traffic to the backends: containers reached at a host and port, or functions invoked through the Lambda API. When backends appear or disappear, it creates or tears down the corresponding service listeners automatically.

## Goals

- **Single binary/container** — no external `tailscaled` daemon required. `tsnet` is compiled in.
- **Tailscale Services, not machines** — uses `tsnet.Server.ListenService`, so the tailnet device list stays clean. Each backend gets its own `svc:` entry, not a machine.
- **HTTPS by default** — all services are exposed over HTTPS with automatic TLS certificates from Let's Encrypt via Tailscale's built-in cert provisioning. Clients access services at `https://servicename.tailnet.ts.net`. This requires MagicDNS and HTTPS to be enabled in the Tailscale admin console.
- **Flexible discovery** — labelled containers can be discovered via a local Docker socket (single-host deployments) or via the AWS ECS API (multi-host clusters where Tailscale must not run on the container hosts), and tagged Lambda functions via the AWS Resource Groups Tagging API. Several discovery modes can run in one process. The `tsserve.*` vocabulary is shared across modes: container labels and function tags use the same keys, with the differences listed in [Lambda Function Backends](#lambda-function-backends).
- **Minimal scope** — label-driven discovery, reverse proxying, lifecycle management. Nothing else.

## Non-goals

- TOML/YAML config file provider (per-backend configuration comes only from container labels or function tags).
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

A single `tsnet.Server` instance registers on the tailnet. For each discovered container, `ListenService` is called to advertise a service, and a Go `httputil.ReverseProxy` handles the actual traffic forwarding to the container's IP on the Docker bridge network. The diagram shows Docker mode; in [ECS Cluster Mode](#ecs-cluster-mode) the backends are task IPs in another VPC, and a [function backend](#lambda-function-backends) is not dialled at all — the proxy hands each request to an invoker that calls the Lambda API.

---

## Docker Labels

All labels are prefixed with `tsserve.`. The same keys label ECS task-definition containers ([Where labels live](#where-labels-live)) and, as AWS tags, Lambda functions ([Function tags](#function-tags)).

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

#### Node identity headers

App capabilities carry the *granted capability values*, not the connecting node's own identity, and Tailscale's serve layer does not populate the `Tailscale-User-*` headers for tagged (non-user) nodes. A backend therefore cannot tell one tagged device (e.g. a CI runner) from another, and — unlike `tailscaled` — it cannot run a `whois` itself.

To fill this gap, on **every request to every service**, whatever the backend type and whether or not `tsserve.caps` is set, tsserve resolves the connecting peer in-process (`LocalClient.WhoIs`) and injects node identity headers before proxying to the backend:

| Header | Sent for | Value |
| --- | --- | --- |
| `Tailscale-Node-Name` | Every node, user-owned or tagged | The node's `ComputedName` (its MagicDNS base name / hostname). |
| `Tailscale-Node-Tags` | Tagged nodes only | The node's ACL tags, comma-separated, with the `tag:` prefix stripped (e.g. `github-runner,prod`). |

If the WhoIs lookup fails, no headers are injected.

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
| `TSSERVE_STATE_DIR` | No | `/var/lib/tsserve` | Directory for tsnet state persistence across restarts. Always used as tsnet's var root — the TLS cert directory (`<dir>/certs`) and log config live here even when `TSSERVE_STATE_STORE` is set. |
| `TSSERVE_STATE_STORE` | No | — | Diverts **only** the node identity (`tailscaled.state`: machine/node key and prefs) to an external state store, leaving certs on disk under `TSSERVE_STATE_DIR`. Value is a `store.New` target — most usefully an AWS SSM parameter ARN, `arn:aws:ssm:<region>:<acct>:parameter/<name>[?kmsKey=<id>]`. Unset ⇒ identity is a `tailscaled.state` file under `TSSERVE_STATE_DIR`, as before. Pairs with `TSSERVE_CERT_S3_BUCKET` for a fully durable identity+certs setup on ephemeral filesystems. See [Node state store](#node-state-store). |
| `TSSERVE_LOG_LEVEL` | No | `info` | Log level: `debug`, `info`, `warn`, `error`. |
| **Discovery** | | | |
| `TSSERVE_DISCOVERY` | No | `docker` | Comma-separated list of discovery modes, e.g. `ecs,lambda`. `docker` reads `/var/run/docker.sock`. `ecs` queries the AWS ECS API; see [ECS Cluster Mode](#ecs-cluster-mode). `lambda` discovers tagged Lambda functions; see [Lambda Function Backends](#lambda-function-backends). Every listed mode runs in the one process against the same set of services. Entries are matched case-insensitively and may carry surrounding whitespace; an unknown, empty or repeated entry is a startup error. |
| `TSSERVE_ECS_CLUSTER` | ✱✱✱ | — | ECS cluster name or ARN to watch. Required when `TSSERVE_DISCOVERY` includes `ecs`, unless every entry in `TSSERVE_ECS_ACCOUNTS` supplies its own `cluster`. In multi-account mode it is the shared default cluster. |
| `TSSERVE_ECS_ACCOUNTS` | No | — | JSON array of accounts to discover, one poller per entry, each with its own credential context. Unset ⇒ single-account mode using the vars below. See [Multiple AWS accounts](#multiple-aws-accounts). |
| `TSSERVE_ECS_POLL_INTERVAL` | No | `10s` | How often to poll the ECS API for task changes. Go duration format. Applies to every account. |
| `TSSERVE_ECS_RETRY_INTERVAL` | No | `2m` | Slower cadence used to retry an **unhealthy** reader (e.g. one whose role is not assumable yet). While a reader is failing it polls at this interval instead of `TSSERVE_ECS_POLL_INTERVAL`, so a single broken reader keeps retrying — and recovers automatically once fixed — without hammering the API or blocking the daemon. Never faster than the poll interval. |
| `TSSERVE_ECS_AWS_PROFILE` | No | — | Shared-config profile to use **only** for the ECS discovery client. Set this instead of `AWS_PROFILE` so tsnet's WIF flow still resolves to the default (instance-role) identity Tailscale trusts. In multi-account mode it is the shared default profile. |
| `TSSERVE_ECS_AWS_CONFIG_FILE` | No | — | Shared-config file path for the ECS discovery client only. Set this instead of `AWS_CONFIG_FILE` to keep the WIF credential chain clean. In multi-account mode it is the shared default config file. |
| `TSSERVE_LAMBDA_ACCOUNTS` | No | — | JSON array of accounts (or account-and-region pairs) to discover functions in, one Lambda reader per entry. Same fields as `TSSERVE_ECS_ACCOUNTS` minus `cluster`. Unset ⇒ one reader from the shared vars below. See [Multiple accounts and regions](#multiple-accounts-and-regions). |
| `TSSERVE_LAMBDA_POLL_INTERVAL` | No | `30s` | How often each Lambda reader polls the Tagging API for tagged functions. Go duration format. Applies to every Lambda reader. |
| `TSSERVE_LAMBDA_RETRY_INTERVAL` | No | `2m` | Slower cadence for an **unhealthy** Lambda reader, as `TSSERVE_ECS_RETRY_INTERVAL` is for ECS readers. Never faster than the poll interval. |
| `TSSERVE_LAMBDA_AWS_PROFILE` | No | — | Shared-config profile used **only** by the Lambda readers' clients: Tagging and STS for discovery, and the Lambda client that invokes functions. Set this instead of `AWS_PROFILE`, for the same reason as `TSSERVE_ECS_AWS_PROFILE`. With `TSSERVE_LAMBDA_ACCOUNTS` it is the shared default profile. |
| `TSSERVE_LAMBDA_AWS_CONFIG_FILE` | No | — | Shared-config file path for the Lambda readers' clients only. With `TSSERVE_LAMBDA_ACCOUNTS` it is the shared default config file. |
| `AWS_REGION` | ✱✱✱ | — | Standard AWS env var. Required when `TSSERVE_DISCOVERY` includes `ecs` or `lambda`, unless every account entry supplies its own `region` or the region is derivable from instance metadata or the IAM role. Safe to set process-wide — it does not affect identity. In multi-account mode it is the shared default region, for ECS and Lambda readers alike. |
| **Certificate cache** | | | |
| `TSSERVE_CERT_S3_BUCKET` | No | — | Enables the S3-backed TLS certificate cache. When set, tsnet's on-disk cert cache (`<TSSERVE_STATE_DIR>/certs`) is restored from this bucket at startup and any cert tsnet provisions or renews is uploaded back. Intended for ephemeral filesystems (ECS/Fargate) so restarts reuse existing Let's Encrypt certs instead of re-running ACME. Unset ⇒ certs stay on local disk only, as before. See [TLS certificate cache](#tls-certificate-cache). |
| `TSSERVE_CERT_S3_PREFIX` | No | — | Optional key prefix within the bucket (e.g. `tsserve/prod/`). Lets one bucket back multiple nodes. |
| `TSSERVE_CERT_S3_REGION` | No | `AWS_REGION` | Region for the cert-cache S3 client. Defaults to `AWS_REGION`. The cache client loads its own AWS config, so this does not affect tsnet's credential resolution. |
| **Observability** | | | |
| `TSSERVE_METRICS_ADDR` | No | `127.0.0.1:9090` | `host:port` for the loopback Prometheus listener that serves `/metrics`. Set to `""` to disable, or `0.0.0.0:9100` to expose to a remote scraper (the operator owns host-firewall enforcement in that case). |
| `TSSERVE_TRAEFIK_PORT` | No | — | If set, the tailnet HTTPS listener proxies `/traefik` to `http://localhost:<port>`. Intended for reaching a co-located Traefik dashboard. Omit to disable. |

✱ One authentication method must be provided. ✱✱ Required for OAuth and OIDC modes. ✱✱✱ Required when `TSSERVE_DISCOVERY` includes `ecs` (`TSSERVE_ECS_CLUSTER`, `AWS_REGION`) or `lambda` (`AWS_REGION`). Each mode's variables are read only when that mode is listed.

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

When `TSSERVE_DISCOVERY` includes `ecs`, tsserve discovers backends by querying the AWS ECS API rather than a local Docker socket (the two modes can also run side by side). This is the deployment topology for clusters where Tailscale must not run on the container hosts.

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

### Multiple AWS accounts

A single credential context authenticates with one set of AWS credentials and therefore
discovers tasks in one account. To discover tasks across several accounts, set
`TSSERVE_ECS_ACCOUNTS` to a JSON array: tsserve builds one independent poller per entry,
each with its own credential context, and reconciles them all against the same set of
Tailscale Services.

Registration keys are the task ARN plus the container name, and task ARNs embed the account,
region, and cluster, so pollers never collide even when two accounts run identically-named
tasks. The single-account (`TSSERVE_ECS_ACCOUNTS` unset) path is unchanged.

Each array entry accepts these optional fields; a blank field inherits the shared default
from the single-account env vars:

| Field | Inherits from | Purpose |
|---|---|---|
| `name` | AWS account ID (resolved via `sts:GetCallerIdentity`) | Identity label in logs / startup summary. Must be unique across accounts; set it explicitly when two entries resolve to the same account ID (e.g. one account in two regions). |
| `cluster` | `TSSERVE_ECS_CLUSTER` | Cluster this poller watches. |
| `region` | `AWS_REGION` | AWS region for this account's clients. |
| `profile` | `TSSERVE_ECS_AWS_PROFILE` | AWS shared-config profile. |
| `configFile` | `TSSERVE_ECS_AWS_CONFIG_FILE` | AWS shared-config file path. |
| `accessKeyID` + `secretAccessKey` | — | Static credentials (set both or neither). |

A typical border-gateway setup grants the host an IAM reader role in each target account and
exposes one shared-config profile per account (named by account ID), letting tsserve resolve
the identities itself:

```jsonc
// TSSERVE_ECS_ACCOUNTS (single line in the systemd EnvironmentFile)
[
  {"profile": "ecs-reader-111", "cluster": "infra"},
  {"profile": "ecs-reader-222", "cluster": "ocopt-front", "region": "ap-southeast-2"}
]
```

Each account exposes the [IAM permissions](#iam-permissions) above through the role its
profile assumes; when `name` is omitted, that role must additionally allow
`sts:GetCallerIdentity` so tsserve can derive the account ID. As in single-account mode, these
profiles are scoped to the ECS clients only — tsnet's WIF auth flow keeps using the host's
default (instance-role) identity, never a cross-account reader role.

#### Reader resilience

One misconfigured reader never blocks the rest. If a reader's role is not assumable — a
cross-account trust policy not yet in place, so `sts:GetCallerIdentity` or `ListTasks` returns
`403 AccessDenied` — that reader does **not** abort daemon startup. It is created anyway, its
account identity is resolved lazily, and it retries on the slower `TSSERVE_ECS_RETRY_INTERVAL`
cadence (default `2m`) until the role becomes assumable, at which point it recovers on its own
with no restart. The other readers start and serve normally throughout.

Every configured reader — healthy, still resolving, or failing — appears in the **Readers**
table on the [status page](#observability-and-local-endpoints), with its cluster, region, resolved account ID, poll
interval, last-polled time, and current state. A failing reader is highlighted with its last
error, so an unassumable role is diagnosable at a glance rather than only in the logs. Each
service in the **Services** table is likewise tagged with the account and cluster it was
discovered from.

Only a hard *local* misconfiguration disables a reader outright (rather than retrying): AWS
config that will not load. Two readers resolving to the same account identity are logged at
Warn and both kept running. If every
reader hits such an error the daemon still runs — discovering nothing — instead of
crash-looping.

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

---

## Lambda Function Backends

When `TSSERVE_DISCOVERY` includes `lambda`, tsserve discovers AWS Lambda functions by their resource tags and serves each as a **function backend**. A function backend is not dialled: each request to it is translated into an event, delivered with one synchronous `Invoke` call to the Lambda API, and the function's return value is translated back into the response. Otherwise it is a backend like any other — it joins its service's backend pool, is served round-robin, and withdraws the service when it is the last member to leave.

### When to use this mode

- The workload is a small request/response HTTP function: a handful of routes, small JSON or form bodies, small responses, no streaming or WebSockets.
- The function should be reachable on the tailnet, with the caller's Tailscale identity, without a function URL, API Gateway, or load balancer in front of it.
- tsserve already runs on an AWS proxy host (typically for [ECS Cluster Mode](#ecs-cluster-mode)); `TSSERVE_DISCOVERY=ecs,lambda` serves ECS-backed and function-backed services from the one process.

### Deployment topology

```
┌──────────────────────────────────────────────────────────────┐
│ Border VPC                                                   │
│   ┌──────────────────────────────────────────────────────┐   │
│   │ Proxy host (EC2)                                     │   │
│   │   tsserve                                            │   │
│   │     tsnet.Server ───────────────▶ Tailscale (HTTPS)  │   │
│   │     Lambda reader ──▶ Tagging API, STS               │   │
│   │     Manager + ReverseProxy                           │   │
│   │       └─ invoker ──▶ Lambda API (Invoke)             │   │
│   └──────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────┘
                              │ regional AWS APIs
                              ▼
              Lambda function (any VPC config, or none;
              no function URL, no public endpoint)
```

All three APIs are regional AWS endpoints. The proxy host reaches them through the border VPC's internet egress, or privately through the `com.amazonaws.<region>.lambda`, `com.amazonaws.<region>.tagging` and `com.amazonaws.<region>.sts` interface VPC endpoints. The function needs no VPC attachment, no network path to or from the proxy host, and no public endpoint: the Lambda service delivers the invocation. Only STS is optional — see the IAM table below.

### Function tags

Functions are configured with AWS resource tags on the function, using the same `tsserve.*` keys as container labels:

| Tag | Required | Description |
|---|---|---|
| `tsserve.enable` | yes | Must be `true`. The reader asks the Tagging API only for functions carrying this tag and value. |
| `tsserve.service` | yes | The Tailscale service name; must start with `svc:`. As for containers, it must match a service defined in the Tailscale admin console. |
| `tsserve.caps` | no | Comma-separated app capability names, as for containers. See [App Capabilities](#app-capabilities). |
| `tsserve.qualifier` | no | A published version number, alias name, or `$LATEST` to invoke. Unset ⇒ the unqualified function (`$LATEST`) is invoked. |
| `tsserve.port`, `tsserve.network`, `tsserve.scheme` | — | Ignored on a function. A function has no port or network, and its scheme is always the internal `lambda`, which cannot be set by a tag or a container label. |

Lambda tags belong to the function, not to its versions or aliases, so one tag set covers every qualifier. A function whose tags fail validation — a missing or malformed `tsserve.service`, a malformed `tsserve.qualifier` — is logged at Warn once (quiet while the same failure persists), skipped, and re-examined on every poll. If it was already serving under an earlier, valid tag set, it stays registered until its tags are fixed or it disappears.

Tags can change on a live function. A change to a function's service, caps or qualifier is applied on the next poll as the backend moving: the old registration is withdrawn and the new one made. If the function was its service's only backend, the service is withdrawn and re-advertised (re-provisioning its certificate); if it shares a pool, new caps that differ from the pool's are refused by the [conflict rule](#2-service-manager) and retried each poll.

A function backend's registration key is its unqualified function ARN, and its **target** — shown in logs and on the status page — is `lambda://<function ARN>`, with `:<qualifier>` appended when one is set.

### Lifecycle (polling model)

Each Lambda reader polls one region with one credential context:

1. Every `TSSERVE_LAMBDA_POLL_INTERVAL` (default `30s`), call the Resource Groups Tagging API `GetResources` with resource type `lambda:function` and the tag filter `tsserve.enable=true`, following pagination to the last page. Tag values come from the same response; no Lambda API calls are made to discover functions.
2. Register every tagged function not yet registered, or whose tags changed.
3. Deregister every previously registered function missing from this poll — deleted, untagged, or `tsserve.enable` no longer `true`.

An initial poll runs at start. A reader whose poll fails is unhealthy and polls at the slower `TSSERVE_LAMBDA_RETRY_INTERVAL` (default `2m`) until it succeeds; a function whose registration failed or was refused is retried on every poll while it is still tagged. There is no event-driven path, so a tag change takes effect within one poll interval.

### API quota notes

`GetResources` returns up to 100 functions per call, so a reader makes one call per 100 tagged functions per poll. The Tagging API's `GetResources` rate limit is 15 calls/second per account and region, shared with every other caller in that account and region; at the 30s default even hundreds of tagged functions cost a fraction of a call per second. Discovery clients use the SDK's adaptive retry mode, as the ECS readers do, and a failed poll is reconciled by the next one.

`Invoke` is subject to the function's own concurrency and the account's Lambda quotas. Those are the function owner's concern; tsserve does not manage them, and reports a throttled `Invoke` as the `throttled` backend-error reason.

### Multiple accounts and regions

A Lambda reader discovers the functions of one account in one region. With `TSSERVE_LAMBDA_ACCOUNTS` unset, tsserve runs a single reader built from `AWS_REGION`, `TSSERVE_LAMBDA_AWS_PROFILE` and `TSSERVE_LAMBDA_AWS_CONFIG_FILE`, named `default`. To discover functions in several accounts, or in several regions of one account, set `TSSERVE_LAMBDA_ACCOUNTS` to a JSON array: tsserve builds one independent reader per entry.

Each entry accepts the fields of a [`TSSERVE_ECS_ACCOUNTS`](#multiple-aws-accounts) entry except `cluster`; a blank field inherits the shared default:

| Field | Inherits from | Purpose |
|---|---|---|
| `name` | AWS account ID (resolved via `sts:GetCallerIdentity`) | Identity label in logs, on the status page, and in each function backend's origin. Must be unique across entries. |
| `region` | `AWS_REGION` | Region this reader discovers and invokes functions in. |
| `profile` | `TSSERVE_LAMBDA_AWS_PROFILE` | AWS shared-config profile. |
| `configFile` | `TSSERVE_LAMBDA_AWS_CONFIG_FILE` | AWS shared-config file path. |
| `accessKeyID` + `secretAccessKey` | — | Static credentials (set both or neither). |

```jsonc
// TSSERVE_LAMBDA_ACCOUNTS (single line in the systemd EnvironmentFile)
[
  {"profile": "fn-reader-111"},
  {"name": "222-use1", "profile": "fn-reader-222", "region": "us-east-1"},
  {"name": "222-apse2", "profile": "fn-reader-222", "region": "ap-southeast-2"}
]
```

Because a reader is per region, the usual multi-region layout is several entries for one account with different `region`s. Their STS identities are equal, so **such entries need explicit, distinct `name`s**, as in the second and third entries above. Two readers resolving to the same identity are logged at Warn and both kept running.

A reader's profile is used for all three of its clients: discovery (Tagging, STS) and invocation (Lambda). A function is therefore invoked by the principal of the reader that discovered it — in a cross-account setup, the role that reader's profile assumes in the function's account. As for ECS, these profiles are scoped to the Lambda readers only; tsnet's WIF auth flow keeps using the host's default identity.

Lambda readers have the [reader resilience](#reader-resilience) of ECS readers: an unassumable role leaves that reader unhealthy and retrying at `TSSERVE_LAMBDA_RETRY_INTERVAL` while every other reader serves; only an AWS config that will not load disables a reader outright; and if every reader is disabled the daemon still runs, discovering nothing. Lambda readers appear in the status page's **Readers** table beside ECS readers, with mode `lambda`.

### IAM permissions

The principal each reader runs as — the host's IAM role, or the role its profile assumes — must permit:

| Action | Resource | Purpose |
|---|---|---|
| `tag:GetResources` | `*` (the action cannot be scoped to a resource) | Discover tagged functions and read their tags. |
| `lambda:InvokeFunction` | The functions to be served | Invoke them. Scope it with a `StringEquals` condition on `aws:ResourceTag/tsserve.enable` = `true`, or list the function ARNs. When functions use `tsserve.qualifier`, grant the qualified ARNs too (`arn:aws:lambda:<region>:<account>:function:<name>:*`): permission on the unqualified ARN does not cover a version or alias. |
| `sts:GetCallerIdentity` | `*` | Resolve the account ID used as the reader's name. Called only for a `TSSERVE_LAMBDA_ACCOUNTS` entry that omits `name`. AWS allows this call without an IAM grant; it needs only a reachable STS endpoint. |

tsserve never creates or modifies functions, aliases, tags, resource policies or IAM.

> **Warning — the forwarded identity headers are only as trustworthy as the function's invocation path.** Client-supplied copies of these headers never reach the backend — Tailscale's serve layer manages `Tailscale-User-*` and `Tailscale-App-Capabilities`, and tsserve strips inbound `Tailscale-Node-*` — so a function invoked by tsserve can rely on them. Anyone else who can invoke the function can put whatever headers they like in the event. A function that makes authorisation decisions from these headers must be invokable **only by tsserve's principal**: no function URL, no API Gateway or load-balancer integration, no resource-policy grant to another principal, and no other role holding `lambda:InvokeFunction` on it.

### Requests and responses

Each attempt against a function backend is one `Invoke` with `InvocationType=RequestResponse`, the function ARN, and the qualifier if one is set. The proxy's usual request handling — `X-Forwarded-*`, the identity headers, the [node identity headers](#node-identity-headers), metrics — runs first, so the function sees the headers a container backend would.

**Event.** The payload is the payload-format-2.0 event a Lambda function URL delivers:

| Field | Value |
|---|---|
| `version`, `routeKey` | `"2.0"`, `"$default"` |
| `rawPath`, `requestContext.http.path` | The request path as sent, percent-encoding preserved. |
| `rawQueryString`, `queryStringParameters` | The query string, and its decoded parameters with several values of one name joined by `,`. |
| `headers` | Every forwarded request header with a non-empty value, name lowercased, several values joined by `,`. `host` is the service FQDN the client addressed. `Cookie` is not repeated here. |
| `cookies` | The `Cookie` header split into one entry per `;`-separated pair. |
| `requestContext.domainName` | The service FQDN. |
| `requestContext.stage` | `"$default"` |
| `requestContext.http` | `method`, `path`, `protocol`, `sourceIp` (the caller's tailnet IP), `userAgent`. |
| `body`, `isBase64Encoded` | The request body, as text when the `Content-Type` is `text/*`, `application/json`, `application/x-www-form-urlencoded`, `application/xml` or a `+json`/`+xml` type, and base64-encoded with `isBase64Encoded: true` otherwise. Empty with no body. |

Other function-URL fields (`requestContext.accountId`, `requestId`, `time`, and so on) are left out.

**Response.** tsserve applies the function-URL inference rule and nothing more:

- If the function returns a JSON object containing `statusCode`, that is the response status; each entry of `headers` becomes a response header; each entry of `cookies` becomes a `Set-Cookie` header; and the body is `body`, base64-decoded when `isBase64Encoded` is `true`. `Content-Length` is set from the body; a `content-length` or `transfer-encoding` entry in `headers` is dropped.
- Any other return value — a string, number, array, `null`, or an object without `statusCode` — is a `200` with `Content-Type: application/json` and the returned JSON as the body.
- A structured result that cannot be used — a `statusCode` outside 100–999, or a `body` that is not valid base64 when `isBase64Encoded` is `true` — is a 502. tsserve does not default the `Content-Type` of a structured result; a function should set it.

### Limits containers do not have

- **Buffered bodies.** The request body is read in full before `Invoke`, and the response is complete before any of it reaches the client. A request body over 4 MiB is answered with `413 Payload Too Large` without invoking the function — one limit whatever the encoding, so that a base64-encoded body still fits Lambda's 6 MB synchronous payload maximum. A text body is JSON-escaped in the event, so one under the cap but dense with quotes, backslashes or control characters can still exceed Lambda's limit; `Invoke` then fails and the client gets a 502. Text bodies are expected to be UTF-8. A response over Lambda's own 6 MB limit fails in Lambda and reaches the client as a 502.
- **No WebSockets or streaming.** Protocol upgrades, server-sent events and response streaming are not supported. A function that returns `101` produces a 502.
- **Timeouts.** tsserve adds no response timeout; the function's own configured timeout governs, and a function that times out is a function error. Connecting to the Lambda endpoint is bounded by the same 5s budget as a container dial, so a missing VPC endpoint route costs one dial timeout. A client that cancels its request cancels the `Invoke` in flight.

### Failures and retries

| Outcome | Client sees | Backend-error `reason` | Retried against another pool member? |
|---|---|---|---|
| The function ran and failed (threw, timed out, or returned a response too large to deliver) | 502 | `function-error` | No — the function ran. The log line names the target and the function's `errorType` and `errorMessage`. |
| `Invoke` throttled (`TooManyRequestsException`) | 502 if not retried, or every member failed | `throttled` | Yes, for requests without a body. |
| Function not found (deleted since the last poll) or invoke permission denied | 502 if not retried, or every member failed | `other` | Yes, for requests without a body. A deleted function leaves the pool at the first poll that no longer lists it. |
| The Lambda endpoint could not be reached (dial, DNS, TLS handshake) | 502 if not retried, or every member failed | the connection reasons: `timeout`, `connection-refused`, `dns`, … | Yes, for requests without a body. |
| Any other failure — a 5xx from the Lambda API, a failure after the call was sent | 502 | `other`; `eof` or `timeout` when the connection dropped or timed out mid-call | No. |

This is the proxy's [retry rule](#3-reverse-proxy) unchanged: a request is retried against another member only when the attempt certainly did not run the function, and only when it carries no body. A throttle that applies account-wide fails every member alike. The Lambda client used for `Invoke` has the AWS SDK's own retries **disabled** — one HTTP attempt per call — because the SDK would re-send on a connection error or a 5xx without knowing whether the function ran (running a `POST` twice), and would hide throttling. The readers' discovery clients keep the SDK's adaptive retries.

### Homogeneous pools

A backend pool holds only container backends or only function backends. The first backend registered for a service fixes its kind: a service whose first backend is a function has the scheme `lambda`, and a container offered to it — or a function offered to an `http` or `https` service — is refused by the [conflict rule](#2-service-manager) like any scheme mismatch, logged at Warn and retried by its watcher. To move a service from containers to a function (or back), retag: once the last old backend leaves, the service is withdrawn and re-advertised by the first new one.

### Example

A function in account `111111111111`, served as `svc:tokens` from a proxy host running both ECS and Lambda discovery:

```
# Function tags
tsserve.enable    = true
tsserve.service   = svc:tokens
tsserve.qualifier = live
```

```
# /etc/tsserve/tsserve.env
TSSERVE_DISCOVERY=ecs,lambda
AWS_REGION=us-east-1
TSSERVE_ECS_CLUSTER=prod-apps
TSSERVE_LAMBDA_AWS_PROFILE=fn-reader
```

Within one poll interval the status page lists `svc:tokens` with backend `lambda://arn:aws:lambda:us-east-1:111111111111:function:tokens:live`, and `https://tokens.<tailnet>.ts.net/` returns the function's response.

---

## Core Components

`TSSERVE_DISCOVERY` lists the discovery modes to run, and every listed mode's watchers run side by side in one process against one Service Manager. The Docker and ECS watchers feed the same `Registrar` interface, registering container backends by `host:port`; the Lambda watcher registers function backends through a separate entry that carries each function's invoker instead of an address. Apart from the scheme a function-backed service is advertised with and the transport its requests go through, the manager treats both kinds alike. A terminal error from any mode's watcher ends the process, as it did when only one mode could run; a mode whose readers are all disabled by configuration errors reports nothing and leaves the other modes running.

### 1a. Docker Watcher (`docker`)

Connects to the Docker daemon via the Docker socket and monitors container lifecycle events.

**Responsibilities:**
- On startup, list all running containers and register any with valid `tsserve.*` labels.
- Subscribe to Docker events. React to `start`, `die`, and `stop` events.
- Between events, repeat the list/diff on a fixed sweep interval, registering labelled containers that are not serving and deregistering those that have gone.
- For each qualifying container, resolve its IP on the configured Docker network.
- Pass discovered service definitions to the Service Manager.
- Two or more containers sharing a `tsserve.service` label both serve it: each joins that service's backend pool, keyed by its container ID.

**Implementation notes:**
- Use the `github.com/docker/docker/client` Go SDK.
- Filter events to container types only.
- When a container starts, wait briefly (e.g. 1s) for its network to be ready before resolving its IP.
- Docker discovery is event-driven, with a reconciliation sweep behind it: every 30 seconds the watcher re-lists containers and brings the registered set into line with what Docker reports. A container that is running and labelled but not serving — because its registration failed or was refused for a conflict — is retried on that cadence and registers once the cause clears, with no restart and no new start event. A container that has gone is deregistered, and its retry state is dropped with it.
- Only record a container as active once `Register` reports the backend is serving, and log a failure at Warn once per distinct failure rather than once per sweep — the same rule the ECS watcher follows.

### 1b. ECS Watcher (`ecs`)

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
- Tasks sharing a `tsserve.service` value all back that service: replicas of one ECS service, containers of different ECS services, and tasks found by different readers (different accounts or clusters) are all members of the same backend pool.
- Only record a registration key as active once `Register` reports the backend is serving. A key whose registration failed or was refused is retried on every subsequent cycle while its task is still seen, and logged at Warn once per distinct failure rather than once per poll interval.
- Handle ECS API throttling: SDK retries with exponential backoff are sufficient at expected scales (a few `Describe*` calls per poll cycle).

### 1c. Lambda Watcher (`lambda`)

Polls the Resource Groups Tagging API for tagged functions and registers each as a function backend. See [Lambda Function Backends](#lambda-function-backends) for the user-facing description; this section covers implementation.

**Responsibilities:**
- Build one reader per `TSSERVE_LAMBDA_ACCOUNTS` entry (or one from the shared defaults), each with its own AWS credential context and region.
- On startup and every `TSSERVE_LAMBDA_POLL_INTERVAL`, list every function tagged `tsserve.enable=true` in the reader's region, consuming every page before acting, then register new or changed functions and deregister those no longer listed.
- Validate each function's tags and build its invoker, bound to the reader's Lambda client and the function's qualifier.
- Pass each function backend to the Service Manager with an origin naming the reader's account and region.

**Implementation notes:**
- Use `github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi` for discovery and `.../service/lambda` for invocation. Each reader loads its AWS config through the same loader as the ECS readers (region, profile, config file, static keys, adaptive retry), from `TSSERVE_LAMBDA_AWS_PROFILE` / `TSSERVE_LAMBDA_AWS_CONFIG_FILE` rather than the process-wide variables. The reader's Lambda client is built from the same credentials with retries disabled and the proxy's dial budget.
- Pagination ends when `GetResources` returns an empty `PaginationToken`. Deregistering before the last page is read would tear down every function on a later page each cycle.
- Use the unqualified function ARN as the registration key. Record what each active key was registered with — service, caps, qualifier — and treat a change in any of them as the backend moving: deregister, then register again.
- Do not use the container-label parser: it requires `tsserve.port`, which a function never has. Service and caps are validated by the same rules; `tsserve.port`, `tsserve.network` and `tsserve.scheme` are ignored.
- Only record a function as active once registration reports it serving; retry failures every cycle and log each distinct failure at Warn once. A function whose tags fail validation is not treated as gone, so a function already serving stays registered.
- Readers share the ECS readers' registry and resilience rules: an unassumable role leaves the reader unhealthy and retrying, resolving its account ID lazily; an AWS config that will not load disables it.

### 2. Service Manager

Owns the `tsnet.Server` instance and manages the mapping from Tailscale services to active listeners.

A **backend** is one discovered endpoint that can serve a service: a **container backend**, reached at a host and port, or a **function backend**, a Lambda function reached through the Lambda API. The watcher-supplied **registration key** identifies it (a Docker container ID, `taskArn#containerName` in ECS, or the unqualified function ARN). Its **target** — `scheme://host:port`, or `lambda://<function ARN>[:<qualifier>]` — names it in logs and on the status page. An **advertised service** is a Tailscale Service tsserve holds an open `ListenService` listener for, and its **backend pool** is the ordered set of backends currently registered against it. Scheme and caps are properties of the advertised service, not of any one backend.

**Responsibilities:**
- Maintain a map of `serviceName → advertised service`, each owning its `ServiceListener`, `http.Server`, reverse proxy, cancel function and backend pool, plus an index from registration key to service name.
- When a watcher reports a backend for a service that is **not yet advertised**:
  1. Build the `ServiceModeHTTP` config: always `HTTPS: true, Port: 443`. If `tsserve.caps` is set, parse the comma-separated capability names and set `AcceptAppCaps: map[string][]string{"/": caps}`.
  2. Call `tsnet.Server.ListenService(serviceName, mode)`. This tells tsnet to listen for HTTPS connections and automatically provision a TLS certificate from Let's Encrypt for the service's FQDN.
  3. Create one `httputil.ReverseProxy` for the service, reading its backend pool per request.
  4. Serve HTTP on the returned `ServiceListener` in a goroutine. Note: despite calling `http.Serve` (not `http.ServeTLS`), TLS is handled by tsnet's listener — the `ServiceListener` already terminates TLS before handing the request to the handler.
  5. Put the backend in the pool and record the advertised service.
- When a watcher reports a backend for a service that **is** advertised: add it to that service's pool. `ListenService` is called once per advertised service, never once per backend. Every backend carrying the label joins the pool regardless of provenance — several tasks of one ECS service, tasks of different ECS services, tasks found by different readers, and Docker containers; likewise several functions tagged with one service, from any Lambda reader.
- **Conflict rule.** A backend whose scheme differs from the advertised service's, or whose `tsserve.caps` differ from it as a set, is refused: it joins no pool and is recorded nowhere. Caps in particular cannot vary, because tsnet fixes them when the listener opens. A function backend's scheme is always `lambda`, so the same comparison keeps pools homogeneous: a container is refused by a function-backed service and a function by a container-backed one. The backend port is *not* compared — in ECS bridge mode each task has its own dynamic host port, which is exactly the shape a pool exists to serve. Once the pool empties, the next registration (including a previously refused one) advertises the service afresh with its own scheme and caps.
- When a watcher reports a backend has gone:
  1. Look up its service by registration key and remove that backend from the pool. The listener, the HTTP server and in-flight requests to other members are untouched.
  2. When the pool is now empty, close the `ServiceListener` (this stops accepting new connections) and drop the advertised service. The name is free for a fresh registration afterwards.
- **Register return contract.** Return "registered" only when the backend is a member of the pool of a service with an open listener. A refused conflict and a non-fatal `ListenService` failure both return an error the caller can tell apart, so a watcher never records a backend as active with nothing serving it. See [Error Handling](#error-handling) for what each watcher does with that error.

**Implementation notes:**
- `ListenService` can be called multiple times on the same `tsnet.Server` for different services — this is the core of how one node hosts many services.
- Use a `sync.Mutex` or similar to protect the service map, since events arrive asynchronously and several ECS readers can register against one service name concurrently. A registration that finds the name occupied by a transition already in flight — a listener being opened, or one being closed after the last backend left — must wait for that transition rather than act on its own. `ListenService` is refused while a handler for the service's port is still in the serve config, so a registration that opened a listener before the outgoing one had finished closing would fail outright and leave the service dark until discovery came round again.
- Serialise serve-config mutations across **all** service names, not just within one name. `ListenService` and the listener close that withdraws a service both edit the node's serve config as a read-modify-write guarded by an ETag, and tsnet neither locks nor retries: two such edits in flight together — two services advertising at once at start-up, or an advertisement racing another service's withdrawal — make one of them fail with an `etag mismatch`, leaving a running, labelled backend unserved. A second mutex, held only around those calls and never while the service-map mutex is held, is enough. Put it on the listener rather than on each caller: a service listener is closed by the manager's own teardown, by `http.Server.Close`, and by `http.Server.Serve` on its way out, and all three withdraw the handler. The cost is that advertisement becomes sequential — start-up advertises services one at a time, and a slow or hung `tailscaled` delays other services' advertisements and withdrawals rather than just the one registration waiting on it. Serialisation does not make the retry paths optional: `tailscaled` can rewrite the serve config independently of tsserve, so a failed registration must stay recoverable by the watchers.
- Serving a request must never wait on a `ListenService` call or a listener teardown. Discovery writes a pool at most once per task or container change while the request path reads it on every request, so the pool is published copy-on-write and read without a lock.
- Use `context.Context` cancellation for clean goroutine shutdown.

### 3. Reverse Proxy

One Go `net/http/httputil.ReverseProxy` per advertised service, shared by every backend in its pool. A container-backed service's proxy sends each attempt through one shared `http.Transport`; a function-backed service's proxy hands each attempt to the chosen member's **invoker**, which performs it as one Lambda `Invoke` (see [Requests and responses](#requests-and-responses)).

**Responsibilities:**
- Forward each request to a backend chosen from the service's pool, round-robin in pool order. The rotation advances once per request. A backend added to the pool starts receiving requests on the next request; one removed receives no further requests. No proxy is built per backend or per request.
- **Retry rule.** When an attempt fails *before the request was written to the backend* (dial refused, dial timeout, no route, DNS failure, TLS handshake failure — or, for a function backend, any failure that means the function did not run: see [Failures and retries](#failures-and-retries)) and the request carries no body, try the remaining pool members in rotation order, at most once each, and return the first successful response. In every other case — a failure after the request was written, or a request carrying a body — there is no retry and the client gets a 502. Retrying is done at the `RoundTrip` boundary, where nothing has yet been written to the client, so it can never replay a request a backend has already acted on. Server requests have no rewindable body, which is why a request carrying one is never retried; clients needing that reliability retry themselves.
- Bound connection establishment to a few seconds (5s) for `http` and `https` services, and for the connection to the Lambda endpoint, so a blackholed member costs a request one dial timeout rather than the Go default of 30s before the next member is tried.
- Set `X-Forwarded-For`, `X-Forwarded-Proto` headers (Go's ReverseProxy does `X-Forwarded-For` by default). For a container backend, the outbound URL host and `Host` header name the backend that actually receives each attempt; a function backend has no host of its own, so its event carries the service FQDN the client addressed.
- Inject [node identity headers](#node-identity-headers) (`Tailscale-Node-Name` for every peer, `Tailscale-Node-Tags` for tagged peers), resolved via `LocalClient.WhoIs`, and strip any inbound copies of those headers first to prevent spoofing.
- Log proxy errors, naming the backend each failed attempt targeted.

**Implementation notes:**
- Override the `ErrorHandler` to log errors and return 502. It is not the place to retry: it is also reached after a backend has returned `101 Switching Protocols`, and after the client connection has been hijacked.
- Decide whether an attempt failed before the request was written from the error's type, not its text — string matching cannot tell a dial timeout from a read timeout on a request that was already sent.
- If `tsserve.scheme=https`, set the proxy target scheme to `https` and configure the transport to skip TLS verification (for self-signed backend certs). The transport is shared by all pool members. Function backends do not use it: their Lambda client verifies the Lambda endpoint's certificate as usual.
- A hijacked (protocol-upgrade) connection stays bound to the backend that accepted it for its lifetime. Removing that backend from the pool does not close the connection; it ends when the backend does.
- There are no health checks: a backend stays in the pool until discovery removes it, and per-request retry covers the window in between.

---

## Lifecycle Handling

The "Container Start / Stop / Restart" subsections below describe Docker mode. The ECS and Lambda lifecycles are poll-driven and are described in [ECS Cluster Mode → Lifecycle (polling model)](#lifecycle-polling-model) and [Lambda Function Backends → Lifecycle (polling model)](#lifecycle-polling-model-1). The shared parts — startup, shutdown, and how the Service Manager reacts to register/deregister calls — are documented here.

### Startup

1. Parse environment variables. Validate that at least one authentication method is configured.
2. Initialise `tsnet.Server` with all configured fields (`Hostname`, `Dir`, `AuthKey`, `ClientID`, `ClientSecret`, `IDToken`, `Audience`, `AdvertiseTags`). Call `srv.Start()`. tsnet handles authentication internally — OIDC token exchange, OAuth key generation, or auth key login.
3. Wait for tsnet to be ready (connected to tailnet).
4. Obtain `LocalClient` via `srv.LocalClient()` for the status-page tailnet view.
5. Parse `TSSERVE_DISCOVERY` into its list of modes (`docker`, `ecs`, `lambda`), rejecting an unknown, empty or repeated entry. Each mode reads and validates its own variables only when it is listed.
6. Start every listed mode's watchers. Each performs an initial enumeration (Docker: `ContainerList` + event subscription; ECS: `ListTasks` + the first poll; Lambda: the first `GetResources` poll) and registers any qualifying backends. Every watcher then keeps repeating that enumeration — the ECS and Lambda poll intervals, the Docker sweep interval — so a backend that failed to register the first time is not stranded.

### Container Start

1. Docker Watcher receives `start` event.
2. Inspect container for `tsserve.*` labels. Ignore if missing or `tsserve.enable != "true"`.
3. Resolve container IP on the specified Docker network.
4. Pass service definition to Service Manager.
5. If the service is not yet advertised, the Service Manager calls `ListenService` and starts the reverse proxy goroutine; otherwise the container joins the existing service's backend pool.
6. Log: `"service advertised: svc:web -> 172.17.0.3:80"`, or `"backend joined service"` with the resulting pool size.

### Container Stop

1. Docker Watcher receives `die` or `stop` event.
2. Look up the container's service by registration key and remove it from that service's backend pool.
3. If other backends remain, that is all: the listener stays open and requests keep being served by the rest of the pool.
4. If the pool is now empty, close the `ServiceListener`. In-flight requests will complete; new connections are refused. Drop the advertised service.
5. Log: `"backend left service"` with the resulting pool size, or `"service withdrawn; last backend left"`.
6. A container that disappears without an event — a dropped event stream, a daemon restart — is picked up by the next sweep instead: it is no longer listed, so it is deregistered by the same steps, logged as `"container gone; deregistered"`.

### Container IP Change (Restart)

When a container restarts, Docker emits `die` then `start`. The stop handler removes the old backend from the pool, and the start handler adds one with the new container IP. If it was the only backend, the listener is torn down and re-advertised, re-provisioning the service's certificate. No special handling needed.

### tsserve Shutdown

1. Receive `SIGINT` or `SIGTERM`.
2. Close all active `ServiceListener`s.
3. Call `tsnet.Server.Close()`.
4. Exit.

---

## Error Handling

| Scenario | Behaviour |
|---|---|
| `ListenService` fails (e.g. service not defined, node untagged, serve-config `etag mismatch`) | `Register` returns an error; the backend is not recorded as active. Do not crash. Every watcher logs it at Warn on the first failure (quiet while the same failure persists) and retries the container, task or function on its next cycle — the ECS or Lambda poll interval, or the Docker sweep interval — so a failure is recovered from without a restart. |
| `ListenService` fails because HTTPS/MagicDNS not enabled | Fatal error on first occurrence. Exit with a clear message telling the user to enable HTTPS and MagicDNS in the admin console. |
| Container IP cannot be resolved | Log warning (Debug while the same failure repeats, so a sweep does not warn on every cycle). Skip this container. Retry on the next Docker event for this container, or on the next sweep. |
| Backend unreachable (container crashed but event not yet received) | The reverse proxy tries the remaining pool members when the request carries no body, and returns the first successful response. When every member fails, or the request carries a body, the client gets a 502. Normal behaviour. |
| Docker socket unavailable at startup (Docker mode) | Fatal error. Exit with message. |
| ECS `DescribeTasks` returns `AccessDeniedException` (ECS mode) | Fatal error on first occurrence. Exit with message naming the IAM action that was denied. |
| ECS `TSSERVE_ECS_CLUSTER` does not exist (ECS mode) | Fatal error. Exit with message. |
| ECS API throttling (`ThrottlingException`, `RequestLimitExceeded`) | Rely on SDK retry-with-backoff. If a poll cycle still fails after retries, log a warning and continue; the next poll will catch up. Do not crash. |
| Bridge-mode task with no `networkBinding` matching `tsserve.port` | Log warning naming the task ARN and labelled port. Skip this container. Retry on next poll cycle. |
| awsvpc-mode task with no `ElasticNetworkInterface` attachment yet | Log debug message. Skip this poll. The next cycle will pick it up once the ENI is attached. |
| `ec2:DescribeInstances` fails for a container instance (ECS mode) | Log warning. Skip this container. Cached host IPs are invalidated on failure so the next poll retries cleanly. |
| `tsnet.Server.Start()` fails (e.g. bad auth key, OIDC token exchange failure, expired credentials) | Fatal error. Exit with message. For OIDC failures, suggest checking the federated identity configuration in the Tailscale admin console. |
| Same `tsserve.service` on two containers (Docker mode), two tasks (ECS mode) or two functions (Lambda mode) | Both join the service's backend pool and both receive traffic. The service stays advertised while either remains. |
| A backend's scheme or `tsserve.caps` disagree with the advertised service — including a container offered to a function-backed service, or a function to a container-backed one | The backend is refused and not added to the pool; existing members keep serving. Logged at Warn by the watcher. Retried per the `ListenService` row above. |
| Last pool member leaves before its replacement is discovered | The service is torn down and re-advertised when the replacement appears, re-provisioning its certificate. Within one ECS poll cycle the watcher registers new tasks before deregistering missing ones, so a replacement that is already RUNNING joins the pool before the old member leaves. |
| Function error: the function threw, timed out, or returned a response over Lambda's limit | 502. Logged with the target and the function's `errorType` and `errorMessage`; counted as `function-error`. Not retried against another member — the function ran. |
| `Invoke` throttled (`TooManyRequestsException`) | Counted as `throttled`. A request without a body is retried against the remaining pool members; otherwise, or when every member is throttled, 502. The SDK does not retry `Invoke` itself. |
| Request body over a function backend's 4 MiB cap | `413 Payload Too Large` from tsserve; the function is not invoked and no backend error is counted. |
| Function deleted between polls, or `lambda:InvokeFunction` denied | The function did not run: a request without a body is retried against the remaining pool members; otherwise, or when none succeeds, 502. A deleted function is deregistered at the first poll that no longer lists it. |
| Tagging API `GetResources` fails (Lambda mode) | The reader is marked unhealthy on the status page with the error, and retries at `TSSERVE_LAMBDA_RETRY_INTERVAL`. Functions it already registered stay registered until a poll succeeds without them. Do not crash. |
| Function tags fail validation (missing or malformed `tsserve.service`, malformed `tsserve.qualifier`) | Logged at Warn naming the function ARN, once while the failure persists. Skipped and re-examined each poll; a function already serving under earlier valid tags stays registered. |
| A discovery mode fails terminally (e.g. the Docker event stream fails) while other modes are running | The process exits with that error, as it does when a single mode fails, withdrawing every service — including those the other modes discovered. |

---

## Node state store

By default tsnet writes the node's identity — machine key, node key, and prefs —
to a `tailscaled.state` file under `TSSERVE_STATE_DIR`. On an ephemeral
filesystem that identity is lost on restart, so the node re-registers as a new
machine each time.

`TSSERVE_STATE_STORE` sets tsnet's `Server.Store` to an external
[`ipn.StateStore`](https://pkg.go.dev/tailscale.com/ipn/store), moving that
identity blob off local disk. The value is passed to `store.New`; the primary
target is AWS SSM Parameter Store:

```
TSSERVE_STATE_STORE=arn:aws:ssm:us-east-1:123456789012:parameter/tsserve/node1
```

An optional `?kmsKey=<alias|id|arn>` encrypts the parameter with a specific KMS
key (otherwise the account default SSM key is used).

How this interacts with `TSSERVE_STATE_DIR` (see `tsnet.Server.Start`):

- `Store` gates **only** whether tsnet creates the on-disk `tailscaled.state`
  file. When set, that file is never written; the identity lives in the store.
- `Dir` (`TSSERVE_STATE_DIR`) is **always** tsnet's var root regardless of
  `Store`: the directory is still created, and the TLS cert directory
  (`<dir>/certs`) and `tailscaled.log.conf` still live under it. So
  `TSSERVE_STATE_DIR` remains required and meaningful.

This composes with the [TLS certificate cache](#tls-certificate-cache) to make
both halves durable on ECS/Fargate: **identity → SSM, certs → disk mirrored to
S3.** The whole identity blob is a single SSM parameter and must stay under the
8 KB advanced-tier limit — not a concern for a normal single-profile node, whose
state is a few KB.

### IAM permissions

The host's IAM role must permit, scoped to the parameter ARN:

| Action | Purpose |
|---|---|
| `ssm:GetParameter` | Load node identity at startup. |
| `ssm:PutParameter` | Persist identity on change (and create it on first run). |

Add `kms:Encrypt` / `kms:Decrypt` on the key if `?kmsKey=` is used.

---

## TLS certificate cache

tsnet provisions TLS certificates for each served FQDN from Let's Encrypt (via
Tailscale's ACME flow) and caches them as files under `<TSSERVE_STATE_DIR>/certs`
— one `<domain>.crt` and `<domain>.key` per service, plus a shared
`acme-account.key.pem`. On an ephemeral filesystem (ECS/Fargate) that directory
is discarded on every restart, so each fresh task re-runs the ACME flow and can
hit Let's Encrypt [rate limits](https://letsencrypt.org/docs/rate-limits/).

Setting `TSSERVE_CERT_S3_BUCKET` turns that directory into a cache backed by S3:

- **Restore on startup.** Before `tsnet.Server.Start()`, the cert objects under
  `TSSERVE_CERT_S3_PREFIX` are downloaded into `<TSSERVE_STATE_DIR>/certs`. A
  restored certificate is bound to the FQDN, not the node identity, so it stays
  valid even though a fresh task registers a new node key. An empty or missing
  bucket is not an error — there is simply nothing to restore.
- **Upload on change.** A filesystem watch on the certs directory uploads any
  cert tsnet writes (issuance or renewal) within a couple of seconds, and a
  final sweep runs on graceful shutdown (`SIGTERM`/`SIGINT`) so a cert issued
  just before a task is replaced still reaches the bucket.

Scope and limits, by design:

- **Certs only.** Only `*.crt`, `*.key`, and `acme-account.key.pem` are
  mirrored. Node identity (`tailscaled.state`) is **not** synced — a restarted
  task re-registers as a new node. This keeps the node's private key off S3;
  the tradeoff is that node identity is not preserved across restarts.
- **Overwrite-only.** A renewal reuses the same object key; nothing is ever
  deleted from the bucket. Prune stale objects out of band if needed.
- **One prefix per node.** Use `TSSERVE_CERT_S3_PREFIX` to share one bucket
  across multiple nodes without collisions.

The cache's S3 client loads its own AWS config (region from
`TSSERVE_CERT_S3_REGION`, else `AWS_REGION`), deliberately separate from tsnet's
credential resolution so it never perturbs the workload-identity flow — the same
isolation the ECS discovery client uses.

### IAM permissions

The host's IAM role must permit, scoped to the bucket (and prefix) in use:

| Action | Purpose |
|---|---|
| `s3:ListBucket` | Enumerate cert objects under the prefix at startup (restore). |
| `s3:GetObject` | Download each cert object into the local cache. |
| `s3:PutObject` | Upload certs on issuance, renewal, and shutdown. |

`s3:ListBucket` is granted on the bucket ARN; `s3:GetObject`/`s3:PutObject` on
the object ARN (e.g. `arn:aws:s3:::my-bucket/tsserve/prod/*`). No delete
permission is required.

---

## Observability and local endpoints

tsserve exposes a small in-process control surface on **two** local listeners. The split is deliberate: status and dashboards are tailnet-scoped (any peer that can resolve the node's hostname can reach them), while Prometheus metrics stay on the host loopback by default so scrape traffic never traverses the tailnet.

### Tailnet HTTPS listener — `:443`

Available at `https://<TSSERVE_HOSTNAME>.<tailnet>.ts.net/`. Uses `tsnet.Server.ListenTLS`, which provisions a TLS certificate for the node's own hostname automatically — the same MagicDNS/HTTPS Certificates prerequisites the service listeners already require. The listener is always on; no env var disables it.

| Path | Purpose |
|---|---|
| `/` | Single-page HTML status: tailnet identity (hostname, FQDN, tailnet name, MagicDNS suffix, node IPs, backend state), discovery modes, uptime, build info, and a table of currently registered services (service name → backend → caps → account → cluster → key → registered time). The backend is the backend's target, so a function backend shows its `lambda://` target, `—` for cluster, and the function name as its key. When any ECS or Lambda reader is configured it additionally renders a **Readers** table — one row per configured reader (name → mode → account ID → cluster → region → poll interval → last polled → state), with unhealthy readers highlighted and their last error shown, so an unassumable role is visible at a glance. A Lambda reader's cluster is `—`. No JavaScript, no external assets. |
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
| `tsserve_proxy_backend_errors_total` | Counter | `service`, `reason` | One increment per failed attempt against a backend, so a request retried against a second member contributes more than one. `reason` is a coarse classification: `timeout`, `connection-refused`, `dns`, `eof`, `no-backend`, `function-error` (a function backend's function ran and failed), `throttled` (a function backend's `Invoke` was throttled), `other`. A request answered with a 413 because its body exceeds a function backend's cap is not a backend error. |
| `tsserve_services_active` | Gauge | — | Number of Tailscale Services currently advertised. Backends joining or leaving an advertised service do not change it. |
| `tsserve_service_backends` | Gauge | `service` | Number of backends currently in an advertised service's pool. The series is removed when the service is torn down. |
| `tsserve_build_info` | Gauge | `version`, `revision`, `go_version` (const) | Constant `1`. Useful for grouping in dashboards. |

Standard `go_*` and `process_*` collectors are also registered.

---

## Project Structure

```
tsserve/
├── main.go              # Entry point, signal handling, discovery-mode startup, wiring
├── docker/
│   └── watcher.go       # Docker event subscription and container inspection
├── labels/
│   └── labels.go        # Label parsing and validation (shared by docker/, ecs/ and lambda/)
├── ecs/
│   ├── watcher.go       # ECS poll-and-diff loop; drives the same Registrar
│   ├── resolve.go       # Backend resolution: bridge (host:hostPort) vs awsvpc (eni:containerPort)
│   └── cache.go         # Task-definition and container-instance → host-IP caches
├── lambda/
│   ├── watcher.go       # Lambda reader: Tagging API poll-and-diff loop, function-tag validation
│   └── account.go       # TSSERVE_LAMBDA_ACCOUNTS parsing and reader planning
├── proxy/
│   ├── manager.go       # Service Manager: tsnet.Server + active service map
│   ├── reverseproxy.go  # Reverse proxy factory with error handling
│   └── invoker.go       # Function backend invoker: HTTP request ↔ Lambda Invoke translation
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

The `Registrar` interface declared in `docker/watcher.go` is the seam between discovery and proxying. The ECS watcher implements no new contract; it satisfies the same interface. The Lambda watcher registers through its own entry point, since a function backend carries an invoker rather than an address. Label parsing is shared — `labels/labels.go` operates on `map[string]string`, which is the shape of both Docker's `Config.Labels` and ECS's `containerDefinitions[*].dockerLabels`; the Lambda watcher reuses its service and caps validation for function tags.

---

## Dependencies

| Dependency | Purpose |
|---|---|
| `tailscale.com/tsnet` | Embedded Tailscale node, `ListenService` API, `LocalClient` for the status page tailnet view |
| `tailscale.com/client/tailscale` | `LocalClient` type used by the status page |
| `github.com/docker/docker/client` | Docker daemon API client (Docker discovery mode) |
| `github.com/docker/docker/api/types` | Docker API types for events and container inspection |
| `github.com/aws/aws-sdk-go-v2/config` | AWS SDK config resolution (ECS and Lambda discovery modes) |
| `github.com/aws/aws-sdk-go-v2/service/ecs` | ECS API client: `ListTasks`, `DescribeTasks`, `DescribeTaskDefinition`, `DescribeContainerInstances` |
| `github.com/aws/aws-sdk-go-v2/service/ec2` | EC2 API client: `DescribeInstances` for bridge-mode host IP resolution |
| `github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi` | Tagging API client: `GetResources` for Lambda function discovery |
| `github.com/aws/aws-sdk-go-v2/service/lambda` | Lambda API client: `Invoke` for function backends |
| `github.com/prometheus/client_golang` | Prometheus collectors and `promhttp` exposition handler |
| Go stdlib `net/http/httputil` | Reverse proxy |
| Go stdlib `html/template` | Status page rendering |
| Go stdlib `log/slog` | Structured logging |

Notes:
- `tailscale.com/client/tailscale` is pulled in transitively by `tailscale.com/tsnet` — it's not an additional module dependency, just an additional import path within the same module.
- The AWS SDK is imported by the `ecs/`, `lambda/` and `certsync/` packages, by the function backend invoker in `proxy/`, and by the startup wiring. Every build includes every watcher.

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

The capability names in the ACL grants must match those listed in the backend's `tsserve.caps` label or function tag.

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
- **Function URLs and API Gateway** — invoking a function through an IAM-gated function URL, or fronting it with API Gateway or a load balancer, instead of the Lambda `Invoke` API.
- **Response streaming for function backends** — `InvokeWithResponseStream`, server-sent events, and bodies beyond the buffered limits.
- **Mixed pools** — a service backed by both containers and functions at once, for a gradual cutover between them.
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
