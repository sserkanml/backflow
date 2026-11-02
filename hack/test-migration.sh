#!/usr/bin/env bash
#
# The upgrade from the last release to this version, on a kind cluster of its own. No tokens, no Git
# host, no Argo CD: the objects are written straight into the API.
#
#   1. the CRDs of the last released chart (helm show crds): backflow.io/v1alpha1 only
#   2. a ScmConnection, a BackflowPolicy and DriftProposals (Proposed with an open merge request in its
#      status, Merged, Unmapped, Superseded with its annotation), their status through the status subresource
#   3. applying the CRDs of this version is refused while objects are stored as v1alpha1
#   4. manager migrate --dry-run changes nothing
#   5. the migration, and every object reads as v1beta1 with its status; storedVersions is [v1beta1] and
#      v1alpha1 is gone from the CRDs
#   6. the migration again changes nothing
#   7. the operator of this version runs for 60 seconds and does not rewrite or drop the git status of the
#      migrated proposals
#   8. manager migrate convert-files on sample GitOps manifests, and the result is accepted by the new API
#
# MODE=binary (the default): the migration is run by hand with bin/manager, and the operator is bin/manager
#   started on the workstation.
# MODE=cluster: the last release is installed as it is (helm install of the chart 0.17.0 with its image),
#   the objects are made, and the upgrade is `helm upgrade` with the chart and the image of this checkout and
#   migration.enabled (the pre-upgrade hook Job stops the operator, backs up and migrates, the post-upgrade
#   hook starts it); the operator of this version is the Deployment in the cluster.
#
#   ./hack/test-migration.sh
#   MODE=cluster ./hack/test-migration.sh
#
# Environment:
#   MODE          binary or cluster
#   FROM_VERSION  the chart version of the last release (default: the newest v*.*.* tag, without the v)
#   BIN           the manager binary (default: bin/manager, built with make build when missing)
#   IMAGE         MODE=cluster: the image of this checkout (default backflow-migration:test, built here and loaded into
#                 kind); SKIP_BUILD=true uses one that exists
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
MODE="${MODE:-binary}"
CLUSTER="${CLUSTER:-backflow-migrate}"
KEEP="${KEEP:-false}"
BIN="${BIN:-$ROOT/bin/manager}"
WATCH_SECONDS="${WATCH_SECONDS:-60}"
FROM_VERSION="${FROM_VERSION:-}"
IMAGE="${IMAGE:-backflow-migration:test}"
SKIP_BUILD="${SKIP_BUILD:-false}"
NS=team-a
SYS=backflow-system
CRDS="scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io"
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

tools="kind kubectl helm jq curl"
[[ "$MODE" == "cluster" ]] && tools="$tools docker"
for tool in $tools; do
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
step "a cluster with the last release ($FROM_VERSION), mode $MODE"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER exists: delete it or set CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} >/dev/null 2>&1 \
  || die "kind create cluster failed"
CHART="oci://ghcr.io/sserkanml/charts/backflow"
if [[ "$MODE" == "cluster" ]]; then
  # The chart as it was released, with its operator image: it installs the CRDs and starts the old operator.
  helm install backflow "$CHART" --version "$FROM_VERSION" -n "$SYS" --create-namespace --wait --timeout 5m >/dev/null \
    || die "cannot install the chart $FROM_VERSION"
  OLD_DEPLOY="$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].metadata.name}')"
  [[ -n "$OLD_DEPLOY" ]] || die "no operator Deployment of the last release"
  expect "the old operator runs" "$(kubectl -n "$SYS" get deploy "$OLD_DEPLOY" -o jsonpath='{.status.readyReplicas}')" "1"
  # The objects below are made while it is stopped, so that it does not touch their status before they are measured.
  kubectl -n "$SYS" scale deploy "$OLD_DEPLOY" --replicas=0 >/dev/null
  kubectl -n "$SYS" wait --for=delete pod -l app.kubernetes.io/name=backflow --timeout=120s >/dev/null 2>&1
  # The leader election lease of a stopped operator is held until it expires (15 seconds); the tool refuses before that.
  sleep 30
else
  helm show crds "$CHART" --version "$FROM_VERSION" >"$WORK/old-crds.yaml" || die "cannot read the CRDs of chart $FROM_VERSION"
  kubectl apply --server-side -f "$WORK/old-crds.yaml" >/dev/null || die "cannot apply the old CRDs"
fi
for crd in $CRDS; do kubectl wait --for=condition=Established "crd/$crd" --timeout=60s >/dev/null || die "$crd is not established"; done
for crd in $CRDS; do
  expect "$crd has v1alpha1 only" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[] | select(.served) | .name]')" '["v1alpha1"]'
done

# ---------------------------------------------------------------------------
step "objects of the last release"
kubectl get namespace "$NS" >/dev/null 2>&1 || kubectl create namespace "$NS" >/dev/null
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

# ids: the kind, name and uid of every object, which an operator that updates statuses does not change.
ids() {
  kubectl get scmconnections.backflow.io,backflowpolicies.backflow.io,driftproposals.backflow.io -A -o json \
    | jq -S '[.items[] | {kind, name: .metadata.name, uid: .metadata.uid}]'
}

# snapshot: the resourceVersion of every object of the three kinds, as they are now.
snapshot() {
  kubectl get scmconnections.backflow.io,backflowpolicies.backflow.io,driftproposals.backflow.io -A -o json \
    | jq -S '[.items[] | {kind, name: .metadata.name, rv: .metadata.resourceVersion, uid: .metadata.uid}]'
}

# ---------------------------------------------------------------------------
step "the CRDs of this version are refused while objects are stored as v1alpha1"
SNAP0="$(snapshot)"
if kubectl apply --server-side --force-conflicts -f "$ROOT/charts/backflow/crds/" >"$WORK/apply-early.log" 2>&1; then
  bad "the API server accepted the new CRDs while objects are stored as v1alpha1"
else
  ok "the API server refused them"
  grep -q "Invalid" "$WORK/apply-early.log" && ok "with an Invalid error for a CRD" \
    || bad "the refusal is not an Invalid error: $(head -c 600 "$WORK/apply-early.log")"
  note "$(grep -m1 'is invalid' "$WORK/apply-early.log" | cut -c1-300)"
fi
expect "no object changed" "$(snapshot)" "$SNAP0"
for crd in $CRDS; do
  expect "$crd still has v1alpha1 only" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[].name]')" '["v1alpha1"]'
done

if [[ "$MODE" == "binary" ]]; then
  step "the operator of this version refuses to start on the CRDs of the last release"
  "$BIN" --leader-elect=false --health-probe-bind-address=:18081 --metrics-bind-address=0 >"$WORK/operator-early.log" 2>&1 &
  EARLY_PID=$!
  for _ in $(seq 1 30); do kill -0 "$EARLY_PID" 2>/dev/null || break; sleep 1; done
  if kill -0 "$EARLY_PID" 2>/dev/null; then
    kill "$EARLY_PID" 2>/dev/null
    bad "the operator is running on the CRDs of the last release"
  else
    wait "$EARLY_PID"
    code=$?
    [[ "$code" != "0" ]] && ok "the operator exited ($code)" || bad "the operator exited 0"
    grep -q "backflow migrate" "$WORK/operator-early.log" && ok "and says to run backflow migrate" \
      || bad "its log does not mention backflow migrate"
  fi
fi

# ---------------------------------------------------------------------------
step "migrate --dry-run"
"$BIN" migrate --dry-run >"$WORK/dry-run.log" 2>&1
expect "the dry run exits 0" "$?" "0"
sed 's/^/   | /' "$WORK/dry-run.log" | head -40
grep -q "DRY RUN" "$WORK/dry-run.log" && ok "it says it is a dry run" || bad "no DRY RUN line"
for want in "DriftProposal team-a/dp-open (phase Proposed): would be deleted and created again as v1beta1" \
  "DriftProposal team-a/dp-merged (phase Merged)" "BackflowPolicy team-a/p1: would be converted" \
  "ScmConnection team-a/gitlab: would be converted" "spec.policyName -> spec.policyRef.name" \
  "the CRD would get v1beta1"; do
  grep -qF "$want" "$WORK/dry-run.log" && ok "it says: $want" || bad "the dry run does not say: $want"
done
expect "the dry run changed no object" "$(snapshot)" "$SNAP0"
for crd in $CRDS; do
  expect "$crd: the dry run left the CRD alone" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[].name]')" '["v1alpha1"]'
done

# ---------------------------------------------------------------------------
BACKUP="$WORK/backup"
# upgrade_chart: helm upgrade to this checkout, with the image of this checkout and the migration hook.
upgrade_chart() {
  helm upgrade backflow "$ROOT/charts/backflow" -n "$SYS" --wait --timeout 10m \
    --set "image.repository=${IMAGE%:*}" --set "image.tag=${IMAGE##*:}" --set image.pullPolicy=Never \
    --set migration.enabled=true --set migration.backup.existingClaim=backflow-migration-backup \
    --set 'scm.allowedHosts={gitlab.invalid}'
}
if [[ "$MODE" == "binary" ]]; then
  step "migrate"
  "$BIN" migrate --backup-dir "$BACKUP" >"$WORK/migrate.log" 2>&1
  expect "migrate exits 0" "$?" "0"
  sed 's/^/   | /' "$WORK/migrate.log" | head -50
else
  step "helm upgrade to this checkout, with the migration hook"
  if [[ "$SKIP_BUILD" != "true" ]]; then
    docker build -q -t "$IMAGE" "$ROOT" >/dev/null || die "docker build failed"
  fi
  kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null 2>&1 || die "cannot load $IMAGE into kind"
  kubectl -n "$SYS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: backflow-migration-backup}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 100Mi}}}
EOF
  upgrade_chart >"$WORK/upgrade.log" 2>&1
  code=$?
  expect "helm upgrade exits 0" "$code" "0"
  [[ "$code" == "0" ]] || { sed 's/^/   | /' "$WORK/upgrade.log" | tail -30; kubectl -n "$SYS" get jobs,pods 2>&1 | sed 's/^/   | /'; }
  expect "the hook Jobs are gone after they succeeded" "$(kubectl -n "$SYS" get jobs -o name 2>/dev/null | wc -l | tr -d ' ')" "0"
  expect "the migration permissions are gone" "$(kubectl get clusterrole -o name | grep -c 'migrate' || true)" "0"
  expect "the operator is running again, as the new image" \
    "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].spec.replicas}/{.items[0].status.readyReplicas}/{.items[0].spec.template.spec.containers[0].image}')" \
    "1/1/$IMAGE"
  expect "the annotation that remembers the replicas is gone" \
    "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o json | jq -r '.items[0].metadata.annotations["migrate.backflow.io/replicas-before"] // "none"')" "none"
  # The backup is in the claim.
  kubectl -n "$SYS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: backup-reader}
spec:
  restartPolicy: Never
  containers:
    - {name: r, image: "busybox:1.37", command: [sleep, "600"], volumeMounts: [{name: b, mountPath: /backup}]}
  volumes: [{name: b, persistentVolumeClaim: {claimName: backflow-migration-backup}}]
EOF
  kubectl -n "$SYS" wait --for=condition=Ready pod/backup-reader --timeout=180s >/dev/null 2>&1 || bad "the pod that reads the backup did not start"
  mkdir -p "$BACKUP"
  kubectl -n "$SYS" exec backup-reader -- tar -C /backup -cf - . | tar -C "$BACKUP" -xf - 2>/dev/null
  kubectl -n "$SYS" delete pod backup-reader --wait=false >/dev/null 2>&1
fi

for crd in $CRDS; do
  expect "$crd: storedVersions is [v1beta1]" "$(kubectl get crd "$crd" -o json | jq -c '.status.storedVersions')" '["v1beta1"]'
  expect "$crd: only v1beta1 is in the CRD" "$(kubectl get crd "$crd" -o json | jq -c '[.spec.versions[].name]')" '["v1beta1"]'
done
expect "the CRD of the internal ChangeRecords exists" "$(kubectl get crd changerecords.internal.backflow.io -o name 2>/dev/null)" \
  "customresourcedefinition.apiextensions.k8s.io/changerecords.internal.backflow.io"
for f in ScmConnection/$NS.gitlab BackflowPolicy/$NS.p1 DriftProposal/$NS.dp-open DriftProposal/$NS.dp-merged \
  DriftProposal/$NS.dp-unmapped DriftProposal/$NS.dp-superseded; do
  [[ -f "$BACKUP/$f.yaml" ]] && ok "backup $f.yaml" || bad "no backup $f.yaml"
done
grep -q "apiVersion: backflow.io/v1alpha1" "$BACKUP/DriftProposal/$NS.dp-open.yaml" 2>/dev/null && ok "a backup is the v1alpha1 object" \
  || bad "a backup is not v1alpha1"

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
# The operator of this version, which runs in the cluster in the cluster mode, rewrites the message when it cannot reach GitLab.
[[ "$MODE" == "binary" ]] && expect "dp-open: message" "$(jq1 dp-open .status.message)" '"The merge request is open."'
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
IDS1="$(ids)"
for i in 1 2; do
  "$BIN" migrate --backup-dir "$WORK/backup-again" >"$WORK/migrate-$i.log" 2>&1
  expect "run $((i + 1)) exits 0" "$?" "0"
  expect "run $((i + 1)) changed no object" "$(snapshot)" "$SNAP1"
done
grep -q "nothing to do\|nothing to migrate" "$WORK/migrate-1.log" && ok "it says there is nothing to do" || bad "no 'nothing to do' in the output"
[[ -e "$WORK/backup-again" ]] && bad "a run with nothing to do wrote a backup" || ok "and wrote no backup"

# ---------------------------------------------------------------------------
step "the operator of this version"
for p in $PROPOSALS; do kubectl -n "$NS" get driftproposals.backflow.io "$p" -o json >"$WORK/migrated-$p.json"; done
if [[ "$MODE" == "binary" ]]; then
  "$BIN" --leader-elect=false --health-probe-bind-address=:18081 --metrics-bind-address=0 >"$WORK/operator.log" 2>&1 &
  OPERATOR_PID=$!
  ready=false
  for _ in $(seq 1 60); do
    kill -0 "$OPERATOR_PID" 2>/dev/null || break
    if [[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18081/readyz)" == "200" ]]; then ready=true; break; fi
    sleep 1
  done
  [[ "$ready" == "true" ]] && ok "the operator is ready" || bad "the operator did not become ready"
  operator_alive() { kill -0 "$OPERATOR_PID" 2>/dev/null; }
  operator_log() { cat "$WORK/operator.log"; }
else
  kubectl -n "$SYS" rollout status deploy -l app.kubernetes.io/name=backflow --timeout=180s >/dev/null 2>&1 \
    && ok "the operator is ready" || bad "the operator is not ready"
  operator_alive() { [[ "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].status.readyReplicas}')" == "1" ]]; }
  operator_log() { kubectl -n "$SYS" logs deploy/"$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].metadata.name}')" 2>&1; }
fi

note "watching for ${WATCH_SECONDS}s: the git status and the existence of the proposals"
elapsed=0
changed=""
while [[ "$elapsed" -lt "$WATCH_SECONDS" ]]; do
  sleep 5
  elapsed=$((elapsed + 5))
  operator_alive || { changed="the operator stopped"; break; }
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
operator_alive && ok "the operator is still running" || bad "the operator is not running"
operator_log >"$WORK/operator-final.log" 2>&1
if grep -qiE "panic|unknown field" "$WORK/operator-final.log"; then bad "the operator log has panic or unknown field"; fi
for p in $PROPOSALS; do
  expect "$p: spec untouched by the operator" "$(jq1 "$p" .spec)" "$(jq -c .spec "$WORK/migrated-$p.json")"
done
note "what the operator logged as an error (the SCM and Argo CD are not reachable, so some are expected):"
grep -E "ERROR|error" "$WORK/operator-final.log" | cut -c1-220 | sort | uniq -c | sort -rn | head -8 | sed 's/^/   | /'
if [[ -n "$OPERATOR_PID" ]]; then
  kill "$OPERATOR_PID" 2>/dev/null
  wait "$OPERATOR_PID" 2>/dev/null
  OPERATOR_PID=""
fi

# ---------------------------------------------------------------------------
if [[ "$MODE" == "cluster" ]]; then
  step "a second helm upgrade with the hook still on: nothing to migrate, the operator is left running"
  pod_of() { kubectl -n "$SYS" get pod -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].metadata.name}'; }
  POD_BEFORE="$(pod_of)"
  upgrade_chart >"$WORK/upgrade2.log" 2>&1
  expect "helm upgrade exits 0" "$?" "0"
  expect "the operator pod was not restarted" "$(pod_of)" "$POD_BEFORE"
  expect "and is ready" "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].status.readyReplicas}')" "1"
  expect "no object was deleted or made again" "$(ids)" "$IDS1"
fi

# ---------------------------------------------------------------------------
step "convert-files on manifests of a GitOps repository"
GITOPS="$WORK/gitops"
cp -R "$ROOT/hack/testdata/migration/gitops" "$GITOPS"
"$BIN" migrate convert-files --check "$GITOPS" >"$WORK/convert-check.log" 2>&1
expect "--check exits 1 while a file would change" "$?" "1"
"$BIN" migrate convert-files "$GITOPS" >"$WORK/convert.log" 2>&1
expect "convert-files exits 0" "$?" "0"
sed 's/^/   | /' "$WORK/convert.log"
"$BIN" migrate convert-files --check "$GITOPS" >/dev/null 2>&1
expect "--check exits 0 afterwards" "$?" "0"
cmp -s "$GITOPS/app.yaml" "$ROOT/hack/testdata/migration/gitops/app.yaml" && ok "the Application document is untouched, byte for byte" \
  || bad "app.yaml changed"
cmp -s "$GITOPS/kustomization.yaml" "$ROOT/hack/testdata/migration/gitops/kustomization.yaml" && ok "the Kustomization is untouched" \
  || bad "kustomization.yaml changed"
grep -q "backflow.io/v1alpha1" "$GITOPS/backflow.yaml" && bad "backflow.yaml still has v1alpha1" || ok "backflow.yaml has no v1alpha1"
grep -q "# The GitLab of the company." "$GITOPS/backflow.yaml" && grep -q "# not the cluster-local one" "$GITOPS/backflow.yaml" \
  && ok "the comments are kept" || bad "a comment was lost"
if kubectl apply --dry-run=server -f "$GITOPS/backflow.yaml" >"$WORK/apply-converted.log" 2>&1; then
  ok "the API server accepts the converted manifests as v1beta1"
else
  bad "the API server refuses the converted manifests: $(head -c 400 "$WORK/apply-converted.log")"
fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" == "0" ]]
