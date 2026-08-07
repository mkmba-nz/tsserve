# tsserve

A single-binary Docker → Tailscale Services proxy built on `tsnet`.

`tsserve` watches Docker for containers labelled with `tsserve.*`, registers
them as Tailscale Services using `tsnet.Server.ListenService`, and reverse
proxies HTTPS traffic to them. All services share one tailnet node, so the
device list stays clean.

See [SPEC.md](./SPEC.md) for the full design.

## Quick start

```yaml
services:
  tsserve:
    image: ghcr.io/you/tsserve:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - tsserve-state:/var/lib/tsserve
    environment:
      - TS_AUTHKEY=${TS_AUTHKEY}

  web:
    image: nginx:latest
    labels:
      - tsserve.enable=true
      - tsserve.service=svc:web
      - tsserve.port=80

volumes:
  tsserve-state:
```

`svc:web` is reachable at `https://web.<tailnet>.ts.net` once the service is
defined in the Tailscale admin console.

## Labels

| Label | Required | Default | Description |
|---|---|---|---|
| `tsserve.enable` | yes | — | Must be `"true"` to opt in. |
| `tsserve.service` | yes | — | Tailscale service name, e.g. `svc:web`. |
| `tsserve.port` | yes | — | Container port to proxy to. |
| `tsserve.network` | no | `bridge` | Docker network for IP resolution. |
| `tsserve.scheme` | no | `http` | `http` or `https` for the backend. |
| `tsserve.caps` | no | — | Comma-separated app capability names. Also enables node identity headers (see below). |

When `tsserve.caps` is set, tsserve additionally resolves each connecting peer
(in-process `whois`) and, for **tagged nodes**, injects two headers so the
backend can do per-device accounting/routing:

- `Tailscale-Node-Tags` — the node's ACL tags, comma-separated, `tag:` prefix
  stripped (e.g. `github-runner,prod`).
- `Tailscale-Node-Name` — the node's hostname (`ComputedName`).

Untagged (user) nodes get neither header. Both headers are stripped from inbound
requests before forwarding, so clients cannot spoof them.

## Environment variables

Authentication (one of):

- `TS_AUTHKEY` — pre-issued auth key for a tagged node (local dev).
- `TS_CLIENT_ID` + `TS_CLIENT_SECRET` + `TSSERVE_TAGS` — OAuth client.
- `TS_CLIENT_ID` + `TS_AUDIENCE` (or `TS_ID_TOKEN`) + `TSSERVE_TAGS` —
  workload identity federation.

General:

- `TSSERVE_HOSTNAME` (default `tsserve`)
- `TSSERVE_STATE_DIR` (default `/var/lib/tsserve`) — tsnet's var root; holds the
  cert directory and (by default) the node identity file.
- `TSSERVE_STATE_STORE` — move node identity (machine/node key) off local disk
  into an external store, e.g. an AWS SSM parameter ARN
  `arn:aws:ssm:<region>:<acct>:parameter/<name>`. `TSSERVE_STATE_DIR` is still
  used for certs. Requires `ssm:GetParameter`, `ssm:PutParameter`. Combine with
  `TSSERVE_CERT_S3_BUCKET` for durable identity + certs on ephemeral hosts.
- `TSSERVE_LOG_LEVEL` (`debug` | `info` | `warn` | `error`)

Certificate cache (optional; for ephemeral filesystems like ECS/Fargate):

- `TSSERVE_CERT_S3_BUCKET` — back tsnet's TLS cert cache with S3. Certs are
  restored from the bucket at startup and re-uploaded on issuance, renewal, and
  shutdown, so restarts reuse existing Let's Encrypt certs instead of re-running
  ACME. Unset ⇒ certs stay on local disk only. Only certificates are synced, not
  node identity. Requires `s3:ListBucket`, `s3:GetObject`, `s3:PutObject`.
- `TSSERVE_CERT_S3_PREFIX` — optional key prefix, to share one bucket across
  nodes.
- `TSSERVE_CERT_S3_REGION` (default `AWS_REGION`) — region for the cache client.

Observability:

- `TSSERVE_METRICS_ADDR` (default `127.0.0.1:9090`) — loopback HTTP listener
  serving `/metrics` for Prometheus. Set to `""` to disable; set to
  `0.0.0.0:9100` to expose to a remote scraper (you own the firewall).
- `TSSERVE_TRAEFIK_PORT` — if set, the tailnet HTTPS listener proxies
  `/traefik` to `http://localhost:<port>` (useful when tsserve runs on a
  gateway host alongside Traefik).

## Local endpoints

In addition to the per-service Tailscale listeners, tsserve always runs:

- `https://<TSSERVE_HOSTNAME>.<tailnet>.ts.net/` — status page (tailnet
  identity, discovery mode, registered backends, build info).
- `https://<TSSERVE_HOSTNAME>.<tailnet>.ts.net/traefik/...` — reverse proxy
  to `http://localhost:<TSSERVE_TRAEFIK_PORT>` when configured.
- `http://<TSSERVE_METRICS_ADDR>/metrics` — Prometheus exposition, default
  `127.0.0.1:9090`.

See [SPEC.md → Observability and local endpoints](./SPEC.md#observability-and-local-endpoints) for the metric inventory and a caveat about Traefik dashboard path-prefix behavior.

## Prerequisites in the Tailscale admin console

1. Enable **HTTPS Certificates** and **MagicDNS** under DNS.
2. Define each `svc:` under Services on TCP/443.
3. Set up `tagOwners`, `autoApprovers.services`, and `grants` in your ACL.

See [SPEC.md](./SPEC.md#tailscale-admin-setup-user-prerequisite) for examples.

## Build

```sh
make            # builds ./tsserve for the host arch
# or
docker build -t tsserve .
```

## Install as a systemd service

`make install` lays down everything needed to run tsserve as a systemd
service on Linux:

- `/usr/local/bin/tsserve` — the binary.
- `/etc/systemd/system/tsserve.service` — hardened unit, enabled on boot.
- `/etc/tsserve/tsserve.env.example` — annotated example of every supported
  env var.
- A dedicated `tsserve` system user.

Intended to be invoked from an image-build pipeline (Packer, Ansible, etc.)
that has checked this repo out into a temp directory on the target host:

```sh
make
sudo make install   # installs to /
```

The service is enabled-on-boot but **not** started — there is no config
file yet. Provide `/etc/tsserve/tsserve.env` at first launch (e.g.
cloud-init / user_data) and run `systemctl restart tsserve`. See
`systemd/tsserve.env.example` for every supported variable.
