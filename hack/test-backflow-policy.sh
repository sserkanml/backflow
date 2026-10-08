#!/usr/bin/env bash
#
# Backflow - BackflowPolicy end-to-end test
#
# Requires the environment from ./hack/dev-up.sh (kind, Argo CD, demo-app).
# Starts the operator in the background and checks that a BackflowPolicy:
#   - reports MissingScmConnection when no ScmConnection matches the repo
#   - is Ready in ReportOnly mode even without an ScmConnection
#   - becomes Resolved by itself when a matching ScmConnection appears
#   - records repo, path, revisions, source type and resource count
#   - reports NoApplicationsMatched and InvalidSpec
#   - follows label changes on the Argo CD Application
#   - lets only the oldest policy manage an Application: a second policy that
#     selects it reports ApplicationConflict, and recovers when the first goes
#
# An Application can be managed by one policy only, so the scenarios below
# never keep two policies on demo-app at the same time, except where the
# conflict itself is tested.
#
# No GitLab token is needed: matching only looks at hosts.
#
# Usage (from the repository root):
#   ./hack/test-policy.sh
#   KEEP=true ./hack/test-policy.sh      # keep test resources for inspection

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
NS="${NS:-backflow-policy-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
APP="${APP:-demo-app}"
KEEP="${KEEP:-false}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-30s}"
# Shorter than the 5 minute resync, so a pass proves the watch fired.
WATCH_TIMEOUT="${WATCH_TIMEOUT:-20s}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT/tmp"
LOG_FILE="$LOG_DIR/test-policy-manager.log"
MANAGER_PID=""
ORIGINAL_ENV_LABEL=""
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

set_app_env_label() {
  if [[ -n "$1" ]]; then
    k -n "$ARGOCD_NS" label application "$APP" env="$1" --overwrite >/dev/null
  else
    k -n "$ARGOCD_NS" label application "$APP" env- >/dev/null 2>&1 || true
  fi
}

cleanup() {
  # Always put the Application's label back the way we found it.
  set_app_env_label "$ORIGINAL_ENV_LABEL" 2>/dev/null || true
  if [[ -n "$MANAGER_PID" ]] && kill -0 "$MANAGER_PID" 2>/dev/null; then
    kill "$MANAGER_PID" 2>/dev/null || true
    wait "$MANAGER_PID" 2>/dev/null || true
  fi
  if [[ "$KEEP" != "true" ]]; then
    k delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

ready_status() {
  k -n "$NS" get bfp "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}: {.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true
}

# expect_reason <policy> <reason> <timeout> <description>
expect_reason() {
  local name="$1" reason="$2" timeout="$3" desc="$4"
  if k -n "$NS" wait "bfp/$name" \
      --for=jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'="$reason" \
      --timeout="$timeout" >/dev/null 2>&1; then
    pass "$desc ($reason)"
  else
    fail "$desc: expected reason=$reason, got: $(ready_status "$name")"
  fi
}

# expect_field <policy> <jsonpath> <expected> <description>
expect_field() {
  local name="$1" path="$2" expected="$3" desc="$4" got
  got="$(k -n "$NS" get bfp "$name" -o jsonpath="{$path}")"
  if [[ "$got" == "$expected" ]]; then
    pass "$desc = $expected"
  else
    fail "$desc: expected '$expected', got '$got'"
  fi
}

# expect_nonempty <policy> <jsonpath> <description>
expect_nonempty() {
  local name="$1" path="$2" desc="$3" got
  got="$(k -n "$NS" get bfp "$name" -o jsonpath="{$path}")"
  if [[ -n "$got" && "$got" != "0" ]]; then
    pass "$desc = $got"
  else
    fail "$desc is empty"
  fi
}

apply_policy() {
  local name="$1" applications="$2" mode="${3:-MergeRequest}"
  k apply -f - >/dev/null <<EOF
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata:
  name: $name
  namespace: $NS
spec:
  argoCDNamespace: $ARGOCD_NS
  mode: $mode
  applications: $applications
EOF
}

# repo_host <git url>: https://host/..., ssh://git@host/..., git@host:path
repo_host() {
  local u="$1"
  if [[ "$u" == *://* ]]; then
    u="${u#*://}"
    u="${u#*@}"
    u="${u%%/*}"
    echo "${u%%:*}"
  else
    u="${u#*@}"
    echo "${u%%:*}"
  fi
}

cd "$ROOT"

# ---------------------------------------------------------------------------
step "Prerequisites"
for cmd in kubectl make curl; do
  command -v "$cmd" >/dev/null || die "'$cmd' not found"
done
k cluster-info >/dev/null 2>&1 || die "cluster kind-$CLUSTER_NAME is not reachable. Run ./hack/dev-up.sh first."
k get crd applications.argoproj.io >/dev/null 2>&1 || die "Argo CD is not installed. Run ./hack/dev-up.sh first."
k -n "$ARGOCD_NS" get application "$APP" >/dev/null 2>&1 || die "Application $APP not found. Run ./hack/dev-up.sh first."
if curl -fs -o /dev/null http://localhost:8081/healthz 2>/dev/null; then
  die "An operator is already running on port 8081 (probably 'make run'). Stop it first."
fi

REPO_URL="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.source.repoURL}')"
APP_PATH="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.source.path}')"
REPO_HOST="$(repo_host "$REPO_URL")"
ORIGINAL_ENV_LABEL="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.metadata.labels.env}')"
set_app_env_label test
pass "cluster, Argo CD and $APP found (repo host: $REPO_HOST)"

# ---------------------------------------------------------------------------
step "Building and starting the operator"
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
if grep -q "Argo CD Application CRD not found" "$LOG_FILE"; then
  fail "operator did not start watching Argo CD Applications"
else
  pass "operator running and watching Argo CD Applications"
fi

# ---------------------------------------------------------------------------
step "Test namespace: $NS"
k delete namespace "$NS" --ignore-not-found --wait=true >/dev/null
k create namespace "$NS" >/dev/null
pass "fresh namespace created"

# ---------------------------------------------------------------------------
step "Without an ScmConnection"
apply_policy by-label '{selector: {matchLabels: {env: test}}}'
expect_reason by-label MissingScmConnection "$WAIT_TIMEOUT" "MergeRequest mode needs a connection"
expect_field by-label '.status.matchedApplications[0]' "$APP" "matchedApplications[0]"

# One policy per Application: swap by-label for report-only, then back.
k -n "$NS" delete bfp by-label >/dev/null
apply_policy report-only "{names: [\"$APP\"]}" ReportOnly
expect_reason report-only Resolved "$WAIT_TIMEOUT" "ReportOnly mode works without a connection"
k -n "$NS" delete bfp report-only >/dev/null
apply_policy by-label '{selector: {matchLabels: {env: test}}}'
expect_reason by-label MissingScmConnection "$WAIT_TIMEOUT" "by-label is back without a connection"

# ---------------------------------------------------------------------------
step "ScmConnection appears (watch on ScmConnection)"
k -n "$NS" create secret generic scm-token --from-literal=token=dummy >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata:
  name: test-scm
  namespace: $NS
spec:
  provider: gitlab
  url: https://$REPO_HOST
  tokenSecretRef:
    name: scm-token
    key: token
EOF
expect_reason by-label Resolved "$WATCH_TIMEOUT" "Policy fixed itself without being touched"

# ---------------------------------------------------------------------------
step "Application details in status"
expect_field by-label '.status.applications[0].name' "$APP" "name"
expect_field by-label '.status.applications[0].repoURL' "$REPO_URL" "repoURL"
expect_field by-label '.status.applications[0].path' "$APP_PATH" "path"
expect_field by-label '.status.applications[0].scmConnection' "test-scm" "scmConnection"
expect_field by-label '.status.applications[0].sourceType' "Directory" "sourceType"
expect_nonempty by-label '.status.applications[0].syncedRevision' "syncedRevision"
expect_nonempty by-label '.status.applications[0].managedResources' "managedResources"
expect_field by-label '.status.observedGeneration' "1" "observedGeneration"

# ---------------------------------------------------------------------------
step "One policy per Application"
# by-label is the oldest policy on $APP; a second one must not manage it too.
apply_policy second "{names: [\"$APP\"]}"
expect_reason second ApplicationConflict "$WAIT_TIMEOUT" "A second policy on $APP is blocked"
got="$(k -n "$NS" get bfp second -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].message}')"
case "$got" in
  False/*"$NS/by-label"*) pass "the conflict names the policy that manages $APP ($NS/by-label)" ;;
  *) fail "conflict message: expected Ready=False naming $NS/by-label, got: $got" ;;
esac
expect_reason by-label Resolved "$WAIT_TIMEOUT" "The oldest policy keeps managing $APP"
k -n "$NS" delete bfp by-label >/dev/null
expect_reason second Resolved "$WATCH_TIMEOUT" "The blocked policy takes over when the first one is gone"
k -n "$NS" delete bfp second >/dev/null
apply_policy by-label '{selector: {matchLabels: {env: test}}}'
expect_reason by-label Resolved "$WAIT_TIMEOUT" "by-label manages $APP again"

# ---------------------------------------------------------------------------
step "Policies that cannot resolve"
apply_policy no-match '{names: ["does-not-exist"]}'
expect_reason no-match NoApplicationsMatched "$WAIT_TIMEOUT" "Unknown Application name"

apply_policy empty-selector '{}'
expect_reason empty-selector InvalidSpec "$WAIT_TIMEOUT" "Neither names nor selector"

# ---------------------------------------------------------------------------
step "Application label changes (watch on Argo CD Applications)"
set_app_env_label prod
expect_reason by-label NoApplicationsMatched "$WATCH_TIMEOUT" "Label env=prod no longer matches"
set_app_env_label test
expect_reason by-label Resolved "$WATCH_TIMEOUT" "Label env=test matches again"

# ---------------------------------------------------------------------------
echo
echo "${BOLD}Summary:${NC} ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}"
if [[ "$KEEP" == "true" ]]; then
  echo "Test resources kept in namespace '$NS': kubectl -n $NS get bfp,scm"
fi
if (( FAIL > 0 )); then
  echo "Operator log: tmp/test-policy-manager.log"
  exit 1
fi