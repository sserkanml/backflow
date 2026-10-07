#!/usr/bin/env bash
#
# Backflow - local development environment
#
# Brings up everything needed to develop and test Backflow locally.
# Safe to run any number of times: steps that are already done are skipped.
#
#   1. kind cluster (created, or restarted if its container is stopped)
#   2. Argo CD
#   3. Backflow CRDs
#   4. Demo Argo CD Application pointing at the demo repository
#   5. Read-only Argo CD account "backflow" and its API token Secret
#
# Usage (from the repository root):
#   ./hack/dev-up.sh
#   DEMO_REPO=https://gitlab.com/<user>/backflow-demo.git ./hack/dev-up.sh
#
# Podman users: KIND_EXPERIMENTAL_PROVIDER=podman ./hack/dev-up.sh

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
ARGOCD_MANIFEST="${ARGOCD_MANIFEST:-https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml}"
DEMO_REPO="${DEMO_REPO:-https://gitlab.com/sserkanml/backflow-demo.git}"
DEMO_REVISION="${DEMO_REVISION:-main}"
DEMO_PATH="${DEMO_PATH:-apps/demo}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTEXT="kind-$CLUSTER_NAME"
NODE="$CLUSTER_NAME-control-plane"
RUNTIME="docker"
if [[ "${KIND_EXPERIMENTAL_PROVIDER:-}" == "podman" ]]; then
  RUNTIME="podman"
fi

if [[ -t 1 ]]; then
  GREEN=$'\e[32m'; RED=$'\e[31m'; BOLD=$'\e[1m'; NC=$'\e[0m'
else
  GREEN=""; RED=""; BOLD=""; NC=""
fi
step() { echo; echo "${BOLD}▶ $*${NC}"; }
ok()   { echo "  ${GREEN}✔${NC} $*"; }
die()  { echo "${RED}ERROR:${NC} $*" >&2; exit 1; }

k() { kubectl --context "$CONTEXT" "$@"; }

cd "$ROOT"

# ---------------------------------------------------------------------------
step "Prerequisites"
for cmd in kind kubectl make "$RUNTIME"; do
  command -v "$cmd" >/dev/null || die "'$cmd' not found"
done
"$RUNTIME" info >/dev/null 2>&1 || die "$RUNTIME is not running. Start it first (e.g. 'sudo systemctl start $RUNTIME')."
ok "kind, kubectl, make, $RUNTIME available"

# ---------------------------------------------------------------------------
step "kind cluster: $CLUSTER_NAME"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  running="$("$RUNTIME" inspect -f '{{.State.Running}}' "$NODE" 2>/dev/null || echo false)"
  if [[ "$running" != "true" ]]; then
    "$RUNTIME" start "$NODE" >/dev/null
    ok "stopped cluster container restarted"
  else
    ok "cluster is running"
  fi
else
  kind create cluster --name "$CLUSTER_NAME"
  ok "cluster created"
fi
kubectl config use-context "$CONTEXT" >/dev/null

for _ in $(seq 1 60); do
  k get --raw /readyz >/dev/null 2>&1 && break
  sleep 2
done
k get --raw /readyz >/dev/null 2>&1 || die "API server did not become ready"
k wait --for=condition=Ready node --all --timeout=120s >/dev/null
ok "API server and node ready"

# ---------------------------------------------------------------------------
step "Argo CD"
if k get crd applications.argoproj.io >/dev/null 2>&1; then
  ok "already installed"
else
  k create namespace "$ARGOCD_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
  k apply -n "$ARGOCD_NS" --server-side --force-conflicts -f "$ARGOCD_MANIFEST" >/dev/null
  ok "installed"
fi
echo "  waiting for Argo CD deployments (can take a few minutes after a restart)..."
k -n "$ARGOCD_NS" wait --for=condition=Available deployment --all --timeout=300s >/dev/null
k -n "$ARGOCD_NS" rollout status statefulset --timeout=300s >/dev/null 2>&1 || true
ok "Argo CD ready"

# ---------------------------------------------------------------------------
step "Backflow CRDs"
make install >/dev/null
ok "installed"

# ---------------------------------------------------------------------------
step "Demo Application"
k apply -f - >/dev/null <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: demo-app
  namespace: $ARGOCD_NS
  labels:
    env: test
spec:
  project: default
  source:
    repoURL: $DEMO_REPO
    targetRevision: $DEMO_REVISION
    path: $DEMO_PATH
  destination:
    server: https://kubernetes.default.svc
    namespace: demo
  syncPolicy:
    automated: {}
    syncOptions:
      - CreateNamespace=true
EOF
ok "demo-app applied ($DEMO_REPO, $DEMO_PATH)"

echo "  waiting for demo-app to become Synced/Healthy..."
synced=false
for _ in $(seq 1 60); do
  sync="$(k -n "$ARGOCD_NS" get application demo-app -o jsonpath='{.status.sync.status}' 2>/dev/null || true)"
  health="$(k -n "$ARGOCD_NS" get application demo-app -o jsonpath='{.status.health.status}' 2>/dev/null || true)"
  if [[ "$sync" == "Synced" && "$health" == "Healthy" ]]; then
    synced=true
    break
  fi
  sleep 5
done
if [[ "$synced" == "true" ]]; then
  ok "demo-app Synced/Healthy"
else
  echo "  ${RED}✘${NC} demo-app is $sync/$health. Check: kubectl -n $ARGOCD_NS get application demo-app -o jsonpath='{.status.conditions}'"
fi

# ---------------------------------------------------------------------------
step "Argo CD account for Backflow"
BF_NS="${BF_NS:-backflow-system}"
BF_TOKEN_SECRET="${BF_TOKEN_SECRET:-argocd-token}"

k -n "$ARGOCD_NS" patch configmap argocd-cm --type merge \
  -p '{"data":{"accounts.backflow":"apiKey"}}' >/dev/null
ok "account backflow (apiKey) enabled in argocd-cm"

# Merge into the existing policy.csv; only append lines that are missing.
policy="$(k -n "$ARGOCD_NS" get configmap argocd-rbac-cm -o jsonpath='{.data.policy\.csv}' 2>/dev/null || true)"
for line in "p, role:backflow, applications, get, */*, allow" "g, backflow, role:backflow"; do
  if ! printf '%s\n' "$policy" | grep -qxF "$line"; then
    if [[ -n "$policy" ]]; then policy="$policy"$'\n'"$line"; else policy="$line"; fi
  fi
done
patch="$(POLICY="$policy" python3 -c 'import json,os; print(json.dumps({"data":{"policy.csv":os.environ["POLICY"]+"\n"}}))')"
k -n "$ARGOCD_NS" patch configmap argocd-rbac-cm --type merge -p "$patch" >/dev/null
ok "read-only RBAC policy for backflow merged into argocd-rbac-cm"

k create namespace "$BF_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
if k -n "$BF_NS" get secret "$BF_TOKEN_SECRET" >/dev/null 2>&1; then
  ok "token Secret $BF_NS/$BF_TOKEN_SECRET already exists, skipping generation"
else
  # argocd-cm / rbac changes are picked up by argocd-server without a restart,
  # but give it a moment.
  sleep 3
  admin_pw="$(k -n "$ARGOCD_NS" get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' 2>/dev/null | base64 -d || true)"
  [[ -n "$admin_pw" ]] || die "argocd-initial-admin-secret not found; cannot generate a token"

  free_port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  # Call kubectl directly (not through k) so $! is the kubectl process itself.
  kubectl --context "$CONTEXT" -n "$ARGOCD_NS" port-forward svc/argocd-server "$free_port":443 >/dev/null 2>&1 &
  pf_pid=$!
  trap 'kill "$pf_pid" 2>/dev/null || true' EXIT
  for _ in $(seq 1 30); do
    curl -ksf -o /dev/null "https://localhost:$free_port/api/version" && break
    sleep 1
  done

  session="$(curl -ks "https://localhost:$free_port/api/v1/session" \
    -H 'Content-Type: application/json' \
    -d "$(ADMIN_PW="$admin_pw" python3 -c 'import json,os; print(json.dumps({"username":"admin","password":os.environ["ADMIN_PW"]}))')" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token",""))' 2>/dev/null || true)"
  [[ -n "$session" ]] || die "could not log in to Argo CD as admin"

  token=""
  for _ in $(seq 1 10); do
    token="$(curl -ks -X POST "https://localhost:$free_port/api/v1/account/backflow/token" \
      -H "Authorization: Bearer $session" -H 'Content-Type: application/json' -d '{}' \
      | python3 -c 'import json,sys; print(json.load(sys.stdin).get("token",""))' 2>/dev/null || true)"
    [[ -n "$token" ]] && break
    sleep 3
  done
  kill "$pf_pid" 2>/dev/null || true
  wait "$pf_pid" 2>/dev/null || true
  trap - EXIT
  [[ -n "$token" ]] || die "could not generate a token for account backflow"

  k -n "$BF_NS" create secret generic "$BF_TOKEN_SECRET" --from-literal=token="$token" >/dev/null
  ok "token stored in Secret $BF_NS/$BF_TOKEN_SECRET (key: token)"
fi

# ---------------------------------------------------------------------------
echo
echo "${BOLD}Environment ready.${NC}"
echo "  Run the operator:   make run"
echo "  Argo CD password:   kubectl -n $ARGOCD_NS get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo"
echo "  Argo CD UI:         kubectl -n $ARGOCD_NS port-forward svc/argocd-server 8080:443   (https://localhost:8080)"
echo "  Long-running port-forward for 'make run' (set spec.argoCD.url: https://localhost:8080, insecureSkipTLSVerify: true):"
echo "                      kubectl -n $ARGOCD_NS port-forward svc/argocd-server 8080:443"
echo "  Token Secret:       $BF_NS/$BF_TOKEN_SECRET (key: token)"
