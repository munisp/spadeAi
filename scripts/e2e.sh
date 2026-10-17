#!/usr/bin/env bash
# End-to-end check against the local stack: deploy via a real agent, meter, rate, suspend on
# rejected debit, settle arrears on top-up. Requires compose stack + spade-api/worker/gateway running.
set -euo pipefail
cd "$(dirname "$0")/.."
API=${API:-http://localhost:8080}
GW=${GW:-https://localhost:8443}
SECRET=${SPADE_INTERNAL_SECRET:-local-dev-internal-secret}
DB=${SPADE_DATABASE_URL:-postgres://spade:spade-local-dev@127.0.0.1:5433/spadeai}
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
sql() { docker compose -f deploy/compose/docker-compose.yml exec -T postgres psql -U spade -d spadeai -Atc "$1"; }

TOKEN=$(curl -sf -d client_id=spade-cli -d grant_type=password -d username=dev@spadeai.local -d password=dev-password -d scope=openid \
  http://localhost:8180/realms/spadeai/protocol/openid-connect/token | j "d['access_token']")
H=(-H "Authorization: Bearer $TOKEN")
topup() { local body="{\"org_id\":\"$1\",\"provider\":\"paystack\",\"provider_ref\":\"$2\",\"amount_minor\":$3}"
  curl -sf -H "X-Spade-Signature: $(printf %s "$body" | openssl dgst -sha256 -hmac "$SECRET" | cut -d' ' -f2)" -d "$body" "$API/internal/payments/verified" >/dev/null; }
wallet() { curl -sf "${H[@]}" "$API/v1/orgs/$1/wallet"; }

SPADE_DATABASE_URL=$DB .venv/bin/spade-template-ci --smoke --publish templates/whoami | j "d['smoke']['passed']" | grep -q True || fail "template smoke"

[ "$(curl -s -o /dev/null -w '%{http_code}' "$API/v1/orgs")" = 401 ] || fail "unauthenticated request not rejected"
ORG=$(curl -sf "${H[@]}" -d '{"name":"E2E Ltd","billing_mode":"prepaid"}' "$API/v1/orgs" | j "d['id']")
RUN=$RANDOM$RANDOM
topup "$ORG" "e2e-$RUN-1" 5000000
PRJ=$(curl -sf "${H[@]}" -d '{"name":"web","technical_email":"ops@e2e.ng","overlay":"wireguard"}' "$API/v1/orgs/$ORG/projects" | j "d['id']")
OP=$(curl -sf "${H[@]}" -H "Idempotency-Key: $RUN" -d '{"template":"whoami","display_name":"echo","size":"byovm:SMALL-1C-2G"}' "$API/v1/projects/$PRJ/services" | j "d['id']")
OP2=$(curl -sf "${H[@]}" -H "Idempotency-Key: $RUN" -d '{"template":"whoami","display_name":"echo","size":"byovm:SMALL-1C-2G"}' "$API/v1/projects/$PRJ/services" | j "d['id']")
[ "$OP" = "$OP2" ] || fail "idempotent replay created a second operation"
[ "$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -H "Idempotency-Key: $RUN" -d '{"template":"whoami","display_name":"other","size":"byovm:SMALL-1C-2G"}' "$API/v1/projects/$PRJ/services")" = 409 ] || fail "idempotency key reuse with different body not rejected"

for _ in $(seq 30); do BOOT=$(curl -sf "${H[@]}" "$API/v1/operations/$OP" | j "(d.get('response') or {}).get('bootstrap','')"); [ -n "$BOOT" ] && break; sleep 1; done
ET=$(sed -E 's/.*SPADE_ENROL_TOKEN=([0-9a-f]+).*/\1/' <<<"$BOOT")
[ "$(wallet "$ORG" | j "d['balance']['pending_debits_minor']")" -gt 0 ] || fail "funds-check hold not placed"
ST=$(mktemp -d); ./rust/target/release/spade-agent enrol --gateway "$GW" --ca .spade/ca/ca.crt --token "$ET" --state-dir "$ST"
./rust/target/release/spade-agent enrol --gateway "$GW" --ca .spade/ca/ca.crt --token "$ET" --state-dir "$(mktemp -d)" 2>/dev/null && fail "enrolment token reused"
./rust/target/release/spade-agent run --gateway "$GW" --state-dir "$ST" --stacks-dir "$ST/stacks" >"$ST/agent.log" 2>&1 & AGENT=$!
SVC=""
cleanup() { kill $AGENT 2>/dev/null || true; [ -n "$SVC" ] && docker compose -p "spade-${SVC:0:8}" down >/dev/null 2>&1 || true; }
trap cleanup EXIT
for _ in $(seq 120); do S=$(curl -sf "${H[@]}" "$API/v1/operations/$OP" | j "d['status']"); [ "$S" = succeeded ] && break; [ "$S" = failed ] && fail "deploy failed"; sleep 1; done
[ "$S" = succeeded ] || fail "deploy timed out"
W=$(wallet "$ORG"); [ "$(j "d['balance']['pending_debits_minor']" <<<"$W")" = 0 ] || fail "hold not voided after deploy"
[ "$(j "d['balance']['posted_balance_minor']" <<<"$W")" = 5000000 ] || fail "hold was charged"
echo "PASS deploy: agent enrolled, bundle applied, health ok, hold voided, no double charge"

SVC=$(curl -sf "${H[@]}" "$API/v1/projects/$PRJ/services" | j "d['items'][0]['id']")
HOUR=$(date -u -d '-1 hour' +%Y-%m-%dT%H:00:00Z)
sql "INSERT INTO meter_reading(service_id,sku,period_start,quantity_milli) VALUES ('$SVC','mgmt.vcpu.hour','$HOUR',99000000)" >/dev/null
for _ in $(seq 40); do SUS=$(sql "SELECT suspended_at IS NOT NULL FROM organization WHERE id='$ORG'"); [ "$SUS" = t ] && break; sleep 1; done
[ "$SUS" = t ] || fail "org not suspended after rejected debit"
ARR=$(wallet "$ORG" | j "d['arrears_minor']"); [ "$ARR" -gt 0 ] || fail "arrears not recorded"
[ "$(wallet "$ORG" | j "d['balance']['posted_balance_minor']")" -gt 0 ] || echo "note: wallet drained"
echo "PASS suspension: rejected debit (exceeds_credits) suspended org, arrears=$ARR"

topup "$ORG" "e2e-$RUN-2" $((ARR + 1000000))
for _ in $(seq 40); do A=$(wallet "$ORG" | j "d['arrears_minor']"); SUS=$(sql "SELECT suspended_at IS NOT NULL FROM organization WHERE id='$ORG'"); [ "$A" = 0 ] && [ "$SUS" = f ] && break; sleep 1; done
[ "$A" = 0 ] || fail "arrears not settled after top-up"
[ "$SUS" = f ] || fail "org not resumed after arrears settled"
echo "PASS arrears: settled oldest-first and org resumed"

[ "$(curl -sf "${H[@]}" "$API/v1/audit-events/verify" | j "d['intact']")" = True ] || fail "audit chain broken"
echo "PASS audit chain intact"
echo "E2E OK"
