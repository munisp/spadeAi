#!/usr/bin/env bash
# Two-host private-network check for both overlay kinds. Each "VM" is a privileged docker-in-docker
# container running the static Rust agent. Per kind: deploy one service on each VM into one project,
# wait for both hosts to apply the same mesh generation, ping across the overlay, then delete one
# service and check the survivor's mesh shrinks. Requires spade-api/worker/gateway running, with the
# gateway certificate covering the e2e network gateway IP (GATEWAY_HOSTS=...,172.31.250.1).
# The fixed subnet matters: inside docker-in-docker, host.docker.internal (172.17.0.1) is the VM's own
# docker0 bridge, not the real host.
set -euo pipefail
cd "$(dirname "$0")/.."
API=${API:-http://localhost:8080}
SECRET=${SPADE_INTERNAL_SECRET:?export the same SPADE_INTERNAL_SECRET used by spade-api}
INTERNAL=${INTERNAL:-http://127.0.0.1:8081}
GW_IN=${GW_IN:-https://172.31.250.1:8443}
KINDS=${KINDS:-wireguard nebula}
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
sql() { docker compose -f deploy/compose/docker-compose.yml exec -T postgres psql -U spade -d spadeai -Atc "$1"; }
TOKEN=$(curl -sf -d client_id=spade-cli -d grant_type=password -d username=dev@spadeai.local -d password=dev-password -d scope=openid \
  http://localhost:8180/realms/spadeai/protocol/openid-connect/token | j "d['access_token']")
H=(-H "Authorization: Bearer $TOKEN")
topup() { local body="{\"org_id\":\"$1\",\"provider\":\"paystack\",\"provider_ref\":\"$2\",\"amount_minor\":$3}"
  curl -sf -H "X-Spade-Signature: $(printf %s "$body" | openssl dgst -sha256 -hmac "$SECRET" | cut -d' ' -f2)" -d "$body" "$INTERNAL/internal/payments/verified" >/dev/null; }
key() { openssl rand -hex 16; }
VMS=()
cleanup() { [ -n "${KEEP:-}" ] && return; for v in "${VMS[@]}"; do docker rm -f "$v" >/dev/null 2>&1 || true; done; docker network rm spade-e2e >/dev/null 2>&1 || true; }
trap cleanup EXIT

CTX=$(mktemp -d)
cp rust/target/x86_64-unknown-linux-musl/release/spade-agent .spade/ca/ca.crt "$(command -v nebula)" "$(command -v nebula-cert)" "$CTX/"
docker build -q -t spade-e2e-vm -f deploy/e2e/vm.Dockerfile "$CTX" >/dev/null
docker network rm spade-e2e >/dev/null 2>&1 || true
docker network create --subnet 172.31.250.0/24 --gateway 172.31.250.1 spade-e2e >/dev/null
vm() {
  docker rm -f "$1" >/dev/null 2>&1 || true
  docker run -d --privileged --name "$1" --network spade-e2e spade-e2e-vm >/dev/null
  VMS+=("$1")
  for _ in $(seq 60); do docker exec "$1" docker info >/dev/null 2>&1 && return; sleep 1; done
  fail "dockerd in $1 did not start"
}
wait_op() { local s=""; for _ in $(seq 240); do s=$(curl -sf "${H[@]}" "$API/v1/operations/$1" | j "d['status']"); [ "$s" = succeeded ] && return; [ "$s" = failed ] && break; sleep 1; done
  curl -sf "${H[@]}" "$API/v1/operations/$1" >&2; echo >&2; docker exec "$2" sh -c 'tail -20 /var/log/agent.log' >&2 || true; fail "operation $1 $s"; }

ORG=$(curl -sf "${H[@]}" -d '{"name":"Overlay E2E","billing_mode":"prepaid"}' "$API/v1/orgs" | j "d['id']")
topup "$ORG" "ovl-$(key)" 50000000

for KIND in $KINDS; do
  PRJ=$(curl -sf "${H[@]}" -d "{\"name\":\"net-$KIND-$RANDOM\",\"technical_email\":\"ops@e2e.ng\",\"overlay\":\"$KIND\"}" "$API/v1/orgs/$ORG/projects" | j "d['id']")
  SVCS=()
  for N in 1 2; do
    V="spade-e2e-$KIND-$N"; vm "$V"
    OP=$(curl -sf "${H[@]}" -H "Idempotency-Key: $(key)" -d "{\"template\":\"whoami\",\"display_name\":\"$KIND-$N\",\"size\":\"byovm:SMALL-1C-2G\"}" "$API/v1/projects/$PRJ/services" | j "d['id']")
    BOOT=""; for _ in $(seq 30); do BOOT=$(curl -sf "${H[@]}" "$API/v1/operations/$OP" | j "(d.get('response') or {}).get('bootstrap','')"); [ -n "$BOOT" ] && break; sleep 1; done
    ET=$(sed -E 's/.*SPADE_ENROL_TOKEN=([0-9a-f]+).*/\1/' <<<"$BOOT")
    docker exec "$V" spade-agent enrol --gateway "$GW_IN" --ca /etc/spade/ca.pem --token "$ET" --state-dir /etc/spade >/dev/null
    docker exec -d "$V" sh -c "spade-agent run --gateway $GW_IN --state-dir /etc/spade --stacks-dir /var/lib/spade/stacks >/var/log/agent.log 2>&1"
    wait_op "$OP" "$V"
    SVCS+=("$(curl -sf "${H[@]}" "$API/v1/operations/$OP" | j "d['response']['service_id']")")
  done
  converged() { curl -sf "${H[@]}" "$API/v1/projects/$PRJ/overlay" | python3 -c "
import sys,json; d=json.load(sys.stdin); p=d['peers']
ok=len(p)==$1 and all(x['last_apply_status']=='succeeded' and x['applied_generation']==d['generation'] for x in p)
print('yes' if ok else 'no')"; }
  for _ in $(seq 60); do [ "$(converged 2)" = yes ] && break; sleep 2; done
  [ "$(converged 2)" = yes ] || { curl -s "${H[@]}" "$API/v1/projects/$PRJ/overlay" >&2; fail "$KIND mesh did not converge on both hosts"; }
  IP2=$(curl -sf "${H[@]}" "$API/v1/services/${SVCS[1]}" | j "d['overlay_ip']")
  IP1=$(curl -sf "${H[@]}" "$API/v1/services/${SVCS[0]}" | j "d['overlay_ip']")
  PINGED=no; for _ in $(seq 15); do docker exec "spade-e2e-$KIND-1" ping -c 1 -W 2 "$IP2" >/dev/null 2>&1 && { PINGED=yes; break; }; sleep 2; done
  [ "$PINGED" = yes ] || fail "$KIND: host 1 cannot reach $IP2 over the overlay"
  docker exec "spade-e2e-$KIND-2" ping -c 1 -W 2 "$IP1" >/dev/null || fail "$KIND: host 2 cannot reach $IP1 over the overlay"
  echo "PASS $KIND: two hosts joined ($IP1, $IP2), same mesh generation applied, ping both ways over the overlay"

  DOP=$(curl -sf "${H[@]}" -X DELETE -H "Idempotency-Key: $(key)" "$API/v1/services/${SVCS[1]}" | j "d['id']")
  wait_op "$DOP" "spade-e2e-$KIND-2"
  for _ in $(seq 60); do [ "$(converged 1)" = yes ] && break; sleep 2; done
  [ "$(converged 1)" = yes ] || fail "$KIND: survivor did not apply the shrunken mesh"
  if [ "$KIND" = wireguard ]; then
    IF="spw-${PRJ:0:8}"
    [ "$(docker exec spade-e2e-wireguard-1 wg show "$IF" peers | wc -l)" = 0 ] || fail "deleted peer still configured on survivor"
    docker exec spade-e2e-wireguard-2 ip link show "$IF" >/dev/null 2>&1 && fail "overlay interface left on deleted host"
  fi
  echo "PASS $KIND: deleted service left the overlay and the survivor's mesh shrank"
done

[ "$(sql "SELECT count(*) FROM agent_command WHERE verb LIKE 'overlay_%' AND (payload::text ILIKE '%PRIVATE KEY%' OR payload::text ILIKE '%privatekey%')")" = 0 ] \
  || fail "a private key was sent through the command queue"
echo "PASS no overlay private key ever left a host"
echo "OVERLAY E2E OK"
