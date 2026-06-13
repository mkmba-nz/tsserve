#!/usr/bin/env bash
# tsserve Tier 2 smoke test.
#
# Brings up the docker compose stack, waits for both labelled services to be
# registered, then exercises the tailnet from the host (which must be logged in
# to the same tailnet as tsserve).
#
# Usage:
#   cp .env.example .env  # then edit
#   ./smoke.sh            # bring up + test
#   ./smoke.sh down       # stop containers, KEEP state volume (no re-auth next run)
#   ./smoke.sh clean      # stop containers AND remove state volume (forces re-auth)
#   ./smoke.sh logs       # follow tsserve logs
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$SCRIPT_DIR"

# Load .env if present.
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

TS_AUTHKEY=${TS_AUTHKEY:?TS_AUTHKEY must be set (in .env or environment)}
TAILNET_SUFFIX=${TAILNET_SUFFIX:?TAILNET_SUFFIX must be set, e.g. tailXXXX.ts.net}
TSSERVE_HOSTNAME=${TSSERVE_HOSTNAME:-tsserve-tier2}
HOST_METRICS_PORT=${HOST_METRICS_PORT:-9090}

# Optional tagged-peer validation (Tailscale-Node-Tags/-Name). Enabled when a
# tagged auth key is provided; selects the "tagged-client" compose profile.
CLIENT_NODE_TAG=${CLIENT_NODE_TAG:-tsserve}
CLIENT_HOSTNAME=${CLIENT_HOSTNAME:-tsserve-tier2-client}
PROFILE_ARGS=()
if [[ -n "${TS_CLIENT_AUTHKEY:-}" ]]; then
  PROFILE_ARGS=(--profile tagged-client)
fi

WEB_URL="https://tsserve-test-web.${TAILNET_SUFFIX}"
ECHO_URL="https://tsserve-test-echo.${TAILNET_SUFFIX}"
CAPS_URL="https://tsserve-test-caps.${TAILNET_SUFFIX}"
STATUS_URL="https://${TSSERVE_HOSTNAME}.${TAILNET_SUFFIX}"
METRICS_URL="http://127.0.0.1:${HOST_METRICS_PORT}/metrics"

# Colors
G=$'\e[32m'; R=$'\e[31m'; Y=$'\e[33m'; C=$'\e[36m'; N=$'\e[0m'
say()  { printf "${C}==>${N} %s\n" "$*"; }
ok()   { printf "${G}ok${N}  %s\n" "$*"; }
fail() { printf "${R}FAIL${N} %s\n" "$*" >&2; FAILED=1; }
warn() { printf "${Y}warn${N} %s\n" "$*"; }

FAILED=0

case "${1:-up}" in
  down)
    say "stopping stack (state volume preserved — use 'clean' to also wipe)"
    docker compose --profile tagged-client down
    exit 0
    ;;
  clean)
    say "stopping stack and removing state volume (next run will need a fresh auth key)"
    docker compose --profile tagged-client down -v
    exit 0
    ;;
  logs)
    docker compose logs -f tsserve
    exit 0
    ;;
  up)
    ;;
  *)
    echo "usage: $0 [up|down|clean|logs]" >&2
    exit 2
    ;;
esac

say "bringing up stack"
if [[ ${#PROFILE_ARGS[@]} -gt 0 ]]; then
  say "  (tagged-client profile enabled — TS_CLIENT_AUTHKEY is set)"
fi
docker compose ${PROFILE_ARGS[@]+"${PROFILE_ARGS[@]}"} up -d --build

trap 'say "tail of tsserve logs (on exit):"; docker compose logs --tail=50 tsserve || true' EXIT

# --- 1. Wait for tsserve to register all three services ---------------------
say "waiting for tsserve to register all three backends (up to 90s)"
for i in $(seq 1 90); do
  if docker logs tsserve-tier2 2>&1 | grep -q "service registered.*svc:tsserve-test-web" \
     && docker logs tsserve-tier2 2>&1 | grep -q "service registered.*svc:tsserve-test-echo" \
     && docker logs tsserve-tier2 2>&1 | grep -q "service registered.*svc:tsserve-test-caps"; then
    ok "all three services registered after ${i}s"
    break
  fi
  if [[ $i -eq 90 ]]; then
    fail "timed out waiting for service registration"
    docker compose logs --tail=80 tsserve
    exit 1
  fi
  sleep 1
done

# --- 2. /metrics on the host --------------------------------------------------
say "scraping ${METRICS_URL}"
if curl -fsS "$METRICS_URL" > /tmp/tsserve-metrics.txt; then
  if grep -q "^tsserve_services_active 3" /tmp/tsserve-metrics.txt; then
    ok "tsserve_services_active = 3"
  else
    fail "tsserve_services_active != 3"
    grep "^tsserve_services_active" /tmp/tsserve-metrics.txt || true
  fi
  if grep -q "^tsserve_build_info" /tmp/tsserve-metrics.txt; then
    ok "tsserve_build_info present"
  else
    fail "tsserve_build_info missing"
  fi
else
  fail "could not reach metrics endpoint at $METRICS_URL"
fi

# --- 3. Tailnet HTTPS to the status page -------------------------------------
say "GET ${STATUS_URL}/  (status page)"
if status_html=$(curl -fsS --max-time 10 "${STATUS_URL}/"); then
  if grep -q "svc:tsserve-test-web" <<<"$status_html" \
     && grep -q "svc:tsserve-test-echo" <<<"$status_html" \
     && grep -q "svc:tsserve-test-caps" <<<"$status_html"; then
    ok "status page lists all three services"
  else
    fail "status page missing one or more services"
  fi
  if grep -q "Services (3)" <<<"$status_html"; then
    ok "status page reports Services (3)"
  else
    warn "status page service count not '3' (template change?)"
  fi
else
  fail "could not reach status page at ${STATUS_URL}/ — is the caller on the tailnet?"
fi

# --- 4. nginx via Tailscale ---------------------------------------------------
say "GET ${WEB_URL}/"
if web_body=$(curl -fsS --max-time 10 "${WEB_URL}/"); then
  if grep -qi "welcome to nginx" <<<"$web_body"; then
    ok "nginx default page reachable through svc:tsserve-test-web"
  else
    fail "got response from ${WEB_URL}/ but body unexpected"
    echo "--- body head ---"
    head -c 400 <<<"$web_body"
  fi
else
  fail "could not reach ${WEB_URL}/"
fi

# --- 5. whoami via Tailscale, with identity header injection ----------------
say "GET ${ECHO_URL}/  (expecting Tailscale-User-Login header)"
if echo_body=$(curl -fsS --max-time 10 "${ECHO_URL}/"); then
  # traefik/whoami prints each header on its own line. The backend must see
  # the Tailscale-User-Login header that Tailscale's serve layer injected.
  if grep -qi "^Tailscale-User-Login:" <<<"$echo_body"; then
    ok "Tailscale-User-Login header reached the backend"
    login=$(grep -i "^Tailscale-User-Login:" <<<"$echo_body" | head -1)
    echo "    $login"
  else
    fail "Tailscale-User-Login header NOT present in backend response"
    echo "--- whoami body ---"
    echo "$echo_body"
  fi
  # Sanity: X-Forwarded-Proto should be https (set by reverseproxy.go).
  if grep -qi "^X-Forwarded-Proto: https" <<<"$echo_body"; then
    ok "X-Forwarded-Proto: https reached the backend"
  else
    warn "X-Forwarded-Proto: https not present (check reverseproxy.go)"
  fi
else
  fail "could not reach ${ECHO_URL}/"
fi

# --- 6. App capabilities ------------------------------------------------------
say "GET ${CAPS_URL}/  (expecting Tailscale-App-Capabilities header)"
if caps_body=$(curl -fsS --max-time 10 "${CAPS_URL}/"); then
  if grep -qi "^Tailscale-App-Capabilities:" <<<"$caps_body"; then
    cap_line=$(grep -i "^Tailscale-App-Capabilities:" <<<"$caps_body" | head -1)
    ok "Tailscale-App-Capabilities header reached the backend"
    echo "    $cap_line"
    if grep -qi "tsserve.test/cap/read" <<<"$cap_line"; then
      ok "header contains the declared capability 'tsserve.test/cap/read'"
    else
      fail "header does not name 'tsserve.test/cap/read' — check ACL grants for svc:tsserve-test-caps"
    fi
  else
    fail "Tailscale-App-Capabilities header NOT present at backend — check ACL grants for svc:tsserve-test-caps"
    echo "--- whoami body ---"
    echo "$caps_body"
  fi
else
  fail "could not reach ${CAPS_URL}/"
fi

# --- 6b. Node identity headers (tagged peer) ---------------------------------
# Node headers inject for TAGGED peers only. The host above is typically a user
# node, so confirm it did NOT receive them, then drive the positive path from a
# genuinely tagged client (only when TS_CLIENT_AUTHKEY is set).
if [[ -n "${caps_body:-}" ]] && grep -qi "^Tailscale-Node-Tags:" <<<"$caps_body"; then
  warn "host request carried Tailscale-Node-Tags — the smoke host appears to be a tagged node, not a user node"
fi

if [[ -n "${TS_CLIENT_AUTHKEY:-}" ]]; then
  say "validating node-identity headers from a TAGGED peer (svc:tsserve-test-caps)"

  # Wait for the tagged client to authenticate and come up on the tailnet.
  client_up=0
  for i in $(seq 1 60); do
    if docker exec tsserve-tier2-client tailscale ip -4 >/dev/null 2>&1; then
      client_up=1
      break
    fi
    sleep 1
  done

  if [[ $client_up -ne 1 ]]; then
    fail "tagged client did not come up on the tailnet within 60s"
    docker compose ${PROFILE_ARGS[@]+"${PROFILE_ARGS[@]}"} logs --tail=40 client || true
  else
    ok "tagged client is up on the tailnet"

    # curl runs inside the clienttools sidecar, which shares the client's netns
    # and reaches the tailnet through the userspace SOCKS5 proxy on :1055.
    cexec() {
      docker exec tsserve-tier2-clienttools \
        curl -fsS --max-time 15 --socks5-hostname localhost:1055 "$@"
    }

    # 6b.1 Positive: tagged peer -> Node-Tags (tag: stripped) + Node-Name.
    if node_body=$(cexec "${CAPS_URL}/"); then
      if grep -qi "^Tailscale-Node-Tags:" <<<"$node_body"; then
        tag_line=$(grep -i "^Tailscale-Node-Tags:" <<<"$node_body" | head -1)
        ok "Tailscale-Node-Tags reached the backend"
        echo "    $tag_line"
        if grep -qiw "$CLIENT_NODE_TAG" <<<"$tag_line"; then
          ok "Node-Tags contains expected tag '$CLIENT_NODE_TAG'"
        else
          fail "Node-Tags does not contain '$CLIENT_NODE_TAG' — check TS_CLIENT_AUTHKEY's tags / CLIENT_NODE_TAG"
        fi
        if grep -qi "^Tailscale-Node-Tags:.*tag:" <<<"$node_body"; then
          fail "Node-Tags still contains a 'tag:' prefix — it should be stripped"
        else
          ok "Node-Tags has the 'tag:' prefix stripped"
        fi
      else
        fail "Tailscale-Node-Tags header NOT present for a tagged peer"
        echo "--- whoami body ---"
        echo "$node_body"
      fi

      if grep -qi "^Tailscale-Node-Name:" <<<"$node_body"; then
        name_line=$(grep -i "^Tailscale-Node-Name:" <<<"$node_body" | head -1)
        ok "Tailscale-Node-Name reached the backend"
        echo "    $name_line"
        if grep -qi "$CLIENT_HOSTNAME" <<<"$name_line"; then
          ok "Node-Name matches the client hostname '$CLIENT_HOSTNAME'"
        else
          warn "Node-Name does not contain '$CLIENT_HOSTNAME' (MagicDNS may have renamed the node)"
        fi
      else
        fail "Tailscale-Node-Name header NOT present for a tagged peer"
      fi
    else
      fail "tagged client could not reach ${CAPS_URL}/ — check ACL grants for the client's tag"
    fi

    # 6b.2 Spoof prevention: client-supplied node headers must be stripped and
    #      replaced with the real whois result, never forwarded as-is.
    if spoof_body=$(cexec \
        -H "Tailscale-Node-Tags: tag:spoofed-admin" \
        -H "Tailscale-Node-Name: spoofed-host" "${CAPS_URL}/"); then
      if grep -qi "^Tailscale-Node-Tags:.*spoofed-admin" <<<"$spoof_body" \
         || grep -qi "^Tailscale-Node-Name:.*spoofed-host" <<<"$spoof_body"; then
        fail "spoofed node headers reached the backend — inbound stripping is broken"
        grep -i "^Tailscale-Node-" <<<"$spoof_body" || true
      else
        ok "spoofed node headers were stripped (backend saw the real whois result)"
      fi
    else
      fail "spoof-check request to ${CAPS_URL}/ failed"
    fi
  fi
else
  warn "TS_CLIENT_AUTHKEY not set — skipping tagged-peer node-header checks"
  warn "  set it to a TAGGED auth key in .env to validate Tailscale-Node-Tags/-Name"
fi

# --- 7. Lifecycle: stop nginx, verify deregistration -------------------------
say "stopping nginx container to test deregistration"
docker stop tsserve-tier2-web > /dev/null

say "waiting up to 15s for tsserve to deregister svc:tsserve-test-web"
deregistered=0
for i in $(seq 1 15); do
  if docker logs tsserve-tier2 2>&1 | grep -q "service deregistered.*svc:tsserve-test-web"; then
    ok "svc:tsserve-test-web deregistered after ${i}s"
    deregistered=1
    break
  fi
  sleep 1
done
if [[ $deregistered -ne 1 ]]; then
  fail "tsserve did not deregister svc:tsserve-test-web within 15s"
fi

# Status page should now list only the remaining two services.
if status_html=$(curl -fsS --max-time 10 "${STATUS_URL}/"); then
  if grep -q "Services (2)" <<<"$status_html"; then
    ok "status page now reports Services (2)"
  else
    fail "status page did not drop to Services (2)"
  fi
fi

# /metrics should now show 2 active services.
if curl -fsS "$METRICS_URL" > /tmp/tsserve-metrics-after.txt; then
  if grep -q "^tsserve_services_active 2" /tmp/tsserve-metrics-after.txt; then
    ok "tsserve_services_active dropped to 2"
  else
    fail "tsserve_services_active did not drop to 2"
    grep "^tsserve_services_after" /tmp/tsserve-metrics-after.txt || true
  fi
fi

# --- 8. Summary ---------------------------------------------------------------
echo
if [[ $FAILED -ne 0 ]]; then
  printf "${R}smoke test FAILED${N} — see messages above\n" >&2
  exit 1
fi
printf "${G}smoke test passed${N}\n"
echo
echo "Leave the stack running for further poking, or:"
echo "  ./smoke.sh down    # stop, keep state (no re-auth next run)"
echo "  ./smoke.sh clean   # stop, wipe state (forces re-auth)"
