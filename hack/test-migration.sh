#!/usr/bin/env bash
#
# The upgrade from the last release to this version, on a kind cluster of its own. No tokens, no Git
# host, no Argo CD: the objects are written straight into the API.
#
#   1. install the CRDs of the last released chart (helm show crds): backflow.io/v1alpha1 only
#   2. create a ScmConnection, a BackflowPolicy and DriftProposals (Proposed with an open merge request in its
#      status, Merged, Unmapped, Superseded with its annotation), their status through the status subresource
#   3. apply the CRDs of this version; the operator of this version refuses to start (v1alpha1 is stored)
#   4. backflow migrate --dry-run: nothing changes
#   5. backflow migrate: every object reads as v1beta1 with its status, storedVersions is [v1beta1]
#   6. backflow migrate again: nothing changes
#   7. the operator of this version starts and, for 60 seconds, does not rewrite or drop the git status of the
#      migrated proposals
#
#   ./hack/test-migration.sh
#
# Environment:
#   FROM_VERSION  the chart version whose CRDs are the "before" (default: the newest v*.*.* tag, without the v)
#   BIN           the manager binary (default: bin/manager, built with make build when missing)
#   CLUSTER       the name of the kind cluster, created and deleted by the script (default: backflow-migrate)
#   KEEP          true: keep the cluster and the work directory
#   KIND_NODE_IMAGE  pins the Kubernetes version of the cluster
#   WATCH_SECONDS  how long the operator is watched (default 60)
#
# It never touches another kubeconfig or cluster. bash 3.2 compatible.

# ok and bad always succeed, so "A && ok || bad" is an if-then-else here.
# shellcheck disable=SC2015
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${CLUSTER:-backflow-migrate}"
KEEP="${KEEP:-false}"
BIN="${BIN:-$ROOT/bin/manager}"
WATCH_SECONDS="${WATCH_SECONDS:-60}"
FROM_VERSION="${FROM_VERSION:-}"
NS=team-a
CRDS="scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io"

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

# expect <what> <actual> <expected>
expect() {
  if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi
}

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

for tool in kind kubectl helm jq curl; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is needed"
done

if [[ -z "$FROM_VERSION" ]]; then
  FROM_VERSION="$(git -C "$ROOT" tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1)"
  FROM_VERSION="${FROM_VERSION#v}"
fi
[[ -n "$FROM_VERSION" ]] || die "no release tag found: set FROM_VERSION"

if [[ ! -x "$BIN" ]]; then
  step "build"
  make -C "$ROOT" build >/dev/null || die "make build failed"
fi

# ---------------------------------------------------------------------------
step "a cluster, and the CRDs of $FROM_VERSION (the last release)"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER exists: delete it or set CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} >/dev/null 2>&1 \
  || die "kind create cluster failed"
helm show crds oci://ghcr.io/sserkanml/charts/backflow --version "$FROM_VERSION" >"$WORK/old-crds.yaml" \
  || die "cannot read the CRDs of chart $FROM_VERSION"
kubectl apply --server-side -f "$WORK/old-crds.yaml" >/dev/null || die "cannot apply the old CRDs"
for crd in $CRDS; do kubectl wait --for=condition=Established "crd/$crd" --timeout=60s >/dev/null || die "$crd is not established"; done
for crd in $CRDS; do
  expect "$crd serves v1alpha1 only" \
    "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[] | select(.served) | .name]')" '["v1alpha1"]'
done

# ---------------------------------------------------------------------------
step "objects of the last release"
kubectl create namespace "$NS" >/dev/null
kubectl -n "$NS" create secret generic gitlab-token --from-literal=token=not-a-real-token >/dev/null
kubectl -n "$NS" create secret generic argocd-token --from-literal=token=not-a-real-token >/dev/null

kubectl create -f - >/dev/null <<EOF || die "cannot create the connection"
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata: {name: gitlab, namespace: $NS}
spec:
  provider: gitlab
  url: https://gitlab.invalid
  hosts: [gitlab.invalid]
  tokenSecretRef: {name: gitlab-token, key: token}
EOF
kubectl create -f - >/dev/null <<EOF || die "cannot create the policy"
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata: {name: p1, namespace: $NS}
spec:
  argoCDNamespace: argocd
  argoCD:
    url: https://argocd.invalid
    tokenSecretRef: {name: argocd-token, key: token}
  applications: {names: [web]}
  mode: MergeRequest
  helm: {fallbackToApplicationOverride: true}
  mergeRequest: {branchPrefix: backflow/, labels: [backflow], assignActor: false}
EOF
POLICY_UID="$(kubectl -n "$NS" get backflowpolicies.backflow.io p1 -o jsonpath='{.metadata.uid}')"
[[ -n "$POLICY_UID" ]] || die "no uid for the policy"

# proposal <name> <annotations-yaml>: a proposal of the last release, owned by the policy as the operator makes them.
proposal() {
  kubectl create -f - >/dev/null <<EOF || die "cannot create proposal $1"
apiVersion: backflow.io/v1alpha1
kind: DriftProposal
metadata:
  name: $1
  namespace: $NS
  labels: {backflow.io/policy: p1, backflow.io/application: web}
  annotations: {$2}
  ownerReferences:
    - {apiVersion: backflow.io/v1alpha1, kind: BackflowPolicy, name: p1, uid: $POLICY_UID}
spec:
  policyName: p1
  application: {name: web, namespace: argocd}
  resource: {group: apps, version: v1, kind: Deployment, namespace: demo, name: web}
  source: {repoURL: "https://gitlab.invalid/x/y.git", revision: abc123, targetRevision: main, path: apps/web, type: Kustomize}
  changes:
    - {path: /spec/replicas, op: Replace, desired: "1", live: "3"}
  actor: {username: alice, kind: person, manager: kubectl-edit, operation: Update, time: "2026-11-01T07:59:00Z", fields: [/spec/replicas]}
  detectedAt: "2026-11-01T08:00:00Z"
EOF
}
status() {
  kubectl -n "$NS" patch driftproposals.backflow.io "$1" --subresource=status --type=merge -p "$2" >/dev/null \
    || die "cannot write the status of $1"
}

proposal dp-open ""
status dp-open '{"status":{"phase":"Proposed","message":"The merge request is open.","branch":"backflow/web-1",
  "pushRejections":2,"lastPushRejectedAt":"2026-11-01T08:05:00Z",
  "mergeRequest":{"url":"https://gitlab.invalid/x/y/-/merge_requests/7","number":7,"branch":"backflow/web-1","state":"opened"},
  "mapping":{"strategy":"Direct","verified":true,"diff":"--- a\n+++ b\n","edits":[{"file":"apps/web/deploy.yaml","location":"/spec/replicas","value":"3","changeIndex":0}]},
  "conditions":[{"type":"Mapped","status":"True","reason":"Verified","message":"ok","lastTransitionTime":"2026-11-01T08:01:00Z"},
                {"type":"Proposed","status":"True","reason":"MergeRequestOpen","message":"open","lastTransitionTime":"2026-11-01T08:02:00Z"}]}}'
proposal dp-merged ""
status dp-merged '{"status":{"phase":"Merged","commitSHA":"0123456789abcdef","branch":"backflow/web-0",
  "mergeRequest":{"url":"https://gitlab.invalid/x/y/-/merge_requests/3","number":3,"branch":"backflow/web-0","state":"merged"},
  "conditions":[{"type":"Proposed","status":"True","reason":"Merged","message":"merged","lastTransitionTime":"2026-10-29T09:00:00Z"}]}}'
proposal dp-unmapped ""
status dp-unmapped '{"status":{"phase":"Unmapped","message":"no file owns it","mapping":{"strategy":"Unmapped","reason":"NoVerifiedCandidate"},
  "conditions":[{"type":"Mapped","status":"False","reason":"NoVerifiedCandidate","message":"no file owns it","lastTransitionTime":"2026-11-01T08:01:00Z"}]}}'
proposal dp-superseded '"backflow.io/superseded-by": dp-open'
status dp-superseded '{"status":{"phase":"Superseded","supersededBy":"dp-open",
  "conditions":[{"type":"Proposed","status":"False","reason":"Superseded","message":"replaced","lastTransitionTime":"2026-11-01T08:00:30Z"}]}}'

PROPOSALS="dp-open dp-merged dp-unmapped dp-superseded"
for p in $PROPOSALS; do
  kubectl -n "$NS" get driftproposals.backflow.io "$p" -o json >"$WORK/before-$p.json"
done
expect "the proposals were made as v1alpha1" "$(jq -r .apiVersion "$WORK/before-dp-open.json")" "backflow.io/v1alpha1"

# ---------------------------------------------------------------------------
step "the CRDs of this version"
kubectl apply --server-side --force-conflicts -f "$ROOT/charts/backflow/crds/" >/dev/null || die "cannot apply the new CRDs"
for crd in $CRDS; do kubectl wait --for=condition=Established "crd/$crd" --timeout=60s >/dev/null; done
for i in $(seq 1 30); do
  kubectl get backflowpolicies.v1beta1.backflow.io -A >/dev/null 2>&1 && break
  sleep 1
done
for crd in $CRDS; do
  expect "$crd stores both versions until migrate has run" \
    "$(kubectl get crd "$crd" -o json | jq -c '.status.storedVersions')" '["v1alpha1","v1beta1"]'
  expect "$crd serves v1beta1 only" \
    "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[] | select(.served) | .name]')" '["v1beta1"]'
done

step "the operator of this version refuses to start while v1alpha1 is stored"
"$BIN" --leader-elect=false --health-probe-bind-address=:18081 --metrics-bind-address=0 >"$WORK/operator-early.log" 2>&1 &
EARLY_PID=$!
for i in $(seq 1 30); do kill -0 "$EARLY_PID" 2>/dev/null || break; sleep 1; done
if kill -0 "$EARLY_PID" 2>/dev/null; then
  kill "$EARLY_PID" 2>/dev/null
  bad "the operator is running although objects are stored as v1alpha1"
else
  wait "$EARLY_PID"
  code=$?
  [[ "$code" != "0" ]] && ok "the operator exited ($code)" || bad "the operator exited 0"
  if grep -q "backflow migrate" "$WORK/operator-early.log"; then ok "and says to run backflow migrate"; else bad "its log does not mention backflow migrate"; fi
fi

# ---------------------------------------------------------------------------
# snapshot: the resourceVersion of every object of the three kinds, as v1beta1 sees them.
snapshot() {
  kubectl get scmconnections.v1beta1.backflow.io,backflowpolicies.v1beta1.backflow.io,driftproposals.v1beta1.backflow.io -A -o json \
    | jq -S '[.items[] | {kind, name: .metadata.name, rv: .metadata.resourceVersion, uid: .metadata.uid}]'
}
set_served() { # <true|false> for v1alpha1 in the three CRDs
  local crd idx
  for crd in $CRDS; do
    idx="$(kubectl get crd "$crd" -o json | jq '.spec.versions | map(.name) | index("v1alpha1")')"
    kubectl patch crd "$crd" --type=json -p "[{\"op\":\"replace\",\"path\":\"/spec/versions/$idx/served\",\"value\":$1}]" >/dev/null
  done
  sleep 3
}

step "migrate --dry-run"
set_served true
V1ALPHA1_BEFORE="$(kubectl get scmconnections.v1alpha1.backflow.io,backflowpolicies.v1alpha1.backflow.io,driftproposals.v1alpha1.backflow.io -A -o json \
  | jq -S '[.items[] | {kind, name: .metadata.name, rv: .metadata.resourceVersion}]')"
"$BIN" migrate --dry-run >"$WORK/dry-run.log" 2>&1
expect "the dry run exits 0" "$?" "0"
sed 's/^/   | /' "$WORK/dry-run.log" | head -60
grep -q "DRY RUN" "$WORK/dry-run.log" && ok "it says it is a dry run" || bad "no DRY RUN line"
for want in "DriftProposal team-a/dp-open (phase Proposed): would be deleted and created again as v1beta1" \
  "DriftProposal team-a/dp-merged (phase Merged)" "BackflowPolicy team-a/p1: would be converted" \
  "ScmConnection team-a/gitlab: would be converted" "spec.policyName -> spec.policyRef.name"; do
  grep -qF "$want" "$WORK/dry-run.log" && ok "it says: $want" || bad "the dry run does not say: $want"
done
V1ALPHA1_AFTER="$(kubectl get scmconnections.v1alpha1.backflow.io,backflowpolicies.v1alpha1.backflow.io,driftproposals.v1alpha1.backflow.io -A -o json \
  | jq -S '[.items[] | {kind, name: .metadata.name, rv: .metadata.resourceVersion}]')"
expect "the dry run changed no object" "$V1ALPHA1_AFTER" "$V1ALPHA1_BEFORE"
for crd in $CRDS; do
  expect "$crd: storedVersions is still both" "$(kubectl get crd "$crd" -o json | jq -c '.status.storedVersions')" '["v1alpha1","v1beta1"]'
done
set_served false

# ---------------------------------------------------------------------------
step "migrate"
BACKUP="$WORK/backup"
"$BIN" migrate --backup-dir "$BACKUP" --serve-v1alpha1 >"$WORK/migrate.log" 2>&1
expect "migrate exits 0" "$?" "0"
sed 's/^/   | /' "$WORK/migrate.log" | head -60

for crd in $CRDS; do
  expect "$crd: storedVersions is [v1beta1]" "$(kubectl get crd "$crd" -o json | jq -c '.status.storedVersions')" '["v1beta1"]'
  expect "$crd: v1alpha1 is not served after the run" \
    "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[] | select(.served) | .name]')" '["v1beta1"]'
done
for f in ScmConnection/$NS.gitlab BackflowPolicy/$NS.p1 DriftProposal/$NS.dp-open DriftProposal/$NS.dp-merged \
  DriftProposal/$NS.dp-unmapped DriftProposal/$NS.dp-superseded; do
  [[ -f "$BACKUP/$f.yaml" ]] && ok "backup $f.yaml" || bad "no backup $f.yaml"
done
grep -q "apiVersion: backflow.io/v1alpha1" "$BACKUP/DriftProposal/$NS.dp-open.yaml" && ok "a backup is the v1alpha1 object" || bad "a backup is not v1alpha1"

for kind in scmconnections backflowpolicies driftproposals; do
  versions="$(kubectl get "$kind.backflow.io" -A -o json | jq -r '[.items[].apiVersion] | unique | join(",")')"
  expect "every $kind reads as v1beta1" "$versions" "backflow.io/v1beta1"
done

# jq1 <proposal> <filter>: a value of a migrated proposal.
jq1() { kubectl -n "$NS" get driftproposals.backflow.io "$1" -o json | jq -c "$2"; }
orig() { jq -c "$2" "$WORK/before-$1.json"; }

step "what the migrated objects hold"
expect "connection: provider" "$(kubectl -n $NS get scmconnections.backflow.io gitlab -o json | jq -c '.spec.provider')" '"GitLab"'
expect "policy: argoCD.server.url" "$(kubectl -n $NS get backflowpolicies.backflow.io p1 -o json | jq -c '.spec.argoCD.server.url')" '"https://argocd.invalid"'
expect "policy: git.mergeRequest.assignActor stays false" \
  "$(kubectl -n $NS get backflowpolicies.backflow.io p1 -o json | jq -c '.spec.git.mergeRequest.assignActor')" 'false'
expect "policy: the uid is the same" "$(kubectl -n $NS get backflowpolicies.backflow.io p1 -o jsonpath='{.metadata.uid}')" "$POLICY_UID"

for p in $PROPOSALS; do
  expect "$p: phase" "$(jq1 "$p" .status.phase)" "$(orig "$p" .status.phase)"
  expect "$p: conditions" "$(jq1 "$p" '[.status.conditions[] | {type, status, reason, message, lastTransitionTime}]')" \
    "$(orig "$p" '[.status.conditions[] | {type, status, reason, message, lastTransitionTime}]')"
  expect "$p: policyRef" "$(jq1 "$p" .spec.policyRef.name)" '"p1"'
  expect "$p: applicationRef" "$(jq1 "$p" .spec.applicationRef)" "$(orig "$p" .spec.application)"
  expect "$p: resourceRef" "$(jq1 "$p" .spec.resourceRef)" "$(orig "$p" .spec.resource)"
  expect "$p: changes" "$(jq1 "$p" .spec.changes)" "$(orig "$p" .spec.changes)"
  expect "$p: detectedAt" "$(jq1 "$p" .spec.detectedAt)" "$(orig "$p" .spec.detectedAt)"
  expect "$p: the creationTimestamp is kept in an annotation" \
    "$(jq1 "$p" '.metadata.annotations["migrate.backflow.io/created"]')" "$(orig "$p" .metadata.creationTimestamp)"
  expect "$p: marked as migrated" "$(jq1 "$p" '.metadata.annotations["migrate.backflow.io/migrated-from"]')" '"v1alpha1"'
  expect "$p: owned by the policy, with its uid" "$(jq1 "$p" '.metadata.ownerReferences[0] | {apiVersion, kind, name, uid}')" \
    "{\"apiVersion\":\"backflow.io/v1beta1\",\"kind\":\"BackflowPolicy\",\"name\":\"p1\",\"uid\":\"$POLICY_UID\"}"
  expect "$p: labels" "$(jq1 "$p" .metadata.labels)" "$(orig "$p" .metadata.labels)"
  expect "$p: the actor is split" "$(jq1 "$p" '[.spec.actor, .spec.managers[0].manager]')" '[{"kind":"Person","username":"alice"},"kubectl-edit"]'
  expect "$p: no controller annotation is left" "$(jq1 "$p" '[.metadata.annotations | keys[] | select(startswith("backflow.io/"))] | length')" "0"
done

expect "dp-open: merge request url" "$(jq1 dp-open .status.git.mergeRequest.url)" '"https://gitlab.invalid/x/y/-/merge_requests/7"'
expect "dp-open: merge request number" "$(jq1 dp-open .status.git.mergeRequest.number)" "7"
expect "dp-open: merge request state" "$(jq1 dp-open .status.git.mergeRequest.state)" '"opened"'
expect "dp-open: branch" "$(jq1 dp-open .status.git.branch)" '"backflow/web-1"'
expect "dp-open: push counters" "$(jq1 dp-open '.status.git.push')" '{"lastRejectedAt":"2026-11-01T08:05:00Z","rejections":2}'
expect "dp-open: mapping" "$(jq1 dp-open '.status.mapping | {strategy, verified, diff, edits}')" \
  '{"strategy":"Direct","verified":true,"diff":"--- a\n+++ b\n","edits":[{"changeIndex":0,"file":"apps/web/deploy.yaml","location":"/spec/replicas","value":"3"}]}'
expect "dp-open: message" "$(jq1 dp-open .status.message)" '"The merge request is open."'
expect "dp-open: not finished" "$(jq1 dp-open .status.finishedAt)" "null"
expect "dp-merged: commit" "$(jq1 dp-merged .status.git.commitSHA)" '"0123456789abcdef"'
expect "dp-merged: finishedAt is the last transition, not the migration" "$(jq1 dp-merged .status.finishedAt)" '"2026-10-29T09:00:00Z"'
expect "dp-unmapped: no strategy" "$(jq1 dp-unmapped '.status.mapping.strategy')" "null"
expect "dp-unmapped: not finished" "$(jq1 dp-unmapped .status.finishedAt)" "null"
expect "dp-superseded: status.supersededBy" "$(jq1 dp-superseded .status.supersededBy)" '"dp-open"'
expect "dp-superseded: signals.supersededBy (was an annotation)" "$(jq1 dp-superseded .status.signals.supersededBy)" '"dp-open"'
expect "dp-superseded: finishedAt" "$(jq1 dp-superseded .status.finishedAt)" '"2026-11-01T08:00:30Z"'

# ---------------------------------------------------------------------------
step "migrate again: nothing changes"
SNAP1="$(snapshot)"
for i in 1 2; do
  "$BIN" migrate --backup-dir "$BACKUP" >"$WORK/migrate-$i.log" 2>&1
  expect "run $((i + 1)) exits 0" "$?" "0"
  expect "run $((i + 1)) changed no object" "$(snapshot)" "$SNAP1"
done
grep -q "nothing to do" "$WORK/migrate-1.log" && ok "it says there is nothing to do" || bad "no 'nothing to do' in the output"

# ---------------------------------------------------------------------------
step "the operator of this version"
for p in $PROPOSALS; do kubectl -n "$NS" get driftproposals.backflow.io "$p" -o json >"$WORK/migrated-$p.json"; done
"$BIN" --leader-elect=false --health-probe-bind-address=:18081 --metrics-bind-address=0 >"$WORK/operator.log" 2>&1 &
OPERATOR_PID=$!
ready=false
for i in $(seq 1 60); do
  kill -0 "$OPERATOR_PID" 2>/dev/null || break
  if [[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18081/readyz)" == "200" ]]; then ready=true; break; fi
  sleep 1
done
[[ "$ready" == "true" ]] && ok "the operator is ready" || bad "the operator did not become ready (see the log below)"

note "watching for ${WATCH_SECONDS}s: the git status and the existence of the proposals"
elapsed=0
changed=""
while [[ "$elapsed" -lt "$WATCH_SECONDS" ]]; do
  sleep 5
  elapsed=$((elapsed + 5))
  kill -0 "$OPERATOR_PID" 2>/dev/null || { changed="the operator exited"; break; }
  for p in $PROPOSALS; do
    now="$(kubectl -n "$NS" get driftproposals.backflow.io "$p" -o json 2>/dev/null)"
    if [[ -z "$now" ]]; then changed="$changed $p was deleted"; continue; fi
    a="$(echo "$now" | jq -cS '.status.git')"
    b="$(jq -cS '.status.git' "$WORK/migrated-$p.json")"
    [[ "$a" == "$b" ]] || changed="$changed $p: git status changed ($b -> $a)"
    [[ "$(echo "$now" | jq -r .metadata.uid)" == "$(jq -r .metadata.uid "$WORK/migrated-$p.json")" ]] || changed="$changed $p was created again"
  done
done
[[ -z "$changed" ]] && ok "the operator ran ${WATCH_SECONDS}s and left the git status of the proposals as it was" || bad "$changed"
kill -0 "$OPERATOR_PID" 2>/dev/null && ok "the operator is still running" || bad "the operator is not running"
if grep -qiE "panic|unknown field" "$WORK/operator.log"; then bad "the operator log has panic or unknown field"; fi
for p in $PROPOSALS; do
  expect "$p: spec untouched by the operator" "$(jq1 "$p" .spec)" "$(jq -c .spec "$WORK/migrated-$p.json")"
done
kill "$OPERATOR_PID" 2>/dev/null
wait "$OPERATOR_PID" 2>/dev/null
OPERATOR_PID=""
note "what the operator logged as an error (the SCM and Argo CD are not reachable, so some are expected):"
grep -E "ERROR|error" "$WORK/operator.log" | cut -c1-220 | sort | uniq -c | sort -rn | head -8 | sed 's/^/   | /'

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" == "0" ]]
