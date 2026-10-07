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
echo
echo "${BOLD}Environment ready.${NC}"
echo "  Run the operator:   make run"
echo "  Argo CD password:   kubectl -n $ARGOCD_NS get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d; echo"
echo "  Argo CD UI:         kubectl -n $ARGOCD_NS port-forward svc/argocd-server 8080:443   (https://localhost:8080)"