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
#   4. Demo Argo CD Applications pointing at the demo repository: demo-app
#      (plain manifests), the Helm ones demo-helm, shared-alpha, shared-beta and
#      the Kustomize one demo-kustomize (kustomize/overlays/dev over
#      kustomize/base). The Kustomize files live in hack/demo/kustomize/; with
#      GITLAB_TOKEN set (api scope) this script copies the ones the demo
#      repository lacks into it with one commit through the GitLab API. It never
#      overwrites a file. Without the token it only tells you what is missing.
#   5. Read-only Argo CD account "backflow" and its API token Secret, and
#      timeout.reconciliation 30s in argocd-cm (new commits are seen quickly)
#
# Usage (from the repository root):
#   ./hack/dev-up.sh
#   DEMO_REPO=https://gitlab.com/<user>/backflow-demo.git ./hack/dev-up.sh
#
# Podman users: KIND_EXPERIMENTAL_PROVIDER=podman ./hack/dev-up.sh

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
# Argo CD is pinned to the version the real installation runs, so the tests do
# not change under us when "stable" moves. Override with ARGOCD_VERSION, or
# point ARGOCD_MANIFEST at any install manifest.
ARGOCD_VERSION="${ARGOCD_VERSION:-v3.5.3}"
CHECK_ARGOCD_VERSION=false
if [[ -z "${ARGOCD_MANIFEST:-}" ]]; then
  ARGOCD_MANIFEST="https://raw.githubusercontent.com/argoproj/argo-cd/$ARGOCD_VERSION/manifests/install.yaml"
  CHECK_ARGOCD_VERSION=true
fi
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
warn() { echo "  ${BOLD}!${NC} $*"; }
bad()  { echo "  ${RED}✘${NC} $*"; }
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
if [[ "$CHECK_ARGOCD_VERSION" == "true" ]]; then
  installed="$(k -n "$ARGOCD_NS" get deployment argocd-server -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null | sed 's/.*://')"
  if [[ "$installed" == "$ARGOCD_VERSION" ]]; then
    ok "Argo CD $installed (pinned)"
  else
    warn "Argo CD $installed is installed, but this script pins $ARGOCD_VERSION."
    warn "Recreate the cluster to get the pinned version: kind delete cluster --name $CLUSTER_NAME"
  fi
fi

# ---------------------------------------------------------------------------
step "Backflow CRDs"
make install >/dev/null
ok "installed"

# ---------------------------------------------------------------------------
step "Demo Applications"

# apply_app <name> <path> <destination namespace> <env label> [helm block] [demo label]
# All demo Applications sync automatically without selfHeal, so drift stays
# visible. The Helm ones carry env=helm-test: the tests that select
# Applications by env=test must not see them.
apply_app() {
  local name="$1" path="$2" ns="$3" env="$4" helm="${5:-}" demo="${6:-}" demolabel=""
  [[ -z "$demo" ]] || demolabel="    backflow.io/demo: $demo"
  k apply -f - >/dev/null <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: $name
  namespace: $ARGOCD_NS
  labels:
    env: $env
$demolabel
spec:
  project: default
  source:
    repoURL: $DEMO_REPO
    targetRevision: $DEMO_REVISION
    path: $path
$helm
  destination:
    server: https://kubernetes.default.svc
    namespace: $ns
  syncPolicy:
    automated: {}
    syncOptions:
      - CreateNamespace=true
EOF
  ok "$name applied ($path -> namespace $ns)"
}

# seed_demo_kustomize: add hack/demo/kustomize/* as kustomize/* to the demo
# repository when they are missing there. The token is read from the
# environment by python3 and is never printed or put in an argument.
seed_demo_kustomize() {
  local project="${DEMO_REPO#https://gitlab.com/}"
  project="${project%.git}"
  if [[ "$DEMO_REPO" != https://gitlab.com/* ]]; then
    warn "the demo repository is not on gitlab.com; copy hack/demo/kustomize/ to kustomize/ in $DEMO_REPO yourself"
    return 0
  fi
  if [[ -z "${GITLAB_TOKEN:-}" ]]; then
    warn "GITLAB_TOKEN is not set: not checking that kustomize/ exists in $DEMO_REPO."
    warn "demo-kustomize needs hack/demo/kustomize/ copied to kustomize/ there (rerun with GITLAB_TOKEN=... to do it)"
    return 0
  fi
  python3 - "$ROOT/hack/demo/kustomize" "$project" "$DEMO_REVISION" <<'PY'
import json, os, sys, urllib.error, urllib.parse, urllib.request
src, project, ref = sys.argv[1:4]
token = os.environ["GITLAB_TOKEN"]
api = "https://gitlab.com/api/v4/projects/" + urllib.parse.quote(project, safe="")
def call(method, url, body=None):
    req = urllib.request.Request(url, method=method, data=json.dumps(body).encode() if body is not None else None,
                                 headers={"PRIVATE-TOKEN": token, "Content-Type": "application/json"})
    return urllib.request.urlopen(req, timeout=30)
actions, present = [], 0
for dirpath, _, files in os.walk(src):
    for name in sorted(files):
        full = os.path.join(dirpath, name)
        rel = "kustomize/" + os.path.relpath(full, src)
        url = "%s/repository/files/%s?ref=%s" % (api, urllib.parse.quote(rel, safe=""), urllib.parse.quote(ref, safe=""))
        try:
            call("HEAD", url)
            present += 1
        except urllib.error.HTTPError as e:
            if e.code != 404:
                sys.exit("checking %s: HTTP %d" % (rel, e.code))
            actions.append({"action": "create", "file_path": rel, "content": open(full, encoding="utf-8").read()})
if not actions:
    print("  kustomize/ is complete in the demo repository (%d files)" % present)
    sys.exit(0)
call("POST", api + "/repository/commits", {"branch": ref, "commit_message": "demo: add the Kustomize demo (base and dev overlay)", "actions": actions})
print("  added %d file(s) to kustomize/ in the demo repository" % len(actions))
PY
}
seed_demo_kustomize

apply_app demo-app "$DEMO_PATH" demo test

# One chart per service, values per environment next to it; the parameter
# overrides config.feature of values-dev.yaml.
apply_app demo-helm helm/demo demo-helm helm-test "    helm:
      valueFiles:
        - values-dev.yaml
      parameters:
        - name: config.feature
          value: beta"

# One chart shared by several services, one values file per service.
apply_app shared-alpha helm/shared shared-alpha helm-test "    helm:
      valueFiles:
        - services/alpha/dev.yaml"
apply_app shared-beta helm/shared shared-beta helm-test "    helm:
      valueFiles:
        - services/beta/dev.yaml"

# Kustomize: base shared by overlays, a setting and a patch in the dev overlay.
# Its own label, and not env=test: the policies of the other tests select
# env=test and must not see it. test-kustomize.sh selects backflow.io/demo=kustomize.
apply_app demo-kustomize kustomize/overlays/dev demo-kustomize kustomize-test "" kustomize

# wait_apps <name>...: wait until each Application is Synced/Healthy. An
# Application whose path is missing in the repository fails at once instead of
# after the timeout. A failure is reported, not fatal.
wait_apps() {
  local pending="$*" next app sync health msg
  echo "  waiting for $pending to become Synced/Healthy..."
  for _ in $(seq 1 60); do
    next=""
    for app in $pending; do
      sync="$(k -n "$ARGOCD_NS" get application "$app" -o jsonpath='{.status.sync.status}' 2>/dev/null || true)"
      health="$(k -n "$ARGOCD_NS" get application "$app" -o jsonpath='{.status.health.status}' 2>/dev/null || true)"
      if [[ "$sync" == "Synced" && "$health" == "Healthy" ]]; then
        ok "$app Synced/Healthy"
        continue
      fi
      msg="$(k -n "$ARGOCD_NS" get application "$app" -o jsonpath='{.status.conditions[?(@.type=="ComparisonError")].message}' 2>/dev/null || true)"
      if [[ "$msg" == *"does not exist"* ]]; then
        bad "$app: $msg"
        echo "    The path is missing in $DEMO_REPO ($DEMO_REVISION). Has the demo repository been pushed?"
        continue
      fi
      next="$next $app"
    done
    pending="${next# }"
    [[ -n "$pending" ]] || return 0
    sleep 5
  done
  for app in $pending; do
    sync="$(k -n "$ARGOCD_NS" get application "$app" -o jsonpath='{.status.sync.status}' 2>/dev/null || true)"
    health="$(k -n "$ARGOCD_NS" get application "$app" -o jsonpath='{.status.health.status}' 2>/dev/null || true)"
    bad "$app is $sync/$health. Check: kubectl -n $ARGOCD_NS get application $app -o jsonpath='{.status.conditions}'"
  done
}
wait_apps demo-app demo-helm shared-alpha shared-beta demo-kustomize

# expect_source_type <name> <type>
expect_source_type() {
  local got
  got="$(k -n "$ARGOCD_NS" get application "$1" -o jsonpath='{.status.sourceType}' 2>/dev/null || true)"
  if [[ "$got" == "$2" ]]; then ok "$1 sourceType $got"; else bad "$1 sourceType is '$got', expected $2"; fi
}
expect_source_type demo-app Directory
expect_source_type demo-helm Helm
expect_source_type shared-alpha Helm
expect_source_type shared-beta Helm
expect_source_type demo-kustomize Kustomize

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

# Argo CD looks for new commits every three minutes (plus up to a minute of
# jitter) unless told otherwise. The tests wait for it, so poll every 30s with
# little jitter. The application controller reads the settings at start, so it
# is restarted, but only when a value changed.
want_reconciliation="30s"
want_jitter="5s"
have_reconciliation="$(k -n "$ARGOCD_NS" get configmap argocd-cm -o jsonpath='{.data.timeout\.reconciliation}' 2>/dev/null || true)"
have_jitter="$(k -n "$ARGOCD_NS" get configmap argocd-cm -o jsonpath='{.data.timeout\.reconciliation\.jitter}' 2>/dev/null || true)"
if [[ "$have_reconciliation" == "$want_reconciliation" && "$have_jitter" == "$want_jitter" ]]; then
  ok "argocd-cm already has timeout.reconciliation $want_reconciliation (jitter $want_jitter)"
else
  k -n "$ARGOCD_NS" patch configmap argocd-cm --type merge \
    -p "{\"data\":{\"timeout.reconciliation\":\"$want_reconciliation\",\"timeout.reconciliation.jitter\":\"$want_jitter\"}}" >/dev/null
  ok "argocd-cm: timeout.reconciliation $want_reconciliation, jitter $want_jitter (was '${have_reconciliation:-default}', '${have_jitter:-default}')"
  k -n "$ARGOCD_NS" rollout restart statefulset argocd-application-controller >/dev/null
  k -n "$ARGOCD_NS" rollout status statefulset argocd-application-controller --timeout=300s >/dev/null
  ok "application controller restarted"
fi

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
