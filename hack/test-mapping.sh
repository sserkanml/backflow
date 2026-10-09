#!/usr/bin/env bash
#
# Backflow - mapping end-to-end test
#
# Requires the environment from ./hack/dev-up.sh (kind, Argo CD, demo-app and
# the backflow-system/argocd-token Secret). The demo repository is public, so
# no Git token is needed.
# Starts its own port-forward to Argo CD on a free local port and the operator
# in the background, then drifts the demo resources and checks that each
# DriftProposal is mapped to the file in Git that defines the resource:
#   - a changed ConfigMap value: file, location, verified, and a diff that
#     touches only the changed line (comments around it stay out of the change)
#   - a replica change in the Deployment
#   - proposals that cannot be mapped become Unmapped with a reason
#   - a repository that cannot be reached or seen (private, no access) is
#     retried, never given up on; only a commit that does not exist is final
#
# The Unmapped and unreachable-repository cases use DriftProposals created by
# hand: the demo repository defines every resource exactly once, so a real
# drift can always be mapped. The operator maps any proposal the same way.
#
# The demo resources are always restored to their Git state on exit.
#
# With IN_CLUSTER=true the operator is the one deployed by 'make deploy'
# (see "In-cluster" in the README); the script then starts no local operator
# and no port-forward, and reaches Argo CD at its in-cluster service URL.
#
# Usage (from the repository root):
#   ./hack/test-mapping.sh
#   KEEP=true ./hack/test-mapping.sh      # keep test resources for inspection
#   IN_CLUSTER=true ./hack/test-mapping.sh   # test the deployed operator

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
NS="${NS:-backflow-mapping-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
APP="${APP:-demo-app}"
DEMO_NS="${DEMO_NS:-demo}"
POLICY="mapping-policy"
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
LOG_FILE="$LOG_DIR/test-mapping-manager.log"
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

expect_eq() {
  if [[ "$3" == "$2" ]]; then
    pass "$1 = $2"
  else
    fail "$1: expected '$2', got '$3'"
  fi
}

# app_resource_status <kind> <name>
app_resource_status() {
  k -n "$ARGOCD_NS" get application "$APP" \
    -o jsonpath="{.status.resources[?(@.kind==\"$1\")].status}" 2>/dev/null
}

# Conditions for wait_for; they are functions so they are evaluated on every poll.
app_is_synced()        { [[ "$(app_sync_status)" == "Synced" ]]; }

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


# proposals_of <resource name>: names of the proposals of the policy for it.
proposals_of() {
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.spec.resource.name}{"\n"}{end}' 2>/dev/null |
    awk -F'|' -v r="$1" '$2 == r { print $1 }'
}

# dp_field <proposal> <jsonpath>
dp_field() { k -n "$NS" get driftproposal "$1" -o jsonpath="{$2}" 2>/dev/null || true; }

# Conditions for wait_for; they are functions so they are evaluated on every poll.
dp_phase_is() { [[ "$(dp_field "$1" .status.phase)" == "$2" ]]; }
reason_is()   { [[ "$(mapped_reason "$1")" == "$2" ]]; }

mapped_status() { dp_field "$1" '.status.conditions[?(@.type=="Mapped")].status'; }
mapped_reason() { dp_field "$1" '.status.conditions[?(@.type=="Mapped")].reason'; }

# The proposal of the policy for <resource name> that is mapped, if any.
mapped_proposal() {
  local p
  for p in $(proposals_of "$1"); do
    if [[ "$(mapped_status "$p")" == "True" ]]; then echo "$p"; return 0; fi
  done
  return 1
}

# expect_diff_line <description> <proposal> <exact diff line>
expect_diff_line() {
  if dp_field "$2" .status.mapping.diff | grep -qxF -- "$3"; then
    pass "$1: diff has '$3'"
  else
    fail "$1: diff lacks the line '$3'"
  fi
}

# create_proposal <name> <kind> <resource ns> <resource name> <source type> <repoURL> <revision>
# creates a Directory proposal of the policy by hand, like the drift controller would.
create_proposal() {
  k apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata:
  name: $1
  namespace: $NS
spec:
  policyName: $POLICY
  application:
    name: $APP
    namespace: $ARGOCD_NS
  resource:
    version: v1
    kind: $2
    namespace: $3
    name: $4
  source:
    repoURL: $6
    revision: $7
    targetRevision: $APP_TARGET
    path: $APP_PATH
    type: $5
  changes:
    - path: /data/LOG_LEVEL
      op: Replace
      desired: '"info"'
      live: '"debug"'
  detectedAt: "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
YAML
}

app_revision() { k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}' 2>/dev/null; }


# ---------------------------------------------------------------------------
step "The demo repository at its latest commit"
# The comment above LOG_LEVEL only exists in recent commits, so wait until
# Argo CD has synced the branch head the mapping will read.
REMOTE_HEAD=""
if command -v git >/dev/null 2>&1; then
  REMOTE_HEAD="$(git ls-remote "$APP_REPO" "refs/heads/$APP_TARGET" 2>/dev/null | cut -f1 || true)"
fi
app_at_head() { [[ "$(app_revision)" == "$REMOTE_HEAD" && "$(app_sync_status)" == "Synced" ]]; }
if [[ -n "$REMOTE_HEAD" ]]; then
  k -n "$ARGOCD_NS" annotate application "$APP" argocd.argoproj.io/refresh=hard --overwrite >/dev/null 2>&1 || true
  wait_for "$APP is synced to $APP_TARGET at ${REMOTE_HEAD:0:12}" app_at_head
else
  pass "no branch head to compare with; using the revision $APP is synced to ($(app_revision | cut -c1-12))"
fi
SYNCED_REV="$(app_revision)"
ORIG_LOG_LEVEL="$(k -n "$DEMO_NS" get configmap demo-config -o jsonpath='{.data.LOG_LEVEL}')"
ORIG_REPLICAS="$(k -n "$DEMO_NS" get deployment demo -o jsonpath='{.spec.replicas}')"

# ---------------------------------------------------------------------------
step "1. A ConfigMap value is mapped to its line in Git"
set_log_level debug
wait_for "the demo-config proposal is mapped" mapped_proposal demo-config
P1="$(mapped_proposal demo-config || true)"
if [[ -n "$P1" ]]; then
  expect_eq "phase (the merge request milestone moves it on)" "Mapping" "$(dp_field "$P1" .status.phase)"
  expect_eq "Mapped reason" "Mapped" "$(mapped_reason "$P1")"
  expect_eq "strategy" "Direct" "$(dp_field "$P1" .status.mapping.strategy)"
  expect_eq "verified" "true" "$(dp_field "$P1" .status.mapping.verified)"
  expect_eq "number of edits" "1" "$(dp_field "$P1" '.status.mapping.edits[*].file' | wc -w | tr -d ' ')"
  expect_eq "edit file" "$APP_PATH/configmap.yaml" "$(dp_field "$P1" .status.mapping.edits[0].file)"
  expect_eq "edit location" "/data/LOG_LEVEL" "$(dp_field "$P1" .status.mapping.edits[0].location)"
  expect_eq "edit value" '"debug"' "$(dp_field "$P1" .status.mapping.edits[0].value)"
  expect_eq "edit changeIndex" "0" "$(dp_field "$P1" .status.mapping.edits[0].changeIndex)"
  expect_eq "mapped at the synced revision" "$SYNCED_REV" "$(dp_field "$P1" .spec.source.revision)"

  DIFF1="$(dp_field "$P1" .status.mapping.diff)"
  expect_diff_line "diff" "$P1" "--- a/$APP_PATH/configmap.yaml"
  expect_diff_line "diff" "$P1" "-  LOG_LEVEL: info"
  expect_diff_line "diff" "$P1" "+  LOG_LEVEL: debug"
  expect_eq "changed lines in the diff" "2" \
    "$(printf '%s\n' "$DIFF1" | grep -E '^[+-]' | grep -cvE '^(\+\+\+|---) ')"
  if printf '%s\n' "$DIFF1" | grep -E '^[+-]' | grep -vE '^(\+\+\+|---) ' | grep -q '#'; then
    fail "a comment line is part of the change"
  else
    pass "no comment line is part of the change"
  fi
  if printf '%s\n' "$DIFF1" | grep -qE '^ +# Log verbosity'; then
    pass "the comment above LOG_LEVEL is untouched context"
  else
    fail "the comment above LOG_LEVEL is not in the diff context (does $APP_REPO have it at ${SYNCED_REV:0:12}?)"
  fi
fi

# ---------------------------------------------------------------------------
step "2. A replica change is mapped to the Deployment"
# Put LOG_LEVEL back first; the open proposal is reverted.
set_log_level "$ORIG_LOG_LEVEL"
wait_for "the LOG_LEVEL proposal is reverted" dp_phase_is "${P1:-none}" Reverted
set_replicas 3
wait_for "the demo proposal is mapped" mapped_proposal demo
P2="$(mapped_proposal demo || true)"
if [[ -n "$P2" ]]; then
  expect_eq "verified" "true" "$(dp_field "$P2" .status.mapping.verified)"
  expect_eq "edit file" "$APP_PATH/deployment.yaml" "$(dp_field "$P2" .status.mapping.edits[0].file)"
  expect_eq "edit location" "/spec/replicas" "$(dp_field "$P2" .status.mapping.edits[0].location)"
  expect_eq "edit value" "3" "$(dp_field "$P2" .status.mapping.edits[0].value)"
  expect_diff_line "diff" "$P2" "-  replicas: $ORIG_REPLICAS"
  expect_diff_line "diff" "$P2" "+  replicas: 3"
  expect_eq "changed lines in the diff" "2" \
    "$(dp_field "$P2" .status.mapping.diff | grep -E '^[+-]' | grep -cvE '^(\+\+\+|---) ')"
fi

# ---------------------------------------------------------------------------
step "3. What cannot be traced back to Git becomes Unmapped, with a reason"
create_proposal ghost ConfigMap "$DEMO_NS" ghost Directory "$APP_REPO" "$SYNCED_REV"
create_proposal not-directory ConfigMap "$DEMO_NS" demo-config Helm "$APP_REPO" "$SYNCED_REV"
create_proposal unknown-commit ConfigMap "$DEMO_NS" demo-config Directory "$APP_REPO" 0123456789abcdef0123456789abcdef01234567

# expect_unmapped <proposal> <reason> <description>
expect_unmapped() {
  wait_for "$3 is Unmapped" dp_phase_is "$1" Unmapped
  expect_eq "$3: Mapped reason" "$2" "$(mapped_reason "$1")"
  expect_eq "$3: Mapped status" "False" "$(mapped_status "$1")"
  expect_eq "$3: strategy" "Unmapped" "$(dp_field "$1" .status.mapping.strategy)"
  expect_nonempty "$3: mapping.reason" "$(dp_field "$1" .status.mapping.reason)"
  expect_eq "$3: number of edits" "0" "$(dp_field "$1" '.status.mapping.edits[*].file' | wc -w | tr -d ' ')"
}
expect_nonempty() { if [[ -n "$2" ]]; then pass "$1 = $2"; else fail "$1 is empty"; fi; }
expect_unmapped ghost NotFound "a resource that is not in the repository"
expect_unmapped not-directory UnsupportedSourceType "a Helm source"
expect_unmapped unknown-commit RevisionNotFound "a commit that does not exist"

# ---------------------------------------------------------------------------
step "4. A repository that cannot be reached or seen is retried, not given up on"
create_proposal unreachable ConfigMap "$DEMO_NS" demo-config Directory "https://localhost:1/none.git" "$SYNCED_REV"
# GitLab and GitHub answer 404 or 401 for a repository the caller cannot see;
# either way it is a matter of access, which the user can fix.
create_proposal no-access ConfigMap "$DEMO_NS" demo-config Directory "https://gitlab.com/sserkanml/backflow-no-such-repository.git" "$SYNCED_REV"
wait_for "the unreachable proposal reports RepositoryUnavailable" reason_is unreachable RepositoryUnavailable
sleep $((QUIET + 5))
expect_eq "phase after waiting" "Mapping" "$(dp_field unreachable .status.phase)"
expect_eq "Mapped reason after waiting" "RepositoryUnavailable" "$(mapped_reason unreachable)"
expect_eq "no-access: phase after waiting" "Mapping" "$(dp_field no-access .status.phase)"
case "$(mapped_reason no-access)" in
  RepositoryNotFound | RepositoryAuthFailed) pass "no-access: Mapped reason is $(mapped_reason no-access)" ;;
  *) fail "no-access: expected RepositoryNotFound or RepositoryAuthFailed, got '$(mapped_reason no-access)'" ;;
esac
expect_eq "no-access: Mapped status" "False" "$(mapped_status no-access)"
refresh_log
RETRIES="$(grep -c 'Cannot map the proposal yet' "$LOG_FILE" || true)"
if (( RETRIES >= 2 )); then
  pass "the operator keeps retrying ($RETRIES attempts logged)"
else
  fail "expected repeated retries, saw $RETRIES attempt(s) in $LOG_FILE"
fi

# ---------------------------------------------------------------------------
step "5. The demo resources are restored"
RESTORE=true
if restore_demo; then
  pass "$APP is Synced and the demo resources are back to Git"
  RESTORE=false
else
  fail "$APP did not become Synced again"
fi

echo
echo "${BOLD}Summary:${NC} ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}"
if [[ "$KEEP" == "true" ]]; then
  echo "Test resources kept in namespace '$NS': kubectl -n $NS get bfp,driftproposals"
fi
if (( FAIL > 0 )); then
  echo "Operator log: tmp/test-mapping-manager.log"
  exit 1
fi
