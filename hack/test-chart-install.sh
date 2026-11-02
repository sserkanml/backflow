#!/usr/bin/env bash
#
# The chart of the working tree, installed from scratch on a kind cluster of its own: no CRDs, no operator before. No tokens, no
# Git host, no Argo CD.
#
#   1. helm install with the default values, and the image built from this checkout: Helm applies charts/backflow/crds, which
#      the API server rejects if one of them is not valid (the released 0.18.0 had three without a group)
#   2. every CRD is Established and the operator is Ready
#   3. config/samples are applied and reconciled (a Ready condition appears; Argo CD is not installed, so they are not
#      Ready=True, and the policy says why)
#   4. helm uninstall removes the operator, and keeps the CRDs and the samples (Helm never deletes CRDs)
#
#   ./hack/test-chart-install.sh
#
# Environment:
#   CLUSTER       the name of the kind cluster, created and deleted by the script (default: backflow-chart)
#   IMAGE         the operator image (default backflow-chart:test, built here and loaded into kind); SKIP_BUILD=true uses one that exists
#   KEEP          true: keep the cluster
#   KIND_NODE_IMAGE  pins the Kubernetes version of the cluster
#
# It never touches another kubeconfig or cluster. bash 3.2 compatible.

# ok and bad always succeed, so "A && ok || bad" is an if-then-else here.
# shellcheck disable=SC2015
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${CLUSTER:-backflow-chart}"
IMAGE="${IMAGE:-backflow-chart:test}"
SKIP_BUILD="${SKIP_BUILD:-false}"
KEEP="${KEEP:-false}"
SYS=backflow-system
CRDS="scmconnections.backflow.io backflowpolicies.backflow.io driftproposals.backflow.io changerecords.internal.backflow.io"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/backflow-chart.XXXXXX")"
export KUBECONFIG="$WORK/kubeconfig"
PASS=0
FAIL=0

step() { printf '\n== %s\n' "$*"; }
note() { printf '   %s\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
expect() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi; }

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
    note "kept: cluster $CLUSTER (KUBECONFIG=$KUBECONFIG)"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    rm -rf "$WORK"
  fi
  exit "$code"
}
trap cleanup EXIT

for tool in kind kubectl helm docker jq; do command -v "$tool" >/dev/null 2>&1 || die "$tool is needed"; done

step "a cluster, and the image of this checkout"
kind get clusters 2>/dev/null | grep -qx "$CLUSTER" && die "a kind cluster named $CLUSTER exists: delete it or set CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} >/dev/null 2>&1 \
  || die "kind create cluster failed"
if [[ "$SKIP_BUILD" != "true" ]]; then
  docker build -q -t "$IMAGE" "$ROOT" >/dev/null || die "docker build failed"
fi
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null 2>&1 || die "cannot load $IMAGE into kind"
expect "no Backflow CRD is there before" "$(kubectl get crd -o name | grep -c 'backflow.io' || true)" "0"

step "helm install, from scratch"
helm install backflow "$ROOT/charts/backflow" -n "$SYS" --create-namespace --wait --timeout 5m \
  --set "image.repository=${IMAGE%:*}" --set "image.tag=${IMAGE##*:}" --set image.pullPolicy=Never >"$WORK/install.log" 2>&1
code=$?
expect "helm install exits 0" "$code" "0"
[[ "$code" == "0" ]] || { sed 's/^/   | /' "$WORK/install.log" | tail -20; }

step "the CRDs and the operator"
for crd in $CRDS; do
  wait_for "$crd is Established" 60 \
    bash -c "[ \"\$(kubectl get crd $crd -o jsonpath='{.status.conditions[?(@.type==\"Established\")].status}')\" = True ]"
done
expect "no CRD without a group" "$(kubectl get crd -o json | jq '[.items[] | select(.metadata.name | endswith("backflow.io")) | select(.spec.group == "")] | length')" "0"
expect "exactly the four CRDs of Backflow" "$(kubectl get crd -o name | grep -c 'backflow.io' || true)" "4"
wait_for "the operator is Ready" 120 \
  bash -c "[ \"\$(kubectl -n $SYS get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].status.readyReplicas}')\" = 1 ]"
expect "the operator runs the image of this checkout" \
  "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].spec.template.spec.containers[0].image}')" "$IMAGE"

step "the samples"
kubectl -n "$SYS" create secret generic gitlab-token --from-literal=token=not-a-real-token >/dev/null
kubectl -n "$SYS" create secret generic argocd-token --from-literal=token=not-a-real-token >/dev/null
kubectl apply -k "$ROOT/config/samples" >"$WORK/samples.log" 2>&1 && ok "config/samples are accepted by the API server" \
  || bad "config/samples are refused: $(head -c 400 "$WORK/samples.log")"
reason() { kubectl -n "$SYS" get "$1" "$2" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'; }
wait_for "the ScmConnection is reconciled" 90 \
  bash -c "[ -n \"\$(kubectl -n $SYS get scmconnections.backflow.io gitlab -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" ]"
wait_for "the BackflowPolicy is reconciled" 90 \
  bash -c "[ -n \"\$(kubectl -n $SYS get backflowpolicies.backflow.io test-apps -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}')\" ]"
note "the connection says: $(reason scmconnections.backflow.io gitlab); the policy says: $(reason backflowpolicies.backflow.io test-apps)"
expect "the operator is still running" "$(kubectl -n "$SYS" get deploy -l app.kubernetes.io/name=backflow -o jsonpath='{.items[0].status.readyReplicas}')" "1"

step "helm uninstall"
helm uninstall backflow -n "$SYS" --wait >"$WORK/uninstall.log" 2>&1
expect "helm uninstall exits 0" "$?" "0"
expect "the operator is gone" "$(kubectl -n "$SYS" get deploy -o name 2>/dev/null | wc -l | tr -d ' ')" "0"
expect "the CRDs are kept (Helm never deletes them)" "$(kubectl get crd -o name | grep -c 'backflow.io' || true)" "4"
expect "and so are the samples" "$(kubectl -n "$SYS" get backflowpolicies.backflow.io -o name | wc -l | tr -d ' ')" "1"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" == "0" ]]
