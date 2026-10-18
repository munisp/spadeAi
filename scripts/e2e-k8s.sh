#!/usr/bin/env bash
# Kubernetes end to end against a local k3s cluster (container spade-k3s): Helm-installed outbound agent,
# configuration menu from cluster inventory, deploy/scale/stop/start/delete, NetworkPolicy, residency, billing.
set -euo pipefail
cd "$(dirname "$0")/.."
API=${API:-http://localhost:8080}
GW=${GW:-https://localhost:8443}
SECRET=${SPADE_INTERNAL_SECRET:?export the same SPADE_INTERNAL_SECRET (>=32 chars) used by spade-api}
INTERNAL=${INTERNAL:-http://127.0.0.1:8081}
DB=${SPADE_DATABASE_URL:-postgres://spade:spade-local-dev@127.0.0.1:5433/spadeai}
TEMPLATE_CI=${TEMPLATE_CI:-.venv/bin/spade-template-ci}
j() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$1"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
sql() { docker compose -f deploy/compose/docker-compose.yml exec -T postgres psql -U spade -d spadeai -Atc "$1"; }

TOKEN=$(curl -sf -d client_id=spade-cli -d grant_type=password -d username=dev@spadeai.local -d password=dev-password -d scope=openid \
  http://localhost:8180/realms/spadeai/protocol/openid-connect/token | j "d['access_token']")
H=(-H "Authorization: Bearer $TOKEN")
topup() { local body="{\"org_id\":\"$1\",\"provider\":\"paystack\",\"provider_ref\":\"$2\",\"amount_minor\":$3}"
  curl -sf -H "X-Spade-Signature: $(printf %s "$body" | openssl dgst -sha256 -hmac "$SECRET" | cut -d' ' -f2)" -d "$body" "$INTERNAL/internal/payments/verified" >/dev/null; }
wallet() { curl -sf "${H[@]}" "$API/v1/orgs/$1/wallet"; }

SPADE_DATABASE_URL=$DB "$TEMPLATE_CI" --smoke --publish templates/whoami | j "d['smoke']['passed']" | grep -q True || fail "template smoke"
GW_IN=${GW_IN:-https://172.17.0.1:8443}   # gateway as seen from pods; must be in GATEWAY_HOSTS
K3S=${K3S:-spade-k3s}
k() { docker exec -i "$K3S" kubectl "$@"; }
op() { local s; for _ in $(seq 240); do s=$(curl -sf "${H[@]}" "$API/v1/operations/$1" | j "d['status']"); case $s in succeeded|failed) echo "$s"; return;; esac; sleep 1; done; echo timeout; }
code() { curl -s -o /tmp/e2e-k8s.out -w '%{http_code}' "${H[@]}" "$@"; }
docker inspect "$K3S" >/dev/null 2>&1 || docker run -d --name "$K3S" --privileged --tmpfs /run --tmpfs /var/run -p 16443:6443 rancher/k3s:v1.31.4-k3s1 server --disable=metrics-server --tls-san 127.0.0.1 >/dev/null
for _ in $(seq 60); do k get nodes 2>/dev/null | grep -q " Ready" && break; sleep 2; done
docker image inspect spade-agent-k8s:dev >/dev/null || fail "build spade-agent-k8s:dev first (see rust/agent/Dockerfile.k8s)"
docker save spade-agent-k8s:dev | docker exec -i "$K3S" ctr images import - >/dev/null
KCFG=$(mktemp); docker exec "$K3S" cat /etc/rancher/k3s/k3s.yaml | sed 's#https://127.0.0.1:6443#https://127.0.0.1:16443#' >"$KCFG"

RUN=k8s-$(date +%s)
ORG=$(curl -sf "${H[@]}" -d '{"name":"K8s E2E Ltd","billing_mode":"prepaid"}' "$API/v1/orgs" | j "d['id']")
topup "$ORG" "$RUN-1" 50000000
PRJ=$(curl -sf "${H[@]}" -d '{"name":"onprem","technical_email":"ops@e2e.ng","overlay":"wireguard"}' "$API/v1/orgs/$ORG/projects" | j "d['id']")

# 1. Register the cluster; install the agent with the Helm command SpadeAi returns.
CL=$(curl -sf "${H[@]}" -d '{"name":"lagos-dc1","distribution":"k3s","in_nigeria":true}' "$API/v1/projects/$PRJ/clusters")
CID=$(echo "$CL" | j "d['id']"); HELM=$(echo "$CL" | j "d['helm']")
TOK=$(sed -E 's/.*enrolToken=([0-9a-f]+).*/\1/' <<<"$HELM"); PIN=$(sed -E 's/.*caSha256=([0-9a-f]+).*/\1/' <<<"$HELM")
[ "$(code -d '{"name":"lagos-dc1"}' "$API/v1/projects/$PRJ/clusters")" = 422 ] || fail "duplicate cluster name accepted"
helm --kubeconfig "$KCFG" upgrade --install spade-agent deploy/helm/spade-agent -n spade-system --create-namespace \
  --set gateway="$GW_IN" --set caSha256="$PIN" --set enrolToken="$TOK" --set image.pullPolicy=Never >/dev/null
for _ in $(seq 90); do [ "$(curl -sf "${H[@]}" "$API/v1/projects/$PRJ/clusters" | j "d['items'][0]['online']")" = True ] && break; sleep 2; done
[ "$(curl -sf "${H[@]}" "$API/v1/projects/$PRJ/clusters" | j "d['items'][0]['online']")" = True ] || { k logs -n spade-system deploy/spade-agent | tail; fail "cluster agent never came online"; }
k get secret -n spade-system spade-agent-identity >/dev/null || fail "agent identity not persisted"
# Restart the agent pod: it must come back with its stored identity (the enrolment token is already spent).
k delete pod -n spade-system -l app.kubernetes.io/name=spade-agent --wait=true >/dev/null
k wait -n spade-system --for=condition=Ready pod -l app.kubernetes.io/name=spade-agent --timeout=120s >/dev/null
echo "ok: agent enrolled outbound via Helm"

# 2. Configuration menu is built from what the cluster reported.
[ "$(code "$API/v1/clusters/$CID/options")" = 409 ] || fail "options served before inventory"
curl -sf "${H[@]}" -X POST "$API/v1/clusters/$CID/refresh" >/dev/null
for _ in $(seq 60); do [ "$(code "$API/v1/clusters/$CID/options")" = 200 ] && break; sleep 2; done
OPTS=$(cat /tmp/e2e-k8s.out)
echo "$OPTS" | j "[f for f in d['fields'] if f['name']=='storage_class'][0]['default']" | grep -qx local-path || fail "storage classes missing: $OPTS"
echo "$OPTS" | j "[f for f in d['fields'] if f['name']=='ingress_class'][0]['options']" | grep -q traefik || fail "ingress classes missing"
echo "$OPTS" | j "d['kubernetes_version']" | grep -q v1.31 || fail "version missing"
k logs -n spade-system -l app.kubernetes.io/name=spade-agent --tail=-1 | grep -q "restored agent identity" || fail "restarted agent did not reuse its stored identity"
echo "ok: menu lists storage/ingress classes from the cluster"

# 3. Bad configurations are refused before anything reaches the cluster.
dep() { code -H "Idempotency-Key: $RUN-$1" -d "$2" "$API/v1/projects/$PRJ/services"; }
KB() { echo "{\"template\":\"$1\",\"display_name\":\"$2\",\"size\":\"kubernetes:SMALL-1C-2G\",\"kubernetes\":{\"cluster_id\":\"$CID\",$3}}"; }
[ "$(dep bad1 "$(KB whoami w '"namespace":"kube-system"')")" = 422 ] || fail "reserved namespace accepted"
[ "$(dep bad2 "$(KB whoami w '"namespace":"x","storage_class":"gold"')")" = 422 ] || fail "unknown storage class accepted"
[ "$(dep bad3 "$(KB static-site k '"namespace":"site","replicas":2')")" = 422 ] || fail "RWO template accepted 2 replicas"
grep -q ReadWriteOnce /tmp/e2e-k8s.out || fail "RWO refusal not explained"
[ "$(dep bad4 "$(KB whoami z '"namespace":"zero","replicas":0')")" = 422 ] || fail "replicas=0 accepted"
sql "UPDATE organization SET residency_locked=true WHERE id='$ORG'" >/dev/null
sql "UPDATE k8s_cluster SET in_nigeria=false WHERE id='$CID'" >/dev/null
[ "$(dep bad4 "$(KB whoami w '"namespace":"res"')")" = 422 ] || fail "residency-locked org deployed to a non-Nigerian cluster"
sql "UPDATE k8s_cluster SET in_nigeria=true WHERE id='$CID'" >/dev/null
echo "ok: invalid namespace/storage/replicas/residency refused"

# 4. Deploy whoami: 2 replicas, ingress host, isolated network policy (residency-locked org, Nigerian cluster).
[ "$(dep w1 "$(KB whoami echo '"namespace":"e2e-whoami","replicas":2,"host":"whoami.e2e.local","network_policy":"isolated"')")" = 202 ] || fail "deploy: $(cat /tmp/e2e-k8s.out)"
OP=$(j "d['id']" </tmp/e2e-k8s.out); [ "$(op "$OP")" = succeeded ] || fail "k8s deploy failed: $(curl -sf "${H[@]}" "$API/v1/operations/$OP")"
SVC=$(curl -sf "${H[@]}" "$API/v1/operations/$OP" | j "d['target_id']")
[ "$(k get deploy app -n e2e-whoami -o jsonpath='{.status.readyReplicas}')" = 2 ] || fail "expected 2 ready replicas"
IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$K3S")
for _ in $(seq 30); do curl -sf -H 'Host: whoami.e2e.local' "http://$IP/" | grep -q Hostname && break; sleep 2; done
curl -sf -H 'Host: whoami.e2e.local' "http://$IP/" | grep -q Hostname || fail "ingress does not route to the app"
k run probe-in -n e2e-whoami --image=busybox:1.36 --restart=Never --rm -i --quiet -- wget -T5 -qO- http://app >/dev/null || fail "same-namespace traffic blocked"
k run probe-out -n default --image=busybox:1.36 --restart=Never --rm -i --quiet -- wget -T5 -qO- http://app.e2e-whoami.svc.cluster.local >/dev/null 2>&1 && fail "isolated network policy let another namespace in"
[ "$(sql "SELECT replicas FROM service_state_interval WHERE service_id='$SVC' AND ended_at IS NULL")" = 2 ] || fail "billing interval not at 2 replicas"
echo "ok: deployed 2 replicas, ingress routes, other namespaces blocked, billed x2"

# 5. Day 2: scale, stop/start, and a namespace SpadeAi does not own.
SC=$(curl -sf "${H[@]}" -H "Idempotency-Key: $RUN-s3" -d '{"replicas":3}' "$API/v1/services/$SVC/actions/scale" | j "d['id']")
[ "$(op "$SC")" = succeeded ] || fail "scale failed: $(curl -sf "${H[@]}" "$API/v1/operations/$SC")"
[ "$(k get deploy app -n e2e-whoami -o jsonpath='{.status.readyReplicas}')" = 3 ] || fail "expected 3 replicas after scale"
[ "$(sql "SELECT string_agg(replicas::text, ',' ORDER BY started_at) FROM service_state_interval WHERE service_id='$SVC'")" = "2,3" ] || fail "scale did not split billing intervals"
ST=$(curl -sf "${H[@]}" -H "Idempotency-Key: $RUN-stop" -X POST "$API/v1/services/$SVC/actions/stop" | j "d['id']"); [ "$(op "$ST")" = succeeded ] || fail stop
[ "$(k get deploy app -n e2e-whoami -o jsonpath='{.spec.replicas}')" = 0 ] || fail "stop did not scale to 0"
SA=$(curl -sf "${H[@]}" -H "Idempotency-Key: $RUN-start" -X POST "$API/v1/services/$SVC/actions/start" | j "d['id']"); [ "$(op "$SA")" = succeeded ] || fail start
[ "$(k get deploy app -n e2e-whoami -o jsonpath='{.status.readyReplicas}')" = 3 ] || fail "start did not restore 3 replicas"
k create namespace taken >/dev/null 2>&1 || true
[ "$(dep t1 "$(KB whoami t '"namespace":"taken"')")" = 202 ] || fail "deploy request"
[ "$(op "$(j "d['id']" </tmp/e2e-k8s.out)")" = failed ] || fail "agent deployed into a namespace it does not own"
k get namespace taken >/dev/null || fail "pre-existing namespace was deleted by compensation"
echo "ok: scale 2->3 (billing split), stop/start, foreign namespace refused and left intact"

# 6. Stateful template on a PVC, then delete both.
[ "$(dep k1 "$(KB static-site status '"namespace":"e2e-site","storage_gb":2')")" = 202 ] || fail "site deploy: $(cat /tmp/e2e-k8s.out)"
KOP=$(j "d['id']" </tmp/e2e-k8s.out); [ "$(op "$KOP")" = succeeded ] || fail "site deploy failed: $(curl -sf "${H[@]}" "$API/v1/operations/$KOP")"
[ "$(k get pvc data-site -n e2e-site -o jsonpath='{.status.phase}')" = Bound ] || fail "site PVC not bound"
KSVC=$(curl -sf "${H[@]}" "$API/v1/operations/$KOP" | j "d['target_id']")
[ "$(curl -s -o /tmp/e2e-k8s.out -w '%{http_code}' "${H[@]}" -H "Idempotency-Key: $RUN-b" -X POST "$API/v1/services/$KSVC/backups")" = 422 ] \
  || fail "backup of a k8s service not refused: $(cat /tmp/e2e-k8s.out)"
for s in "$SVC" "$KSVC"; do
  D=$(curl -sf "${H[@]}" -H "Idempotency-Key: $RUN-d-$s" -X DELETE "$API/v1/services/$s" | j "d['id']"); [ "$(op "$D")" = succeeded ] || fail "delete $s failed"
done
k get namespace e2e-whoami e2e-site 2>/dev/null | grep -q e2e && fail "namespaces not removed on delete"
[ "$(curl -sf "${H[@]}" "$API/v1/projects/$PRJ/clusters" | j "d['items'][0]['online']")" = True ] || fail "cluster agent lost after deletes"
echo "ok: stateful app on PVC, backups fail closed, delete removes namespaces, cluster agent stays"
echo "PASS: kubernetes e2e"
