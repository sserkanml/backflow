#!/usr/bin/env bash
#
# Backflow - ScmConnection end-to-end test
#
# Sets up a kind cluster (if needed), installs the CRDs, starts the operator
# in the background and checks:
#   - CRD validation rejects bad input
#   - CRD defaults are filled in
#   - ScmConnection reports Unreachable / SecretError / Unauthorized correctly
#   - A real token turns Ready=True (only when GITLAB_TOKEN is set)
#   - Updating the token Secret is picked up immediately (Secret watch)
#
# Usage (from the repository root):
#   ./hack/test-scm.sh
#   GITLAB_TOKEN=glpat-xxxx ./hack/test-scm.sh
#   GITLAB_URL=https://gitlab.example.com GITLAB_TOKEN=... ./hack/test-scm.sh
#   KEEP=true ./hack/test-scm.sh        # keep test resources for inspection
#
# Podman users: KIND_EXPERIMENTAL_PROVIDER=podman ./hack/test-scm.sh

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
NS="${NS:-backflow-test}"
GITLAB_URL="${GITLAB_URL:-https://gitlab.com}"
GITLAB_TOKEN="${GITLAB_TOKEN:-}"
KEEP="${KEEP:-false}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-90s}"
# Must be shorter than the 1 minute retry interval, so a pass proves the
# Secret watch fired rather than the periodic retry.
ROTATE_TIMEOUT="${ROTATE_TIMEOUT:-30s}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT/tmp"
LOG_FILE="$LOG_DIR/test-scm-manager.log"
MANAGER_PID=""
PASS=0
FAIL=0
SKIP=0

if [[ -t 1 ]]; then
  GREEN=$'\e[32m'; RED=$'\e[31m'; YELLOW=$'\e[33m'; BOLD=$'\e[1m'; NC=$'\e[0m'
else
  GREEN=""; RED=""; YELLOW=""; BOLD=""; NC=""
fi

step() { echo; echo "${BOLD}▶ $*${NC}"; }
pass() { echo "  ${GREEN}✔${NC} $*"; PASS=$((PASS + 1)); }
fail() { echo "  ${RED}✘${NC} $*"; FAIL=$((FAIL + 1)); }
skip() { echo "  ${YELLOW}–${NC} $*"; SKIP=$((SKIP + 1)); }
die()  { echo "${RED}ERROR:${NC} $*" >&2; exit 1; }

k() { kubectl --context "kind-$CLUSTER_NAME" "$@"; }

cleanup() {
  if [[ -n "$MANAGER_PID" ]] && kill -0 "$MANAGER_PID" 2>/dev/null; then
    kill "$MANAGER_PID" 2>/dev/null || true
    wait "$MANAGER_PID" 2>/dev/null || true
  fi
  if [[ "$KEEP" != "true" ]]; then
    k delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# ready_status <scm-name>  -> prints "status/reason: message"
ready_status() {
  k -n "$NS" get scm "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}: {.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true
}

# expect_reason <scm-name> <reason> <timeout> <description>
expect_reason() {
  local name="$1" reason="$2" timeout="$3" desc="$4"
  if k -n "$NS" wait "scm/$name" \
      --for=jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'="$reason" \
      --timeout="$timeout" >/dev/null 2>&1; then
    pass "$desc ($reason)"
  else
    fail "$desc: expected reason=$reason, got: $(ready_status "$name")"
  fi
}

# expect_rejected <description> <expected error substring>  (manifest on stdin)
expect_rejected() {
  local desc="$1" expected="$2" out
  if out="$(k apply -f - 2>&1)"; then
    fail "$desc: was accepted but should have been rejected"
  elif grep -q -- "$expected" <<<"$out"; then
    pass "$desc rejected"
  else
    fail "$desc: rejected with unexpected error: $out"
  fi
}

set_token() {
  k -n "$NS" create secret generic "$1" --from-literal=token="$2" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
}

apply_scm() {
  local name="$1" url="$2" secret="$3"
  k apply -f - >/dev/null <<EOF
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata:
  name: $name
  namespace: $NS
spec:
  provider: gitlab
  url: $url
  tokenSecretRef:
    name: $secret
    key: token
EOF
}

cd "$ROOT"

# ---------------------------------------------------------------------------
step "Prerequisites"
for cmd in kind kubectl make curl go; do
  command -v "$cmd" >/dev/null || die "'$cmd' not found"
done
pass "kind, kubectl, make, curl, go found"

if curl -fs -o /dev/null http://localhost:8081/healthz 2>/dev/null; then
  die "An operator is already running on port 8081 (probably 'make run'). Stop it first."
fi

# ---------------------------------------------------------------------------
step "kind cluster: $CLUSTER_NAME"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  pass "cluster already exists"
else
  kind create cluster --name "$CLUSTER_NAME"
  pass "cluster created"
fi
k cluster-info >/dev/null || die "cannot connect to the cluster"
# make install / the manager use the current context
kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null

# ---------------------------------------------------------------------------
step "CRDs"
make install >/dev/null
for crd in scmconnections backflowpolicies driftproposals; do
  if k get crd "$crd.backflow.io" >/dev/null 2>&1; then
    pass "$crd.backflow.io installed"
  else
    fail "$crd.backflow.io not found"
  fi
done

# ---------------------------------------------------------------------------
step "Building and starting the operator"
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
pass "operator running (log: tmp/test-scm-manager.log)"

# ---------------------------------------------------------------------------
step "Test namespace: $NS"
k delete namespace "$NS" --ignore-not-found --wait=true >/dev/null
k create namespace "$NS" >/dev/null
pass "fresh namespace created"

# ---------------------------------------------------------------------------
step "CRD validation"
expect_rejected "Unsupported provider (bitbucket)" "provider" <<EOF
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata: {name: bad-provider, namespace: $NS}
spec:
  provider: bitbucket
  url: https://bitbucket.org
  tokenSecretRef: {name: x, key: token}
EOF

expect_rejected "Non-http(s) URL" "url" <<EOF
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata: {name: bad-url, namespace: $NS}
spec:
  provider: gitlab
  url: gitlab.example.com
  tokenSecretRef: {name: x, key: token}
EOF

expect_rejected "Invalid mode (AutoMerge)" "mode" <<EOF
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata: {name: bad-mode, namespace: $NS}
spec:
  applications: {names: ["demo-app"]}
  mode: AutoMerge
EOF

expect_rejected "Empty changes list" "changes" <<EOF
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata: {name: bad-drift, namespace: $NS}
spec:
  policyName: p
  application: {name: a, namespace: argocd}
  resource: {version: v1, kind: ConfigMap, name: c}
  source: {repoURL: https://example.com/r.git, revision: abc, type: Directory}
  changes: []
  detectedAt: "2026-01-01T00:00:00Z"
EOF

# ---------------------------------------------------------------------------
step "Defaults (BackflowPolicy)"
k apply -f - >/dev/null <<EOF
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata: {name: defaults, namespace: $NS}
spec:
  applications: {names: ["demo-app"]}
EOF
check_default() {
  local path="$1" expected="$2" got
  got="$(k -n "$NS" get bfp defaults -o jsonpath="{$path}")"
  if [[ "$got" == "$expected" ]]; then
    pass "$path = $expected"
  else
    fail "$path: expected '$expected', got '$got'"
  fi
}
check_default .spec.mode MergeRequest
check_default .spec.argoCDNamespace argocd
check_default .spec.batchWindow 30s

# ---------------------------------------------------------------------------
step "ScmConnection: failure cases"
set_token dummy-token dummy

# .invalid is a reserved TLD that never resolves.
apply_scm unreachable https://gitlab.invalid dummy-token
expect_reason unreachable Unreachable "$WAIT_TIMEOUT" "Non-existent host"

apply_scm missing-secret https://gitlab.invalid does-not-exist
expect_reason missing-secret SecretError "$WAIT_TIMEOUT" "Non-existent Secret"

if k -n "$NS" get scm unreachable -o jsonpath='{.status.lastCheckTime}' | grep -q .; then
  pass "status.lastCheckTime is set"
else
  fail "status.lastCheckTime is empty"
fi

# ---------------------------------------------------------------------------
step "ScmConnection: $GITLAB_URL"
if [[ -n "$GITLAB_TOKEN" ]]; then
  set_token real-token "$GITLAB_TOKEN"
  apply_scm real "$GITLAB_URL" real-token
  expect_reason real Authenticated "$WAIT_TIMEOUT" "Valid token"
  user="$(k -n "$NS" get scm real -o jsonpath='{.status.authenticatedAs}')"
  if [[ -n "$user" ]]; then
    pass "authenticatedAs = $user"
  else
    fail "authenticatedAs is empty"
  fi

  set_token real-token wrong-token
  expect_reason real Unauthorized "$ROTATE_TIMEOUT" "Token replaced with a wrong one, picked up via Secret watch"

  set_token real-token "$GITLAB_TOKEN"
  expect_reason real Authenticated "$ROTATE_TIMEOUT" "Token restored, picked up via Secret watch"
else
  skip "GITLAB_TOKEN not set, valid token test skipped"
  set_token wrong-token wrong-token
  apply_scm wrong "$GITLAB_URL" wrong-token
  expect_reason wrong Unauthorized "$WAIT_TIMEOUT" "Wrong token (requires internet access)"
fi

# ---------------------------------------------------------------------------
echo
echo "${BOLD}Summary:${NC} ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}, ${YELLOW}$SKIP skipped${NC}"
if [[ "$KEEP" == "true" ]]; then
  echo "Test resources kept in namespace '$NS': kubectl -n $NS get scm,bfp"
fi
if (( FAIL > 0 )); then
  echo "Operator log: tmp/test-scm-manager.log"
  exit 1
fi