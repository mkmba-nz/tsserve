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
| `tsserve.caps` | no | — | Comma-separated app capability names. |

## Environment variables

Authentication (one of):

- `TS_AUTHKEY` — pre-issued auth key for a tagged node (local dev).
- `TS_CLIENT_ID` + `TS_CLIENT_SECRET` + `TSSERVE_TAGS` — OAuth client.
- `TS_CLIENT_ID` + `TS_AUDIENCE` (or `TS_ID_TOKEN`) + `TSSERVE_TAGS` —
  workload identity federation.

General:

- `TSSERVE_HOSTNAME` (default `tsserve`)
- `TSSERVE_STATE_DIR` (default `/var/lib/tsserve`)
- `TSSERVE_LOG_LEVEL` (`debug` | `info` | `warn` | `error`)

## Prerequisites in the Tailscale admin console

1. Enable **HTTPS Certificates** and **MagicDNS** under DNS.
2. Define each `svc:` under Services on TCP/443.
3. Set up `tagOwners`, `autoApprovers.services`, and `grants` in your ACL.

See [SPEC.md](./SPEC.md#tailscale-admin-setup-user-prerequisite) for examples.

## Build

```sh
go build .
# or
docker build -t tsserve .
```
