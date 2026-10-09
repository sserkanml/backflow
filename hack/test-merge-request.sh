#!/usr/bin/env bash
#
# Backflow - merge request end-to-end test
#
# Runs the full path against the real demo repository on gitlab.com:
#   a. A LOG_LEVEL drift becomes a merge request from backflow/<proposal> into
#      main. The file on the branch has the live value and keeps its comment
#      line; the proposal is Proposed with the merge request URL; main is not
#      touched.
#   b. Closing the merge request makes the proposal Rejected. The same drift is
#      not proposed again (nor a new merge request opened), the rejection is
#      reported once as an Event, and Argo CD can then revert the cluster.
#   c. A different drift opens a merge request; merging it through the API makes
#      the proposal Merged with the merge commit. After Argo CD syncs the new
#      commit the resource is Synced and no new proposal appears.
#   d. A drift whose merge request is open and whose live value then goes back to
#      Git (same revision) makes the proposal Reverted; its merge request is
#      closed with a comment and its branch deleted.
#   e. A drift whose merge request is open, then an unrelated commit to main:
#      Argo CD syncs the new revision and resets the cluster. The change is not
#      lost: the proposal stays Proposed, the merge request stays open with one
#      comment saying so, and merging it brings the change back to the cluster.
#
# Requires the environment from ./hack/dev-up.sh and GITLAB_TOKEN with the api
# and write_repository scopes (owner or maintainer of the demo project). Without
# GITLAB_TOKEN the test is skipped with a message and exits 0.
#
# Everything the script creates is removed on exit: branches, merge requests,
# the test label, the test namespace. Content that scenario c merged into main is
# restored with a follow-up commit through the API. Because merged changes cannot
# be undone, that restore commit stays in the history of main.
#
# The operator runs locally (bin/manager, polling merge requests every POLL)
# unless IN_CLUSTER=true, which uses the deployed operator (polling every two
# minutes by default; raise WAIT accordingly).
#
# Usage (from the repository root):
#   GITLAB_TOKEN=... ./hack/test-merge-request.sh
#   KEEP=true ./hack/test-merge-request.sh      # keep the test namespace

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
NS="${NS:-backflow-mr-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
APP="${APP:-demo-app}"
DEMO_NS="${DEMO_NS:-demo}"
POLICY="mr-policy"
SCM_NAME="gitlab"
LABEL="backflow-e2e"
TOKEN_SECRET_NS="${TOKEN_SECRET_NS:-backflow-system}"
TOKEN_SECRET="${TOKEN_SECRET:-argocd-token}"
GITLAB_API="${GITLAB_API:-https://gitlab.com/api/v4}"
GITLAB_PROJECT="${GITLAB_PROJECT:-sserkanml/backflow-demo}"
KEEP="${KEEP:-false}"
IN_CLUSTER="${IN_CLUSTER:-false}"
MANAGER_NS="${MANAGER_NS:-backflow-system}"
MANAGER_DEPLOY="${MANAGER_DEPLOY:-backflow-controller-manager}"
ARGOCD_URL="${ARGOCD_URL:-https://argocd-server.argocd.svc}"
# How often the local operator polls merge requests.
POLL="${POLL:-10s}"
# Seconds to wait for Argo CD, the operator and GitLab to react.
if [[ "$IN_CLUSTER" == "true" ]]; then WAIT="${WAIT:-300}"; else WAIT="${WAIT:-120}"; fi
# Seconds to wait before asserting that nothing happened.
QUIET="${QUIET:-20}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT/tmp"
LOG_FILE="$LOG_DIR/test-merge-request-manager.log"

if [[ -z "${GITLAB_TOKEN:-}" ]]; then
  echo "SKIPPED: GITLAB_TOKEN is not set."
  echo "This test pushes branches and merges into $GITLAB_PROJECT on gitlab.com and needs a"
  echo "token with the api and write_repository scopes: GITLAB_TOKEN=... $0"
  exit 0
fi

MANAGER_PID=""
PF_PID=""
WORKDIR=""
P_A=""
P_C=""
P_D=""
P_E=""
RESTORE_CLUSTER=false
LABEL_PREEXISTED=true
MAIN_ORIG_SHA=""
ORIG_LOG_LEVEL=""
ORIG_REPLICAS=""
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
note() { echo "    $*"; }

k() { kubectl --context "kind-$CLUSTER_NAME" "$@"; }

# ---------------------------------------------------------------------------
# GitLab API. The token travels in the environment only: never in an argument
# list, a log or a file other than the 0600 Secret input.
# ---------------------------------------------------------------------------

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/backflow-mr-test.XXXXXX")"
chmod 700 "$WORKDIR"
mkdir -p "$WORKDIR/orig"

cat >"$WORKDIR/gl.py" <<'PY'
import json, os, sys, urllib.error, urllib.request

base = os.environ.get("GITLAB_API", "https://gitlab.com/api/v4")
token = os.environ["GITLAB_TOKEN"]
method, path = sys.argv[1], sys.argv[2]
body = sys.argv[3].encode() if len(sys.argv) > 3 else None
status_only = method.startswith("STATUS-")
if status_only:
    method = method[len("STATUS-"):]
req = urllib.request.Request(base + path, data=body, method=method,
                             headers={"PRIVATE-TOKEN": token, "Content-Type": "application/json"})
try:
    resp = urllib.request.urlopen(req, timeout=30)
    code, data = resp.status, resp.read().decode()
except urllib.error.HTTPError as e:
    code, data = e.code, e.read().decode()
if status_only:
    print(code)
    sys.exit(0)
if code >= 400:
    sys.stderr.write("HTTP %d: %s\n" % (code, data[:300]))
    sys.exit(22)
sys.stdout.write(data)
PY

PID_ENC="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$GITLAB_PROJECT")"
enc() { python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }
gl() { python3 "$WORKDIR/gl.py" "$@"; }
# gl_status <METHOD> <path>: HTTP status code only.
gl_status() { python3 "$WORKDIR/gl.py" "STATUS-$1" "$2"; }
# jx <python expression on d>: read JSON on stdin.
jx() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }

project_path() { echo "/projects/$PID_ENC$1"; }

# mr_info <branch>: "<iid>|<state>|<web_url>|<merge_commit_sha>" of the newest merge request from the branch.
mr_info() {
  gl GET "$(project_path "/merge_requests?source_branch=$(enc "$1")&state=all&order_by=created_at&sort=desc")" |
    jx '"|".join(str(x) for x in ((d[0]["iid"], d[0]["state"], d[0]["web_url"], d[0].get("merge_commit_sha") or "") if d else ("", "", "", "")))'
}
branch_exists() { [[ "$(gl_status GET "$(project_path "/repository/branches/$(enc "$1")")")" == "200" ]]; }
main_sha() { gl GET "$(project_path "/repository/branches/main")" | jx 'd["commit"]["id"]'; }
raw_file() { gl GET "$(project_path "/repository/files/$(enc "$1")/raw?ref=$(enc "$2")")"; }

# ---------------------------------------------------------------------------
# Cluster helpers
# ---------------------------------------------------------------------------

refresh_log() {
  if [[ "$IN_CLUSTER" == "true" ]]; then
    mkdir -p "$LOG_DIR"
    k -n "$MANAGER_NS" logs "deploy/$MANAGER_DEPLOY" >"$LOG_FILE" 2>&1 || true
  fi
}

wait_for() {
  local desc="$1" waited=0
  shift
  while ! "$@" >/dev/null 2>&1; do
    waited=$((waited + 2))
    if (( waited > WAIT )); then
      fail "$desc (timed out after ${WAIT}s)"
      return 0
    fi
    sleep 2
  done
  pass "$desc"
}

refresh_app() {
  k -n "$ARGOCD_NS" annotate application "$APP" "argocd.argoproj.io/refresh=${1:-normal}" --overwrite >/dev/null 2>&1 || true
}
app_sync_status() { k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.status}' 2>/dev/null; }
app_is_synced()   { refresh_app; [[ "$(app_sync_status)" == "Synced" ]]; }
live_log_level()  { k -n "$DEMO_NS" get configmap demo-config -o jsonpath='{.data.LOG_LEVEL}'; }
set_log_level() {
  k -n "$DEMO_NS" patch configmap demo-config --type merge -p "{\"data\":{\"LOG_LEVEL\":\"$1\"}}" >/dev/null
  refresh_app
}

# sync_app: run one sync of the Application and wait until it is Synced. The
# demo Application has automated sync (without selfHeal), which applies each new
# Git commit once, the first time the Application is OutOfSync at it. After
# scenario c the merge commit has not been applied by a sync operation yet, so
# the next drift would be reset by that automated sync straight away. Spending
# it here keeps the drifts of scenarios d and e visible until the test itself
# resets them (d) or pushes a new commit (e).
sync_app() {
  k -n "$ARGOCD_NS" patch application "$APP" --type merge \
    -p '{"operation":{"initiatedBy":{"username":"backflow-test"},"sync":{}}}' >/dev/null 2>&1 || true
  local i
  for i in $(seq 1 45); do
    refresh_app
    [[ "$(app_sync_status)" == "Synced" ]] && return 0
    sleep 2
  done
  return 1
}

# restore_demo [revision]: put the live demo back to what Git says, via Argo CD.
# With a revision, first wait until Argo CD has seen that commit, so the sync
# does not apply a stale one.
restore_demo() {
  local want="${1:-}" j
  if [[ -n "$want" ]]; then
    for j in $(seq 1 60); do
      refresh_app hard
      [[ "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}' 2>/dev/null)" == "$want" ]] && break
      sleep 2
    done
  fi
  k -n "$DEMO_NS" patch configmap demo-config --type merge \
    -p "{\"data\":{\"LOG_LEVEL\":\"$ORIG_LOG_LEVEL\"}}" >/dev/null 2>&1 || true
  k -n "$DEMO_NS" scale deployment demo --replicas="$ORIG_REPLICAS" >/dev/null 2>&1 || true
  k -n "$ARGOCD_NS" patch application "$APP" --type merge \
    -p '{"operation":{"initiatedBy":{"username":"backflow-test"},"sync":{}}}' >/dev/null 2>&1 || true
  local i
  for i in $(seq 1 60); do
    refresh_app hard
    [[ "$(app_sync_status)" == "Synced" ]] && return 0
    sleep 2
  done
  return 1
}

# proposals: one line per DriftProposal of the policy for demo-config:
#   <name>|<phase>|<supersededBy>|<path>:<op>:<desired>><live>;...
proposals() {
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" -o jsonpath='{range .items[*]}{.metadata.name}|{.status.phase}|{.spec.resource.name}|{.status.supersededBy}|{range .spec.changes[*]}{.path}:{.op}:{.desired}>{.live};{end}{"\n"}{end}' 2>/dev/null |
    awk -F'|' '$3 == "demo-config" { print $1 "|" $2 "|" $4 "|" $5 }'
}
# proposal_with <phase regex> <changes substring>: names, one per line.
proposal_with() {
  proposals | awk -F'|' -v p="$1" -v c="$2" '$2 ~ "^(" p ")$" && index($4, c) > 0 { print $1 }'
}
has_proposal() { [[ -n "$(proposal_with "$1" "$2")" ]]; }
proposal_count() { proposals | wc -l | tr -d ' '; }

phase_is() { [[ "$(dp_field "$1" .status.phase)" == "$2" ]]; }
cleaned_up() { [[ "$(condition "$1" CleanedUp status)" == "True" ]]; }
rejection_reported() {
  k -n "$NS" get events -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' | grep -q ChangeRejected
}
app_synced_at() {
  refresh_app hard
  [[ "$(app_sync_status)" == "Synced" &&
     "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}')" == "$1" ]]
}

dp_field() { k -n "$NS" get driftproposal "$1" -o jsonpath="{$2}" 2>/dev/null; }
condition()  { dp_field "$1" ".status.conditions[?(@.type==\"$2\")].$3"; }

expect_eq() {
  if [[ "$3" == "$2" ]]; then pass "$1 = $2"; else fail "$1: expected '$2', got '$3'"; fi
}
expect_contains() {
  if [[ "$3" == *"$2"* ]]; then pass "$1 contains '$2'"; else fail "$1: expected to contain '$2', got '$3'"; fi
}
expect_nonempty() {
  if [[ -n "$2" ]]; then pass "$1 = $2"; else fail "$1 is empty"; fi
}

# Branches and merge requests this run may have created: every proposal name
# seen in the namespace, remembered here so cleanup survives deleting the namespace.
SEEN_FILE="$WORKDIR/seen"
: >"$SEEN_FILE"
remember_proposals() {
  k -n "$NS" get driftproposals -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null >>"$SEEN_FILE" || true
}

# ---------------------------------------------------------------------------
# Cleanup: leave the project and the cluster as they were.
# ---------------------------------------------------------------------------

# restore_main: put every original file back on main with one commit, if any differ.
restore_main() {
  python3 - "$WORKDIR" "$PID_ENC" <<'PY'
import json, os, subprocess, sys, urllib.parse
work, pid = sys.argv[1], sys.argv[2]
gl = os.path.join(work, "gl.py")
def call(*args):
    return subprocess.run(["python3", gl, *args], capture_output=True, text=True)
actions = []
orig = os.path.join(work, "orig")
for dirpath, _, files in os.walk(orig):
    for name in files:
        full = os.path.join(dirpath, name)
        rel = os.path.relpath(full, orig)
        want = open(full, encoding="utf-8").read()
        r = call("GET", "/projects/%s/repository/files/%s/raw?ref=main" % (pid, urllib.parse.quote(rel, safe="")))
        if r.returncode != 0:
            actions.append({"action": "create", "file_path": rel, "content": want})
        elif r.stdout != want:
            actions.append({"action": "update", "file_path": rel, "content": want})
if not actions:
    print("main already has its original content")
    sys.exit(0)
body = json.dumps({"branch": "main", "commit_message": "test: restore the demo content after the Backflow e2e test",
                   "actions": actions})
r = call("POST", "/projects/%s/repository/commits" % pid, body)
if r.returncode != 0:
    sys.stderr.write("restoring main failed: %s\n" % r.stderr)
    sys.exit(1)
print("restored %d file(s) on main with %s" % (len(actions), json.loads(r.stdout)["short_id"]))
PY
}

cleanup() {
  local status=$?
  set +e
  trap - EXIT

  # Stop the operator first so nothing reopens what is closed below.
  if [[ -n "$MANAGER_PID" ]] && kill -0 "$MANAGER_PID" 2>/dev/null; then
    kill "$MANAGER_PID" 2>/dev/null
    wait "$MANAGER_PID" 2>/dev/null
  fi
  if [[ -n "$PF_PID" ]] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null
    wait "$PF_PID" 2>/dev/null
  fi
  if [[ "$IN_CLUSTER" == "true" ]]; then
    refresh_log
    k -n "$NS" delete bfp "$POLICY" --ignore-not-found >/dev/null 2>&1
  fi

  if [[ -n "$WORKDIR" && -f "$WORKDIR/gl.py" ]]; then
    echo
    echo "Cleaning up gitlab.com/$GITLAB_PROJECT..."
    remember_proposals
    local name branch info iid state
    # Every branch with the Backflow prefix was created by this run: the test
    # refuses to start when there are leftovers.
    local branches
    branches="$( { sort -u "$WORKDIR/seen" | sed 's|^|backflow/|'; \
      gl GET "$(project_path "/repository/branches?search=backflow/&per_page=100")" 2>/dev/null |
        jx '"\n".join(b["name"] for b in d)' 2>/dev/null; } | sort -u | grep '^backflow/' )"
    for branch in $branches; do
      info="$(mr_info "$branch" 2>/dev/null)"
      iid="${info%%|*}"
      state="$(echo "$info" | cut -d'|' -f2)"
      if [[ -n "$iid" && "$state" == "opened" ]]; then
        gl PUT "$(project_path "/merge_requests/$iid")" '{"state_event":"close"}' >/dev/null 2>&1 &&
          echo "  closed merge request !$iid"
      fi
      if branch_exists "$branch"; then
        gl DELETE "$(project_path "/repository/branches/$(enc "$branch")")" >/dev/null 2>&1 &&
          echo "  deleted branch $branch"
      fi
    done
    if [[ "$LABEL_PREEXISTED" == "false" ]]; then
      gl DELETE "$(project_path "/labels/$(enc "$LABEL")")" >/dev/null 2>&1 && echo "  deleted label $LABEL"
    fi
    if [[ -n "$MAIN_ORIG_SHA" ]]; then
      restore_main || echo "  ${RED}WARNING:${NC} main could not be restored; compare it with $MAIN_ORIG_SHA" >&2
    fi
  fi

  if [[ "$RESTORE_CLUSTER" == "true" ]]; then
    echo "Restoring demo resources to their Git state..."
    if restore_demo "$(main_sha 2>/dev/null)"; then
      echo "  $APP is Synced again"
    else
      echo "  ${RED}WARNING:${NC} $APP is not Synced; run: kubectl -n $ARGOCD_NS get application $APP" >&2
    fi
  fi
  if [[ "$KEEP" != "true" ]]; then
    k delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1
  fi
  if [[ -n "$WORKDIR" && "$WORKDIR" == *backflow-mr-test.* ]]; then
    rm -f "$WORKDIR/gl.py" "$WORKDIR/seen" "$WORKDIR/token" "$WORKDIR/unrelated.json" "$WORKDIR/paths"
    find "$WORKDIR/orig" -type f -exec rm -f {} + 2>/dev/null
    find "$WORKDIR/orig" -depth -type d -exec rmdir {} + 2>/dev/null
    rmdir "$WORKDIR" 2>/dev/null
  fi
  exit "$status"
}
trap cleanup EXIT

cd "$ROOT"

# ---------------------------------------------------------------------------
step "Prerequisites"
for cmd in kubectl make curl python3; do
  command -v "$cmd" >/dev/null || die "'$cmd' not found"
done
k cluster-info >/dev/null 2>&1 || die "cluster kind-$CLUSTER_NAME is not reachable. Run ./hack/dev-up.sh first."
k get crd applications.argoproj.io >/dev/null 2>&1 || die "Argo CD is not installed. Run ./hack/dev-up.sh first."
k -n "$ARGOCD_NS" get application "$APP" >/dev/null 2>&1 || die "Application $APP not found. Run ./hack/dev-up.sh first."
k -n "$TOKEN_SECRET_NS" get secret "$TOKEN_SECRET" >/dev/null 2>&1 || die "Secret $TOKEN_SECRET_NS/$TOKEN_SECRET not found. Run ./hack/dev-up.sh first."

if [[ "$IN_CLUSTER" == "true" ]]; then
  k -n "$MANAGER_NS" get deployment "$MANAGER_DEPLOY" >/dev/null 2>&1 || die "Deployment $MANAGER_NS/$MANAGER_DEPLOY not found."
else
  if curl -fs -o /dev/null http://localhost:8081/healthz 2>/dev/null; then
    die "An operator is already running on port 8081 (probably 'make run'). Stop it first."
  fi
  if [[ "$(k -n "$MANAGER_NS" get deployment "$MANAGER_DEPLOY" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" =~ ^[1-9] ]]; then
    die "The operator is deployed in the cluster and would race the local one. Run: kubectl -n $MANAGER_NS scale deploy/$MANAGER_DEPLOY --replicas=0 (or use IN_CLUSTER=true)."
  fi
fi

APP_REPO="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.source.repoURL}')"
[[ "$APP_REPO" == *"gitlab.com/$GITLAB_PROJECT"* ]] || die "$APP tracks $APP_REPO, not gitlab.com/$GITLAB_PROJECT"

step "GitLab project: $GITLAB_PROJECT"
gl GET /user >/dev/null || die "GitLab rejected the token"
LEVEL="$(gl GET "$(project_path "")" | jx 'max(d["permissions"]["project_access"]["access_level"] if d["permissions"].get("project_access") else 0, d["permissions"]["group_access"]["access_level"] if d["permissions"].get("group_access") else 0)')" ||
  die "project $GITLAB_PROJECT is not reachable with this token"
(( LEVEL >= 40 )) || die "the token needs at least Maintainer access to restore main; it has level $LEVEL"
pass "token accepted, access level $LEVEL"

LEFTOVERS="$(gl GET "$(project_path "/repository/branches?search=backflow/&per_page=100")" | jx '" ".join(b["name"] for b in d if b["name"].startswith("backflow/"))')"
[[ -z "$LEFTOVERS" ]] || die "branches from an earlier run exist: $LEFTOVERS. Delete them (and their merge requests) first."
OPEN_MRS="$(gl GET "$(project_path "/merge_requests?state=opened&per_page=100")" | jx 'len(d)')"
[[ "$OPEN_MRS" == "0" ]] || die "$OPEN_MRS merge request(s) are open in $GITLAB_PROJECT; close them first so this test can tell its own apart."
[[ "$(gl_status GET "$(project_path "/labels/$(enc "$LABEL")")")" == "200" ]] || LABEL_PREEXISTED=false

MAIN_ORIG_SHA="$(main_sha)"
# Remember every file of main so cleanup can restore it byte for byte.
gl GET "$(project_path "/repository/tree?recursive=true&per_page=100&ref=main")" |
  jx '"\n".join(t["path"] for t in d if t["type"] == "blob")' >"$WORKDIR/paths"
while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  mkdir -p "$WORKDIR/orig/$(dirname "$path")"
  raw_file "$path" "$MAIN_ORIG_SHA" >"$WORKDIR/orig/$path"
done <"$WORKDIR/paths"
rm -f "$WORKDIR/paths"
ORIG_CONFIGMAP="$WORKDIR/orig/apps/demo/configmap.yaml"
[[ -s "$ORIG_CONFIGMAP" ]] || die "apps/demo/configmap.yaml not found on main"
grep -q 'LOG_LEVEL: info' "$ORIG_CONFIGMAP" || die "the demo ConfigMap does not have LOG_LEVEL: info on main; this test expects the original demo content"
pass "main is at ${MAIN_ORIG_SHA:0:8}; $(find "$WORKDIR/orig" -type f | wc -l | tr -d ' ') file(s) saved for restoring"

# expected_configmap <value>: the original file with only LOG_LEVEL changed.
expected_configmap() { sed "s/LOG_LEVEL: info/LOG_LEVEL: $1/" "$ORIG_CONFIGMAP"; }

# Everything below assumes the demo resources match Git.
ORIG_LOG_LEVEL="$(live_log_level)"
ORIG_REPLICAS="$(k -n "$DEMO_NS" get deployment demo -o jsonpath='{.spec.replicas}')"
RESTORE_CLUSTER=true
if [[ "$(app_sync_status)" != "Synced" ]]; then
  echo "  $APP is not Synced; syncing it first..."
  restore_demo || die "$APP did not become Synced"
  ORIG_LOG_LEVEL="$(live_log_level)"
fi
pass "cluster, Argo CD and $APP found (LOG_LEVEL=$ORIG_LOG_LEVEL)"

# ---------------------------------------------------------------------------
if [[ "$IN_CLUSTER" == "true" ]]; then
  step "Operator (deployed)"
  k -n "$MANAGER_NS" rollout status "deploy/$MANAGER_DEPLOY" --timeout=120s >/dev/null || die "deployment is not available"
else
  step "Argo CD port-forward and local operator"
  PF_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  kubectl --context "kind-$CLUSTER_NAME" -n "$ARGOCD_NS" port-forward svc/argocd-server "$PF_PORT":443 >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    curl -ksf -o /dev/null "https://localhost:$PF_PORT/api/version" && break
    kill -0 "$PF_PID" 2>/dev/null || die "port-forward exited"
    sleep 1
  done
  curl -ksf -o /dev/null "https://localhost:$PF_PORT/api/version" || die "Argo CD is not reachable on localhost:$PF_PORT"
  ARGOCD_URL="https://localhost:$PF_PORT"
  make manifests install >/dev/null
  make build >/dev/null
  mkdir -p "$LOG_DIR"
  "$ROOT/bin/manager" --merge-request-poll-interval="$POLL" >"$LOG_FILE" 2>&1 &
  MANAGER_PID=$!
  for _ in $(seq 1 60); do
    curl -fs -o /dev/null http://localhost:8081/readyz 2>/dev/null && break
    kill -0 "$MANAGER_PID" 2>/dev/null || die "operator exited, log: $LOG_FILE"
    sleep 1
  done
  curl -fs -o /dev/null http://localhost:8081/readyz || die "operator did not become ready, log: $LOG_FILE"
  pass "operator running (merge requests polled every $POLL), Argo CD on $ARGOCD_URL"
fi

# ---------------------------------------------------------------------------
step "Test namespace, ScmConnection and policy: $NS/$POLICY"
k delete namespace "$NS" --ignore-not-found --wait=true >/dev/null
k create namespace "$NS" >/dev/null
ARGO_TOKEN="$(k -n "$TOKEN_SECRET_NS" get secret "$TOKEN_SECRET" -o jsonpath='{.data.token}' | base64 -d)"
k -n "$NS" create secret generic argocd-token --from-literal=token="$ARGO_TOKEN" >/dev/null
ARGO_TOKEN=""
# The GitLab token goes into the Secret through a 0600 file, not an argument.
(umask 077; printf '%s' "$GITLAB_TOKEN" >"$WORKDIR/token")
k -n "$NS" create secret generic gitlab-token --from-file=token="$WORKDIR/token" >/dev/null
rm -f "$WORKDIR/token"

k apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata:
  name: $SCM_NAME
  namespace: $NS
spec:
  provider: gitlab
  url: https://gitlab.com
  tokenSecretRef:
    name: gitlab-token
    key: token
YAML
k apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata:
  name: $POLICY
  namespace: $NS
spec:
  argoCDNamespace: $ARGOCD_NS
  mode: MergeRequest
  applications:
    names: ["$APP"]
  argoCD:
    url: $ARGOCD_URL
    insecureSkipTLSVerify: true
    tokenSecretRef:
      name: argocd-token
      key: token
  mergeRequest:
    branchPrefix: backflow/
    labels: ["$LABEL"]
    assignActor: false
YAML
wait_for "ScmConnection is Ready (token accepted by gitlab.com)" k -n "$NS" wait "scm/$SCM_NAME" --for=condition=Ready --timeout=1s
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
policy_has_connection() { [[ "$(k -n "$NS" get bfp "$POLICY" -o jsonpath='{.status.applications[0].scmConnection}')" == "$SCM_NAME" ]]; }
wait_for "policy matched the repository to the ScmConnection" policy_has_connection

# ---------------------------------------------------------------------------
step "a. A drift becomes a merge request"
set_log_level debug
wait_for "proposal is Proposed (info -> debug)" has_proposal Proposed '/data/LOG_LEVEL:Replace:"info">"debug"'
remember_proposals
P_A="$(proposal_with Proposed '"debug"' | head -n 1)"
if [[ -n "$P_A" ]]; then
  BRANCH_A="backflow/$P_A"
  IFS='|' read -r IID_A STATE_A URL_A _ <<<"$(mr_info "$BRANCH_A")"
  expect_nonempty "merge request from $BRANCH_A" "$IID_A"
  expect_eq "merge request state" "opened" "$STATE_A"
  expect_eq "status.mergeRequest.url" "$URL_A" "$(dp_field "$P_A" .status.mergeRequest.url)"
  expect_eq "status.mergeRequest.number" "$IID_A" "$(dp_field "$P_A" .status.mergeRequest.number)"
  expect_eq "status.mergeRequest.branch" "$BRANCH_A" "$(dp_field "$P_A" .status.mergeRequest.branch)"
  expect_eq "Mapped condition" "True" "$(condition "$P_A" Mapped status)"
  expect_eq "Proposed condition" "True" "$(condition "$P_A" Proposed status)"
  MR_JSON="$(gl GET "$(project_path "/merge_requests/$IID_A")")"
  expect_eq "target branch" "main" "$(echo "$MR_JSON" | jx 'd["target_branch"]')"
  expect_eq "title" "backflow: sync ConfigMap demo/demo-config from cluster" "$(echo "$MR_JSON" | jx 'd["title"]')"
  expect_contains "labels" "$LABEL" "$(echo "$MR_JSON" | jx 'd["labels"]')"
  expect_contains "description (changed path)" "/data/LOG_LEVEL" "$(echo "$MR_JSON" | jx 'd["description"]')"
  expect_contains "description (diff)" "+  LOG_LEVEL: debug" "$(echo "$MR_JSON" | jx 'd["description"]')"
  expect_contains "description (proposal)" "$P_A" "$(echo "$MR_JSON" | jx 'd["description"]')"
  expect_eq "source branch is kept after merging is up to the reviewer" "False" "$(echo "$MR_JSON" | jx 'd["force_remove_source_branch"] or False')"

  if [[ "$(raw_file apps/demo/configmap.yaml "$BRANCH_A")" == "$(expected_configmap debug)" ]]; then
    pass "the file on the branch has LOG_LEVEL: debug and nothing else changed (the comment line is kept)"
  else
    fail "the file on $BRANCH_A differs from the original in more than LOG_LEVEL"
  fi
  expect_contains "comment line on the branch" "# Log verbosity: debug, info, warn, error, trace" "$(raw_file apps/demo/configmap.yaml "$BRANCH_A")"
  expect_eq "main was not touched" "$MAIN_ORIG_SHA" "$(main_sha)"
  COMMIT_JSON="$(gl GET "$(project_path "/repository/commits/$(enc "$BRANCH_A")")")"
  expect_eq "committer" "Backflow" "$(echo "$COMMIT_JSON" | jx 'd["committer_name"]')"
  expect_contains "commit message trailer" "Proposal: $P_A" "$(echo "$COMMIT_JSON" | jx 'd["message"]')"
fi

# ---------------------------------------------------------------------------
step "b. Closing the merge request rejects the proposal"
if [[ -n "${P_A:-}" ]]; then
  gl PUT "$(project_path "/merge_requests/$IID_A")" '{"state_event":"close"}' >/dev/null || fail "could not close !$IID_A"
  wait_for "proposal is Rejected" phase_is "$P_A" Rejected
  expect_eq "status.mergeRequest.state" "closed" "$(dp_field "$P_A" .status.mergeRequest.state)"
  expect_contains "message" "Argo CD can revert" "$(dp_field "$P_A" .status.message)"
  expect_eq "the branch is left for the reviewer" "true" "$(branch_exists "$BRANCH_A" && echo true || echo false)"

  echo "  waiting ${QUIET}s to see that nothing is reopened..."
  sleep "$QUIET"
  expect_eq "proposals for demo-config" "1" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
  refresh_app
  wait_for "the rejection is reported as an Event on the policy" rejection_reported
  expect_eq "ChangeRejected events" "1" "$(k -n "$NS" get events -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' | grep -c ChangeRejected || true)"

  echo "  syncing $APP lets Argo CD revert the cluster..."
  if restore_demo; then pass "$APP is Synced again"; else fail "$APP did not become Synced"; fi
  expect_eq "live LOG_LEVEL after the sync" "$ORIG_LOG_LEVEL" "$(live_log_level)"

  echo "  the same drift again must not be proposed (same changes, same revision)..."
  set_log_level debug
  sleep "$QUIET"
  expect_eq "proposals for demo-config" "1" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
  if branch_exists "$BRANCH_A"; then
    gl DELETE "$(project_path "/repository/branches/$(enc "$BRANCH_A")")" >/dev/null && note "deleted $BRANCH_A"
  fi
fi

# ---------------------------------------------------------------------------
step "c. A different drift is merged"
set_log_level warn
wait_for "proposal is Proposed (info -> warn)" has_proposal Proposed '/data/LOG_LEVEL:Replace:"info">"warn"'
remember_proposals
P_C="$(proposal_with Proposed '"warn"' | head -n 1)"
if [[ -n "$P_C" ]]; then
  BRANCH_C="backflow/$P_C"
  IFS='|' read -r IID_C STATE_C URL_C _ <<<"$(mr_info "$BRANCH_C")"
  expect_nonempty "merge request from $BRANCH_C" "$IID_C"
  expect_eq "merge request state" "opened" "$STATE_C"
  expect_eq "main was not touched" "$MAIN_ORIG_SHA" "$(main_sha)"

  merged=false
  for _ in $(seq 1 20); do
    if gl PUT "$(project_path "/merge_requests/$IID_C/merge")" '{"should_remove_source_branch":false}' >/dev/null 2>&1; then
      merged=true; break
    fi
    sleep 3
  done
  if [[ "$merged" == "true" ]]; then pass "merged !$IID_C through the API"; else fail "could not merge !$IID_C"; fi
  MERGE_SHA="$(gl GET "$(project_path "/merge_requests/$IID_C")" | jx 'd.get("merge_commit_sha") or d.get("squash_commit_sha") or d["sha"]')"

  wait_for "proposal is Merged" phase_is "$P_C" Merged
  expect_eq "status.commitSHA is the merge commit" "$MERGE_SHA" "$(dp_field "$P_C" .status.commitSHA)"
  expect_eq "status.mergeRequest.state" "merged" "$(dp_field "$P_C" .status.mergeRequest.state)"
  expect_eq "main is at the merge commit" "$MERGE_SHA" "$(main_sha)"
  if [[ "$(raw_file apps/demo/configmap.yaml main)" == "$(expected_configmap warn)" ]]; then
    pass "main has LOG_LEVEL: warn"
  else
    fail "the file on main is not the original with LOG_LEVEL: warn"
  fi

  echo "  Argo CD picks up the merged commit..."
  wait_for "$APP is Synced at the merge commit" app_synced_at "$MERGE_SHA"
  expect_eq "live LOG_LEVEL is unchanged" "warn" "$(live_log_level)"
  if sync_app; then pass "one sync at the merge commit (so Argo CD's automated sync will not revert the next drift)"; else fail "$APP did not stay Synced"; fi
  sleep "$QUIET"
  expect_eq "proposal stays Merged" "Merged" "$(dp_field "$P_C" .status.phase)"
  expect_eq "proposals for demo-config (no new one)" "2" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
fi

# ---------------------------------------------------------------------------
step "d. The live value goes back to Git while the merge request is open"
set_log_level debug
wait_for "proposal is Proposed (warn -> debug)" has_proposal Proposed '/data/LOG_LEVEL:Replace:"warn">"debug"'
remember_proposals
P_D="$(proposal_with Proposed '>"debug"' | head -n 1)"
if [[ -n "$P_D" ]]; then
  BRANCH_D="backflow/$P_D"
  IFS='|' read -r IID_D STATE_D _ _ <<<"$(mr_info "$BRANCH_D")"
  expect_eq "merge request state" "opened" "$STATE_D"
  expect_eq "the branch exists" "true" "$(branch_exists "$BRANCH_D" && echo true || echo false)"

  set_log_level warn
  wait_for "proposal is Reverted" phase_is "$P_D" Reverted
  wait_for "cleanup finished (CleanedUp)" cleaned_up "$P_D"
  IFS='|' read -r _ STATE_D2 _ _ <<<"$(mr_info "$BRANCH_D")"
  expect_eq "merge request state" "closed" "$STATE_D2"
  expect_eq "the branch is deleted" "false" "$(branch_exists "$BRANCH_D" && echo true || echo false)"
  NOTES="$(gl GET "$(project_path "/merge_requests/$IID_D/notes?per_page=50")" | jx '" ".join(n["body"] for n in d if not n.get("system"))')"
  expect_contains "closing comment" "Backflow is closing this merge request" "$NOTES"
  expect_contains "closing comment (reason)" "back in sync with Git" "$NOTES"
  expect_eq "status.mergeRequest.state" "closed" "$(dp_field "$P_D" .status.mergeRequest.state)"
fi

# ---------------------------------------------------------------------------
step "e. A new sync resets the cluster while the merge request is open"
set_log_level error
wait_for "proposal is Proposed (warn -> error)" has_proposal Proposed '/data/LOG_LEVEL:Replace:"warn">"error"'
remember_proposals
P_E="$(proposal_with Proposed '>"error"' | head -n 1)"
if [[ -n "$P_E" ]]; then
  BRANCH_E="backflow/$P_E"
  IFS='|' read -r IID_E STATE_E _ _ <<<"$(mr_info "$BRANCH_E")"
  expect_eq "merge request state" "opened" "$STATE_E"

  # An unrelated commit to main: the Application's revision changes, Argo CD's
  # automated sync applies it and resets the drifted ConfigMap to Git.
  python3 - "$WORKDIR" >"$WORKDIR/unrelated.json" <<'PY'
import json, sys
work = sys.argv[1]
readme = open(work + "/orig/README.md", encoding="utf-8").read()
print(json.dumps({"branch": "main", "commit_message": "test: an unrelated change (Backflow e2e)",
                  "actions": [{"action": "update", "file_path": "README.md",
                               "content": readme + "\nAn unrelated line added by the Backflow e2e test.\n"}]}))
PY
  UNRELATED_SHA="$(gl POST "$(project_path "/repository/commits")" "$(cat "$WORKDIR/unrelated.json")" | jx 'd["id"]')"
  rm -f "$WORKDIR/unrelated.json"
  expect_nonempty "unrelated commit on main" "$UNRELATED_SHA"

  wait_for "$APP is Synced at the unrelated commit" app_synced_at "$UNRELATED_SHA"
  expect_eq "Argo CD reset the live LOG_LEVEL to Git" "warn" "$(live_log_level)"
  live_reverted() { [[ "$(condition "$P_E" LiveReverted status)" == "True" ]]; }
  wait_for "LiveReverted=True on the proposal" live_reverted
  expect_eq "LiveReverted reason" "SyncedNewRevision" "$(condition "$P_E" LiveReverted reason)"
  expect_eq "the proposal stays Proposed" "Proposed" "$(dp_field "$P_E" .status.phase)"
  IFS='|' read -r _ STATE_E2 _ _ <<<"$(mr_info "$BRANCH_E")"
  expect_eq "the merge request stays open" "opened" "$STATE_E2"
  expect_eq "the branch is kept" "true" "$(branch_exists "$BRANCH_E" && echo true || echo false)"
  notes_e() { gl GET "$(project_path "/merge_requests/$IID_E/notes?per_page=50")" | jx '"\n".join(n["body"] for n in d if not n.get("system"))'; }
  NOTES_E="$(notes_e)"
  expect_contains "comment" "The cluster was reset to Git by an Argo CD sync of $UNRELATED_SHA." "$NOTES_E"
  expect_contains "comment" "Merging this merge request makes the change permanent again." "$NOTES_E"
  sleep "$QUIET"
  expect_eq "the comment is posted once" "1" "$(notes_e | grep -c 'reset to Git by an Argo CD sync' || true)"
  expect_eq "the proposal is still Proposed" "Proposed" "$(dp_field "$P_E" .status.phase)"
  expect_eq "the merge request is still open" "opened" "$(mr_info "$BRANCH_E" | cut -d'|' -f2)"

  merged=false
  for _ in $(seq 1 20); do
    if gl PUT "$(project_path "/merge_requests/$IID_E/merge")" '{"should_remove_source_branch":false}' >/dev/null 2>&1; then
      merged=true; break
    fi
    sleep 3
  done
  if [[ "$merged" == "true" ]]; then pass "merged !$IID_E through the API"; else fail "could not merge !$IID_E"; fi
  MERGE_E="$(gl GET "$(project_path "/merge_requests/$IID_E")" | jx 'd.get("merge_commit_sha") or d["sha"]')"
  wait_for "proposal is Merged" phase_is "$P_E" Merged
  expect_eq "status.commitSHA is the merge commit" "$MERGE_E" "$(dp_field "$P_E" .status.commitSHA)"
  wait_for "$APP is Synced at the merge commit" app_synced_at "$MERGE_E"
  level_is_error() { [[ "$(live_log_level)" == "error" ]]; }
  wait_for "the change is back in the cluster after the sync" level_is_error
  sleep "$QUIET"
  expect_eq "proposals for demo-config (no new one)" "4" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
fi

# ---------------------------------------------------------------------------
step "Secrets"
refresh_log
if grep -qF -- "$GITLAB_TOKEN" "$LOG_FILE" 2>/dev/null; then
  fail "the GitLab token appears in the operator log"
else
  pass "the GitLab token does not appear in the operator log"
fi
LEAK=false
for p in "$P_A" "$P_C" "$P_D" "$P_E"; do
  [[ -n "$p" ]] || continue
  if k -n "$NS" get driftproposal "$p" -o yaml | grep -qF -- "$GITLAB_TOKEN"; then LEAK=true; fi
done
if [[ "$LEAK" == "true" ]]; then fail "the GitLab token appears in a DriftProposal"; else pass "the GitLab token does not appear in any DriftProposal"; fi

# ---------------------------------------------------------------------------
echo
echo "${BOLD}Summary:${NC} ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}"
if [[ "$KEEP" == "true" ]]; then
  echo "Test namespace '$NS' kept: kubectl -n $NS get bfp,scm,driftproposals"
fi
if (( FAIL > 0 )); then
  echo "Operator log: ${LOG_FILE#"$ROOT"/}"
  exit 1
fi
