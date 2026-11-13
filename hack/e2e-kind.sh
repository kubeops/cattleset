#!/usr/bin/env bash

# Copyright AppsCode Inc. and Contributors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Runs the real CattleSet hub controller and gate agent, deployed from deploy/,
# against a real API server (one kind cluster that plays the hub and spoke-1 at
# once), and optionally dr-controlplane's FailoverGroup controller from a build of
# its dc-dr-refresh branch.
#
# What is REAL: the API server (server-side apply, RBAC, patch semantics, CRD
# validation, the label-scoped caches), both CattleSet roles as deployed by
# deploy/*.yaml, and, in phase B, dr-controlplane's controller.
#
# What is PLAYED BY THIS SCRIPT, because nothing in one kind cluster supplies it:
#   - the OCM work agent: it copies the rendered Service into the "spoke" and
#     writes the ManifestWork status feedback (including the admission annotation);
#   - dr-controlplane's per-DC agents: it renews the Leases, writes the scope
#     marker ConfigMap, and performs the Lease release and acquire of a handoff;
#   - the database: a Service and an EndpointSlice for pg-docs.
# It is therefore NOT the joined DC-DR path, which needs OCM, the per-DC agents and
# a database. spoke-2 does not exist: its ManifestWork feedback is written, but it
# has no Service.
#
# Usage:
#   hack/e2e-kind.sh                       # phase A only
#   DRC_BIN=/path/to/dr-controlplane hack/e2e-kind.sh   # phases A and B
#   KEEP=1 hack/e2e-kind.sh                # leave the cluster for inspection

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLUSTER=${CLUSTER:-cattleset-e2e}
DRC_BIN=${DRC_BIN:-}
KEEP=${KEEP:-}
TMP=$(mktemp -d)
KUBECONFIG_FILE=$TMP/kubeconfig
export KUBECONFIG=$KUBECONFIG_FILE
BG_PIDS=()
PASS=0
COMPLETED=

kc() { kubectl --context "kind-${CLUSTER}" "$@"; }
log() { printf '\n=== %s\n' "$*"; }
ok() {
    PASS=$((PASS + 1))
    printf '  ok: %s\n' "$*"
}
fail() {
    printf '\n  FAIL: %s\n' "$*" >&2
    dump
    exit 1
}

dump() {
    echo "--- diagnostics" >&2
    kc -n cattleset-system get pods 2>&1 | head -10 >&2 || true
    for d in cattleset cattleset-gate-agent; do
        echo "--- logs: $d" >&2
        kc -n cattleset-system logs "deploy/$d" --tail=40 2>&1 | tail -40 >&2 || true
    done
    echo "--- cattleset status" >&2
    kc -n orders get cattleset orders-api -o jsonpath='{.status}' 2>&1 | jq . 2>/dev/null | head -60 >&2 || true
    echo "--- orders-api Service" >&2
    kc -n orders get svc orders-api -o yaml 2>&1 | head -40 >&2 || true
}

cleanup() {
    local rc=$?
    for p in "${BG_PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
    if [[ -z $KEEP ]]; then
        kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    else
        echo "kept cluster kind-${CLUSTER} (kubeconfig: $KUBECONFIG_FILE)"
    fi
    if [[ $rc -eq 0 && -n $COMPLETED ]]; then
        echo
        echo "PASSED: $PASS checks"
    fi
    [[ -n $KEEP ]] || rm -rf "$TMP"
    exit $rc
}
trap cleanup EXIT
trap "exit 130" INT TERM

# wait_for DESC TIMEOUT_SECONDS CMD...  (CMD succeeds when the condition holds)
wait_for() {
    local desc=$1 timeout=$2
    shift 2
    local start=$SECONDS
    until "$@" >/dev/null 2>&1; do
        if ((SECONDS - start > timeout)); then fail "timed out after ${timeout}s waiting for: $desc"; fi
        sleep 1
    done
    ok "$desc ($((SECONDS - start))s)"
}

# held_for DESC SECONDS CMD...  (CMD must keep succeeding for the whole period)
held_for() {
    local desc=$1 secs=$2
    shift 2
    local start=$SECONDS
    while ((SECONDS - start < secs)); do
        "$@" >/dev/null 2>&1 || fail "$desc: stopped holding after $((SECONDS - start))s"
        sleep 1
    done
    ok "$desc (held ${secs}s)"
}

now_micro() { date -u +%Y-%m-%dT%H:%M:%S.%6NZ; }
now_sec() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# --- predicates over the orders-api Service (the spoke-1 stand-in) ---
TERM='apps.k8s.appscode.com/admitted'
svc_json() { kc -n orders get svc orders-api -o json; }
svc_admitted() { svc_json | jq -e --arg t "$TERM" '.spec.selector | has($t) | not' >/dev/null; }
svc_unadmitted() { svc_json | jq -e --arg t "$TERM" '.spec.selector | has($t)' >/dev/null; }
svc_reason() { svc_json | jq -r '.metadata.annotations["apps.k8s.appscode.com/admission"] // ""'; }
svc_reason_is() { [[ $(svc_reason) == "$1" ]]; }

# ---------------------------------------------------------------------------
log "build the binary and the image"
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=vendor go build -o "$TMP/cattleset" ./cmd/cattleset
cat >"$TMP/Dockerfile" <<'EOF'
FROM scratch
COPY cattleset /cattleset
USER 65532:65532
ENTRYPOINT ["/cattleset"]
EOF
docker build -q -t cattleset:e2e "$TMP" >/dev/null
ok "image cattleset:e2e built"

log "create the kind cluster"
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kc get nodes >/dev/null
kind load docker-image cattleset:e2e --name "$CLUSTER" >/dev/null
ok "cluster kind-${CLUSTER} is up"

log "install the CRDs the hub needs (CattleSet, petset's, OCM ManifestWork)"
kc apply --server-side -f crds/apps.k8s.appscode.com_cattlesets.yaml >/dev/null
kc apply --server-side -f vendor/kubeops.dev/petset/crds/apps.k8s.appscode.com_placementpolicies.yaml >/dev/null
kc apply --server-side -f vendor/kubeops.dev/petset/crds/apps.k8s.appscode.com_failovergroups.yaml >/dev/null
kc apply --server-side -f vendor/open-cluster-management.io/api/work/v1/0000_00_work.open-cluster-management.io_manifestworks.crd.yaml >/dev/null
kc wait --for=condition=Established crd --all --timeout=60s >/dev/null
ok "CRDs installed; slice is optional in the PlacementPolicy CRD: $(kc get crd placementpolicies.apps.k8s.appscode.com -o json | jq '[.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.clusterSpreadConstraint.required[]] | index("slice") | not')"

log "namespaces: the application, the database, the coordination plane, and one per ManifestWork cluster"
for ns in orders demo dc-failover spoke-1 spoke-2 hub; do kc create namespace "$ns" >/dev/null; done

log "deploy the hub controller from deploy/hub.yaml"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=cattleset-webhook.cattleset-system.svc' \
    -keyout "$TMP/tls.key" -out "$TMP/tls.crt" >/dev/null 2>&1
# The manifests are the production ones, with only the image swapped and the two
# settings this single cluster cannot honor (a separate coordination plane).
sed -e 's#ghcr.io/kubeops/cattleset:latest#cattleset:e2e#' \
    -e '/--coord-kubeconfig=/d' \
    -e '/image: cattleset:e2e/a\          imagePullPolicy: Never' \
    deploy/hub.yaml >"$TMP/hub.yaml"
kc apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata: {name: cattleset-system}
EOF
kc -n cattleset-system create secret tls cattleset-webhook-tls --cert="$TMP/tls.crt" --key="$TMP/tls.key" >/dev/null
kc apply -f "$TMP/hub.yaml" >/dev/null
wait_for "the hub controller Deployment is available (2 replicas, leader elected)" 120 \
    kc -n cattleset-system wait --for=condition=Available deploy/cattleset --timeout=5s
wait_for "one hub replica holds the leader election Lease" 60 \
    kc -n cattleset-system get lease cattleset.k8s.appscode.com

# ---------------------------------------------------------------------------
log "the data tier and the application's ordering"
kc apply -f - >/dev/null <<'EOF'
apiVersion: apps.k8s.appscode.com/v1
kind: PlacementPolicy
metadata: {name: pg-geo}
spec:
  clusterSpreadConstraint:
    distributionRules:
      - {clusterName: spoke-1, replicaIndices: [0], role: Member}
      - {clusterName: spoke-2, replicaIndices: [1], role: Member}
      - {clusterName: hub, replicaIndices: [], role: Arbiter}
    slice: {projectNamespace: kubeslice-demo, sliceName: demo-slice}
    failoverPolicy: {mode: TwoDC, failoverGroupRef: {name: pg-docs}}
---
apiVersion: apps.k8s.appscode.com/v1
kind: PlacementPolicy
metadata: {name: orders-geo}
spec:
  clusterSpreadConstraint:
    distributionRules:
      - {clusterName: spoke-1, replicaIndices: [], role: Member}
      - {clusterName: spoke-2, replicaIndices: [], role: Member}
      - {clusterName: hub, replicaIndices: [], role: Arbiter}
    failoverPolicy: {mode: TwoDC, failoverGroupRef: {name: orders-app}}
---
apiVersion: apps.k8s.appscode.com/v1
kind: FailoverGroup
metadata: {name: pg-docs}
---
apiVersion: apps.k8s.appscode.com/v1
kind: FailoverGroup
metadata: {name: orders-app}
spec: {dependsOn: [pg-docs]}
EOF
ok "the application's PlacementPolicy has no slice (optional in the CRD) and was accepted"
# The database operator reports its member and the group's active DC.
kc patch failovergroup pg-docs --subresource=status --type=merge -p "{\"status\":{\"activeDC\":\"spoke-1\",\"phase\":\"Steady\",\"members\":[{\"apiGroup\":\"kubedb.com\",\"kind\":\"Postgres\",\"namespace\":\"demo\",\"name\":\"pg-docs\",\"dc\":\"spoke-1\",\"ready\":true,\"observedAt\":\"$(now_sec)\"}]}}" >/dev/null
kc patch failovergroup orders-app --subresource=status --type=merge -p '{"status":{"activeDC":"spoke-1","phase":"Steady"}}' >/dev/null

log "play dr-controlplane's agents: Leases, DC health, and the database"
lease() { # name holder
    kc apply -f - >/dev/null <<EOF
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: $1
  namespace: dc-failover
  annotations: {dr.open-cluster-management.io/member-dcs: "spoke-1,spoke-2"}
spec: {holderIdentity: "$2", leaseDurationSeconds: 45, acquireTime: "$(now_micro)", renewTime: "$(now_micro)"}
EOF
}
lease primary-dc-orders-app spoke-1
lease primary-dc-pg-docs spoke-1
lease dc-health-spoke-1 spoke-1
lease dc-health-spoke-2 spoke-2
lease dc-health-hub hub
# A selector-less Service, so nothing else manages its EndpointSlices.
kc apply -f - >/dev/null <<'EOF'
apiVersion: v1
kind: Service
metadata: {name: pg-docs, namespace: demo}
spec: {ports: [{port: 5432}]}
EOF
db_endpoint() { # ready|absent
    kc -n demo delete endpointslice pg-docs-primary --ignore-not-found >/dev/null
    [[ $1 == ready ]] || return 0
    kc apply -f - >/dev/null <<'EOF'
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: pg-docs-primary
  namespace: demo
  labels: {kubernetes.io/service-name: pg-docs}
addressType: IPv4
ports: [{port: 5432}]
endpoints: [{addresses: ["10.0.0.5"], conditions: {ready: true}}]
EOF
}
db_endpoint ready

MARKER_ON=$TMP/marker-on
touch "$MARKER_ON"
echo spoke-1 >"$TMP/marker-dc"
agents() { # renews Leases and markers every 3 s, as the per-DC agents would
    set +e # a background loop must outlive one failed kubectl
    while true; do
        t=$(now_micro)
        for l in primary-dc-orders-app primary-dc-pg-docs dc-health-spoke-1 dc-health-spoke-2 dc-health-hub; do
            kc -n dc-failover patch lease "$l" --type=merge -p "{\"spec\":{\"renewTime\":\"$t\"}}" >/dev/null 2>&1 || true
        done
        if [[ -f $MARKER_ON ]]; then
            kc apply -f - >/dev/null 2>&1 <<EOF || true
apiVersion: v1
kind: ConfigMap
metadata: {name: primary-dc-pg-docs, namespace: dc-failover}
data: {activeDC: "$(cat "$TMP/marker-dc")", renewTime: "$(now_sec)"}
EOF
        fi
        sleep 3
    done
}
agents &
BG_PIDS+=($!)
wait_for "the scope marker for the database exists on the spoke" 30 kc -n dc-failover get cm primary-dc-pg-docs

# ---------------------------------------------------------------------------
log "create the application"
kc -n orders create configmap orders-api-config --from-literal=k=v >/dev/null
kc apply -f - >/dev/null <<'EOF'
apiVersion: apps.k8s.appscode.com/v1
kind: CattleSet
metadata: {name: orders-api, namespace: orders}
spec:
  distributed: true
  replicas: 6
  selector: {matchLabels: {app: orders-api}}
  template:
    metadata: {labels: {app: orders-api}}
    spec:
      containers: [{name: api, image: harbor.local/orders/api:2.4.1}]
  podPlacementPolicy: {name: orders-geo}
  topology: {standbyReplicas: 3}
  syncResources: [{kind: ConfigMap, name: orders-api-config}]
  traffic:
    serviceName: orders-api
    ports: [{name: http, port: 80, targetPort: 8080}]
EOF
MW=cattleset-orders-orders-api
wait_for "the hub controller renders a ManifestWork for spoke-1" 60 kc -n spoke-1 get manifestwork "$MW"
wait_for "...and for spoke-2" 30 kc -n spoke-2 get manifestwork "$MW"
wait_for "...and the dependency ManifestWork carrying the namespace and the ConfigMap" 30 \
    bash -c "kubectl --context kind-${CLUSTER} -n spoke-1 get manifestwork ${MW}-deps -o json | jq -e '[.spec.workload.manifests[].kind] | (index(\"Namespace\") != null) and (index(\"ConfigMap\") != null)'"
wait_for "the CattleSet got its cleanup finalizer" 30 \
    bash -c "kubectl --context kind-${CLUSTER} -n orders get cattleset orders-api -o json | jq -e '.metadata.finalizers | index(\"apps.k8s.appscode.com/cattleset-cleanup\") != null'"
wait_for "status.derivedMode is ActivePassive (Auto over a single-writer database)" 30 \
    bash -c "[[ \$(kubectl --context kind-${CLUSTER} -n orders get cattleset orders-api -o jsonpath='{.status.derivedMode}') == ActivePassive ]]"
wait_for "the dependency was resolved as a SingleWriter Postgres" 30 \
    bash -c "[[ \$(kubectl --context kind-${CLUSTER} -n orders get cattleset orders-api -o jsonpath='{.status.dependencies[0].camp}') == SingleWriter ]]"
gate=$(kc -n spoke-1 get manifestwork "$MW" -o json | jq -r '[.spec.workload.manifests[] | select(.kind=="Service")][0].metadata.annotations["apps.k8s.appscode.com/gate"]')
echo "$gate" | jq -e '.mode=="Tiers" and .tiers[0].lease=="primary-dc-pg-docs" and .tiers[0].service=="pg-docs"' >/dev/null || fail "gate annotation: $gate"
ok "the rendered gate is Tiers over primary-dc-pg-docs / pg-docs"
spec_replicas() { kc -n "$1" get manifestwork "$MW" -o json | jq '[.spec.workload.manifests[] | select(.kind=="Deployment")][0].spec.replicas'; }
[[ $(spec_replicas spoke-1) == 6 && $(spec_replicas spoke-2) == 3 ]] || fail "capacity: spoke-1=$(spec_replicas spoke-1) spoke-2=$(spec_replicas spoke-2)"
ok "capacity: the holder runs 6, the standby 3"

log "play the OCM work agent: apply spoke-1's Service, and report feedback for both sites"
apply_service() {
    kc -n spoke-1 get manifestwork "$MW" -o json |
        jq '[.spec.workload.manifests[] | select(.kind=="Service")][0]' |
        kc apply -f - >/dev/null
}
apply_service
svc_unadmitted || fail "a gated Service must start unadmitted"
ok "the Service starts unadmitted (the closed state), before any agent runs"

feedback() { # site available admission heartbeat
    local site=$1 avail=$2 adm=$3 hb=$4
    local vals
    vals=$(jq -nc --argjson a "$avail" --arg adm "$adm" --arg hb "$hb" '
      [{name:"readyReplicas",fieldValue:{type:"Integer",integer:$a}},
       {name:"updatedReplicas",fieldValue:{type:"Integer",integer:$a}},
       {name:"availableReplicas",fieldValue:{type:"Integer",integer:$a}}]
      + (if $adm != "" then [{name:"admission",fieldValue:{type:"String",string:$adm}},
                             {name:"admissionObservedAt",fieldValue:{type:"String",string:$hb}}] else [] end)')
    local conds
    conds=$(jq -nc --arg t "$(now_sec)" '[{type:"Applied",status:"True",reason:"Applied",message:"applied",lastTransitionTime:$t},{type:"Available",status:"True",reason:"Available",message:"available",lastTransitionTime:$t}]')
    kc -n "$site" patch manifestwork "$MW" --subresource=status --type=merge \
        -p "$(jq -nc --argjson c "$conds" --argjson v "$vals" '{status:{conditions:$c,resourceStatus:{manifests:[{resourceMeta:{ordinal:0,group:"apps",resource:"deployments"},conditions:[],statusFeedback:{values:$v}}]}}}')" >/dev/null
    kc -n "$site" patch manifestwork "${MW}-deps" --subresource=status --type=merge \
        -p "$(jq -nc --argjson c "$conds" '{status:{conditions:$c}}')" >/dev/null 2>&1 || true
}
db_active() { kc get failovergroup pg-docs -o jsonpath='{.status.activeDC}'; }
work_agent() { # as OCM's work agent reports feedback, every 3 s
    set +e
    while true; do
        a=$(svc_json 2>/dev/null | jq -r '.metadata.annotations["apps.k8s.appscode.com/admission"] // ""')
        h=$(svc_json 2>/dev/null | jq -r '.metadata.annotations["apps.k8s.appscode.com/admission-observed-at"] // ""')
        feedback spoke-1 "$(spec_replicas spoke-1 2>/dev/null || echo 0)" "$a" "$h" 2>/dev/null || true
        # spoke-2 does not exist here: report what a healthy agent there would.
        if [[ $(db_active) == spoke-2 ]]; then adm2=Admitted; else adm2=NotActiveHere; fi
        feedback spoke-2 "$(spec_replicas spoke-2 2>/dev/null || echo 0)" "$adm2" "$(now_sec)" 2>/dev/null || true
        sleep 3
    done
}
work_agent &
BG_PIDS+=($!)

log "deploy the gate agent from deploy/gate-agent.yaml (this cluster is spoke-1)"
sed -e 's#ghcr.io/kubeops/cattleset:latest#cattleset:e2e#' \
    -e 's#REPLACE-WITH-THIS-SPOKES-MANAGED-CLUSTER-NAME#spoke-1#' \
    -e '/image: cattleset:e2e/a\          imagePullPolicy: Never' \
    deploy/gate-agent.yaml >"$TMP/agent.yaml"
kc apply -f "$TMP/agent.yaml" >/dev/null
wait_for "the gate agent Deployment is available" 120 kc -n cattleset-system wait --for=condition=Available deploy/cattleset-gate-agent --timeout=5s
wait_for "the agent ADMITS: marker active here, fresh, and the database Service has a ready endpoint" 60 svc_admitted
svc_reason_is Admitted || fail "admission reason: $(svc_reason)"
[[ $(svc_json | jq -r '.spec.selector.app') == orders-api ]] || fail "the real selector was lost"
ok "the admission annotation says Admitted and the real selector is intact"
wait_for "the heartbeat annotation is written" 20 bash -c "kubectl --context kind-${CLUSTER} -n orders get svc orders-api -o json | jq -e '.metadata.annotations[\"apps.k8s.appscode.com/admission-observed-at\"] != null'"

log "the hub sees the spoke: serving DCs and the member entry in the FailoverGroup (real server-side apply)"
wait_for "status.servingDCs is [spoke-1]" 60 \
    bash -c "[[ \$(kubectl --context kind-${CLUSTER} -n orders get cattleset orders-api -o jsonpath='{.status.servingDCs}') == '[\"spoke-1\"]' ]]"
member() { kc get failovergroup orders-app -o json | jq -c '[.status.members[]? | select(.kind=="CattleSet")][0]'; }
wait_for "the herd reported itself ready on spoke-1 in FailoverGroup orders-app" 60 \
    bash -c "$(declare -f member kc); CLUSTER=$CLUSTER; member | jq -e '.dc==\"spoke-1\" and .ready==true and .observedAt != null'"
ok "member entry: $(member)"

# ---------------------------------------------------------------------------
log "the gate follows the database: marker staleness, primary loss grace, recovery"
rm -f "$MARKER_ON"
start=$SECONDS
wait_for "the marker stops being renewed: the gate REVOKES (MarkerStale)" 60 svc_unadmitted
svc_reason_is MarkerStale || fail "reason after marker loss: $(svc_reason)"
echo "  revoked $((SECONDS - start))s after the last marker renewal (30s TTL + up to one resync)"
touch "$MARKER_ON"
wait_for "the marker returns: the gate admits again, with no delay" 30 svc_admitted

echo spoke-2 >"$TMP/marker-dc"
wait_for "the marker names another DC: REVOKED (NotActiveHere)" 30 svc_unadmitted
svc_reason_is NotActiveHere || fail "reason: $(svc_reason)"
echo spoke-1 >"$TMP/marker-dc"
wait_for "...and admitted again when it names this DC" 30 svc_admitted

db_endpoint absent
sleep 6
svc_admitted || fail "the gate flapped inside the 15 s primary-loss grace (6 s in)"
ok "6s after the primary vanished the site is STILL admitted (grace)"
wait_for "...and REVOKED once the grace is over (NoPrimaryHere)" 30 svc_unadmitted
svc_reason_is NoPrimaryHere || fail "reason: $(svc_reason)"
db_endpoint ready
wait_for "a new primary: admitted again" 30 svc_admitted
# a primary change inside the grace never flaps
db_endpoint absent
sleep 4
db_endpoint ready
held_for "a 4s primary change never revoked the site" 8 svc_admitted

kc -n dc-failover create configmap primary-dc-pg-docs-override >/dev/null
rm -f "$MARKER_ON"
kc -n dc-failover delete configmap primary-dc-pg-docs >/dev/null
sleep 6
svc_admitted || fail "break-glass: the application must follow the override"
ok "break-glass override on this spoke keeps the site admitted with no marker"
kc -n dc-failover delete configmap primary-dc-pg-docs-override >/dev/null
wait_for "removing the override with no marker closes the gate" 30 svc_unadmitted
touch "$MARKER_ON"
wait_for "the marker is back: admitted" 30 svc_admitted

# ---------------------------------------------------------------------------
log "capacity holds when the coordination plane cannot say who holds the Lease (review G1)"
cond() { kc -n orders get cattleset orders-api -o json | jq -r --arg t "$1" '.status.conditions[]? | select(.type==$t) | .status'; }
kill "${BG_PIDS[0]}" 2>/dev/null || true # stop renewing the Leases
kc -n dc-failover delete lease primary-dc-orders-app primary-dc-pg-docs >/dev/null
wait_for "CapacityHeld becomes True" 60 bash -c "$(declare -f cond kc); CLUSTER=$CLUSTER; [[ \$(cond CapacityHeld) == True ]]"
[[ $(spec_replicas spoke-1) == 6 ]] || fail "the serving site was scaled to $(spec_replicas spoke-1) with no Lease"
ok "with both Leases gone the serving site stays at 6 (CapacityHeld=True)"
lease primary-dc-orders-app spoke-1
lease primary-dc-pg-docs spoke-1
agents &
BG_PIDS+=($!)
wait_for "the Leases are back: CapacityHeld clears" 60 bash -c "$(declare -f cond kc); CLUSTER=$CLUSTER; [[ \$(cond CapacityHeld) == False ]]"

# ---------------------------------------------------------------------------
if [[ -n $DRC_BIN ]]; then
    log "PHASE B: dr-controlplane's FailoverGroup controller (from the dc-dr-refresh branch) and a planned move"
    "$DRC_BIN" controller --hub-kubeconfig "$KUBECONFIG_FILE" >"$TMP/drc.log" 2>&1 &
    BG_PIDS+=($!)
    sleep 5

    # The database moves to spoke-2: its group is active there and its Lease is held there.
    log "the database group moves to spoke-2"
    kc patch failovergroup pg-docs --subresource=status --type=merge -p "{\"status\":{\"activeDC\":\"spoke-2\",\"members\":[{\"apiGroup\":\"kubedb.com\",\"kind\":\"Postgres\",\"namespace\":\"demo\",\"name\":\"pg-docs\",\"dc\":\"spoke-2\",\"ready\":true,\"observedAt\":\"$(now_sec)\"}]}}" >/dev/null
    kc -n dc-failover patch lease primary-dc-pg-docs --type=merge -p '{"spec":{"holderIdentity":"spoke-2"}}' >/dev/null
    echo spoke-2 >"$TMP/marker-dc"
    wait_for "spoke-1's gate revokes (the database is elsewhere)" 30 svc_unadmitted
    svc_reason_is NotActiveHere || fail "reason: $(svc_reason)"
    wait_for "REVIEW R1: the unadmitted old holder still reports READY on spoke-1" 60 \
        bash -c "$(declare -f member kc); CLUSTER=$CLUSTER; member | jq -e '.dc==\"spoke-1\" and .ready==true'"
    wait_for "dr-controlplane's FailoverGroup controller asks the herd to switch over (switchover-to=spoke-2)" 60 \
        bash -c "[[ \$(kubectl --context kind-${CLUSTER} -n orders get cattleset orders-api -o json | jq -r '.metadata.annotations[\"dr.kubedb.com/switchover-to\"] // \"\"') == spoke-2 ]]"
    wait_for "the herd requests the handoff on its member entry (handoffTo=spoke-2)" 60 \
        bash -c "$(declare -f member kc); CLUSTER=$CLUSTER; member | jq -e '.handoffTo==\"spoke-2\"'"
    lease_ann() { kc -n dc-failover get lease primary-dc-orders-app -o json | jq -r --arg k "$1" '.metadata.annotations[$k] // ""'; }
    wait_for "dr-controlplane validates it and writes handoff-to=spoke-2 on the app Lease" 60 \
        bash -c "$(declare -f lease_ann kc); CLUSTER=$CLUSTER; [[ \$(lease_ann dr.open-cluster-management.io/handoff-to) == spoke-2 ]]"
    [[ $(lease_ann dr.open-cluster-management.io/handoff-request) == spoke-2 ]] || fail "the companion annotation is missing"
    ok "the Lease carries handoff-to and the handoff-request companion"

    log "play the per-DC agents: the holder releases, the target acquires, handoff-to is cleared"
    kc -n dc-failover patch lease primary-dc-orders-app --type=merge \
        -p '{"spec":{"holderIdentity":"spoke-2"},"metadata":{"annotations":{"dr.open-cluster-management.io/handoff-to":null}}}' >/dev/null
    wait_for "the herd sees the Lease on the target and REMOVES switchover-to" 60 \
        bash -c "[[ -z \$(kubectl --context kind-${CLUSTER} -n orders get cattleset orders-api -o json | jq -r '.metadata.annotations[\"dr.kubedb.com/switchover-to\"] // \"\"') ]]"
    wait_for "dr-controlplane removes the leftover handoff-request companion" 60 \
        bash -c "$(declare -f lease_ann kc); CLUSTER=$CLUSTER; [[ -z \$(lease_ann dr.open-cluster-management.io/handoff-request) ]]"
    wait_for "the member entry moves to spoke-2 and the request is withdrawn" 60 \
        bash -c "$(declare -f member kc); CLUSTER=$CLUSTER; member | jq -e '.dc==\"spoke-2\" and (.handoffTo // \"\") == \"\"'"
    wait_for "the application's FailoverGroup is Steady on spoke-2" 90 \
        bash -c "kubectl --context kind-${CLUSTER} get failovergroup orders-app -o json | jq -e '.status.phase==\"Steady\" and .status.activeDC==\"spoke-2\"'"
    ok "a planned move of the application group, request to Lease handoff, completed"
fi

# ---------------------------------------------------------------------------
log "delete the application: the finalizer removes the ManifestWorks and withdraws the member (review G5)"
kc -n orders delete cattleset orders-api --wait=false >/dev/null
wait_for "the CattleSet is gone" 60 bash -c "! kubectl --context kind-${CLUSTER} -n orders get cattleset orders-api"
for site in spoke-1 spoke-2; do
    kc -n "$site" get manifestwork "$MW" >/dev/null 2>&1 && fail "ManifestWork left behind in $site"
    kc -n "$site" get manifestwork "${MW}-deps" >/dev/null 2>&1 && fail "deps ManifestWork left behind in $site"
done
ok "no ManifestWork is left on either site"
[[ $(member) == null ]] || fail "the member entry was not withdrawn: $(member)"
ok "the herd's entry is gone from FailoverGroup orders-app (real server-side apply released it)"

COMPLETED=1
