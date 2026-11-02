#!/usr/bin/env bash
#
# The installation from OperatorHub, on a kind cluster of its own with OLM. No tokens, no Git host, no Argo CD.
#
#   1. OLM (operator-sdk olm install, pinned)
#   2. the operator image, the bundle (make bundle), the bundle image and the file-based catalog image of this checkout,
#      built here and loaded into kind
#   3. a CatalogSource for the catalog, and a Subscription (channel stable, AllNamespaces) with settings in
#      spec.config.env
#   4. the ClusterServiceVersion succeeds and the operator is Ready; the environment of the Subscription reached the operator
#      (an allowed Git host is accepted, another is HostNotAllowed)
#   5. an ScmConnection and a BackflowPolicy are made and reconciled
#   6. operator-sdk scorecard on the bundle (SCORECARD=false to skip)
#   7. an upgrade through the channel: a second bundle, a catalog in which it replaces the first (the CatalogSource gets the new image),
#      the InstallPlan, the new ClusterServiceVersion succeeds, the old one is replaced, the objects still work
#   8. the uninstall: Subscription and ClusterServiceVersion deleted, the operator gone, the CRDs and the objects kept
#
#   ./hack/test-olm.sh
#
# Environment:
#   CLUSTER       the name of the kind cluster, created and deleted by the script (default: backflow-olm)
#   OLM_VERSION   the OLM release (default v0.28.0)
#   KEEP          true: keep the cluster
#   SKIP_BUILD    true: use the images and the bundle that are there (backflow-olm*:test)
#   SCORECARD     false: skip the scorecard
#   KIND_NODE_IMAGE  pins the Kubernetes version of the cluster
#
# It never touches another kubeconfig or cluster. bash 3.2 compatible.

# ok and bad always succeed, so "A && ok || bad" is an if-then-else here.
# shellcheck disable=SC2015
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${CLUSTER:-backflow-olm}"
OLM_VERSION="${OLM_VERSION:-v0.28.0}"
KEEP="${KEEP:-false}"
SKIP_BUILD="${SKIP_BUILD:-false}"
SCORECARD="${SCORECARD:-true}"
OPERATOR_SDK="${OPERATOR_SDK:-$ROOT/bin/operator-sdk}"
# OLM pulls the image of a catalog with imagePullPolicy Always, so an image that was only loaded into kind does not work: the
# images go to a registry of their own that the nodes reach as localhost:$REGISTRY_PORT.
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
REGISTRY_NAME="backflow-olm-registry"
REGISTRY="localhost:$REGISTRY_PORT"
OPERATOR_IMG="$REGISTRY/backflow-olm:test"
BUNDLE_IMG="$REGISTRY/backflow-olm-bundle:test"
CATALOG_IMG="$REGISTRY/backflow-olm-catalog:test"
NS=operators        # the namespace of the global OperatorGroup that OLM installs: AllNamespaces
CATALOG_NS=olm
APP_NS=team-a

WORK="$(mktemp -d "${TMPDIR:-/tmp}/backflow-olm.XXXXXX")"
export KUBECONFIG="$WORK/kubeconfig"
PASS=0
FAIL=0

step() { printf '\n== %s\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
expect() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi; }

# wait_for <what> <seconds> <command...>: run the command until it succeeds.
wait_for() {
  local what="$1" secs="$2" i
  shift 2
  for ((i = 0; i < secs; i += 3)); do
    if "$@" >/dev/null 2>&1; then ok "$what"; return 0; fi
    sleep 3
  done
  bad "$what (not after ${secs}s)"
  return 1
}

cleanup() {
  local code=$?
  if [[ "$KEEP" == "true" ]]; then
    note "kept: cluster $CLUSTER and the registry $REGISTRY_NAME (KUBECONFIG=$KUBECONFIG)"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    docker rm -f "$REGISTRY_NAME" >/dev/null 2>&1
    rm -rf "$WORK"
  fi
  exit "$code"
}
trap cleanup EXIT

for tool in kind kubectl docker jq; do command -v "$tool" >/dev/null 2>&1 || die "$tool is needed"; done
[[ -x "$OPERATOR_SDK" ]] || make -C "$ROOT" operator-sdk opm >/dev/null || die "cannot get operator-sdk"
VERSION="$(sed -n 's/^appVersion: *//p' "$ROOT/charts/backflow/Chart.yaml" | tr -d '"')"
CSV="backflow.v$VERSION"

# ---------------------------------------------------------------------------
step "the images, the bundle and the catalog of this checkout"
if [[ "$SKIP_BUILD" != "true" ]]; then
  docker build -q -t "$OPERATOR_IMG" "$ROOT" >/dev/null || die "docker build of the operator failed"
  make -C "$ROOT" bundle OPERATOR_IMAGE="$OPERATOR_IMG" BUNDLE_IMG="$BUNDLE_IMG" >"$WORK/bundle.log" 2>&1 || { cat "$WORK/bundle.log"; die "make bundle failed"; }
  make -C "$ROOT" bundle-build BUNDLE_IMG="$BUNDLE_IMG" >"$WORK/bundle-build.log" 2>&1 || { cat "$WORK/bundle-build.log"; die "bundle image failed"; }
  make -C "$ROOT" catalog BUNDLE_IMG="$BUNDLE_IMG" CATALOG_IMG="$CATALOG_IMG" >"$WORK/catalog.log" 2>&1 || { cat "$WORK/catalog.log"; die "make catalog failed"; }
  make -C "$ROOT" catalog-build CATALOG_IMG="$CATALOG_IMG" >"$WORK/catalog-build.log" 2>&1 || { cat "$WORK/catalog-build.log"; die "catalog image failed"; }
fi
expect "the bundle has the operator image of this checkout" \
  "$(grep -c "image: $OPERATOR_IMG" "$ROOT/bundle/manifests/backflow.clusterserviceversion.yaml" | tr -d ' ')" "2"
grep -q "image: $BUNDLE_IMG" "$ROOT/catalog/out/backflow/catalog.yaml" && ok "the catalog names the bundle image" || bad "the catalog does not name the bundle image"

# ---------------------------------------------------------------------------
step "a cluster with OLM $OLM_VERSION"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER exists: delete it or set CLUSTER"
docker rm -f "$REGISTRY_NAME" >/dev/null 2>&1
docker run -d --restart=always -p "127.0.0.1:$REGISTRY_PORT:5000" --name "$REGISTRY_NAME" registry:2 >/dev/null \
  || die "cannot start the registry (is port $REGISTRY_PORT in use? set REGISTRY_PORT)"
cat >"$WORK/kind.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
YAML
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --config "$WORK/kind.yaml" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} >/dev/null 2>&1 \
  || die "kind create cluster failed"
# The nodes reach the registry by its name on the kind network, as localhost:$REGISTRY_PORT.
docker network connect kind "$REGISTRY_NAME" >/dev/null 2>&1
for node in $(kind get nodes --name "$CLUSTER"); do
  docker exec "$node" mkdir -p "/etc/containerd/certs.d/$REGISTRY"
  printf '[host."http://%s:5000"]\n' "$REGISTRY_NAME" | docker exec -i "$node" tee "/etc/containerd/certs.d/$REGISTRY/hosts.toml" >/dev/null
done
for img in "$OPERATOR_IMG" "$BUNDLE_IMG" "$CATALOG_IMG"; do
  docker push -q "$img" >/dev/null || die "cannot push $img"
done
"$OPERATOR_SDK" olm install --version "$OLM_VERSION" --timeout 6m >"$WORK/olm-install.log" 2>&1 || { tail -20 "$WORK/olm-install.log"; die "operator-sdk olm install failed"; }
ok "OLM is installed"
wait_for "the OperatorGroup of namespace $NS is global" 60 \
  bash -c "kubectl -n $NS get operatorgroup -o json | jq -e '.items[0] | (.spec.targetNamespaces // []) | length == 0'"

# ---------------------------------------------------------------------------
step "the catalog"
kubectl apply -f - >/dev/null <<YAML || die "cannot create the CatalogSource"
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata: {name: backflow-catalog, namespace: $CATALOG_NS}
spec:
  sourceType: grpc
  image: $CATALOG_IMG
  displayName: Backflow
  publisher: sserkanml
  updateStrategy: {registryPoll: {interval: 30m}}
YAML
wait_for "the catalog is READY" 300 \
  bash -c "[ \"\$(kubectl -n $CATALOG_NS get catalogsource backflow-catalog -o jsonpath='{.status.connectionState.lastObservedState}')\" = READY ]"
wait_for "OLM knows the package backflow" 90 bash -c "kubectl get packagemanifest backflow -n $CATALOG_NS"
expect "the default channel is stable" "$(kubectl get packagemanifest backflow -n $CATALOG_NS -o jsonpath='{.status.defaultChannel}')" "stable"
expect "the package has the CSV of this version" \
  "$(kubectl get packagemanifest backflow -n $CATALOG_NS -o jsonpath='{.status.channels[?(@.name=="stable")].currentCSV}')" "$CSV"

# ---------------------------------------------------------------------------
step "subscribe"
kubectl apply -f - >/dev/null <<YAML || die "cannot create the Subscription"
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata: {name: backflow, namespace: $NS}
spec:
  channel: stable
  name: backflow
  source: backflow-catalog
  sourceNamespace: $CATALOG_NS
  installPlanApproval: Automatic
  config:
    env:
      - {name: BACKFLOW_SCM_ALLOWED_HOSTS, value: "gitlab.invalid"}
      - {name: BACKFLOW_MERGE_REQUEST_POLL_INTERVAL, value: "30s"}
      - {name: BACKFLOW_SECRET_NAMESPACES, value: "$APP_NS"}
YAML
wait_for "the ClusterServiceVersion $CSV exists" 180 bash -c "kubectl -n $NS get csv $CSV"
wait_for "the ClusterServiceVersion succeeded" 300 \
  bash -c "[ \"\$(kubectl -n $NS get csv $CSV -o jsonpath='{.status.phase}')\" = Succeeded ]" \
  || { kubectl -n "$NS" get csv "$CSV" -o jsonpath='{.status.message}{"\n"}{.status.reason}{"\n"}' 2>&1 | sed 's/^/   | /'; kubectl -n "$NS" get pods 2>&1 | sed 's/^/   | /'; }
DEPLOY="backflow-controller-manager"
wait_for "the operator is Ready" 120 bash -c "[ \"\$(kubectl -n $NS get deploy $DEPLOY -o jsonpath='{.status.readyReplicas}')\" = 1 ]"
expect "the operator runs the image of this checkout" "$(kubectl -n "$NS" get deploy "$DEPLOY" -o jsonpath='{.spec.template.spec.containers[0].image}')" "$OPERATOR_IMG"
ENVS="$(kubectl -n "$NS" get deploy "$DEPLOY" -o json | jq -c '[.spec.template.spec.containers[0].env[] | {(.name): .value}] | add')"
expect "the Subscription's allowed hosts replaced the default" "$(echo "$ENVS" | jq -r .BACKFLOW_SCM_ALLOWED_HOSTS)" "gitlab.invalid"
expect "the Subscription's poll interval is there" "$(echo "$ENVS" | jq -r .BACKFLOW_MERGE_REQUEST_POLL_INTERVAL)" "30s"
POD="$(kubectl -n "$NS" get pod -l control-plane=controller-manager -o jsonpath='{.items[0].metadata.name}')"
kubectl -n "$NS" logs "$POD" >"$WORK/operator.log" 2>&1
grep -q '"scmHosts": \["gitlab.invalid"\]' "$WORK/operator.log" && ok "the operator started with the allow list of the environment" \
  || bad "the operator log does not show the allow list: $(grep Allowlists "$WORK/operator.log" | head -c 300)"
expect "the permissions are get-only on Secrets" \
  "$(kubectl get clusterrole -o json | jq -r '[.items[] | select(.metadata.labels["olm.owner"] == "'"$CSV"'") | .rules[]? | select((.resources // []) | index("secrets")) | .verbs | join(",")] | unique | join(";")')" "get"
for crd in scmconnections backflowpolicies driftproposals; do
  expect "$crd.backflow.io stores v1 only" "$(kubectl get crd "$crd.backflow.io" -o jsonpath='{.status.storedVersions}')" '["v1"]'
done

# ---------------------------------------------------------------------------
step "an ScmConnection and a BackflowPolicy"
kubectl create namespace "$APP_NS" >/dev/null
kubectl -n "$APP_NS" create secret generic gitlab-token --from-literal=token=not-a-real-token >/dev/null
kubectl -n "$APP_NS" create secret generic gitlab-token-elsewhere --from-literal=token=not-a-real-token >/dev/null
kubectl create namespace other >/dev/null
kubectl -n other create secret generic gitlab-token --from-literal=token=not-a-real-token >/dev/null
kubectl apply -f - >/dev/null <<YAML || die "cannot create the objects"
apiVersion: backflow.io/v1
kind: ScmConnection
metadata: {name: allowed, namespace: $APP_NS}
spec:
  provider: GitLab
  url: https://gitlab.invalid
  tokenSecretRef: {name: gitlab-token, key: token}
---
apiVersion: backflow.io/v1
kind: ScmConnection
metadata: {name: not-allowed, namespace: $APP_NS}
spec:
  provider: GitLab
  url: https://git.elsewhere.invalid
  tokenSecretRef: {name: gitlab-token, key: token}
---
apiVersion: backflow.io/v1
kind: BackflowPolicy
metadata: {name: p1, namespace: $APP_NS}
spec:
  argoCD: {namespace: argocd}
  applications: {names: [web]}
YAML
reason() { kubectl -n "$APP_NS" get "$1" "$2" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'; }
wait_for "the ScmConnection of the allowed host is reconciled" 90 bash -c "[ -n \"\$(kubectl -n $APP_NS get scmconnections.backflow.io allowed -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" ]"
wait_for "the ScmConnection of the other host is reconciled" 90 bash -c "[ -n \"\$(kubectl -n $APP_NS get scmconnections.backflow.io not-allowed -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" ]"
expect "an allowed host is not refused (it cannot be reached, which is another reason)" \
  "$([[ "$(reason scmconnections.backflow.io allowed)" != "HostNotAllowed" ]] && echo yes || echo no)" "yes"
expect "another host is HostNotAllowed" "$(reason scmconnections.backflow.io not-allowed)" "HostNotAllowed"
wait_for "the BackflowPolicy is reconciled" 90 bash -c "[ -n \"\$(kubectl -n $APP_NS get backflowpolicies.backflow.io p1 -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" ]"
note "the policy says: $(reason backflowpolicies.backflow.io p1) — $(kubectl -n "$APP_NS" get backflowpolicies.backflow.io p1 -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' | cut -c1-160)"
expect "the operator is still running" "$(kubectl -n "$NS" get deploy "$DEPLOY" -o jsonpath='{.status.readyReplicas}')" "1"

# A Secret in a namespace that BACKFLOW_SECRET_NAMESPACES does not list is not read.
kubectl apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1
kind: ScmConnection
metadata: {name: secret-elsewhere, namespace: other}
spec:
  provider: GitLab
  url: https://gitlab.invalid
  tokenSecretRef: {name: gitlab-token, key: token}
YAML
wait_for "a Secret outside BACKFLOW_SECRET_NAMESPACES is SecretForbidden" 90 bash -c "[ \"\$(kubectl -n other get scmconnections.backflow.io secret-elsewhere -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" = SecretForbidden ]"

# ---------------------------------------------------------------------------
if [[ "$SCORECARD" == "true" ]]; then
  step "scorecard"
  kubectl create serviceaccount scorecard -n default >/dev/null 2>&1
  kubectl create clusterrolebinding scorecard --clusterrole=cluster-admin --serviceaccount=default:scorecard >/dev/null 2>&1
  if "$OPERATOR_SDK" scorecard "$ROOT/bundle" -n default -s scorecard -w 240s -o json >"$WORK/scorecard.json" 2>"$WORK/scorecard.err"; then
    ok "scorecard passed"
  else
    bad "scorecard failed: $(head -c 400 "$WORK/scorecard.err")"
  fi
  jq -r '.items[].status.results[] | "\(.state)\t\(.name)\t\(.log // "" | gsub("\n"; " ") | .[0:300])"' "$WORK/scorecard.json" 2>/dev/null | sed 's/^/   | /'
fi

# ---------------------------------------------------------------------------
step "upgrade through the channel"
# A second version is published: its bundle, and a catalog (a new image of the same CatalogSource) in which it replaces the first,
# which is rendered from the registry where its bundle image is. The Subscription (Automatic) gets an InstallPlan.
NEXT="${VERSION%.*}.$((${VERSION##*.} + 1))"
NEXT_CSV="backflow.v$NEXT"
BUNDLE_NEXT="$REGISTRY/backflow-olm-bundle:$NEXT"
CATALOG_NEXT="$REGISTRY/backflow-olm-catalog:$NEXT"
BEFORE_REASON="$(reason scmconnections.backflow.io allowed)"
make -C "$ROOT" bundle BUNDLE_VERSION="$NEXT" OPERATOR_IMAGE="$OPERATOR_IMG" BUNDLE_IMG="$BUNDLE_NEXT" >"$WORK/bundle2.log" 2>&1 \
  || { cat "$WORK/bundle2.log"; die "make bundle of the next version failed"; }
make -C "$ROOT" bundle-build BUNDLE_IMG="$BUNDLE_NEXT" >"$WORK/bundle-build2.log" 2>&1 || { cat "$WORK/bundle-build2.log"; die "bundle image failed"; }
make -C "$ROOT" catalog BUNDLE_VERSION="$NEXT" BUNDLE_IMG="$BUNDLE_NEXT" CATALOG_IMG="$CATALOG_NEXT" \
  PREVIOUS_BUNDLES="$VERSION=$BUNDLE_IMG" OPM_RENDER_FLAGS=--use-http >"$WORK/catalog2.log" 2>&1 || { cat "$WORK/catalog2.log"; die "make catalog failed"; }
make -C "$ROOT" catalog-build CATALOG_IMG="$CATALOG_NEXT" >"$WORK/catalog-build2.log" 2>&1 || { cat "$WORK/catalog-build2.log"; die "catalog image failed"; }
for img in "$BUNDLE_NEXT" "$CATALOG_NEXT"; do docker push -q "$img" >/dev/null || die "cannot push $img"; done
expect "the catalog has both versions, the second replacing the first" \
  "$(awk '/^schema: olm.channel/{c=1} c' "$ROOT/catalog/out/backflow/catalog.yaml" | grep -E 'name: backflow.v|replaces' | tr -d ' ' | tr '\n' ' ')" \
  "name:stable name:backflow.v$VERSION name:backflow.v$NEXT replaces:backflow.v$VERSION "
kubectl -n "$CATALOG_NS" patch catalogsource backflow-catalog --type merge -p "{\"spec\":{\"image\":\"$CATALOG_NEXT\"}}" >/dev/null \
  || die "cannot update the CatalogSource"
wait_for "an InstallPlan for $NEXT_CSV" 240 \
  bash -c "kubectl -n $NS get installplan -o json | jq -e '[.items[] | select(.spec.clusterServiceVersionNames | index(\"$NEXT_CSV\"))] | length > 0'"
wait_for "the ClusterServiceVersion $NEXT_CSV succeeded" 300 \
  bash -c "[ \"\$(kubectl -n $NS get csv $NEXT_CSV -o jsonpath='{.status.phase}')\" = Succeeded ]" \
  || { kubectl -n "$NS" get csv 2>&1 | sed 's/^/   | /'; }
wait_for "the first ClusterServiceVersion is gone (replaced)" 120 bash -c "! kubectl -n $NS get csv $CSV"
expect "the Subscription is at the new version" "$(kubectl -n "$NS" get subscription backflow -o jsonpath='{.status.installedCSV}')" "$NEXT_CSV"
wait_for "the operator is Ready after the upgrade" 120 bash -c "[ \"\$(kubectl -n $NS get deploy $DEPLOY -o jsonpath='{.status.readyReplicas}')\" = 1 ]"
expect "the objects still work: the ScmConnection is judged as before" "$(reason scmconnections.backflow.io allowed)" "$BEFORE_REASON"
expect "and so is the other host" "$(reason scmconnections.backflow.io not-allowed)" "HostNotAllowed"
kubectl -n "$APP_NS" annotate scmconnections.backflow.io allowed upgrade-check="$NEXT" --overwrite >/dev/null
kubectl apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1
kind: BackflowPolicy
metadata: {name: after-upgrade, namespace: $APP_NS}
spec:
  argoCD: {namespace: argocd}
  applications: {names: [web]}
YAML
wait_for "a policy made after the upgrade is reconciled by the new operator" 90 \
  bash -c "[ -n \"\$(kubectl -n $APP_NS get backflowpolicies.backflow.io after-upgrade -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" ]"
VERSION="$NEXT"
CSV="$NEXT_CSV"

# ---------------------------------------------------------------------------
step "uninstall"
kubectl -n "$NS" delete subscription backflow >/dev/null
kubectl -n "$NS" delete csv "$CSV" >/dev/null
wait_for "the operator Deployment is gone" 120 bash -c "! kubectl -n $NS get deploy $DEPLOY"
expect "the CRDs are kept" "$(kubectl get crd scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io -o name | wc -l | tr -d ' ')" "3"
expect "and so are the objects" "$(kubectl -n "$APP_NS" get scmconnections.backflow.io -o name | wc -l | tr -d ' ')" "2"
# The permissions of the operator (the ClusterRoles and bindings OLM made for its service account) are removed by OLM. The
# end-user ClusterRoles that the bundle ships (admin, editor, viewer, metrics-reader) are not: OLM keeps them, and they grant
# nothing to anyone until they are bound.
wait_for "the permissions of the operator are gone" 180 \
  bash -c "[ \"\$(kubectl get clusterrolebinding -o json | jq '[.items[] | select(.subjects[]? | .kind == \"ServiceAccount\" and .name == \"backflow-controller-manager\")] | length')\" = 0 ] && [ \"\$(kubectl get clusterrole -o name | grep -c \"clusterrole.rbac.authorization.k8s.io/$CSV-\" || true)\" = 0 ]"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" == "0" ]]
