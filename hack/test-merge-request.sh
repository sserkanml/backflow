#!/usr/bin/env bash
#
# Backflow - merge request end-to-end test
#
# Runs the full path against the real demo repository on gitlab.com (SCM=github: github.com):
#   a. A LOG_LEVEL drift becomes a merge request from backflow/<proposal>-<uid8> into
#      main. The file on the branch has the live value and keeps its comment
#      line; the proposal is Proposed with the merge request URL; main is not
#      touched.
#   b. Closing the merge request makes the proposal Rejected. The same drift is
#      not proposed again (nor a new merge request opened), the rejection is
#      reported once as an Event, and Argo CD can then revert the cluster.
#   c. A different drift opens a merge request; merging it through the API makes
#      the proposal Merged with the merge commit. Right after the merge, while
#      Argo CD has not seen the commit yet, no second proposal appears for the
#      same change. After Argo CD syncs the new commit the resource is Synced and
#      no new proposal appears.
#   d. A drift whose merge request is open and whose live value then goes back to
#      Git (same revision) makes the proposal Reverted; its merge request is
#      closed with a comment and its branch deleted.
#   e. A drift whose merge request is open, then an unrelated commit to main:
#      Argo CD syncs the new revision and resets the cluster. The change is not
#      lost: the proposal stays Proposed, the merge request stays open with one
#      comment saying so, and merging it brings the change back to the cluster.
#
#   f. With automated sync switched off, the demo is behind the commits to main.
#      A commit outside apps/demo does not matter: a real drift is still proposed.
#      A commit under apps/demo puts the demo ahead of its last sync: the changed
#      ConfigMap is OutOfSync, but no proposal is made for it (it would undo the
#      commit) and one Event says why. After a sync the detection works again.
#      Automated sync is switched back on.
#
#   g. A policy in ReportOnly makes a proposal that waits in Mapping
#      (Proposed=False/ReportOnly, no merge request). Switching the policy to
#      MergeRequest opens the merge request without any other change; the
#      proposal is then retired and cleaned up as usual.
#
#   h. A JSON manifest: the script adds apps/demo/json-config.json (the ConfigMap
#      demo-json) to main through the API. A drift of one value becomes a merge
#      request whose branch differs from main in that one line only: the rest of the
#      file, its indentation and its final newline are byte for byte the same, and
#      the diff in the description changes only that line. Closing the merge request
#      rejects the proposal; the file is deleted from main again (restored on exit).
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
# minutes by default; raise WAIT accordingly). The local operator serves its
# metrics over plain HTTP on a free local port (--metrics-secure=false), and
# sections a and b check the Backflow counters and gauges there; with IN_CLUSTER
# those checks are skipped.
#
# Usage (from the repository root):
#   GITLAB_TOKEN=... ./hack/test-merge-request.sh
#   SCM=github GITHUB_TOKEN=... ./hack/test-merge-request.sh   # against the GitHub demo repository (see the README)
#   SECTIONS=c,e,f ./hack/test-merge-request.sh   # only those sections (default: all);
#       b needs a; d and e need c; f needs c and e (the script refuses otherwise);
#       g and h need nothing (SECTIONS=h is the JSON manifest alone)
#   KEEP=true ./hack/test-merge-request.sh      # keep the test namespace

set -euo pipefail

# Which Git host: SCM=gitlab (default) or SCM=github, DEMO_REPO, GITLAB_TOKEN / GITHUB_TOKEN.
# shellcheck source=lib/scm.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/scm.sh"

CLUSTER_NAME="${CLUSTER_NAME:-$SCM_CLUSTER}"
NS="${NS:-backflow-mr-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
APP="${APP:-demo-app}"
DEMO_NS="${DEMO_NS:-demo}"
POLICY="mr-policy"
SCM_NAME="$SCM"
LABEL="backflow-e2e"
TOKEN_SECRET_NS="${TOKEN_SECRET_NS:-backflow-system}"
TOKEN_SECRET="${TOKEN_SECRET:-argocd-token}"
KEEP="${KEEP:-false}"
IN_CLUSTER="${IN_CLUSTER:-false}"
MANAGER_NS="${MANAGER_NS:-backflow-system}"
MANAGER_DEPLOY="${MANAGER_DEPLOY:-backflow-controller-manager}"
ARGOCD_URL="${ARGOCD_URL:-https://argocd-server.argocd.svc}"
# How often the local operator polls merge requests.
POLL="${POLL:-3s}"
# A drift becomes a proposal after it has been stable for the policy's
# batchWindow across two observations (the policy has no minimum, 0s is valid).
# Every wait for "nothing happens" is derived from it (QUIET below).
WINDOW="${WINDOW:-10s}"
WINDOW_S="${WINDOW%s}"
if ! [[ "$WINDOW_S" =~ ^[0-9]+$ ]]; then
  echo "WINDOW must be a number of seconds like 10s, got '$WINDOW'" >&2
  exit 1
fi
# Seconds to wait for Argo CD, the operator and the Git host to react.
if [[ "$IN_CLUSTER" == "true" ]]; then WAIT="${WAIT:-360}"; else WAIT="${WAIT:-150}"; fi
# Seconds to wait before asserting that nothing happened: long enough for the
# two observations a proposal needs, plus a margin.
QUIET="${QUIET:-$((2 * WINDOW_S + 5))}"

# SECTIONS=a,j runs only those sections (letters, comma separated) after the
# prerequisites; default all. Sections build on each other: b closes the merge request of a, d and e start from the Git value c merged, f from the value e merged; such a section refuses to run without them.
SECTIONS="${SECTIONS:-}"
section_wanted() { [[ -z "$SECTIONS" || ",$SECTIONS," == *",$1,"* ]]; }
section_needs() {
  case "$1" in
    b) echo "a" ;;
    d) echo "c" ;;
    e) echo "c" ;;
    f) echo "c e" ;;
    *) echo "" ;;
  esac
}
check_sections() {
  local s n
  for s in ${SECTIONS//,/ }; do
    # shellcheck disable=SC2194 # a membership test: the constant list is the subject of the case
    case " a b c d e f g h " in
      *" $s "*) ;;
      *) echo "SECTIONS: unknown section '$s' (valid: a,b,c,d,e,f,g,h)" >&2; exit 1 ;;
    esac
    for n in $(section_needs "$s"); do
      section_wanted "$n" || { echo "SECTIONS: section $s needs section $n, before it (see the notes at SECTIONS); add it to the list, for example SECTIONS=$n,$s, or leave SECTIONS out" >&2; exit 1; }
    done
  done
}
check_sections

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT/tmp"
LOG_FILE="$LOG_DIR/test-merge-request-manager.log"

if [[ -z "${SCM_TOKEN:-}" ]]; then
  echo "SKIPPED: $SCM_TOKEN_VAR is not set."
  echo "This test pushes branches and merges into $SCM_PROJECT on $SCM_HOST and needs"
  echo "$SCM_TOKEN_SCOPES: $SCM_TOKEN_VAR=... $0"
  exit 0
fi

MANAGER_PID=""
PF_PID=""
WORKDIR=""
P_A=""
P_C=""
P_D=""
P_E=""
P_F=""
P_F1=""
RESTORE_CLUSTER=false
LABEL_PREEXISTED=true
AUTOMATED_ORIG=""
AUTOSYNC_DISABLED=false
MAIN_ORIG_SHA=""
ORIG_COUNT=""
# Files this test creates on main; restore_main may delete these and nothing else.
TEST_ADDED_FILES=("apps/demo/json-config.json")
ORIG_LOG_LEVEL=""
ORIG_REPLICAS=""
PASS=0
FAIL=0

if [[ -t 1 ]]; then
  GREEN=$'\e[32m'; RED=$'\e[31m'; BOLD=$'\e[1m'; NC=$'\e[0m'
else
  GREEN=""; RED=""; BOLD=""; NC=""
fi

# Seconds per section (bash 3.2: indexed arrays only), for the summary.
TIMING_NAMES=()
TIMING_SECS=()
SECTION_NAME=""
SECTION_START=$SECONDS
SCRIPT_START=$SECONDS
end_section() {
  if [[ -n "$SECTION_NAME" ]]; then
    TIMING_NAMES+=("$SECTION_NAME")
    TIMING_SECS+=("$((SECONDS - SECTION_START))")
  fi
  SECTION_NAME=""
}
begin_section() { end_section; SECTION_NAME="$1"; SECTION_START=$SECONDS; }
print_timing() {
  local i n
  end_section
  echo
  echo "${BOLD}Time per section:${NC}"
  n="${#TIMING_NAMES[@]}"
  for ((i = 0; i < n; i++)); do
    printf '  %5ss  %s\n' "${TIMING_SECS[$i]}" "$(printf '%s' "${TIMING_NAMES[$i]}" | cut -c1-72)"
  done
  printf '  %5ss  %s\n' "$((SECONDS - SCRIPT_START))" "total"
}
step() { begin_section "$*"; echo; echo "${BOLD}▶ $*${NC}"; }
pass() { echo "  ${GREEN}✔${NC} $*"; PASS=$((PASS + 1)); }
fail() { echo "  ${RED}✘${NC} $*"; FAIL=$((FAIL + 1)); }
die()  { echo "${RED}ERROR:${NC} $*" >&2; exit 1; }
note() { echo "    $*"; }

k() { kubectl --context "kind-$CLUSTER_NAME" "$@"; }

# ---------------------------------------------------------------------------
# Git host API (hack/scm.py: GitLab or GitHub). The token travels in the environment only: never in an argument
# list, a log or a file other than the 0600 Secret input.
# ---------------------------------------------------------------------------

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/backflow-mr-test.XXXXXX")"
chmod 700 "$WORKDIR"
mkdir -p "$WORKDIR/orig"

cp "$SCM_HACK_DIR/scm.py" "$WORKDIR/scm.py"

PID_ENC="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$SCM_PROJECT")"
enc() { python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }
gl() { python3 "$WORKDIR/scm.py" "$@"; }
# gl_status <METHOD> <path>: HTTP status code only.
gl_status() { python3 "$WORKDIR/scm.py" "STATUS-$1" "$2"; }
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
# git_level: the LOG_LEVEL that apps/demo/configmap.yaml has on main now.
git_level() { raw_file apps/demo/configmap.yaml main | sed -n 's/^ *LOG_LEVEL: *\([A-Za-z0-9_-]*\).*/\1/p' | head -n 1; }

# require_drift <section> <value>: the value a section drifts to must differ from
# the Git value and from the live value, or there is no drift to see.
require_drift() {
  local git live
  git="$(git_level)"
  live="$(live_log_level)"
  if [[ -z "$2" || "$2" == "$git" || "$2" == "$live" ]]; then
    die "section $1: the drift value '$2' equals the Git value '$git' or the live value '$live', so there would be no drift to see. Is Argo CD still syncing, or is main not in the state this test expects?"
  fi
}

# app_field <jsonpath>: a field of the Application.
app_field() { k -n "$ARGOCD_NS" get application "$APP" -o jsonpath="$1" 2>/dev/null; }

# app_settled_at <sha>: Argo CD compares against the commit, no operation is
# requested or running, and it has compared again since the last one finished
# (reconciledAt >= finishedAt; UTC RFC 3339 times compare as text).
app_settled_at() {
  local phase finished reconciled
  refresh_app
  [[ "$(app_field '{.status.sync.revision}')" == "$1" ]] || return 1
  [[ -z "$(app_field '{.operation}')" ]] || return 1
  phase="$(app_field '{.status.operationState.phase}')"
  [[ "$phase" != "Running" && "$phase" != "Terminating" ]] || return 1
  finished="$(app_field '{.status.operationState.finishedAt}')"
  reconciled="$(app_field '{.status.reconciledAt}')"
  [[ -z "$finished" ]] || { [[ -n "$reconciled" && ! "$reconciled" < "$finished" ]]; }
}

# wait_settled <sha>: wait up to WAIT seconds for app_settled_at.
wait_settled() {
  local waited=0
  while ! app_settled_at "$1"; do
    waited=$((waited + 2))
    (( waited > WAIT )) && return 1
    sleep 2
  done
}

# app_state: what Argo CD says, for a message.
app_state() {
  echo "revision=$(app_field '{.status.sync.revision}') sync=$(app_sync_status) phase=$(app_field '{.status.operationState.phase}') operation=$(app_field '{.operation}') finishedAt=$(app_field '{.status.operationState.finishedAt}') reconciledAt=$(app_field '{.status.reconciledAt}')"
}
set_log_level() {
  k -n "$DEMO_NS" patch configmap demo-config --type merge -p "{\"data\":{\"LOG_LEVEL\":\"$1\"}}" >/dev/null
  refresh_app
}

# app_idle_at <sha>: the Application is Synced at the commit
# and has no operation requested or running: the state needed before a
# commit that the Application is expected to be "ahead" of. A pending sync would
# pick the new commit up.
app_idle_at() {
  local phase
  refresh_app
  [[ "$(app_sync_status)" == "Synced" ]] || return 1
  [[ "$(app_field '{.status.sync.revision}')" == "$1" ]] || return 1
  [[ -z "$(app_field '{.operation}')" ]] || return 1
  phase="$(app_field '{.status.operationState.phase}')"
  [[ "$phase" != "Running" && "$phase" != "Terminating" ]]
}
# wait_idle_at <sha>: wait up to WAIT seconds for app_idle_at.
wait_idle_at() {
  local waited=0
  while ! app_idle_at "$1"; do
    waited=$((waited + 2))
    (( waited > WAIT )) && return 1
    sleep 2
  done
}

# start_sync [sha]: request one sync of the Application at a commit: sha, else the
# head of main now, never an implicit revision (that is resolved when Argo CD
# starts the operation, and a commit made in between would be synced instead).
# A sync that is already requested or running is waited for first.
start_sync() {
  local sha="${1:-}" i
  [[ -n "$sha" ]] || sha="$(main_sha 2>/dev/null || true)"
  [[ -n "$sha" ]] || sha="$(app_field '{.status.sync.revision}')"
  [[ -n "$sha" ]] || return 1
  for i in $(seq 1 30); do
    [[ -z "$(app_field '{.operation}')" && "$(app_field '{.status.operationState.phase}')" != "Running" ]] && break
    sleep 2
  done
  k -n "$ARGOCD_NS" patch application "$APP" --type merge \
    -p "{\"operation\":{\"initiatedBy\":{\"username\":\"backflow-test\"},\"sync\":{\"revision\":\"$sha\"}}}" >/dev/null 2>&1 || true
  SYNC_PINNED="$sha"
}

# sync_app [sha]: run one sync of the Application at a commit (see start_sync) and
# wait until it is Synced at it. The demo Application has automated sync (without
# selfHeal), which applies each new Git commit once, the first time the
# Application is OutOfSync at it. After scenario c the merge commit has not been
# applied by a sync operation yet, so the next drift would be reset by that
# automated sync straight away. Spending it here keeps the drifts of scenarios d
# and e visible until the test itself resets them (d) or pushes a new commit (e).
sync_app() {
  start_sync "${1:-}" || return 1
  local i
  for i in $(seq 1 45); do
    refresh_app
    [[ "$(app_sync_status)" == "Synced" && -z "$(app_field '{.operation}')" &&
       "$(app_field '{.status.operationState.syncResult.revision}')" == "$SYNC_PINNED" ]] && return 0
    sleep 2
  done
  return 1
}

# restore_demo [revision]: put the live demo back to what Git says, via Argo CD.
# With a revision, first wait until Argo CD has seen that commit, so the sync
# does not apply a stale one.
restore_demo() {
  local want="${1:-}"
  if [[ -n "$want" ]]; then
    for _ in $(seq 1 60); do
      refresh_app hard
      [[ "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}' 2>/dev/null)" == "$want" ]] && break
      sleep 2
    done
  fi
  k -n "$DEMO_NS" patch configmap demo-config --type merge \
    -p "{\"data\":{\"LOG_LEVEL\":\"$ORIG_LOG_LEVEL\"}}" >/dev/null 2>&1 || true
  k -n "$DEMO_NS" scale deployment demo --replicas="$ORIG_REPLICAS" >/dev/null 2>&1 || true
  # Argo CD does not prune: the ConfigMap of section h stays after its file is gone.
  k -n "$DEMO_NS" delete configmap demo-json --ignore-not-found >/dev/null 2>&1 || true
  start_sync "$want" || return 1
  local i
  for i in $(seq 1 60); do
    refresh_app hard
    [[ "$(app_sync_status)" == "Synced" && -z "$(app_field '{.operation}')" ]] && return 0
    sleep 2
  done
  return 1
}

# Automated sync of the Application is switched off for scenario f and must
# come back whatever happens.
disable_autosync() {
  k -n "$ARGOCD_NS" patch application "$APP" --type json \
    -p '[{"op":"remove","path":"/spec/syncPolicy/automated"}]' >/dev/null 2>&1 && AUTOSYNC_DISABLED=true
}
restore_autosync() {
  [[ "$AUTOSYNC_DISABLED" == "true" ]] || return 0
  k -n "$ARGOCD_NS" patch application "$APP" --type merge \
    -p '{"spec":{"syncPolicy":{"automated":{}}}}' >/dev/null 2>&1 && AUTOSYNC_DISABLED=false
}
autosync_enabled() { [[ -n "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.syncPolicy.automated}' 2>/dev/null)" ]]; }
# app_ahead_at <sha>: OutOfSync, comparing against sha, which has not been synced.
app_ahead_at() {
  refresh_app hard
  [[ "$(app_sync_status)" == "OutOfSync" &&
     "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}')" == "$1" &&
     "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.operationState.syncResult.revision}')" != "$1" ]]
}
git_ahead_events() {
  k -n "$NS" get events -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}' 2>/dev/null | grep -c GitAhead || true
}
# git_ahead_notes: the text of every GitAhead Event in the test namespace.
git_ahead_notes() {
  k -n "$NS" get events -o json 2>/dev/null | python3 -c '
import sys, json
for e in json.load(sys.stdin)["items"]:
    if e.get("reason") == "GitAhead":
        print(e.get("note") or e.get("message") or "")'
}
events_exceed() { [[ "$(git_ahead_events)" -gt "$1" ]]; }
# app_compared_at <sha>: Argo CD compares against sha (whether or not it is synced).
app_compared_at() {
  refresh_app hard
  [[ "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}')" == "$1" ]]
}

# proposals: one line per DriftProposal of the policy for the resource named in
# PROPOSALS_OF (demo-config unless a section looks at another ConfigMap):
#   <name>|<phase>|<supersededBy>|<path>:<op>:<desired>><live>;...
proposals() {
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" -o jsonpath='{range .items[*]}{.metadata.name}|{.status.phase}|{.spec.resource.name}|{.status.supersededBy}|{range .spec.changes[*]}{.path}:{.op}:{.desired}>{.live};{end}{"\n"}{end}' 2>/dev/null |
    awk -F'|' -v r="${PROPOSALS_OF:-demo-config}" '$3 == r { print $1 "|" $2 "|" $4 "|" $5 }'
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

# branch_of <proposal>: the branch Backflow pushes for a proposal, backflow/<name>-<first 8 of its UID>.
# What it read is kept in $WORKDIR/branches, together with the branch the proposal
# recorded, and printed when the test fails.
branch_of() {
  local uid branch
  uid="$(dp_field "$1" .metadata.uid | cut -c1-8)"
  branch="$(printf 'backflow/%s-%s' "$1" "$uid")"
  printf '%s: uid8=%s -> %s (status.branch=%s)\n' "$1" "${uid:-<empty>}" "$branch" \
    "$(dp_field "$1" .status.branch)" >>"$WORKDIR/branches"
  printf '%s' "$branch"
}

# expect_cleaned_and_closed <proposal>: a retired proposal is cleaned up
# (CleanedUp=True) and its merge request is closed.
expect_cleaned_and_closed() {
  local state
  wait_for "cleanup finished (CleanedUp)" cleaned_up "$1"
  IFS='|' read -r _ state _ _ <<<"$(mr_info "$(branch_of "$1")")"
  expect_eq "merge request state" "closed" "$state"
}
condition()  { dp_field "$1" ".status.conditions[?(@.type==\"$2\")].$3"; }

expect_eq() {
  if [[ "$3" == "$2" ]]; then pass "$1 = $2"; else fail "$1: expected '$2', got '$3'"; fi
}
expect_contains() {
  if [[ "$3" == *"$2"* ]]; then pass "$1 contains '$2'"; else fail "$1: expected to contain '$2', got '$3'"; fi
}
expect_not_contains() {
  if [[ "$3" != *"$2"* ]]; then pass "$1 does not contain '$2'"; else fail "$1: expected not to contain '$2'"; fi
}
expect_nonempty() {
  if [[ -n "$2" ]]; then pass "$1 = $2"; else fail "$1 is empty"; fi
}

# The metrics of the local operator. METRICS_URL is empty for the deployed
# operator, which serves them over HTTPS behind Kubernetes authentication.
METRICS_URL="${METRICS_URL:-}"
# metric_sum NAME TEXT: the sum of the series called NAME whose labels contain
# TEXT, for example: metric_sum backflow_merge_requests_total 'action="opened"'.
metric_sum() {
  curl -fs "$METRICS_URL" | awk -v n="$1" -v t="${2:-}" '
    index($0, n "{") == 1 && index($0, t) > 0 { s += $NF }
    END { printf "%d\n", s }'
}
# expect_metric_at_least DESC NAME TEXT MIN
expect_metric_at_least() {
  local got
  [[ -n "$METRICS_URL" ]] || return 0
  got="$(metric_sum "$2" "$3")"
  if (( got >= $4 )); then pass "metric $1 = $got"; else fail "metric $1: expected at least $4, got $got"; fi
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
  python3 - "$WORKDIR" "$PID_ENC" "$ORIG_COUNT" "${TEST_ADDED_FILES[@]}" <<'PY'
import json, os, subprocess, sys, urllib.parse
work, pid = sys.argv[1], sys.argv[2]
# The copy must be the complete one taken at the start, or main is left alone.
if len(sys.argv) < 4 or not sys.argv[3].isdigit():
    sys.stderr.write("no complete copy of main; not touching it\n")
    sys.exit(1)
count = sum(len(files) for _, _, files in os.walk(os.path.join(sys.argv[1], "orig")))
if count == 0 or count != int(sys.argv[3]):
    sys.stderr.write("the copy of main has %d file(s), expected %s; not touching main\n" % (count, sys.argv[3]))
    sys.exit(1)
gl = os.path.join(work, "scm.py")
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
# Files the test itself added to main are removed again. Only the paths the
# test creates (argv[4:]) can be deleted, and only if they are not part of the
# original copy: a restore never deletes anything else.
added = set(sys.argv[4:])
tree = call("GET", "/projects/%s/repository/tree?recursive=true&per_page=100&ref=main" % pid)
if tree.returncode == 0:
    for t in json.loads(tree.stdout):
        if t["type"] == "blob" and t["path"] in added and not os.path.exists(os.path.join(orig, t["path"])):
            actions.append({"action": "delete", "file_path": t["path"]})
if sum(1 for a in actions if a["action"] == "delete") > len(added):
    sys.stderr.write("refusing to delete more files than the test adds\n")
    sys.exit(1)
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

  if [[ -n "$WORKDIR" && -f "$WORKDIR/scm.py" ]]; then
    echo
    echo "Cleaning up $SCM_HOST/$SCM_PROJECT..."
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

  if [[ "$AUTOSYNC_DISABLED" == "true" ]]; then
    if restore_autosync; then echo "  automated sync of $APP is on again"; else
      echo "  ${RED}WARNING:${NC} could not switch automated sync of $APP back on: kubectl -n $ARGOCD_NS patch application $APP --type merge -p '{\"spec\":{\"syncPolicy\":{\"automated\":{}}}}'" >&2
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
    rm -f "$WORKDIR/scm.py" "$WORKDIR/seen" "$WORKDIR/token" "$WORKDIR/unrelated.json" "$WORKDIR/paths"
    rm -rf "$WORKDIR/orig.part"
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

AUTOMATED_ORIG="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.syncPolicy.automated}')"
APP_REPO="$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.spec.source.repoURL}')"
[[ "$APP_REPO" == *"$SCM_HOST/$SCM_PROJECT"* ]] || die "$APP tracks $APP_REPO, not $SCM_HOST/$SCM_PROJECT"

step "$SCM_LABEL project: $SCM_PROJECT"
gl GET /user >/dev/null || die "$SCM_LABEL rejected the token"
LEVEL="$(gl GET "$(project_path "")" | jx 'max(d["permissions"]["project_access"]["access_level"] if d["permissions"].get("project_access") else 0, d["permissions"]["group_access"]["access_level"] if d["permissions"].get("group_access") else 0)')" ||
  die "project $SCM_PROJECT is not reachable with this token"
(( LEVEL >= SCM_MIN_LEVEL )) || die "the token needs write access to $SCM_PROJECT to restore main (access level $SCM_MIN_LEVEL, GitLab's numbers); it has level $LEVEL"
pass "token accepted, access level $LEVEL"

# leftover_branches: the Backflow branches that still exist. The branch list of the host
# lags behind a deletion, so every listed branch is looked up by name too.
leftover_branches() {
  local listed name out=""
  listed="$(gl GET "$(project_path "/repository/branches?search=backflow/&per_page=100")" 2>/dev/null |
    jx '" ".join(b["name"] for b in d if b["name"].startswith("backflow/"))' 2>/dev/null)"
  for name in $listed; do
    if branch_exists "$name"; then out="$out $name"; fi
  done
  echo "${out# }"
}
# The previous run's cleanup may have deleted its branches a moment ago: look again
# for up to 30s before treating a branch as a leftover.
LEFTOVERS="$(leftover_branches)"
for _ in 1 2 3 4 5 6; do
  [[ -n "$LEFTOVERS" ]] || break
  sleep 5
  LEFTOVERS="$(leftover_branches)"
done
[[ -z "$LEFTOVERS" ]] || die "branches from an earlier run exist: $LEFTOVERS. Delete them (and their merge requests) first."
OPEN_MRS="$(gl GET "$(project_path "/merge_requests?state=opened&per_page=100")" | jx 'len(d)')"
[[ "$OPEN_MRS" == "0" ]] || die "$OPEN_MRS merge request(s) are open in $SCM_PROJECT; close them first so this test can tell its own apart."
[[ "$(gl_status GET "$(project_path "/labels/$(enc "$LABEL")")")" == "200" ]] || LABEL_PREEXISTED=false

SNAPSHOT_SHA="$(main_sha)"
# Remember every file of main so cleanup can restore it byte for byte. The copy
# is built in orig.part and only counts once it is complete: MAIN_ORIG_SHA, which
# enables restore_main, is set after the last file. An interrupted run (Ctrl-C,
# a failing call) therefore never restores main from a partial copy.
gl GET "$(project_path "/repository/tree?recursive=true&per_page=100&ref=main")" |
  jx '"\n".join(t["path"] for t in d if t["type"] == "blob")' >"$WORKDIR/paths"
[[ -s "$WORKDIR/paths" ]] || die "could not list the files of main"
[[ "$(wc -l <"$WORKDIR/paths" | tr -d ' ')" -lt 99 ]] || die "main has too many files for one page of the tree API; this test saves only the first page"
rm -rf "$WORKDIR/orig.part"
while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  mkdir -p "$WORKDIR/orig.part/$(dirname "$path")"
  raw_file "$path" "$SNAPSHOT_SHA" >"$WORKDIR/orig.part/$path.tmp" || die "could not read $path from main"
  mv "$WORKDIR/orig.part/$path.tmp" "$WORKDIR/orig.part/$path"
done <"$WORKDIR/paths"
ORIG_COUNT="$(grep -c . "$WORKDIR/paths")"
[[ "$(find "$WORKDIR/orig.part" -type f | wc -l | tr -d ' ')" == "$ORIG_COUNT" ]] || die "the copy of main is incomplete"
rm -f "$WORKDIR/paths"
rm -rf "$WORKDIR/orig"
mv "$WORKDIR/orig.part" "$WORKDIR/orig"
MAIN_ORIG_SHA="$SNAPSHOT_SHA"
ORIG_CONFIGMAP="$WORKDIR/orig/apps/demo/configmap.yaml"
[[ -s "$ORIG_CONFIGMAP" ]] || die "apps/demo/configmap.yaml not found on main"
grep -q 'LOG_LEVEL: ' "$ORIG_CONFIGMAP" || die "the demo ConfigMap has no LOG_LEVEL on main; this test expects the original demo content"
pass "main is at ${MAIN_ORIG_SHA:0:8}; $(find "$WORKDIR/orig" -type f | wc -l | tr -d ' ') file(s) saved for restoring"

# expected_configmap <value>: the original file with only LOG_LEVEL changed.
# GIT_LEVEL, the value on main, is read below.
expected_configmap() { sed "s/LOG_LEVEL: $GIT_LEVEL/LOG_LEVEL: $1/" "$ORIG_CONFIGMAP"; }

# Start from a settled Argo CD. The demo Application syncs a new commit of main
# by itself: a test that begins while that sync runs would see its first edit
# overwritten, and a drift that comes and goes is (rightly) never proposed.
echo "  waiting for Argo CD to compare against main (${MAIN_ORIG_SHA:0:8}) and to be idle..."
refresh_app hard
wait_settled "$MAIN_ORIG_SHA" ||
  die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} within ${WAIT}s: $(app_state)"
[[ "$(main_sha)" == "$MAIN_ORIG_SHA" ]] || die "main moved while this test was starting; run it again"
GIT_LEVEL="$(sed -n 's/^ *LOG_LEVEL: *\([A-Za-z0-9_-]*\).*/\1/p' "$ORIG_CONFIGMAP" | head -n 1)"
[[ -n "$GIT_LEVEL" ]] || die "could not read LOG_LEVEL from apps/demo/configmap.yaml on main"

# Everything below assumes the demo resources match Git.
ORIG_LOG_LEVEL="$GIT_LEVEL"
ORIG_REPLICAS="$(k -n "$DEMO_NS" get deployment demo -o jsonpath='{.spec.replicas}')"
RESTORE_CLUSTER=true
if [[ "$(app_sync_status)" != "Synced" || "$(live_log_level)" != "$GIT_LEVEL" ]]; then
  echo "  $APP is not Synced at main, or the live LOG_LEVEL ($(live_log_level)) differs from Git ($GIT_LEVEL); syncing it first..."
  restore_demo || die "$APP did not become Synced: $(app_state)"
  wait_settled "$MAIN_ORIG_SHA" ||
    die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} after the sync within ${WAIT}s: $(app_state)"
fi
# Settled at main is not enough. Argo CD's automated sync applies a commit once,
# the first time the Application is OutOfSync at it (see sync_app), unless a sync
# of that commit already happened. If the last sync was of an older commit (main
# moved by a run of another test or by a restore commit, and the Application only
# compared against it), the first drift of this test is reset by that automated
# sync within a second and never proposed: the operator log then shows the
# Application "ahead of the last sync" and the resource OutOfSync with nothing
# that differs. One sync of main now spends that.
SYNCED_AT="$(app_field '{.status.operationState.syncResult.revision}')"
if [[ "$SYNCED_AT" != "$MAIN_ORIG_SHA" ]]; then
  echo "  the last sync of $APP was of ${SYNCED_AT:0:8}, not of main; syncing it once so that its automated sync does not reset the first drift..."
  sync_app || die "$APP did not become Synced: $(app_state)"
  wait_settled "$MAIN_ORIG_SHA" ||
    die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} after the sync within ${WAIT}s: $(app_state)"
  [[ "$(app_field '{.status.operationState.syncResult.revision}')" == "$MAIN_ORIG_SHA" ]] ||
    die "the last sync of $APP is still not of main: $(app_state)"
fi
[[ "$(live_log_level)" == "$GIT_LEVEL" ]] ||
  die "the live LOG_LEVEL is '$(live_log_level)' but Git has '$GIT_LEVEL' although $APP was synced"
pass "cluster, Argo CD and $APP settled at main ${MAIN_ORIG_SHA:0:8} (LOG_LEVEL=$GIT_LEVEL in Git and live)"

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
  # The metrics are served over plain HTTP on a free local port, so the script
  # can read them without a token (the deployed operator serves HTTPS behind
  # Kubernetes authentication; the checks below are skipped with IN_CLUSTER).
  METRICS_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  METRICS_URL="http://127.0.0.1:$METRICS_PORT/metrics"
  "$ROOT/bin/manager" --scm-recheck-interval=10s --merge-request-poll-interval="$POLL" \
    --metrics-bind-address="127.0.0.1:$METRICS_PORT" --metrics-secure=false >"$LOG_FILE" 2>&1 &
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
# The token goes into the Secret through a 0600 file, not an argument.
(umask 077; printf '%s' "$SCM_TOKEN" >"$WORKDIR/token")
k -n "$NS" create secret generic "$SCM-token" --from-file=token="$WORKDIR/token" >/dev/null
rm -f "$WORKDIR/token"

k apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1alpha1
kind: ScmConnection
metadata:
  name: $SCM_NAME
  namespace: $NS
spec:
  provider: $SCM
  url: $SCM_URL
  tokenSecretRef:
    name: $SCM-token
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
  batchWindow: $WINDOW
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
wait_for "ScmConnection is Ready (token accepted by $SCM_HOST)" k -n "$NS" wait "scm/$SCM_NAME" --for=condition=Ready --timeout=1s
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
policy_has_connection() { [[ "$(k -n "$NS" get bfp "$POLICY" -o jsonpath='{.status.applications[0].scmConnection}')" == "$SCM_NAME" ]]; }
wait_for "policy matched the repository to the ScmConnection" policy_has_connection

# ---------------------------------------------------------------------------
if section_wanted a; then
step "a. A drift becomes a merge request"
require_drift a debug
set_log_level debug
wait_for "proposal is Proposed ($GIT_LEVEL -> debug)" has_proposal Proposed "/data/LOG_LEVEL:Replace:\"$GIT_LEVEL\">\"debug\""
remember_proposals
P_A="$(proposal_with Proposed '"debug"' | head -n 1)"
if [[ -n "$P_A" ]]; then
  BRANCH_A="$(branch_of "$P_A")"
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
  # The operator counted what it did (the local operator starts with its counters at zero).
  expect_metric_at_least "backflow_drifts_detected_total for $APP" backflow_drifts_detected_total "application=\"$APP\"" 1
  expect_metric_at_least "backflow_mapping_results_total{reason=Mapped}" backflow_mapping_results_total 'reason="Mapped"' 1
  expect_metric_at_least "backflow_merge_requests_total{action=opened}" backflow_merge_requests_total 'action="opened"' 1
  expect_metric_at_least "backflow_scm_connection_ready" backflow_scm_connection_ready "scm=\"$SCM_NAME\"" 1
  expect_metric_at_least "backflow_policy_ready" backflow_policy_ready "policy=\"$POLICY\"" 1
  expect_metric_at_least "backflow_scm_requests_total{provider=$SCM}" backflow_scm_requests_total "provider=\"$SCM\"" 1
  expect_metric_at_least "backflow_argocd_requests_total{code_class=2xx}" backflow_argocd_requests_total 'code_class="2xx"' 1
  COMMIT_JSON="$(gl GET "$(project_path "/repository/commits/$(enc "$BRANCH_A")")")"
  expect_eq "committer" "Backflow" "$(echo "$COMMIT_JSON" | jx 'd["committer_name"]')"
  expect_contains "commit message trailer" "Proposal: $P_A" "$(echo "$COMMIT_JSON" | jx 'd["message"]')"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted b; then
step "b. Closing the merge request rejects the proposal"
if [[ -n "${P_A:-}" ]]; then
  gl PUT "$(project_path "/merge_requests/$IID_A")" '{"state_event":"close"}' >/dev/null || fail "could not close !$IID_A"
  wait_for "proposal is Rejected" phase_is "$P_A" Rejected
  expect_eq "status.mergeRequest.state" "closed" "$(dp_field "$P_A" .status.mergeRequest.state)"
  expect_contains "message" "Argo CD can revert" "$(dp_field "$P_A" .status.message)"
  expect_metric_at_least "backflow_merge_requests_total{action=closed}" backflow_merge_requests_total 'action="closed"' 1
  expect_eq "the branch is left for the reviewer" "true" "$(branch_exists "$BRANCH_A" && echo true || echo false)"

  BEFORE_B="$(proposal_count)"
  echo "  waiting ${QUIET}s to see that nothing is reopened..."
  sleep "$QUIET"
  expect_eq "proposals for demo-config (none new)" "$BEFORE_B" "$(proposal_count)"
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
  expect_eq "proposals for demo-config (none new)" "$BEFORE_B" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
  if branch_exists "$BRANCH_A"; then
    gl DELETE "$(project_path "/repository/branches/$(enc "$BRANCH_A")")" >/dev/null && note "deleted $BRANCH_A"
  fi
fi

fi

# ---------------------------------------------------------------------------
if section_wanted c; then
step "c. A different drift is merged"
require_drift c warn
set_log_level warn
wait_for "proposal is Proposed ($GIT_LEVEL -> warn)" has_proposal Proposed "/data/LOG_LEVEL:Replace:\"$GIT_LEVEL\">\"warn\""
remember_proposals
P_C="$(proposal_with Proposed '"warn"' | head -n 1)"
if [[ -n "$P_C" ]]; then
  BRANCH_C="$(branch_of "$P_C")"
  IFS='|' read -r IID_C STATE_C _ _ <<<"$(mr_info "$BRANCH_C")"
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

  # Argo CD polls Git (about every three minutes) and has not seen the merge
  # commit yet: it still compares against the old revision, where demo-config is
  # OutOfSync with the very same changes. No second proposal may appear for it.
  # Nothing refreshes or syncs the Application until this check is over.
  HOLD_C=$((2 * WINDOW_S + QUIET))
  BEFORE_C="$(proposal_count)"
  echo "  waiting ${HOLD_C}s: no new proposal for the change that was just merged..."
  sleep "$HOLD_C"
  if [[ "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.sync.revision}')" == "$MERGE_SHA" ]]; then
    # dev-up.sh makes Argo CD poll Git every 30s, so it can be quicker than this
    # wait; the check below then passes without testing the sync lag.
    note "Argo CD already compares against the merge commit: the sync lag was not exercised this time"
  fi
  expect_eq "proposals for demo-config (no duplicate right after the merge)" "$BEFORE_C" "$(proposal_count)"
  expect_eq "proposal stays Merged" "Merged" "$(dp_field "$P_C" .status.phase)"

  echo "  Argo CD picks up the merged commit..."
  wait_for "$APP is Synced at the merge commit" app_synced_at "$MERGE_SHA"
  expect_eq "live LOG_LEVEL is unchanged" "warn" "$(live_log_level)"
  if sync_app; then pass "one sync at the merge commit (so Argo CD's automated sync will not revert the next drift)"; else fail "$APP did not stay Synced"; fi
  sleep "$QUIET"
  expect_eq "proposal stays Merged" "Merged" "$(dp_field "$P_C" .status.phase)"
  expect_eq "proposals for demo-config (no new one)" "$BEFORE_C" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted d; then
step "d. The live value goes back to Git while the merge request is open"
require_drift d debug
set_log_level debug
wait_for "proposal is Proposed (warn -> debug)" has_proposal Proposed '/data/LOG_LEVEL:Replace:"warn">"debug"'
remember_proposals
P_D="$(proposal_with Proposed '>"debug"' | head -n 1)"
if [[ -n "$P_D" ]]; then
  BRANCH_D="$(branch_of "$P_D")"
  IFS='|' read -r IID_D STATE_D _ _ <<<"$(mr_info "$BRANCH_D")"
  expect_eq "merge request state" "opened" "$STATE_D"
  expect_eq "the branch exists" "true" "$(branch_exists "$BRANCH_D" && echo true || echo false)"

  set_log_level warn
  wait_for "proposal is Reverted" phase_is "$P_D" Reverted
  wait_for "cleanup finished (CleanedUp)" cleaned_up "$P_D"
  IFS='|' read -r _ STATE_D2 _ _ <<<"$(mr_info "$BRANCH_D")"
  expect_eq "merge request state" "closed" "$STATE_D2"
  expect_eq "the branch exists" "false" "$(branch_exists "$BRANCH_D" && echo true || echo false)"
  NOTES="$(gl GET "$(project_path "/merge_requests/$IID_D/notes?per_page=50")" | jx '" ".join(n["body"] for n in d if not n.get("system"))')"
  expect_contains "closing comment" "Backflow is closing this merge request" "$NOTES"
  expect_contains "closing comment (reason)" "back in sync with Git" "$NOTES"
  expect_eq "status.mergeRequest.state" "closed" "$(dp_field "$P_D" .status.mergeRequest.state)"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted e; then
step "e. A new sync resets the cluster while the merge request is open"
require_drift e error
set_log_level error
wait_for "proposal is Proposed (warn -> error)" has_proposal Proposed '/data/LOG_LEVEL:Replace:"warn">"error"'
remember_proposals
P_E="$(proposal_with Proposed '>"error"' | head -n 1)"
if [[ -n "$P_E" ]]; then
  BRANCH_E="$(branch_of "$P_E")"
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
  BEFORE_E="$(proposal_count)"
  expect_eq "status.commitSHA is the merge commit" "$MERGE_E" "$(dp_field "$P_E" .status.commitSHA)"
  wait_for "$APP is Synced at the merge commit" app_synced_at "$MERGE_E"
  level_is_error() { [[ "$(live_log_level)" == "error" ]]; }
  wait_for "the change is back in the cluster after the sync" level_is_error
  sleep "$QUIET"
  expect_eq "proposals for demo-config (no new one)" "$BEFORE_E" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted f; then
step "f. Git ahead of the last sync pauses drift detection"
if [[ -z "$AUTOMATED_ORIG" ]]; then
  note "skipped: $APP has no automated sync to switch off"
else
  if disable_autosync; then pass "automated sync switched off"; else fail "could not switch automated sync off"; fi
  # No sync may be pending when the commit is made, or it would apply the commit
  # and the Application would never be ahead of its last sync.
  wait_idle_at "$(main_sha)" || fail "$APP is not idle and synced at main before the commit: $(app_state)"

  echo "  a commit outside apps/demo (the repository has other content too)..."
  python3 - "$WORKDIR" "$(raw_file README.md main)" >"$WORKDIR/outside.json" <<'PY'
import json, sys
print(json.dumps({"branch": "main", "commit_message": "test: a commit outside the Application directory (Backflow e2e)",
                  "actions": [{"action": "update", "file_path": "README.md",
                               "content": sys.argv[2] + "\nA line outside apps/demo, added by the Backflow e2e test.\n"}]}))
PY
  OUTSIDE_SHA="$(gl POST "$(project_path "/repository/commits")" "$(cat "$WORKDIR/outside.json")" | jx 'd["id"]')"
  rm -f "$WORKDIR/outside.json"
  expect_nonempty "commit outside apps/demo" "$OUTSIDE_SHA"
  wait_for "Argo CD compares against it" app_compared_at "$OUTSIDE_SHA"
  expect_eq "it is not synced (manual sync)" "true" "$([[ "$(k -n "$ARGOCD_NS" get application "$APP" -o jsonpath='{.status.operationState.syncResult.revision}')" != "$OUTSIDE_SHA" ]] && echo true || echo false)"
  EVENTS_BEFORE="$(git_ahead_events)"
  require_drift f debug
  set_log_level debug
  wait_for "a real drift is proposed although Git is ahead (error -> debug)" has_proposal 'Detected|Mapping|Proposed' '/data/LOG_LEVEL:Replace:"error">"debug"'
  remember_proposals
  P_F1="$(proposal_with 'Detected|Mapping|Proposed' '>"debug"' | head -n 1)"
  expect_eq "no pause Event for a commit outside the directory" "$EVENTS_BEFORE" "$(git_ahead_events)"
  set_log_level error
  if [[ -n "$P_F1" ]]; then
    wait_for "and retired when the live value goes back" phase_is "$P_F1" Reverted
    expect_cleaned_and_closed "$P_F1"
  fi

  echo "  a commit under apps/demo..."
  BEFORE_F="$(proposal_count)"
  EVENTS_BEFORE="$(git_ahead_events)"
  CURRENT_CM="$(raw_file apps/demo/configmap.yaml main)"
  python3 - "$WORKDIR" "$CURRENT_CM" >"$WORKDIR/ahead.json" <<'PY'
import json, re, sys
content = re.sub(r"LOG_LEVEL: \S+", "LOG_LEVEL: trace", sys.argv[2], count=1)
print(json.dumps({"branch": "main", "commit_message": "test: a commit the Application has not synced yet (Backflow e2e)",
                  "actions": [{"action": "update", "file_path": "apps/demo/configmap.yaml", "content": content}]}))
PY
  AHEAD_SHA="$(gl POST "$(project_path "/repository/commits")" "$(cat "$WORKDIR/ahead.json")" | jx 'd["id"]')"
  rm -f "$WORKDIR/ahead.json"
  expect_nonempty "commit under apps/demo" "$AHEAD_SHA"

  wait_for "$APP is OutOfSync, comparing against the commit but not synced to it" app_ahead_at "$AHEAD_SHA"
  expect_eq "live LOG_LEVEL is still the old one" "error" "$(live_log_level)"
  echo "  waiting ${QUIET}s: no proposal may be made for the pending commit..."
  sleep "$QUIET"
  expect_eq "proposals for demo-config (none new)" "$BEFORE_F" "$(proposal_count)"
  expect_eq "open merge requests" "0" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"
  wait_for "an Event says that detection is paused" events_exceed "$EVENTS_BEFORE"
  expect_eq "new GitAhead events (one per combination of revisions)" "$((EVENTS_BEFORE + 1))" "$(git_ahead_events)"
  expect_contains "Event text" "1 file(s) under apps/demo changed" "$(git_ahead_notes | tail -n 1)"
  expect_contains "Event text" "drift detection for it is paused until Argo CD syncs" "$(git_ahead_notes | tail -n 1)"

  if sync_app; then pass "synced the Application"; else fail "$APP did not become Synced"; fi
  wait_for "$APP is Synced at the commit" app_synced_at "$AHEAD_SHA"
  expect_eq "live LOG_LEVEL after the sync" "trace" "$(live_log_level)"
  expect_eq "proposals for demo-config (still none new)" "$BEFORE_F" "$(proposal_count)"

  echo "  after the sync a real drift is detected again..."
  set_log_level debug
  wait_for "proposal is made for the real drift (trace -> debug)" has_proposal 'Detected|Mapping|Proposed' '/data/LOG_LEVEL:Replace:"trace">"debug"'
  remember_proposals
  P_F="$(proposal_with 'Detected|Mapping|Proposed' '>"debug"' | head -n 1)"
  set_log_level trace
  if [[ -n "$P_F" ]]; then
    wait_for "and retired when the live value goes back" phase_is "$P_F" Reverted
    expect_cleaned_and_closed "$P_F"
  fi

  if restore_autosync; then pass "automated sync switched back on"; else fail "could not switch automated sync back on"; fi
  if autosync_enabled; then pass "$APP has automated sync again"; else fail "$APP has no automated sync"; fi
fi

fi

# ---------------------------------------------------------------------------
if section_wanted g; then
step "g. A proposal made in ReportOnly is proposed when the mode is switched"
held_by_report_only() { [[ "$(condition "$1" Proposed status)/$(condition "$1" Proposed reason)" == "False/ReportOnly" ]]; }
set_mode() { k -n "$NS" patch bfp "$POLICY" --type merge -p "{\"spec\":{\"mode\":\"$1\"}}" >/dev/null; }
LEVEL_G="$(live_log_level)"
GIT_G="$(git_level)"
TARGET_G=""
for candidate in warn error debug trace; do
  if [[ "$candidate" != "$LEVEL_G" && "$candidate" != "$GIT_G" ]]; then TARGET_G="$candidate"; break; fi
done
require_drift g "$TARGET_G"
if set_mode ReportOnly; then pass "policy mode is ReportOnly"; else fail "could not switch the policy to ReportOnly"; fi
OPEN_G="$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"

set_log_level "$TARGET_G"
wait_for "proposal is held in Mapping ($LEVEL_G -> $TARGET_G)" has_proposal Mapping "/data/LOG_LEVEL:Replace:\"$LEVEL_G\">\"$TARGET_G\""
remember_proposals
P_G="$(proposal_with Mapping ">\"$TARGET_G\"" | head -n 1)"
if [[ -n "$P_G" ]]; then
  wait_for "Proposed condition is False/ReportOnly" held_by_report_only "$P_G"
  expect_eq "Mapped condition" "True" "$(condition "$P_G" Mapped status)"
  expect_eq "status.mergeRequest.url" "" "$(dp_field "$P_G" .status.mergeRequest.url)"
  expect_eq "open merge requests" "$OPEN_G" "$(gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)')"

  if set_mode MergeRequest; then pass "policy mode switched to MergeRequest"; else fail "could not switch the policy to MergeRequest"; fi
  wait_for "the held proposal is Proposed" phase_is "$P_G" Proposed
  expect_eq "Proposed condition" "True" "$(condition "$P_G" Proposed status)"
  BRANCH_G="$(branch_of "$P_G")"
  IFS='|' read -r IID_G STATE_G _ _ <<<"$(mr_info "$BRANCH_G")"
  expect_nonempty "merge request from $BRANCH_G" "$IID_G"
  expect_eq "merge request state" "opened" "$STATE_G"
  expect_eq "status.mergeRequest.branch" "$BRANCH_G" "$(dp_field "$P_G" .status.mergeRequest.branch)"

  set_log_level "$LEVEL_G"
  wait_for "proposal is Reverted when the live value goes back" phase_is "$P_G" Reverted
  expect_cleaned_and_closed "$P_G"
else
  set_log_level "$LEVEL_G"
  set_mode MergeRequest || true
fi

fi

# ---------------------------------------------------------------------------
if section_wanted h; then
step "h. A JSON manifest: the merge request changes only the value line"
JSON_FILE="${TEST_ADDED_FILES[0]}"
JSON_CONTENT=$'{\n  "apiVersion": "v1",\n  "kind": "ConfigMap",\n  "metadata": {\n    "name": "demo-json",\n    "namespace": "demo"\n  },\n  "data": {\n    "GREETING": "hello",\n    "LOG_LEVEL": "info"\n  }\n}\n'
NEW_GREETING="howdy"
# main_commit <action> <file> <content> <message>: one commit to main; prints its SHA.
main_commit() {
  local body
  body="$(python3 -c 'import json,sys; a={"action":sys.argv[1],"file_path":sys.argv[2]}
if sys.argv[1] != "delete": a["content"]=sys.argv[3]
print(json.dumps({"branch":"main","commit_message":sys.argv[4],"actions":[a]}))' "$1" "$2" "$3" "$4")"
  gl POST "$(project_path "/repository/commits")" "$body" | jx 'd["id"]'
}
SHA_H="$(main_commit create "$JSON_FILE" "$JSON_CONTENT" "test: a JSON manifest in the demo directory (Backflow e2e)")" || SHA_H=""
expect_nonempty "commit adding $JSON_FILE to main" "$SHA_H"
if [[ -n "$SHA_H" ]]; then
  wait_for "$APP is Synced at the commit that adds the JSON manifest" app_synced_at "$SHA_H"
  if sync_app "$SHA_H"; then pass "one sync at that commit (Argo CD's automated sync will not reset the drift)"; else fail "$APP did not stay Synced"; fi
  wait_for "the ConfigMap demo-json is in the cluster" k -n "$DEMO_NS" get configmap demo-json
  k -n "$DEMO_NS" patch configmap demo-json --type merge -p "{\"data\":{\"GREETING\":\"$NEW_GREETING\"}}" >/dev/null
  refresh_app
  PROPOSALS_OF=demo-json
  wait_for "proposal is Proposed (GREETING hello -> $NEW_GREETING)" has_proposal Proposed "/data/GREETING:Replace:\"hello\">\"$NEW_GREETING\""
  remember_proposals
  P_H="$(proposal_with Proposed "\"$NEW_GREETING\"" | head -n 1)"
  if [[ -n "$P_H" ]]; then
    BRANCH_H="$(branch_of "$P_H")"
    IFS='|' read -r IID_H STATE_H _ _ <<<"$(mr_info "$BRANCH_H")"
    expect_nonempty "merge request from $BRANCH_H" "$IID_H"
    expect_eq "merge request state" "opened" "$STATE_H"
    expect_eq "strategy" "Direct" "$(dp_field "$P_H" .status.mapping.strategy)"
    expect_eq "edited file" "$JSON_FILE" "$(dp_field "$P_H" '.status.mapping.edits[*].file')"
    expect_eq "edited location" "/data/GREETING" "$(dp_field "$P_H" '.status.mapping.edits[*].location')"

    # The file on the branch is the file on main with the one value changed, byte
    # for byte (a trailing newline survives the command substitution by sentinel).
    ON_BRANCH="$(raw_file "$JSON_FILE" "$BRANCH_H"; echo x)"
    WANT_H="$(printf '%s' "$JSON_CONTENT" | sed "s/\"GREETING\": \"hello\"/\"GREETING\": \"$NEW_GREETING\"/"; echo x)"
    if [[ "$ON_BRANCH" == "$WANT_H" ]]; then pass "$JSON_FILE on the branch is the original with only the GREETING value changed"; else fail "$JSON_FILE on $BRANCH_H differs from the original in more than the GREETING value"; fi
    printf '%s' "$JSON_CONTENT" >"$WORKDIR/json-main.txt"
    raw_file "$JSON_FILE" "$BRANCH_H" >"$WORKDIR/json-branch.txt"
    expect_eq "lines removed by the merge request" "1" "$(diff "$WORKDIR/json-main.txt" "$WORKDIR/json-branch.txt" | grep -c '^<' || true)"
    expect_eq "lines added by the merge request" "1" "$(diff "$WORKDIR/json-main.txt" "$WORKDIR/json-branch.txt" | grep -c '^>' || true)"
    BODY_H="$(gl GET "$(project_path "/merge_requests/$IID_H")" | jx 'd["description"]')"
    expect_contains "description (removed line)" '-    "GREETING": "hello",' "$BODY_H"
    expect_contains "description (added line)" '+    "GREETING": "'"$NEW_GREETING"'",' "$BODY_H"
    expect_not_contains "description (the other line is not in the diff)" '-    "LOG_LEVEL"' "$BODY_H"
    expect_eq "main still has the original file" "$SHA_H" "$(main_sha)"

    gl PUT "$(project_path "/merge_requests/$IID_H")" '{"state_event":"close"}' >/dev/null || fail "could not close !$IID_H"
    wait_for "proposal is Rejected" phase_is "$P_H" Rejected
  fi
  PROPOSALS_OF=""
  SHA_H2="$(main_commit delete "$JSON_FILE" "" "test: remove the JSON manifest again (Backflow e2e)")" || SHA_H2=""
  expect_nonempty "commit removing $JSON_FILE from main" "$SHA_H2"
  k -n "$DEMO_NS" delete configmap demo-json --ignore-not-found >/dev/null 2>&1 || true
  if [[ -n "$SHA_H2" ]]; then
    wait_for "$APP is Synced at the commit that removes the JSON manifest" app_synced_at "$SHA_H2"
    if sync_app "$SHA_H2"; then pass "one sync at that commit"; else fail "$APP did not stay Synced"; fi
  fi
fi

fi

# ---------------------------------------------------------------------------
step "Secrets"
refresh_log
if grep -qF -- "$SCM_TOKEN" "$LOG_FILE" 2>/dev/null; then
  fail "the $SCM_LABEL token appears in the operator log"
else
  pass "the $SCM_LABEL token does not appear in the operator log"
fi
LEAK=false
for p in "$P_A" "$P_C" "$P_D" "$P_E" "$P_F1" "$P_F"; do
  [[ -n "$p" ]] || continue
  if k -n "$NS" get driftproposal "$p" -o yaml | grep -qF -- "$SCM_TOKEN"; then LEAK=true; fi
done
if [[ "$LEAK" == "true" ]]; then fail "the $SCM_LABEL token appears in a DriftProposal"; else pass "the $SCM_LABEL token does not appear in any DriftProposal"; fi

# ---------------------------------------------------------------------------
echo
echo "${BOLD}Summary:${NC} ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}"
print_timing
if [[ "$KEEP" == "true" ]]; then
  echo "Test namespace '$NS' kept: kubectl -n $NS get bfp,scm,driftproposals"
fi
if (( FAIL > 0 )); then
  if [[ -s "$WORKDIR/branches" ]]; then
    echo "Branch names read from the proposals (branch_of):"
    awk '!seen[$0]++' "$WORKDIR/branches" | sed 's/^/  /'
  fi
  echo "Operator log: ${LOG_FILE#"$ROOT"/}"
  exit 1
fi
