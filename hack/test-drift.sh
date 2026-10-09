#!/usr/bin/env bash
#
# Backflow - drift detection end-to-end test
#
# Requires the environment from ./hack/dev-up.sh (kind, Argo CD, demo-app and
# the backflow-system/argocd-token Secret).
# Starts its own port-forward to Argo CD on a free local port and the operator
# in the background, then drifts the demo resources and checks that:
#   - a changed ConfigMap value becomes a DriftProposal (Detected, then Mapping) with the
#     right changes, labels and source fields
#   - detecting again does not create a duplicate
#   - a different drift supersedes the open proposal
#   - going back to the Git value reverts the open proposal
#   - a replica change is detected, and ignored with spec.ignoreFields
#
# The demo resources are always restored to their Git state on exit.
#
# With IN_CLUSTER=true the operator is the one deployed by 'make deploy'
# (see "In-cluster" in the README); the script then starts no local operator
# and no port-forward, and reaches Argo CD at its in-cluster service URL.
#
# Usage (from the repository root):
#   ./hack/test-drift.sh
#   KEEP=true ./hack/test-drift.sh      # keep test resources for inspection
#   IN_CLUSTER=true ./hack/test-drift.sh   # test the deployed operator

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
NS="${NS:-backflow-drift-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
APP="${APP:-demo-app}"
DEMO_NS="${DEMO_NS:-demo}"
POLICY="drift-policy"
TOKEN_SECRET_NS="${TOKEN_SECRET_NS:-backflow-system}"
TOKEN_SECRET="${TOKEN_SECRET:-argocd-token}"
KEEP="${KEEP:-false}"
# A drift becomes a proposal after it was stable for this long (the policy's batchWindow).
WINDOW="${WINDOW:-5s}"
# Test the operator deployed in the cluster instead of a local bin/manager.
IN_CLUSTER="${IN_CLUSTER:-false}"
MANAGER_NS="${MANAGER_NS:-backflow-system}"
MANAGER_DEPLOY="${MANAGER_DEPLOY:-backflow-controller-manager}"
ARGOCD_URL="${ARGOCD_URL:-https://argocd-server.argocd.svc}"
# Seconds to wait for Argo CD to notice a change and the operator to react.
WAIT="${WAIT:-90}"
# Seconds to wait before asserting that nothing happened.
QUIET="${QUIET:-10}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT/tmp"
LOG_FILE="$LOG_DIR/test-drift-manager.log"
MANAGER_PID=""
PF_PID=""
RESTORE=false
ORIG_LOG_LEVEL=""
ORIG_REPLICAS=""
PASS=0
FAIL=0

if [[ -t 1 ]]; then
  GREEN=$'\e[32m'; RED=$'\e[31m'; BOLD=$'\e[1m'; NC=$'\e[0m'
else
  GREEN=""; RED=""; BOLD=""; NC=""
fi

step() { echo; echo "${BOLD}▶ $*${NC}"; }
pass() { echo "  ${GREEN}✔${NC} $*"; PASS=$((PASS + 1)); }
fail() { echo "  ${RED}✘${NC} $*"; FAIL=$((FAIL + 1)); }
die()  { echo "${RED}ERROR:${NC} $*" >&2; exit 1; }

k() { kubectl --context "kind-$CLUSTER_NAME" "$@"; }

# refresh_log: make $LOG_FILE hold the current operator log. A local operator
# writes it directly; the deployed one is read from the cluster.
refresh_log() {
  if [[ "$IN_CLUSTER" == "true" ]]; then
    mkdir -p "$LOG_DIR"
    k -n "$MANAGER_NS" logs "deploy/$MANAGER_DEPLOY" >"$LOG_FILE" 2>&1 || true
  fi
}

# wait_for <description> <command...>: poll until the command succeeds.
# A timeout is recorded as a failure; the script carries on.
wait_for() {
  local desc="$1" waited=0
  shift
  while ! "$@" >/dev/null 2>&1; do
    waited=$((waited + 2))
    if (( waited > WAIT )); then
      fail "$desc (timed out after ${WAIT}s)"
      return 0
    fi
    sleep 2
  done
  pass "$desc"
}

# Ask Argo CD to re-compare right away instead of waiting for its timer.
refresh_app() {
  k -n "$ARGOCD_NS" annotate application "$APP" argocd.argoproj.io/refresh=normal --overwrite >/dev/null 2>&1 || true
}

app_sync_status() { k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.status}' 2>/dev/null; }

# Put the demo resources back to their Git state by syncing the Application,
# then wait until Argo CD reports it Synced.
restore_demo() {
  k -n "$DEMO_NS" patch configmap demo-config --type merge \
    -p "{\"data\":{\"LOG_LEVEL\":\"$ORIG_LOG_LEVEL\"}}" >/dev/null 2>&1 || true
  k -n "$DEMO_NS" scale deployment demo --replicas="$ORIG_REPLICAS" >/dev/null 2>&1 || true
  k -n "$ARGOCD_NS" patch application "$APP" --type merge \
    -p '{"operation":{"initiatedBy":{"username":"backflow-test"},"sync":{}}}' >/dev/null 2>&1 || true
  local i
  for i in $(seq 1 45); do
    refresh_app
    [[ "$(app_sync_status)" == "Synced" ]] && return 0
    sleep 2
  done
  return 1
}

cleanup() {
  local status=$?
  set +e
  if [[ "$RESTORE" == "true" ]]; then
    echo
    echo "Restoring demo resources to their Git state..."
    if restore_demo; then
      echo "  demo-app is Synced again"
    else
      echo "  ${RED}WARNING:${NC} demo-app is not Synced; run: kubectl -n $ARGOCD_NS get application $APP" >&2
    fi
  fi
  if [[ -n "$MANAGER_PID" ]] && kill -0 "$MANAGER_PID" 2>/dev/null; then
    kill "$MANAGER_PID" 2>/dev/null
    wait "$MANAGER_PID" 2>/dev/null
  fi
  if [[ -n "$PF_PID" ]] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null
    wait "$PF_PID" 2>/dev/null
  fi
  if [[ "$KEEP" != "true" ]]; then
    k delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1
  fi
  exit "$status"
}
trap cleanup EXIT

# proposals <resource name>: one line per DriftProposal of the policy:
#   <name>|<phase>|<resource name>|<supersededBy>|<path>:<op>:<desired>><live>;...
proposals() {
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" -o jsonpath='{range .items[*]}{.metadata.name}|{.status.phase}|{.spec.resource.name}|{.status.supersededBy}|{range .spec.changes[*]}{.path}:{.op}:{.desired}>{.live};{end}{"\n"}{end}' 2>/dev/null |
    awk -F'|' -v r="$1" '$3 == r'
}

# proposal_with <resource name> <phase> <changes substring>: print matching names.
# <phase> may be several phases separated by "|".
proposal_with() {
  proposals "$1" | awk -F'|' -v p="$2" -v c="$3" '$2 ~ "^(" p ")$" && index($5, c) > 0 { print $1 }'
}

has_proposal()   { [[ -n "$(proposal_with "$1" "$2" "$3")" ]]; }
count_open()     { proposals "$1" | awk -F'|' '$2 == "Detected" || $2 == "Mapping" || $2 == "Proposed" || $2 == "Unmapped" || $2 == ""' | wc -l | tr -d ' '; }
no_open()        { [[ "$(count_open "$1")" == "0" ]]; }

# dp_field <proposal> <jsonpath>
dp_field() { k -n "$NS" get driftproposal "$1" -o jsonpath="{$2}" 2>/dev/null; }

# expect_eq <description> <expected> <actual>
expect_eq() {
  if [[ "$3" == "$2" ]]; then
    pass "$1 = $2"
  else
    fail "$1: expected '$2', got '$3'"
  fi
}

# expect_nonempty <description> <actual>
expect_nonempty() {
  if [[ -n "$2" ]]; then pass "$1 = $2"; else fail "$1 is empty"; fi
}

resource_hash() {
  printf '%s' "$1/$2/$3/$4" | shasum -a 256 | cut -c1-16
}

# app_resource_status <kind> <name>
app_resource_status() {
  k -n "$ARGOCD_NS" get application "$APP" \
    -o jsonpath="{.status.resources[?(@.kind==\"$1\")].status}" 2>/dev/null
}

# Conditions for wait_for; they are functions so they are evaluated on every poll.
app_is_synced()        { [[ "$(app_sync_status)" == "Synced" ]]; }
deployment_drifted()   { [[ "$(app_resource_status Deployment)" == "OutOfSync" ]]; }
one_open_proposal()    { [[ "$(k -n "$NS" get bfp "$POLICY" -o jsonpath='{.status.openProposals}')" == "1" ]]; }

set_log_level() {
  k -n "$DEMO_NS" patch configmap demo-config --type merge -p "{\"data\":{\"LOG_LEVEL\":\"$1\"}}" >/dev/null
  refresh_app
}

set_replicas() {
  k -n "$DEMO_NS" scale deployment demo --replicas="$1" >/dev/null
  refresh_app
}

cd "$ROOT"

# ---------------------------------------------------------------------------
step "Prerequisites"
for cmd in kubectl make curl python3 shasum; do
  command -v "$cmd" >/dev/null || die "'$cmd' not found"
done
k cluster-info >/dev/null 2>&1 || die "cluster kind-$CLUSTER_NAME is not reachable. Run ./hack/dev-up.sh first."
k get crd applications.argoproj.io >/dev/null 2>&1 || die "Argo CD is not installed. Run ./hack/dev-up.sh first."
k -n "$ARGOCD_NS" get application "$APP" >/dev/null 2>&1 || die "Application $APP not found. Run ./hack/dev-up.sh first."
k -n "$TOKEN_SECRET_NS" get secret "$TOKEN_SECRET" >/dev/null 2>&1 || die "Secret $TOKEN_SECRET_NS/$TOKEN_SECRET not found. Run ./hack/dev-up.sh first."
if [[ "$IN_CLUSTER" == "true" ]]; then
  k -n "$MANAGER_NS" get deployment "$MANAGER_DEPLOY" >/dev/null 2>&1 || die "Deployment $MANAGER_NS/$MANAGER_DEPLOY not found. Run make docker-build, kind load docker-image and make deploy first."
elif curl -fs -o /dev/null http://localhost:8081/healthz 2>/dev/null; then
  die "An operator is already running on port 8081 (probably 'make run'). Stop it first."
fi

# Everything below assumes the demo resources match Git.
ORIG_LOG_LEVEL="$(k -n "$DEMO_NS" get configmap demo-config -o jsonpath='{.data.LOG_LEVEL}')"
# The mapping milestone moves a detected proposal on to Mapping within seconds.
DETECTED="Detected|Mapping"
ORIG_REPLICAS="$(k -n "$DEMO_NS" get deployment demo -o jsonpath='{.spec.replicas}')"
RESTORE=true
if [[ "$(app_sync_status)" != "Synced" ]]; then
  echo "  $APP is not Synced; syncing it first..."
  restore_demo || die "$APP did not become Synced"
  ORIG_LOG_LEVEL="$(k -n "$DEMO_NS" get configmap demo-config -o jsonpath='{.data.LOG_LEVEL}')"
  ORIG_REPLICAS="$(k -n "$DEMO_NS" get deployment demo -o jsonpath='{.spec.replicas}')"
fi
pass "cluster, Argo CD and $APP found (LOG_LEVEL=$ORIG_LOG_LEVEL, replicas=$ORIG_REPLICAS)"

APP_REPO="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.source.repoURL}')"
APP_PATH="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.source.path}')"
APP_TARGET="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.source.targetRevision}')"

# ---------------------------------------------------------------------------
step "Argo CD connection"
if [[ "$IN_CLUSTER" == "true" ]]; then
  PF_PORT=""
  pass "operator reaches Argo CD at $ARGOCD_URL"
else
  PF_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  # Call kubectl directly (not through k) so $! is the kubectl process itself.
  kubectl --context "kind-$CLUSTER_NAME" -n "$ARGOCD_NS" port-forward svc/argocd-server "$PF_PORT":443 >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    curl -ksf -o /dev/null "https://localhost:$PF_PORT/api/version" && break
    kill -0 "$PF_PID" 2>/dev/null || die "port-forward exited"
    sleep 1
  done
  curl -ksf -o /dev/null "https://localhost:$PF_PORT/api/version" || die "Argo CD is not reachable on localhost:$PF_PORT"
  ARGOCD_URL="https://localhost:$PF_PORT"
  pass "Argo CD reachable on $ARGOCD_URL"
fi

# ---------------------------------------------------------------------------
step "Operator"
if [[ "$IN_CLUSTER" == "true" ]]; then
  k -n "$MANAGER_NS" rollout status "deploy/$MANAGER_DEPLOY" --timeout=120s >/dev/null || die "deployment $MANAGER_NS/$MANAGER_DEPLOY is not available"
else
  make manifests install >/dev/null
  make build >/dev/null
  mkdir -p "$LOG_DIR"
  "$ROOT/bin/manager" >"$LOG_FILE" 2>&1 &
  MANAGER_PID=$!
  for _ in $(seq 1 60); do
    curl -fs -o /dev/null http://localhost:8081/readyz 2>/dev/null && break
    kill -0 "$MANAGER_PID" 2>/dev/null || die "operator exited, log: $LOG_FILE"
    sleep 1
  done
  curl -fs -o /dev/null http://localhost:8081/readyz || die "operator did not become ready, log: $LOG_FILE"
fi
refresh_log
if grep -q "Argo CD Application CRD not found" "$LOG_FILE"; then
  fail "operator did not start watching Argo CD Applications"
else
  pass "operator running and watching Argo CD Applications"
fi

# ---------------------------------------------------------------------------
step "Test namespace and policy: $NS/$POLICY"
k delete namespace "$NS" --ignore-not-found --wait=true >/dev/null
k create namespace "$NS" >/dev/null
TOKEN="$(k -n "$TOKEN_SECRET_NS" get secret "$TOKEN_SECRET" -o jsonpath='{.data.token}' | base64 -d)"
k -n "$NS" create secret generic argocd-token --from-literal=token="$TOKEN" >/dev/null
TOKEN=""

apply_policy() {
  local ignore="${1:-}"
  k apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata:
  name: $POLICY
  namespace: $NS
spec:
  argoCDNamespace: $ARGOCD_NS
  mode: ReportOnly
  batchWindow: ${WINDOW:-5s}
  applications:
    names: ["$APP"]
  argoCD:
    url: $ARGOCD_URL
    insecureSkipTLSVerify: true
    tokenSecretRef:
      name: argocd-token
      key: token
$ignore
YAML
}
apply_policy
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s

# ---------------------------------------------------------------------------
step "1. ConfigMap value drift becomes a DriftProposal"
set_log_level debug
wait_for "proposal Detected for demo-config LOG_LEVEL info -> debug" \
  has_proposal demo-config "$DETECTED" '/data/LOG_LEVEL:Replace:"info">"debug";'
P1="$(proposal_with demo-config "$DETECTED" '"debug"' | head -n 1)"
if [[ -n "$P1" ]]; then
  case "$(dp_field "$P1" .status.phase)" in
    Detected | Mapping) pass "phase is Detected or Mapping" ;;
    *) fail "phase: expected Detected or Mapping, got '$(dp_field "$P1" .status.phase)'" ;;
  esac
  expect_eq "label policy" "$POLICY" "$(dp_field "$P1" '.metadata.labels.backflow\.io/policy')"
  expect_eq "label application" "$APP" "$(dp_field "$P1" '.metadata.labels.backflow\.io/application')"
  expect_eq "label resource" "$(resource_hash "" ConfigMap "$DEMO_NS" demo-config)" \
    "$(dp_field "$P1" '.metadata.labels.backflow\.io/resource')"
  expect_eq "owner" "$POLICY" "$(dp_field "$P1" '.metadata.ownerReferences[0].name')"
  expect_eq "spec.policyName" "$POLICY" "$(dp_field "$P1" .spec.policyName)"
  expect_eq "spec.application.name" "$APP" "$(dp_field "$P1" .spec.application.name)"
  expect_eq "spec.application.namespace" "$ARGOCD_NS" "$(dp_field "$P1" .spec.application.namespace)"
  expect_eq "spec.resource.kind" "ConfigMap" "$(dp_field "$P1" .spec.resource.kind)"
  expect_eq "spec.resource.version" "v1" "$(dp_field "$P1" .spec.resource.version)"
  expect_eq "spec.resource.namespace" "$DEMO_NS" "$(dp_field "$P1" .spec.resource.namespace)"
  expect_eq "spec.resource.name" "demo-config" "$(dp_field "$P1" .spec.resource.name)"
  expect_eq "spec.source.repoURL" "$APP_REPO" "$(dp_field "$P1" .spec.source.repoURL)"
  expect_eq "spec.source.path" "$APP_PATH" "$(dp_field "$P1" .spec.source.path)"
  expect_eq "spec.source.targetRevision" "$APP_TARGET" "$(dp_field "$P1" .spec.source.targetRevision)"
  expect_eq "spec.source.type" "Directory" "$(dp_field "$P1" .spec.source.type)"
  expect_eq "spec.source.revision" "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}')" \
    "$(dp_field "$P1" .spec.source.revision)"
  expect_eq "spec.changes count" "1" "$(k -n "$NS" get driftproposal "$P1" -o jsonpath='{.spec.changes[*].path}' | wc -w | tr -d ' ')"
  expect_eq "spec.actor" "" "$(dp_field "$P1" .spec.actor.username)"
  expect_nonempty "spec.detectedAt" "$(dp_field "$P1" .spec.detectedAt)"
fi
wait_for "policy status.openProposals = 1" one_open_proposal

# ---------------------------------------------------------------------------
step "2. Detecting again does not create a duplicate"
# A generation bump makes the operator reconcile the policy again.
k -n "$NS" patch bfp "$POLICY" --type merge -p '{"spec":{"batchWindow":"6s"}}' >/dev/null
sleep "$QUIET"
expect_eq "proposals for demo-config" "1" "$(proposals demo-config | wc -l | tr -d ' ')"

# ---------------------------------------------------------------------------
step "3. A different drift supersedes the open proposal"
set_log_level trace
wait_for "new proposal Detected with info -> trace" \
  has_proposal demo-config "$DETECTED" '/data/LOG_LEVEL:Replace:"info">"trace";'
P3="$(proposal_with demo-config "$DETECTED" '"trace"' | head -n 1)"
wait_for "old proposal is Superseded" has_proposal demo-config Superseded '"debug"'
if [[ -n "$P1" && -n "$P3" ]]; then
  expect_eq "old proposal supersededBy" "$P3" "$(dp_field "$P1" .status.supersededBy)"
  expect_eq "old proposal phase" "Superseded" "$(dp_field "$P1" .status.phase)"
fi
expect_eq "open proposals for demo-config" "1" "$(count_open demo-config)"

# ---------------------------------------------------------------------------
step "4. Going back to the Git value reverts the open proposal"
set_log_level "$ORIG_LOG_LEVEL"
wait_for "proposal is Reverted" has_proposal demo-config Reverted '"trace"'
expect_eq "open proposals for demo-config" "0" "$(count_open demo-config)"
expect_eq "superseded proposal stays Superseded" "Superseded" "$(dp_field "$P1" .status.phase)"

# ---------------------------------------------------------------------------
step "5. Replica drift"
set_replicas 3
wait_for "proposal Detected for demo replicas 1 -> 3" \
  has_proposal demo "$DETECTED" "/spec/replicas:Replace:$ORIG_REPLICAS>3;"
P5="$(proposal_with demo "$DETECTED" '/spec/replicas' | head -n 1)"
if [[ -n "$P5" ]]; then
  expect_eq "spec.resource.kind" "Deployment" "$(dp_field "$P5" .spec.resource.kind)"
  expect_eq "spec.resource.group" "apps" "$(dp_field "$P5" .spec.resource.group)"
fi
set_replicas "$ORIG_REPLICAS"
wait_for "replica proposal is Reverted" has_proposal demo Reverted "/spec/replicas:Replace:$ORIG_REPLICAS>3;"
wait_for "$APP is Synced again" app_is_synced

step "5b. spec.ignoreFields hides the replica drift"
apply_policy "  ignoreFields:
    - group: apps
      kind: Deployment
      jsonPointers: [\"/spec/replicas\"]"
set_replicas 3
wait_for "Argo CD reports the Deployment OutOfSync" deployment_drifted
sleep "$QUIET"
expect_eq "open proposals for demo" "0" "$(count_open demo)"
set_replicas "$ORIG_REPLICAS"

# ---------------------------------------------------------------------------
step "6. Restoring the demo resources"
if restore_demo; then
  RESTORE=false
  pass "$APP is Synced and demo resources are back to Git"
else
  fail "$APP did not become Synced again"
fi

# ---------------------------------------------------------------------------
echo
echo "${BOLD}Summary:${NC} ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}"
if [[ "$KEEP" == "true" ]]; then
  echo "Test resources kept in namespace '$NS': kubectl -n $NS get bfp,driftproposals"
fi
if (( FAIL > 0 )); then
  echo "Operator log: tmp/test-drift-manager.log"
  exit 1
fi
