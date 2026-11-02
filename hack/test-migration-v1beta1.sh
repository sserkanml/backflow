#!/usr/bin/env bash
#
# The upgrade from the release 0.18 (backflow.io/v1beta1) to this version (v1), on a kind cluster of its own. No tokens, no Git
# host, no Argo CD. Called by hack/test-migration.sh when FROM_VERSION is a v1beta1 release; it can be run on its own.
#
# The objects of 0.18 are stored as v1beta1. v1 has the same schema, so `manager migrate` stores each again as v1 with an update
# that changes nothing, while the operator runs:
#
#   MODE=binary (default)
#   1. the CRDs of the release 0.18, and a ScmConnection, a BackflowPolicy and DriftProposals (Proposed with an open merge
#      request, Merged, Unmapped, Superseded with its signal) made through v1beta1
#   2. the CRDs of this version are applied: that works (v1 is added, v1beta1 stays), and the objects are still stored as v1beta1
#   3. the operator of this version starts: it works, logs a warning and sets backflow_storage_migration_pending to 3
#   4. migrate --dry-run changes nothing; migrate, WITH THE OPERATOR RUNNING, stores every object as v1: storedVersions is [v1],
#      the objects are written (their resourceVersion changes) and hold what they held, v1beta1 is still served, the operator
#      goes on and the metric falls to 0
#   5. migrate again does nothing; the operator runs for a minute and leaves the git status of the proposals as it was
#   6. convert-files on manifests of v1beta1
#
#   MODE=cluster: the release 0.18 is installed as it is (helm install of its chart and image), and the upgrade is `helm upgrade`
#   with the chart and the image of this checkout and migration.enabled: the pre-upgrade hook Job stores the objects as v1
#   (nothing is stopped, nothing is backed up), the operator is replaced by the new one, and a second upgrade with the hook still
#   on finds nothing to do.
#
#   ./hack/test-migration-v1beta1.sh
#   MODE=cluster ./hack/test-migration-v1beta1.sh
#
# Environment: MODE, FROM_VERSION (default 0.18.1; 0.18.0 cannot be installed from its chart), BIN, IMAGE, SKIP_BUILD, CLUSTER,
# KEEP, KIND_NODE_IMAGE, WATCH_SECONDS: as for hack/test-migration.sh.
#
# It never touches another kubeconfig or cluster. bash 3.2 compatible.

# ok and bad always succeed, so "A && ok || bad" is an if-then-else here.
# shellcheck disable=SC2015
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODE="${MODE:-binary}"
CLUSTER="${CLUSTER:-backflow-migrate}"
KEEP="${KEEP:-false}"
BIN="${BIN:-$ROOT/bin/manager}"
WATCH_SECONDS="${WATCH_SECONDS:-60}"
FROM_VERSION="${FROM_VERSION:-0.18.1}"
IMAGE="${IMAGE:-backflow-migration:test}"
SKIP_BUILD="${SKIP_BUILD:-false}"
NS=team-a
SYS=backflow-system
CRDS="scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io"
CHART="oci://ghcr.io/sserkanml/charts/backflow"
case "$MODE" in binary | cluster) ;; *) echo "MODE is binary or cluster" >&2; exit 1 ;; esac

WORK="$(mktemp -d "${TMPDIR:-/tmp}/backflow-migration.XXXXXX")"
export KUBECONFIG="$WORK/kubeconfig"
OPERATOR_PID=""
PASS=0
FAIL=0

step() { printf '\n== %s\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
expect() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi; }

cleanup() {
  local code=$?
  if [[ -n "$OPERATOR_PID" ]]; then kill "$OPERATOR_PID" 2>/dev/null; wait "$OPERATOR_PID" 2>/dev/null; fi
  if [[ "$KEEP" == "true" ]]; then
    note "kept: cluster $CLUSTER, work directory $WORK (KUBECONFIG=$KUBECONFIG)"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    rm -rf "$WORK"
  fi
  exit "$code"
}
trap cleanup EXIT

tools="kind kubectl helm jq curl"
[[ "$MODE" == "cluster" ]] && tools="$tools docker"
for tool in $tools; do command -v "$tool" >/dev/null 2>&1 || die "$tool is needed"; done
[[ -x "$BIN" ]] || { make -C "$ROOT" build >/dev/null || die "make build failed"; }

helm show crds "$CHART" --version "$FROM_VERSION" >"$WORK/from-crds.yaml" 2>"$WORK/from-crds.err" \
  || die "cannot read the CRDs of chart $FROM_VERSION: $(head -c 200 "$WORK/from-crds.err")"
grep -q "^    name: v1beta1$" "$WORK/from-crds.yaml" && ! grep -q "^    name: v1$" "$WORK/from-crds.yaml" \
  || die "FROM_VERSION=$FROM_VERSION is not a v1beta1 release (its CRDs must have v1beta1 and not v1)"
awk '/^  group:/ { if ($2 == "\"\"") bad = 1 } END { exit bad }' "$WORK/from-crds.yaml" \
  || die "the chart $FROM_VERSION has a CRD without a group: it cannot be installed (use 0.18.1)"

# ---------------------------------------------------------------------------
step "a cluster with the release $FROM_VERSION, mode $MODE"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER exists: delete it or set CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} >/dev/null 2>&1 \
  || die "kind create cluster failed"
if [[ "$MODE" == "cluster" ]]; then
  helm install backflow "$CHART" --version "$FROM_VERSION" -n "$SYS" --create-namespace --wait --timeout 5m >/dev/null \
    || die "cannot install the chart $FROM_VERSION"
  expect "the operator of $FROM_VERSION runs" \
    "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].status.readyReplicas}')" "1"
else
  kubectl apply --server-side -f "$WORK/from-crds.yaml" >/dev/null || die "cannot apply the CRDs of $FROM_VERSION"
fi
for crd in $CRDS; do kubectl wait --for=condition=Established "crd/$crd" --timeout=60s >/dev/null || die "$crd is not established"; done
for crd in $CRDS; do
  expect "$crd has v1beta1 only" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[].name]')" '["v1beta1"]'
done

# ---------------------------------------------------------------------------
step "objects of the release $FROM_VERSION (v1beta1)"
kubectl get namespace "$NS" >/dev/null 2>&1 || kubectl create namespace "$NS" >/dev/null
kubectl -n "$NS" create secret generic gitlab-token --from-literal=token=not-a-real-token >/dev/null
kubectl -n "$NS" create secret generic argocd-token --from-literal=token=not-a-real-token >/dev/null
kubectl create -f - >/dev/null <<YAML || die "cannot create the objects"
apiVersion: backflow.io/v1beta1
kind: ScmConnection
metadata: {name: gitlab, namespace: $NS}
spec:
  provider: GitLab
  url: https://gitlab.invalid
  hosts: [gitlab.invalid]
  tokenSecretRef: {name: gitlab-token, key: token}
---
apiVersion: backflow.io/v1beta1
kind: BackflowPolicy
metadata: {name: p1, namespace: $NS}
spec:
  argoCD:
    namespace: argocd
    server: {url: "https://argocd.invalid", tokenSecretRef: {name: argocd-token, key: token}}
  applications: {names: [web]}
  mode: MergeRequest
  helm: {allowApplicationOverride: true}
  git: {branchPrefix: backflow/, mergeRequest: {labels: [backflow], assignActor: false}}
YAML
POLICY_UID="$(kubectl -n "$NS" get backflowpolicies.backflow.io p1 -o jsonpath='{.metadata.uid}')"

proposal() { # <name> <signals-annotation-free>
  kubectl create -f - >/dev/null <<YAML || die "cannot create proposal $1"
apiVersion: backflow.io/v1beta1
kind: DriftProposal
metadata:
  name: $1
  namespace: $NS
  labels: {backflow.io/policy: p1, backflow.io/application: web}
  annotations: {note: kept}
  ownerReferences:
    - {apiVersion: backflow.io/v1beta1, kind: BackflowPolicy, name: p1, uid: $POLICY_UID}
spec:
  policyRef: {name: p1}
  applicationRef: {name: web, namespace: argocd}
  resourceRef: {group: apps, version: v1, kind: Deployment, namespace: demo, name: web}
  sources: [{position: 1, role: Render, repoURL: "https://gitlab.invalid/x/y.git", revision: abc123, targetRevision: main, path: apps/web, type: Kustomize}]
  changes: [{path: /spec/replicas, op: Replace, desired: "1", live: "3"}]
  actor: {username: alice, kind: Person}
  managers: [{manager: kubectl-edit, operation: Update, time: "2026-11-01T07:59:00Z", fields: [/spec/replicas]}]
  detectedAt: "2026-11-01T08:00:00Z"
YAML
}
status() {
  kubectl -n "$NS" patch driftproposals.backflow.io "$1" --subresource=status --type=merge -p "$2" >/dev/null \
    || die "cannot write the status of $1"
}
proposal dp-open
status dp-open '{"status":{"phase":"Proposed","message":"The merge request is open.",
  "git":{"branch":"backflow/web-1","mergeRequest":{"url":"https://gitlab.invalid/x/y/-/merge_requests/7","number":7,"state":"opened"},
         "push":{"rejections":2,"lastRejectedAt":"2026-11-01T08:05:00Z"}},
  "mapping":{"strategy":"Direct","verified":true,"diff":"--- a\n+++ b\n","edits":[{"file":"apps/web/deploy.yaml","location":"/spec/replicas","value":"3","changeIndex":0}]},
  "conditions":[{"type":"Mapped","status":"True","reason":"Verified","message":"ok","lastTransitionTime":"2026-11-01T08:01:00Z"},
                {"type":"Proposed","status":"True","reason":"MergeRequestOpen","message":"open","lastTransitionTime":"2026-11-01T08:02:00Z"}]}}'
proposal dp-merged
status dp-merged '{"status":{"phase":"Merged","git":{"branch":"backflow/web-0","commitSHA":"0123456789abcdef",
  "mergeRequest":{"url":"https://gitlab.invalid/x/y/-/merge_requests/3","number":3,"state":"merged"}},"finishedAt":"2026-10-29T09:00:00Z",
  "conditions":[{"type":"Proposed","status":"True","reason":"Merged","message":"merged","lastTransitionTime":"2026-10-29T09:00:00Z"}]}}'
proposal dp-unmapped
status dp-unmapped '{"status":{"phase":"Unmapped","message":"no file owns it",
  "conditions":[{"type":"Mapped","status":"False","reason":"NoVerifiedCandidate","message":"no file owns it","lastTransitionTime":"2026-11-01T08:01:00Z"}]}}'
proposal dp-superseded
status dp-superseded '{"status":{"phase":"Superseded","supersededBy":"dp-open","finishedAt":"2026-11-01T08:00:30Z","signals":{"supersededBy":"dp-open"},
  "conditions":[{"type":"Proposed","status":"False","reason":"Superseded","message":"replaced","lastTransitionTime":"2026-11-01T08:00:30Z"}]}}'
PROPOSALS="dp-open dp-merged dp-unmapped dp-superseded"

if [[ "$MODE" == "cluster" ]]; then
  # The operator of $FROM_VERSION runs and works on these objects (it cannot reach GitLab or Argo CD, and says so in the status);
  # let it settle, so that what is measured is what the migration starts from.
  note "the operator of $FROM_VERSION is running: letting it settle for 40s"
  sleep 40
fi
# What must not change: the spec of every object, the git status of the proposals, their uid and labels.
frozen() { # <kind-plural> <name>: the part of an object that the migration must not change
  kubectl -n "$NS" get "$1.backflow.io" "$2" -o json | jq -cS '{uid: .metadata.uid, labels: .metadata.labels, spec: .spec, git: .status.git}'
}
for p in $PROPOSALS; do frozen driftproposals "$p" >"$WORK/before-$p.json"; done
frozen backflowpolicies p1 >"$WORK/before-p1.json"
frozen scmconnections gitlab >"$WORK/before-gitlab.json"
rvs() {
  kubectl get scmconnections.backflow.io,backflowpolicies.backflow.io,driftproposals.backflow.io -A -o json \
    | jq -S '[.items[] | {kind, name: .metadata.name, rv: .metadata.resourceVersion}]'
}
ids() {
  kubectl get scmconnections.backflow.io,backflowpolicies.backflow.io,driftproposals.backflow.io -A -o json \
    | jq -S '[.items[] | {kind, name: .metadata.name, uid: .metadata.uid}]'
}
expect "the objects are stored as v1beta1 (they were made through it)" \
  "$(kubectl get driftproposals.backflow.io dp-open -n "$NS" -o jsonpath='{.apiVersion}')" "backflow.io/v1beta1"
IDS0="$(ids)"

operator_args=(--leader-elect=false --health-probe-bind-address=:18081 --metrics-bind-address=127.0.0.1:18443 --metrics-secure=false
  --storage-check-interval=10s)
pending_metric() { curl -s http://127.0.0.1:18443/metrics 2>/dev/null | sed -n 's/^backflow_storage_migration_pending //p'; }

BACKUP="$WORK/backup"
if [[ "$MODE" == "binary" ]]; then
  # -----------------------------------------------------------------------
  step "the CRDs of this version are applied: v1 is added, nothing is stored as v1 yet"
  kubectl apply --server-side --force-conflicts -f "$ROOT/charts/backflow/crds/" >"$WORK/apply.log" 2>&1 \
    && ok "the API server accepts them (v1beta1 stays in the CRDs, so storedVersions may keep it)" \
    || bad "the CRDs of this version are refused: $(head -c 400 "$WORK/apply.log")"
  for crd in $CRDS; do
    expect "$crd: v1 and v1beta1" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[].name]')" '["v1","v1beta1"]'
    expect "$crd: storedVersions has both until migrate has run" "$(kubectl get crd "$crd" -o json | jq -c '.status.storedVersions')" '["v1beta1","v1"]'
  done

  step "the operator of this version starts, and says that migrate has not run"
  "$BIN" "${operator_args[@]}" >"$WORK/operator.log" 2>&1 &
  OPERATOR_PID=$!
  ready=false
  for _ in $(seq 1 60); do
    kill -0 "$OPERATOR_PID" 2>/dev/null || break
    if [[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18081/readyz)" == "200" ]]; then ready=true; break; fi
    sleep 1
  done
  [[ "$ready" == "true" ]] && ok "the operator is ready with objects stored as v1beta1" || bad "the operator did not become ready"
  grep -q "WARNING: objects may still be stored as backflow.io/v1beta1" "$WORK/operator.log" && ok "it logs the warning" || bad "no warning in the log"
  expect "backflow_storage_migration_pending" "$(pending_metric)" "3"
  operator_alive() { kill -0 "$OPERATOR_PID" 2>/dev/null; }

  step "migrate --dry-run, with the operator running"
  RVS0="$(rvs)"
  "$BIN" migrate --dry-run >"$WORK/dry-run.log" 2>&1
  expect "the dry run exits 0" "$?" "0"
  sed 's/^/   | /' "$WORK/dry-run.log" | head -12
  grep -q "stored again as v1 (no change in them)" "$WORK/dry-run.log" && ok "it says what it would do" || bad "the dry run does not say what it would do"
  expect "the dry run wrote nothing" "$(rvs)" "$RVS0"

  step "migrate, with the operator running, and no backup directory"
  "$BIN" migrate >"$WORK/migrate.log" 2>&1
  expect "migrate exits 0" "$?" "0"
  sed 's/^/   | /' "$WORK/migrate.log" | head -14
  operator_alive && ok "the operator is still running" || bad "the operator stopped"
  [[ -e "$BACKUP" ]] && bad "a backup was written" || ok "no backup was needed or written"
else
  # -----------------------------------------------------------------------
  step "helm upgrade to this checkout, with the migration hook (the operator of $FROM_VERSION is running)"
  if [[ "$SKIP_BUILD" != "true" ]]; then
    docker build -q -t "$IMAGE" "$ROOT" >/dev/null || die "docker build failed"
  fi
  kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null 2>&1 || die "cannot load $IMAGE into kind"
  upgrade_chart() {
    helm upgrade backflow "$ROOT/charts/backflow" -n "$SYS" --wait --timeout 10m \
      --set "image.repository=${IMAGE%:*}" --set "image.tag=${IMAGE##*:}" --set image.pullPolicy=Never \
      --set migration.enabled=true --set migration.backup.emptyDir=true \
      --set 'scm.allowedHosts={gitlab.invalid}'
  }
  RVS0="$(rvs)"
  upgrade_chart >"$WORK/upgrade.log" 2>&1
  code=$?
  expect "helm upgrade exits 0" "$code" "0"
  [[ "$code" == "0" ]] || { sed 's/^/   | /' "$WORK/upgrade.log" | tail -30; kubectl -n "$SYS" get jobs,pods 2>&1 | sed 's/^/   | /'; }
  expect "the hook Jobs are gone after they succeeded" "$(kubectl -n "$SYS" get jobs -o name 2>/dev/null | wc -l | tr -d ' ')" "0"
  expect "the migration permissions are gone" "$(kubectl get clusterrole -o name | grep -c 'migrate' || true)" "0"
  expect "the operator is the new one" \
    "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].spec.replicas}/{.items[0].status.readyReplicas}/{.items[0].spec.template.spec.containers[0].image}')" \
    "1/1/$IMAGE"
  expect "nothing was scaled down by the tool (no annotation)" \
    "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o json | jq -r '.items[0].metadata.annotations["migrate.backflow.io/replicas-before"] // "none"')" "none"
  operator_alive() { [[ "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].status.readyReplicas}')" == "1" ]]; }
fi

# ---------------------------------------------------------------------------
step "what the migration did"
for crd in $CRDS; do
  expect "$crd: storedVersions is [v1]" "$(kubectl get crd "$crd" -o json | jq -c '.status.storedVersions')" '["v1"]'
  expect "$crd: v1 and v1beta1 are served" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[] | select(.served) | .name]')" '["v1","v1beta1"]'
  expect "$crd: v1 is the storage version" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[] | select(.storage) | .name]')" '["v1"]'
done
expect "every object reads as v1" "$(kubectl get scmconnections.backflow.io,backflowpolicies.backflow.io,driftproposals.backflow.io -A -o json | jq -r '[.items[].apiVersion] | unique | join(",")')" "backflow.io/v1"
expect "v1beta1 is still served (deprecated)" "$(kubectl get driftproposals.v1beta1.backflow.io dp-open -n "$NS" -o jsonpath='{.apiVersion}' 2>/dev/null)" "backflow.io/v1beta1"
expect "no object was deleted or made again" "$(ids)" "$IDS0"
if [[ "$MODE" == "binary" ]]; then
  # The operator of this version has been running since the CRDs got v1 as the storage version, and what it wrote since is stored as
  # v1 already (so a no-op update of it writes nothing). The others were stored as v1beta1, and the update writes them, which
  # shows in their resourceVersion; the Go test of the tool checks that every one of them is written.
  RVS1="$(rvs)"
  written="$(jq -n --argjson a "$RVS0" --argjson b "$RVS1" '[range(0; $a|length) | select($a[.].rv != $b[.].rv)] | length')"
  note "$written of $(echo "$RVS0" | jq length) objects were written by migrate; the others had been written by the operator, as v1"
  [[ "$written" -ge 1 ]] && ok "objects that were stored as v1beta1 were written (their resourceVersion changed)" \
    || bad "no object was written: nothing was stored again"
fi
for p in $PROPOSALS; do expect "$p: spec, git status, uid and labels are what they were" "$(frozen driftproposals "$p")" "$(cat "$WORK/before-$p.json")"; done
expect "p1: unchanged" "$(frozen backflowpolicies p1)" "$(cat "$WORK/before-p1.json")"
expect "gitlab: unchanged" "$(frozen scmconnections gitlab)" "$(cat "$WORK/before-gitlab.json")"
expect "assignActor false stays false" "$(kubectl -n "$NS" get backflowpolicies.backflow.io p1 -o json | jq -c '.spec.git.mergeRequest.assignActor')" "false"
expect "dp-merged: finishedAt is what it was" "$(kubectl -n "$NS" get driftproposals.backflow.io dp-merged -o json | jq -c .status.finishedAt)" '"2026-10-29T09:00:00Z"'
expect "dp-superseded: the signal is kept" "$(kubectl -n "$NS" get driftproposals.backflow.io dp-superseded -o json | jq -c .status.signals.supersededBy)" '"dp-open"'
expect "dp-open: the annotation is kept" "$(kubectl -n "$NS" get driftproposals.backflow.io dp-open -o json | jq -c '.metadata.annotations.note')" '"kept"'

if [[ "$MODE" == "binary" ]]; then
  wait_metric() { local i; for ((i = 0; i < 60; i += 3)); do [[ "$(pending_metric)" == "0" ]] && return 0; sleep 3; done; return 1; }
  wait_metric && ok "the operator saw it: backflow_storage_migration_pending is 0" || bad "the metric is $(pending_metric), not 0"
  grep -q "Every Backflow object is stored as v1" "$WORK/operator.log" && ok "and says so in the log" || bad "the log does not say that migrate has run"
fi

step "migrate again: nothing changes"
RVS2="$(rvs)"
for i in 1 2; do
  "$BIN" migrate >"$WORK/migrate-$i.log" 2>&1
  expect "run $((i + 1)) exits 0" "$?" "0"
done
grep -q "nothing to migrate" "$WORK/migrate-1.log" && ok "it says there is nothing to migrate" || bad "no 'nothing to migrate' in the output"
[[ "$MODE" == "binary" ]] && expect "and wrote no object" "$(rvs)" "$RVS2"

if [[ "$MODE" == "cluster" ]]; then
  step "a second helm upgrade with the hook still on: nothing to migrate, the operator is left running"
  pod_of() { kubectl -n "$SYS" get pod -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].metadata.name}'; }
  POD_BEFORE="$(pod_of)"
  upgrade_chart >"$WORK/upgrade2.log" 2>&1
  expect "helm upgrade exits 0" "$?" "0"
  expect "the operator pod was not restarted" "$(pod_of)" "$POD_BEFORE"
  expect "no object was deleted or made again" "$(ids)" "$IDS0"
fi

step "the operator keeps the git status of the proposals for ${WATCH_SECONDS}s"
elapsed=0
changed=""
while [[ "$elapsed" -lt "$WATCH_SECONDS" ]]; do
  sleep 5
  elapsed=$((elapsed + 5))
  operator_alive || { changed="the operator stopped"; break; }
  for p in $PROPOSALS; do
    now="$(frozen driftproposals "$p" 2>/dev/null)"
    [[ -n "$now" ]] || { changed="$changed $p was deleted"; continue; }
    [[ "$now" == "$(cat "$WORK/before-$p.json")" ]] || changed="$changed $p: $now"
  done
done
[[ -z "$changed" ]] && ok "the spec and the git status of every proposal are what they were" || bad "$changed"
operator_alive && ok "the operator is still running" || bad "the operator is not running"
if [[ "$MODE" == "binary" ]]; then
  if grep -qiE "panic|unknown field" "$WORK/operator.log"; then bad "the operator log has panic or unknown field"; else ok "no panic in the log"; fi
fi

# ---------------------------------------------------------------------------
step "convert-files on manifests of v1beta1"
GITOPS="$WORK/gitops"
cp -R "$ROOT/hack/testdata/migration/gitops-v1beta1" "$GITOPS"
"$BIN" migrate convert-files --check "$GITOPS" >/dev/null 2>&1
expect "--check exits 1 while a file would change" "$?" "1"
"$BIN" migrate convert-files "$GITOPS" >"$WORK/convert.log" 2>&1
expect "convert-files exits 0" "$?" "0"
"$BIN" migrate convert-files --check "$GITOPS" >/dev/null 2>&1
expect "--check exits 0 afterwards" "$?" "0"
changed_lines="$(diff "$ROOT/hack/testdata/migration/gitops-v1beta1/backflow.yaml" "$GITOPS/backflow.yaml" | grep -c '^[<>]' || true)"
expect "only the two apiVersion lines changed (a line out, a line in, twice)" "$changed_lines" "4"
grep -q "# only this line changes" "$GITOPS/backflow.yaml" && grep -q "names: \[web,   api\]   # spacing is kept" "$GITOPS/backflow.yaml" \
  && ok "comments and spacing are kept" || bad "something else changed"
kubectl apply --dry-run=server -f "$GITOPS/backflow.yaml" >"$WORK/apply-converted.log" 2>&1 && ok "the API server accepts the converted manifests as v1" \
  || bad "the API server refuses the converted manifests: $(head -c 400 "$WORK/apply-converted.log")"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" == "0" ]]
