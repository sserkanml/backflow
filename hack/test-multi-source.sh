#!/usr/bin/env bash
#
# Backflow - multi-source Helm mapping end-to-end test
#
# Runs the full path for the Application demo-multi against the real demo
# repository on gitlab.com (SCM=github: github.com): the chart podinfo comes from the OCI registry
# ghcr.io (a pinned version, not in Git) and its values from
# multi/values-dev.yaml of the demo repository, the ref source $values. Argo CD
# renders such an Application from commits, so Backflow verifies each candidate
# edit by pushing it to a scratch branch (backflow/scratch/...), rendering that
# commit by its SHA through Argo CD, and deleting the branch again. This needs
# the Argo CD of ./hack/dev-up.sh, the demo-multi Application it creates (and the
# files it seeds into the demo repository), and internet access to ghcr.io.
#
#   a. A replica drift (kubectl scale) becomes a merge request on
#      multi/values-dev.yaml, in the repository of the ref source; the status
#      names that repository, the description lists the sources, the scratch
#      branches are gone. Merging it makes the proposal Merged and, after Argo CD
#      syncs, no new proposal appears. (Commits to main: the merge.)
#   b. A chart default (PODINFO_UI_COLOR comes from ui.color of the chart's own
#      values.yaml, which Argo CD returns) becomes an override: the merge request
#      adds ui.color to the value file and nothing else.
#   c. Maps and lists: removing a pod annotation removes the key from
#      podAnnotations; removing an env var writes extraEnvs back without it, in
#      block style with comments kept and "on" quoted. (Argo CD does not report
#      what is only added in the cluster, so the drifts are removals.)
#   d. A policy in ReportOnly mode (spec.mapping.scratchBranches is Auto): the
#      proposal is Unmapped with ScratchBranchesDisabled, the message names the
#      field, and no scratch branch is ever pushed.
#   e. Scratch branches and pipelines: after a mapping no backflow/scratch/
#      branch is left, and, when the project has pipelines, none ran for one.
#   f. Git ahead, with automated sync off for demo-multi: a commit to the value
#      file pauses it (an Event), a commit to another file does not; a new chart
#      version in the Application is reported as a change of the Application
#      (ApplicationChanged), not as Git being ahead, and also pauses. (The
#      Application's targetRevision is restored. Commits to main.)
#   g. Tokens: the $SCM_LABEL token appears in no operator log line, DriftProposal,
#      merge request or commit message of the run. (The demo chart renders no
#      Secret; the masking of rendered Secrets is covered by unit and envtest
#      tests.)
#
# Requires the environment from ./hack/dev-up.sh and GITLAB_TOKEN with the api
# and write_repository scopes (owner or maintainer of the demo project). Without
# GITLAB_TOKEN the test is skipped with a message and exits 0.
#
# Everything the script creates is removed on exit: branches (scratch ones
# included), merge requests, the test label, the test namespace. Content that was
# merged or committed to main is restored with a follow-up commit through the
# API. Because commits cannot be undone, those restore commits stay in the
# history of main.
#
# The operator runs locally (bin/manager, polling merge requests every POLL)
# unless IN_CLUSTER=true, which uses the deployed operator (polling every two
# minutes by default; raise WAIT accordingly).
#
# Usage (from the repository root):
#   GITLAB_TOKEN=... ./hack/test-multi-source.sh
#   SCM=github GITHUB_TOKEN=... ./hack/test-multi-source.sh   # against the GitHub demo repository (see the README)
#   SECTIONS=d,b ./hack/test-multi-source.sh   # only those sections (default: all);
#                                              every section works on its own
#   WINDOW=10s POLL=3s QUIET=25                # batchWindow, merge request polling, quiet waits
#   KEEP=true ./hack/test-multi-source.sh      # keep the test namespace

set -euo pipefail

# Which Git host: SCM=gitlab (default) or SCM=github, DEMO_REPO, GITLAB_TOKEN / GITHUB_TOKEN.
# shellcheck source=lib/scm.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/scm.sh"

CLUSTER_NAME="${CLUSTER_NAME:-$SCM_CLUSTER}"
NS="${NS:-backflow-multi-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
POLICY="multi-policy"
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
# batchWindow across two observations. Every wait for "nothing happens" is
# derived from it (QUIET below).
WINDOW="${WINDOW:-10s}"
WINDOW_S="${WINDOW%s}"
if ! [[ "$WINDOW_S" =~ ^[0-9]+$ ]]; then
  echo "WINDOW must be a number of seconds like 10s, got '$WINDOW'" >&2
  exit 1
fi
# Seconds to wait for Argo CD, the operator and the Git host to react. Rendering a
# multi-source Application is slower: every candidate is a scratch commit and a
# render through Argo CD.
if [[ "$IN_CLUSTER" == "true" ]]; then WAIT="${WAIT:-420}"; else WAIT="${WAIT:-240}"; fi
# Seconds to wait before asserting that nothing happened.
QUIET="${QUIET:-$((2 * WINDOW_S + 5))}"

# The Application under test, its namespace, its Deployment and the value file.
APP="demo-multi"
APP_NS="demo-multi"
DEPLOY="demo-multi-podinfo"
VALUES="multi/values-dev.yaml"
SCRATCH_PREFIX="backflow/scratch/"

# SECTIONS=a,d runs only those sections (letters, comma separated) after the
# prerequisites; default all. Every section works on its own.
SECTIONS="${SECTIONS:-}"
section_wanted() { [[ -z "$SECTIONS" || ",$SECTIONS," == *",$1,"* ]]; }
section_needs() {
  case "$1" in
    # no section needs another one

    *) echo "" ;;
  esac
}
check_sections() {
  local s n
  for s in ${SECTIONS//,/ }; do
    case " a b c d e f g " in
      *" $s "*) ;;
      *) echo "SECTIONS: unknown section '$s' (valid: a,b,c,d,e,f,g)" >&2; exit 1 ;;
    esac
    for n in $(section_needs "$s"); do
      section_wanted "$n" || { echo "SECTIONS: section $s needs section $n, before it (see the notes at SECTIONS); add it to the list, for example SECTIONS=$n,$s, or leave SECTIONS out" >&2; exit 1; }
    done
  done
}
check_sections

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT/tmp"
LOG_FILE="$LOG_DIR/test-multi-source-manager.log"

if [[ -z "${SCM_TOKEN:-}" ]]; then
  echo "SKIPPED: $SCM_TOKEN_VAR is not set."
  echo "This test pushes scratch branches and merges into $SCM_PROJECT on $SCM_HOST and needs"
  echo "$SCM_TOKEN_SCOPES: $SCM_TOKEN_VAR=... $0"
  exit 0
fi

MANAGER_PID=""
PF_PID=""
WORKDIR=""
RESTORE_CLUSTER=false
LABEL_PREEXISTED=true
# Set when the test changed the chart version of the Application (section f).
RESTORE_TARGET=""
# Whether the Application's automated sync was switched off by this run.
AUTOSYNC_OFF=""
MAIN_ORIG_SHA=""
ORIG_COUNT=""
PASS=0
FAIL=0

if [[ -t 1 ]]; then
  GREEN=$'\e[32m'; RED=$'\e[31m'; BOLD=$'\e[1m'; NC=$'\e[0m'
else
  GREEN=""; RED=""; BOLD=""; NC=""
fi

CURRENT_STEP=""
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
step() { CURRENT_STEP="$*"; begin_section "$*"; echo; echo "${BOLD}▶ $*${NC}"; }
pass() { echo "  ${GREEN}✔${NC} $*"; PASS=$((PASS + 1)); }
fail() { echo "  ${RED}✘${NC} $*"; FAIL=$((FAIL + 1)); }
die()  { echo "${RED}ERROR:${NC} $*" >&2; exit 1; }
note() { echo "    $*"; }

k() { kubectl --context "kind-$CLUSTER_NAME" "$@"; }

# ---------------------------------------------------------------------------
# Git host API (hack/scm.py: GitLab or GitHub). The token travels in the environment only: never in an argument
# list, a log or a file other than the 0600 Secret input.
#
# The Git host is sometimes slow or down for a minute, so scm.py retries: 4 attempts
# with a 60s read timeout and a growing delay (SCM_ATTEMPTS, SCM_TIMEOUT,
# SCM_BACKOFF). Reads and idempotent writes (closing a merge request,
# deleting a branch) are repeated freely. A write whose outcome is unknown after
# a timeout or a 5xx is only repeated after checking that it did not happen: a
# commit is looked for at the head of its branch (same message, on top of the
# head it was made against), a merge by the state of the merge request.
# ---------------------------------------------------------------------------

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/backflow-multi-test.XXXXXX")"
chmod 700 "$WORKDIR"
mkdir -p "$WORKDIR/orig"

cp "$SCM_HACK_DIR/scm.py" "$WORKDIR/scm.py"

# edit.py <project id> <file> <regex> <replacement> <message>: commit to main
# the file with the first match of the regex (multi-line mode) replaced. Prints
# the new commit's SHA. The token is read by scm.py from the environment.
cat >"$WORKDIR/edit.py" <<'PY'
import json, os, re, subprocess, sys, urllib.parse

pid, path, pattern, repl, message = sys.argv[1:6]
gl = os.path.join(os.path.dirname(os.path.abspath(__file__)), "scm.py")

def call(*args):
    r = subprocess.run(["python3", gl, *args], capture_output=True, text=True)
    if r.returncode != 0:
        sys.stderr.write(r.stderr)
        sys.exit(1)
    return r.stdout

enc = urllib.parse.quote(path, safe="")
content = call("GET", "/projects/%s/repository/files/%s/raw?ref=main" % (pid, enc))
new, n = re.subn(pattern, repl, content, count=1, flags=re.M)
if n != 1 or new == content:
    sys.stderr.write("the pattern %r changes nothing in %s\n" % (pattern, path))
    sys.exit(1)
body = json.dumps({"branch": "main", "commit_message": message,
                   "actions": [{"action": "update", "file_path": path, "content": new}]})
print(json.loads(call("POST", "/projects/%s/repository/commits" % pid, body))["id"])
PY

PID_ENC="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$SCM_PROJECT")"
enc() { python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }
gl() { python3 "$WORKDIR/scm.py" "$@"; }
# gl_status <METHOD> <path>: HTTP status code only.
gl_status() { python3 "$WORKDIR/scm.py" "STATUS-$1" "$2"; }
# jx <python expression on d>: read JSON on stdin.
jx() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }

project_path() { echo "/projects/$PID_ENC$1"; }

# commit_edit <file> <regex> <replacement> <message>: a commit to main; prints its SHA.
commit_edit() { python3 "$WORKDIR/edit.py" "$PID_ENC" "$1" "$2" "$3" "$4"; }

# mr_info <branch>: "<iid>|<state>|<web_url>|<merge_commit_sha>" of the newest merge request from the branch.
mr_info() {
  gl GET "$(project_path "/merge_requests?source_branch=$(enc "$1")&state=all&order_by=created_at&sort=desc")" |
    jx '"|".join(str(x) for x in ((d[0]["iid"], d[0]["state"], d[0]["web_url"], d[0].get("merge_commit_sha") or "") if d else ("", "", "", "")))'
}
branch_exists() { [[ "$(gl_status GET "$(project_path "/repository/branches/$(enc "$1")")")" == "200" ]]; }
main_sha() { gl GET "$(project_path "/repository/branches/main")" | jx 'd["commit"]["id"]'; }
raw_file() { gl GET "$(project_path "/repository/files/$(enc "$1")/raw?ref=$(enc "$2")")"; }
open_mrs() { gl GET "$(project_path "/merge_requests?state=opened")" | jx 'len(d)'; }

# yaml_value <file> <ref> <key>: the first scalar value of "key: value" in the
# file (a trailing comment and quotes are not part of it).
yaml_value() {
  raw_file "$1" "$2" | sed -n "s/^ *$3: *\"\{0,1\}\([^ #\"]*\).*/\1/p" | head -n 1
}


# ---------------------------------------------------------------------------
# Cluster helpers for the Application demo-multi. Its revisions are positional:
# status.sync.revisions[0] is the chart version, [1] the commit of the ref
# source $values (the demo repository).
# ---------------------------------------------------------------------------

refresh_log() {
  if [[ "$IN_CLUSTER" == "true" ]]; then
    mkdir -p "$LOG_DIR"
    k -n "$MANAGER_NS" logs "deploy/$MANAGER_DEPLOY" >"$LOG_FILE" 2>&1 || true
  fi
}

# show_state: what the policy's proposals are doing, and the last thing the
# operator said. Printed when a wait times out, so the log says why.
show_state() {
  refresh_log
  echo "    proposals of the policy:"
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" -o jsonpath='{range .items[*]}{.metadata.name}{" phase="}{.status.phase}{" Mapped="}{.status.conditions[?(@.type=="Mapped")].reason}{" ("}{.status.conditions[?(@.type=="Mapped")].message}{")"}{"\n"}{end}' 2>/dev/null | cut -c1-400 | sed 's/^/      /'
  if [[ -f "$LOG_FILE" ]]; then
    echo "    last operator messages about verifying, mapping or proposing:"
    grep -E "Verification finished|Cannot (map|propose)|Proposal (failed|unmapped)|Reconciler error|scratch|Commit built|Pushed the branch|declined" "$LOG_FILE" 2>/dev/null | tail -n 5 | cut -c1-400 | sed 's/^/      /'
  fi
}

wait_for() {
  local desc="$1" waited=0
  shift
  while ! "$@" >/dev/null 2>&1; do
    waited=$((waited + 2))
    if (( waited > WAIT )); then
      fail "$desc (timed out after ${WAIT}s)"
      show_state
      return 0
    fi
    sleep 2
  done
  pass "$desc"
}

refresh_app() {
  k -n "$ARGOCD_NS" annotate application "$APP" "argocd.argoproj.io/refresh=${1:-normal}" --overwrite >/dev/null 2>&1 || true
}
app_field() { k -n "$ARGOCD_NS" get application "$APP" -o jsonpath="$1" 2>/dev/null; }
app_sync_status() { app_field '{.status.sync.status}'; }
# compared_values, synced_values: the commit of $values Argo CD compares against,
# and the one of the last sync.
compared_values() { app_field '{.status.sync.revisions[1]}'; }
synced_values() { app_field '{.status.operationState.syncResult.revisions[1]}'; }
chart_version() { app_field '{.status.sync.revisions[0]}'; }

# app_settled_at <sha>: Argo CD compares $values at the commit, no operation is
# requested or running, and it has compared again since the last one finished
# (reconciledAt >= finishedAt; UTC RFC 3339 times compare as text).
app_settled_at() {
  local phase finished reconciled
  refresh_app
  [[ "$(compared_values)" == "$1" ]] || return 1
  [[ -z "$(app_field '{.operation}')" ]] || return 1
  phase="$(app_field '{.status.operationState.phase}')"
  [[ "$phase" != "Running" && "$phase" != "Terminating" ]] || return 1
  finished="$(app_field '{.status.operationState.finishedAt}')"
  reconciled="$(app_field '{.status.reconciledAt}')"
  [[ -z "$finished" ]] || { [[ -n "$reconciled" && ! "$reconciled" < "$finished" ]]; }
}
wait_settled() {
  local waited=0
  while ! app_settled_at "$1"; do
    waited=$((waited + 2))
    (( waited > WAIT )) && return 1
    sleep 2
  done
}
app_state() {
  echo "revisions=$(app_field '{.status.sync.revisions}') sync=$(app_sync_status) phase=$(app_field '{.status.operationState.phase}') operation=$(app_field '{.operation}') finishedAt=$(app_field '{.status.operationState.finishedAt}') reconciledAt=$(app_field '{.status.reconciledAt}')"
}

# app_idle_at <sha>: Synced with $values at the commit and no operation requested
# or running. The state a section needs before it commits to the demo repository
# and expects the Application to become "Git ahead": a sync that is still pending
# would pick the new commit up.
app_idle_at() {
  local phase
  refresh_app
  [[ "$(app_sync_status)" == "Synced" ]] || return 1
  [[ "$(compared_values)" == "$1" ]] || return 1
  [[ -z "$(app_field '{.operation}')" ]] || return 1
  phase="$(app_field '{.status.operationState.phase}')"
  [[ "$phase" != "Running" && "$phase" != "Terminating" ]]
}
wait_idle_at() {
  local waited=0
  while ! app_idle_at "$1"; do
    waited=$((waited + 2))
    (( waited > WAIT )) && return 1
    sleep 2
  done
}

# sync_app [sha]: one sync of the Application with every source pinned (the
# resolved chart version, and $values at sha, else the head of main now), never an
# implicit revision: that is resolved when Argo CD starts the operation, and a
# commit made in between would be synced instead. A sync that is already
# requested or running is waited for first.
sync_app() {
  local sha="${1:-}" chart i
  [[ -n "$sha" ]] || sha="$(main_sha 2>/dev/null || true)"
  [[ -n "$sha" ]] || sha="$(compared_values)"
  chart="$(chart_version)"
  [[ -n "$sha" && -n "$chart" ]] || return 1
  for i in $(seq 1 30); do
    [[ -z "$(app_field '{.operation}')" && "$(app_field '{.status.operationState.phase}')" != "Running" ]] && break
    sleep 2
  done
  k -n "$ARGOCD_NS" patch application "$APP" --type merge \
    -p "{\"operation\":{\"initiatedBy\":{\"username\":\"backflow-test\"},\"sync\":{\"revisions\":[\"$chart\",\"$sha\"],\"sources\":[]}}}" >/dev/null 2>&1 || true
  for i in $(seq 1 60); do
    refresh_app hard
    [[ "$(app_sync_status)" == "Synced" && -z "$(app_field '{.operation}')" && "$(synced_values)" == "$sha" ]] && return 0
    sleep 2
  done
  return 1
}

# reset_cluster: Git is the truth; sync the Application at the head of main and
# wait until Argo CD is idle there, so the next section starts from a known state.
reset_cluster() {
  local head
  head="$(main_sha)"
  refresh_app hard
  sleep 3
  sync_app "$head" || { fail "the Application did not become Synced: $(app_state)"; return 0; }
  wait_settled "$head" || fail "Argo CD did not settle at ${head:0:8} within ${WAIT}s: $(app_state)"
}

disable_autosync() {
  if k -n "$ARGOCD_NS" patch application "$APP" --type json \
    -p '[{"op":"remove","path":"/spec/syncPolicy/automated"}]' >/dev/null 2>&1; then
    AUTOSYNC_OFF="true"
    return 0
  fi
  return 1
}
restore_autosync() {
  k -n "$ARGOCD_NS" patch application "$APP" --type merge -p '{"spec":{"syncPolicy":{"automated":{}}}}' >/dev/null 2>&1 &&
    AUTOSYNC_OFF=""
}
autosync_enabled() { [[ -n "$(app_field '{.spec.syncPolicy.automated}')" ]]; }

# app_ahead_at <sha>: OutOfSync, comparing $values against sha, which has not been synced.
app_ahead_at() {
  refresh_app hard
  [[ "$(app_sync_status)" == "OutOfSync" && "$(compared_values)" == "$1" && "$(synced_values)" != "$1" ]]
}
app_compared_at() {
  refresh_app hard
  [[ "$(compared_values)" == "$1" ]]
}
app_synced_at() {
  refresh_app hard
  [[ "$(app_sync_status)" == "Synced" && "$(compared_values)" == "$1" ]]
}

# Events of the policy: the text of each of a reason, and how many mention the Application.
events_of() {
  k -n "$NS" get events -o json 2>/dev/null | python3 -c '
import sys, json
for e in json.load(sys.stdin)["items"]:
    if e.get("reason") == sys.argv[1]:
        print(e.get("note") or e.get("message") or "")' "$1"
}
event_count() { events_of "$1" | grep -c -- " $APP " || true; }
event_exceeds() { [[ "$(event_count "$1")" -gt "$2" ]]; }

# live_replicas, live_env <name>: the live Deployment.
live_replicas() { k -n "$APP_NS" get deployment "$DEPLOY" -o jsonpath='{.spec.replicas}' 2>/dev/null; }
live_env() { k -n "$APP_NS" get deployment "$DEPLOY" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name==\"$1\")].value}" 2>/dev/null; }

# require_drift <section> <git> <live> <new>...: every value a section drifts to
# must differ from the Git value and from the live value, or there would be no
# drift to see.
require_drift() {
  local section="$1" git="$2" live="$3" v
  shift 3
  for v in "$@"; do
    if [[ -z "$v" || "$v" == "$git" || "$v" == "$live" ]]; then
      die "section $section: the drift value '$v' equals the Git value '$git' or the live value '$live', so there would be no drift to see. Is Argo CD still syncing, or is main not in the state this test expects?"
    fi
  done
}

# scratch_branches: the scratch branches of the demo repository, one per line.
scratch_branches() {
  gl GET "$(project_path "/repository/branches?search=$(enc "$SCRATCH_PREFIX")&per_page=100")" |
    jx '"\n".join(b["name"] for b in d if b["name"].startswith("'"$SCRATCH_PREFIX"'"))'
}
no_scratch_branches() { [[ -z "$(scratch_branches)" ]]; }


# ---------------------------------------------------------------------------
# Proposals
# ---------------------------------------------------------------------------

# proposals_of <app> <resource name or *>: one line per DriftProposal of the policy:
#   <name>|<phase>|<path>:<op>:<desired>><live>;...
proposals_of() {
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" -o jsonpath='{range .items[*]}{.metadata.name}|{.status.phase}|{.spec.application.name}|{.spec.resource.name}|{range .spec.changes[*]}{.path}:{.op}:{.desired}>{.live};{end}{"\n"}{end}' 2>/dev/null |
    awk -F'|' -v a="$1" -v r="$2" '$3 == a && ($4 == r || r == "*") { print $1 "|" $2 "|" $5 }'
}
# proposal_with <app> <resource> <phase regex> <changes substring>: names, one per line.
proposal_with() {
  proposals_of "$1" "$2" | awk -F'|' -v p="$3" -v c="$4" '$2 ~ "^(" p ")$" && index($3, c) > 0 { print $1 }'
}
has_proposal() { [[ -n "$(proposal_with "$@")" ]]; }
proposal_count() { proposals_of "$1" "${2:-*}" | wc -l | tr -d ' '; }

dp_field() { k -n "$NS" get driftproposal "$1" -o jsonpath="{$2}" 2>/dev/null; }
condition() { dp_field "$1" ".status.conditions[?(@.type==\"$2\")].$3"; }
phase_is() { [[ "$(dp_field "$1" .status.phase)" == "$2" ]]; }
cleaned_up() { [[ "$(condition "$1" CleanedUp status)" == "True" ]]; }

# branch_of <proposal>: the branch Backflow pushes for a proposal, backflow/<name>-<first 8 of its UID>.
branch_of() {
  local uid branch
  uid="$(dp_field "$1" .metadata.uid | cut -c1-8)"
  branch="$(printf 'backflow/%s-%s' "$1" "$uid")"
  printf '%s: uid8=%s -> %s (status.branch=%s)\n' "$1" "${uid:-<empty>}" "$branch" \
    "$(dp_field "$1" .status.branch)" >>"$WORKDIR/branches"
  printf '%s' "$branch"
}

expect_eq() {
  if [[ "$3" == "$2" ]]; then pass "$1 = $2"; else fail "$1: expected '$2', got '$3'"; fi
}
expect_contains() {
  if [[ "$3" == *"$2"* ]]; then pass "$1 contains '$2'"; else fail "$1: expected to contain '$2', got '$3'"; fi
}
expect_not_contains() {
  if [[ "$3" != *"$2"* ]]; then pass "$1 does not contain '$2'"; else fail "$1: expected not to contain '$2', got '$3'"; fi
}
expect_nonempty() {
  if [[ -n "$2" ]]; then pass "$1 = $2"; else fail "$1 is empty"; fi
}

# Proposal names seen in the namespace, remembered so cleanup survives deleting it.
SEEN_FILE="$WORKDIR/seen"
: >"$SEEN_FILE"
remember_proposals() {
  k -n "$NS" get driftproposals -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null >>"$SEEN_FILE" || true
}

# expect_cleaned_and_closed <proposal>: a retired proposal is cleaned up
# (CleanedUp=True) and its merge request is closed.
expect_cleaned_and_closed() {
  local state
  wait_for "cleanup finished (CleanedUp)" cleaned_up "$1"
  IFS='|' read -r _ state _ _ <<<"$(mr_info "$(branch_of "$1")")"
  expect_eq "merge request state" "closed" "$state"
}

# expect_branch_file <branch> <file> <sed expression>: the file on the branch is
# the file on main with only that substitution made.
expect_branch_file() {
  local want got
  want="$(raw_file "$2" main | sed "$3")"
  got="$(raw_file "$2" "$1")"
  if [[ "$got" == "$want" ]]; then
    pass "$2 on the branch is main's file with only '$3' applied (comments and every other line unchanged)"
  else
    fail "$2 on $1 differs from main's file in more than '$3'"
  fi
}

# expect_only_additions <branch> <file>: the file on the branch differs from the
# file on main only by added lines: no line of main's file is changed or removed
# (an override adds a key, and nothing else).
expect_only_additions() {
  local removed added
  raw_file "$2" main >"$WORKDIR/main.txt"
  raw_file "$2" "$1" >"$WORKDIR/branch.txt"
  removed="$(diff "$WORKDIR/main.txt" "$WORKDIR/branch.txt" | grep -c '^<' || true)"
  added="$(diff "$WORKDIR/main.txt" "$WORKDIR/branch.txt" | grep -c '^>' || true)"
  expect_eq "lines of $2 changed or removed on the branch" "0" "$removed"
  expect_eq "lines of $2 added on the branch" "1" "$added"
}

# expect_unmapped <proposal> <reason> <message substring>...
expect_unmapped() {
  local p="$1" reason="$2" msg s
  shift 2
  expect_eq "phase" "Unmapped" "$(dp_field "$p" .status.phase)"
  expect_eq "Mapped reason" "$reason" "$(condition "$p" Mapped reason)"
  msg="$(condition "$p" Mapped message)"
  for s in "$@"; do expect_contains "Mapped message" "$s" "$msg"; done
}

# close_and_reset <iid> <proposal>: closing the merge request rejects the
# proposal; Argo CD then reverts the cluster.

# ---------------------------------------------------------------------------

# restore_main: put every original file back on main with one commit, if any differ.
restore_main() {
  python3 - "$WORKDIR" "$PID_ENC" "$ORIG_COUNT" <<'PY'
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

SUMMARY_PRINTED=false
# print_summary [aborted]: the result so far. It is also printed when the script
# stops early, with the failures up to that point.
print_summary() {
  local aborted="${1:-}"
  SUMMARY_PRINTED=true
  echo
  if [[ -n "$aborted" ]]; then
    echo "${RED}${BOLD}The test stopped early${NC} in: ${CURRENT_STEP:-the start}"
  fi
  echo "${BOLD}Summary:${NC} ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}${aborted:+ (the run did not finish)}"
  print_timing
  if [[ "$KEEP" == "true" ]]; then
    echo "Test namespace '$NS' kept: kubectl -n $NS get bfp,scm,driftproposals"
  fi
  if (( FAIL > 0 )) || [[ -n "$aborted" ]]; then
    if [[ -s "$WORKDIR/branches" ]]; then
      echo "Branch names read from the proposals (branch_of):"
      awk '!seen[$0]++' "$WORKDIR/branches" | sed 's/^/  /'
    fi
    echo "Operator log: ${LOG_FILE#"$ROOT"/}"
  fi
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
    local branch info iid state branches
    # Every branch with the Backflow prefix was created by this run: the test
    # refuses to start when there are leftovers.
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

  if [[ -n "$AUTOSYNC_OFF" ]]; then
    if restore_autosync; then echo "  automated sync of $APP is on again"; else
      echo "  ${RED}WARNING:${NC} automated sync is still off for $APP: kubectl -n $ARGOCD_NS patch application $APP --type merge -p '{\"spec\":{\"syncPolicy\":{\"automated\":{}}}}'" >&2
    fi
  fi
  if [[ -n "$RESTORE_TARGET" ]]; then
    k -n "$ARGOCD_NS" patch application "$APP" --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/sources/0/targetRevision\",\"value\":\"$RESTORE_TARGET\"}]" >/dev/null 2>&1 &&
      echo "  the chart version of $APP is $RESTORE_TARGET again" ||
      echo "  ${RED}WARNING:${NC} set spec.sources[0].targetRevision of $APP back to $RESTORE_TARGET" >&2
  fi
  if [[ "$RESTORE_CLUSTER" == "true" ]]; then
    echo "Restoring the multi-source demo resources to their Git state..."
    refresh_app hard
    if sync_app; then echo "  $APP is Synced again"; else
      echo "  ${RED}WARNING:${NC} $APP is not Synced; run: kubectl -n $ARGOCD_NS get application $APP" >&2
    fi
  fi
  if [[ "$KEEP" != "true" ]]; then
    k delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1
  fi
  if [[ "$SUMMARY_PRINTED" != "true" ]]; then
    # The script stopped before its own summary (a die, a failed command): say
    # what was done up to there, after the cleanup above.
    FAIL=$((FAIL + 1))
    print_summary aborted
    (( status != 0 )) || status=1
  fi
  if [[ -n "$WORKDIR" && "$WORKDIR" == *backflow-multi-test.* ]]; then
    rm -f "$WORKDIR/scm.py" "$WORKDIR/edit.py" "$WORKDIR/seen" "$WORKDIR/token" "$WORKDIR/paths"
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
[[ "$(app_field '{.metadata.labels.backflow\.io/demo}')" == "multi" ]] || die "Application $APP has no label backflow.io/demo=multi. Run ./hack/dev-up.sh first."
[[ "$(app_field '{.spec.sources[1].ref}')" == "values" ]] || die "Application $APP has no ref source 'values' as its second source. Run ./hack/dev-up.sh first."
[[ "$(app_field '{.status.sourceTypes[0]}')" == "Helm" ]] || die "Application $APP is not rendered as a Helm chart (sourceTypes '$(app_field '{.status.sourceTypes}')'): is ghcr.io reachable from the cluster, and does multi/values-dev.yaml exist in the demo repository?"
autosync_enabled || die "Application $APP has no automated sync; this test expects the state ./hack/dev-up.sh creates."
[[ "$(app_field '{.spec.sources[1].repoURL}')" == *"$SCM_HOST/$SCM_PROJECT"* ]] || die "$APP does not track $SCM_HOST/$SCM_PROJECT"
k -n "$TOKEN_SECRET_NS" get secret "$TOKEN_SECRET" >/dev/null 2>&1 || die "Secret $TOKEN_SECRET_NS/$TOKEN_SECRET not found. Run ./hack/dev-up.sh first."
# Argo CD must let the account read the chart: the permission this feature needs.
k -n "$ARGOCD_NS" get configmap argocd-rbac-cm -o jsonpath='{.data.policy\.csv}' 2>/dev/null | grep -q "repositories, get" ||
  die "the Argo CD account of Backflow lacks 'p, role:backflow, repositories, get, *, allow' in argocd-rbac-cm. Run ./hack/dev-up.sh first."

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
OPEN_AT_START="$(open_mrs)"
[[ "$OPEN_AT_START" == "0" ]] || die "$OPEN_AT_START merge request(s) are open in $SCM_PROJECT; close them first so this test can tell its own apart."
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
[[ -s "$WORKDIR/orig/$VALUES" ]] || die "$VALUES not found on main; run ./hack/dev-up.sh with $SCM_TOKEN_VAR set, which adds hack/demo/multi/ to the demo repository."
for key in replicaCount; do
  [[ -n "$(yaml_value "$VALUES" main "$key")" ]] || die "$VALUES has no $key"
done
grep -q "owner:" "$WORKDIR/orig/$VALUES" && grep -q "DEMO_REGION" "$WORKDIR/orig/$VALUES" ||
  die "$VALUES lacks the annotation owner or the env var DEMO_REGION that section c removes; compare it with hack/demo/multi/values-dev.yaml"
pass "main is at ${MAIN_ORIG_SHA:0:8}; $(find "$WORKDIR/orig" -type f | wc -l | tr -d ' ') file(s) saved for restoring"

# Start from a settled Argo CD: a test that begins while the Application syncs a
# new commit of main would see its first edit overwritten.
echo "  waiting for Argo CD to compare $APP against main (${MAIN_ORIG_SHA:0:8}) and to be idle..."
RESTORE_CLUSTER=true
refresh_app hard
sleep 3
# out_of_sync_resources: the resources Argo CD reports OutOfSync, for a message.
out_of_sync_resources() {
  app_field '{range .status.resources[?(@.status=="OutOfSync")]}{.kind}/{.name} {end}'
}
# Settled is not Synced: right after ./hack/dev-up.sh the Application can be
# compared against main and OutOfSync, because its automated sync applies a new
# commit only once and may have tried before the commit (or the chart) was
# reachable. Syncing it once at main is what puts it in the known state.
if ! wait_settled "$MAIN_ORIG_SHA" || [[ "$(app_sync_status)" != "Synced" ]]; then
  echo "  $APP is not settled and Synced yet (sync $(app_sync_status); OutOfSync: $(out_of_sync_resources)); syncing it at main first..."
  sync_app "$MAIN_ORIG_SHA" || die "$APP did not become Synced: $(app_state)"
  wait_settled "$MAIN_ORIG_SHA" || die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} within ${WAIT}s: $(app_state)"
fi
[[ "$(main_sha)" == "$MAIN_ORIG_SHA" ]] || die "main moved while this test was starting; run it again"
[[ "$(app_sync_status)" == "Synced" ]] || die "$APP is not Synced at main after a sync (OutOfSync: $(out_of_sync_resources)): $(app_state)"
# Settled at main is not enough: Argo CD's automated sync applies a commit once,
# the first time an Application is OutOfSync at it, unless a sync of that commit
# already happened. One sync of main spends that.
if [[ "$(synced_values)" != "$MAIN_ORIG_SHA" ]]; then
  echo "  the last sync of $APP was of ${MAIN_ORIG_SHA:0:8}'s predecessor; syncing it once so that its automated sync does not reset the first drift..."
  sync_app "$MAIN_ORIG_SHA" || die "$APP did not become Synced: $(app_state)"
fi
wait_settled "$MAIN_ORIG_SHA" || die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} after the sync within ${WAIT}s: $(app_state)"
[[ "$(synced_values)" == "$MAIN_ORIG_SHA" ]] || die "the last sync of $APP is still not of main: $(app_state)"
pass "$APP is Synced and idle at main ${MAIN_ORIG_SHA:0:8}, chart $(chart_version)"
CHART_ORIG="$(app_field '{.spec.sources[0].targetRevision}')"

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
  # Every verification of a multi-source Application is a series of renders
  # through Argo CD: give it a budget that does not depend on how fast it is today.
  "$ROOT/bin/manager" --scm-recheck-interval=10s --merge-request-poll-interval="$POLL" --render-budget=10m >"$LOG_FILE" 2>&1 &
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
# apply_policy <mode>: the policy of the test. scratchBranches stays at its
# default, Auto: allowed in MergeRequest mode, not in ReportOnly.
apply_policy() {
  k apply -f - >/dev/null <<YAML
apiVersion: backflow.io/v1alpha1
kind: BackflowPolicy
metadata:
  name: $POLICY
  namespace: $NS
spec:
  argoCDNamespace: $ARGOCD_NS
  mode: $1
  batchWindow: $WINDOW
  applications:
    selector:
      matchLabels:
        backflow.io/demo: multi
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
}
apply_policy MergeRequest
wait_for "ScmConnection is Ready (token accepted by $SCM_HOST)" k -n "$NS" wait "scm/$SCM_NAME" --for=condition=Ready --timeout=1s
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
policy_matched() {
  # The ref source has its own ScmConnection, and the chart (a registry) none.
  local out
  out="$(k -n "$NS" get bfp "$POLICY" -o jsonpath='{range .status.applications[*].sources[*]}{.position}={.ref}={.scmConnection}{"\n"}{end}')"
  [[ "$out" == *"2=values=$SCM_NAME"* && "$out" == *"1=="* ]]
}
wait_for "policy lists the sources of $APP and matched the repository of \$values to the ScmConnection" policy_matched
START_TS="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------------------
# helpers of the sections

# expect_mapping_multi <proposal> <file> <location>: mapped with the HelmValues
# strategy to exactly one edit, in the repository of the ref source.
expect_mapping_multi() {
  expect_eq "strategy" "HelmValues" "$(dp_field "$1" .status.mapping.strategy)"
  expect_eq "mapping verified" "true" "$(dp_field "$1" .status.mapping.verified)"
  expect_eq "edited files" "$2" "$(dp_field "$1" '.status.mapping.edits[*].file')"
  expect_eq "edited keys" "$3" "$(dp_field "$1" '.status.mapping.edits[*].location')"
  expect_contains "repository written to (status.mapping.repoURL)" "$SCM_HOST/$SCM_PROJECT" "$(dp_field "$1" .status.mapping.repoURL)"
}

# expect_comments_kept <branch> <file>: every comment line of main's file is still there.
expect_comments_kept() {
  local line missing=0
  raw_file "$2" main >"$WORKDIR/main.txt"
  raw_file "$2" "$1" >"$WORKDIR/branch.txt"
  while IFS= read -r line; do
    grep -qxF -- "$line" "$WORKDIR/branch.txt" || { missing=$((missing + 1)); note "comment lost: $line"; }
  done < <(grep -E '^ *#' "$WORKDIR/main.txt")
  expect_eq "comment lines of $2 lost on the branch" "0" "$missing"
}

# close_and_reset <iid> <proposal>: closing the merge request rejects the
# proposal; Argo CD then reverts the cluster.
close_and_reset() {
  gl PUT "$(project_path "/merge_requests/$1")" '{"state_event":"close"}' >/dev/null || fail "could not close !$1"
  wait_for "proposal is Rejected" phase_is "$2" Rejected
  reset_cluster
}

# drift_replicas: scale the Deployment by two and wait for its proposal; sets
# GIT_REPLICAS, NEW_REPLICAS and P (empty when there is none).
drift_replicas() {
  GIT_REPLICAS="$(yaml_value "$VALUES" main replicaCount)"
  NEW_REPLICAS="$((GIT_REPLICAS + 2))"
  require_drift "$1" "$GIT_REPLICAS" "$(live_replicas)" "$NEW_REPLICAS"
  expect_eq "Git and live replicas agree before the drift" "$GIT_REPLICAS" "$(live_replicas)"
  k -n "$APP_NS" scale "deployment/$DEPLOY" --replicas="$NEW_REPLICAS" >/dev/null
  refresh_app
}

# ---------------------------------------------------------------------------
if section_wanted a; then
step "a. A replica drift becomes a merge request on $VALUES, in the repository of the ref source, and is merged"
drift_replicas a
wait_for "proposal is Proposed (replicas $GIT_REPLICAS -> $NEW_REPLICAS)" has_proposal "$APP" "$DEPLOY" Proposed "/spec/replicas:Replace:$GIT_REPLICAS>$NEW_REPLICAS"
remember_proposals
P_A="$(proposal_with "$APP" "$DEPLOY" Proposed "$NEW_REPLICAS" | head -n 1)"
if [[ -n "$P_A" ]]; then
  BRANCH_A="$(branch_of "$P_A")"
  IFS='|' read -r IID_A STATE_A _ _ <<<"$(mr_info "$BRANCH_A")"
  expect_nonempty "merge request from $BRANCH_A" "$IID_A"
  expect_eq "merge request state" "opened" "$STATE_A"
  expect_eq "Mapped condition" "True" "$(condition "$P_A" Mapped status)"
  expect_eq "Proposed condition" "True" "$(condition "$P_A" Proposed status)"
  expect_mapping_multi "$P_A" "$VALUES" "replicaCount"
  expect_eq "the sources of the proposal" "Render Values" "$(dp_field "$P_A" '.spec.sources[*].role')"
  expect_branch_file "$BRANCH_A" "$VALUES" "s/^replicaCount: $GIT_REPLICAS/replicaCount: $NEW_REPLICAS/"
  expect_eq "main was not touched" "$MAIN_ORIG_SHA" "$(main_sha)"
  wait_for "no scratch branch is left in the demo repository" no_scratch_branches
  BODY_A="$(gl GET "$(project_path "/merge_requests/$IID_A")" | jx 'd["description"]')"
  expect_contains "description (sources)" "**Sources:** chart podinfo" "$BODY_A"
  expect_contains "description (repository of the values)" "values in https://$SCM_HOST/$SCM_PROJECT" "$BODY_A"
  expect_contains "description" "Verified by rendering through Argo CD" "$BODY_A"
  expect_contains "description (value file)" "$VALUES" "$BODY_A"
  expect_contains "description (diff)" "+replicaCount: $NEW_REPLICAS" "$BODY_A"

  merged=false
  for _ in $(seq 1 20); do
    if gl PUT "$(project_path "/merge_requests/$IID_A/merge")" '{"should_remove_source_branch":false}' >/dev/null 2>&1; then
      merged=true; break
    fi
    sleep 3
  done
  if [[ "$merged" == "true" ]]; then pass "merged !$IID_A through the API"; else fail "could not merge !$IID_A"; fi
  MERGE_A="$(gl GET "$(project_path "/merge_requests/$IID_A")" | jx 'd.get("merge_commit_sha") or d.get("squash_commit_sha") or d["sha"]')"
  wait_for "proposal is Merged" phase_is "$P_A" Merged
  expect_eq "status.commitSHA is the merge commit" "$MERGE_A" "$(dp_field "$P_A" .status.commitSHA)"
  BEFORE_A="$(proposal_count "$APP")"
  wait_for "$APP is Synced at the merge commit" app_synced_at "$MERGE_A"
  expect_eq "live replicas are unchanged by the sync" "$NEW_REPLICAS" "$(live_replicas)"
  # One sync operation records the merge commit as the last synced revision;
  # without it the Application would still look behind and detection for it
  # would pause (the commit touched a file its render depends on).
  if sync_app "$MERGE_A"; then pass "one sync of $APP at the merge commit"; else fail "$APP did not stay Synced"; fi
  sleep "$QUIET"
  expect_eq "proposal stays Merged" "Merged" "$(dp_field "$P_A" .status.phase)"
  expect_eq "proposals for $APP (no new one)" "$BEFORE_A" "$(proposal_count "$APP")"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
fi
reset_cluster

fi

# ---------------------------------------------------------------------------
if section_wanted b; then
step "b. A chart default (ui.color) becomes an override in $VALUES"
GIT_COLOR="#34577c"
NEW_COLOR="#ff0000"
require_drift b "$GIT_COLOR" "$(live_env PODINFO_UI_COLOR)" "$NEW_COLOR"
expect_eq "the live color is the chart's default" "$GIT_COLOR" "$(live_env PODINFO_UI_COLOR)"
expect_eq "$VALUES does not set ui.color" "" "$(yaml_value "$VALUES" main color)"
k -n "$APP_NS" set env "deployment/$DEPLOY" "PODINFO_UI_COLOR=$NEW_COLOR" >/dev/null
refresh_app
wait_for "proposal is Proposed (PODINFO_UI_COLOR $GIT_COLOR -> $NEW_COLOR)" has_proposal "$APP" "$DEPLOY" Proposed "Replace:\"$GIT_COLOR\">\"$NEW_COLOR\""
remember_proposals
P_B="$(proposal_with "$APP" "$DEPLOY" Proposed "\"$NEW_COLOR\"" | head -n 1)"
if [[ -n "$P_B" ]]; then
  BRANCH_B="$(branch_of "$P_B")"
  IFS='|' read -r IID_B STATE_B _ _ <<<"$(mr_info "$BRANCH_B")"
  expect_eq "merge request state" "opened" "$STATE_B"
  expect_mapping_multi "$P_B" "$VALUES" "ui.color"
  expect_only_additions "$BRANCH_B" "$VALUES"
  expect_comments_kept "$BRANCH_B" "$VALUES"
  BODY_B="$(gl GET "$(project_path "/merge_requests/$IID_B")" | jx 'd["description"]')"
  expect_contains "description" "Overrides a chart default" "$BODY_B"
  expect_contains "description (the default and where it is defined)" "ui.color" "$BODY_B"
  wait_for "no scratch branch is left in the demo repository" no_scratch_branches
  close_and_reset "$IID_B" "$P_B"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted c; then
step "c. Maps and lists: a pod annotation and an env var of $DEPLOY come from values"
echo "  c1. a pod annotation removed: the key is removed from podAnnotations..."
k -n "$APP_NS" patch deployment "$DEPLOY" --type json -p '[{"op":"remove","path":"/spec/template/metadata/annotations/owner"}]' >/dev/null
refresh_app
wait_for "proposal is Proposed (annotation owner removed)" has_proposal "$APP" "$DEPLOY" Proposed "/spec/template/metadata/annotations/owner:Remove:"
remember_proposals
P_C1="$(proposal_with "$APP" "$DEPLOY" Proposed "/spec/template/metadata/annotations/owner:Remove:" | head -n 1)"
if [[ -n "$P_C1" ]]; then
  BRANCH_C1="$(branch_of "$P_C1")"
  IFS='|' read -r IID_C1 STATE_C1 _ _ <<<"$(mr_info "$BRANCH_C1")"
  expect_eq "merge request state" "opened" "$STATE_C1"
  expect_mapping_multi "$P_C1" "$VALUES" "podAnnotations.owner"
  raw_file "$VALUES" "$BRANCH_C1" >"$WORKDIR/branch.txt"
  if grep -qE '^  owner:' "$WORKDIR/branch.txt"; then fail "the branch still has the annotation owner"; else pass "the annotation owner is gone from the branch"; fi
  if grep -qxF '  team: platform' "$WORKDIR/branch.txt"; then pass "the other annotation stays, in block style"; else fail "the annotation team is not on the branch as a block entry"; fi
  expect_comments_kept "$BRANCH_C1" "$VALUES"
  wait_for "no scratch branch is left in the demo repository" no_scratch_branches
  close_and_reset "$IID_C1" "$P_C1"
fi

echo "  c2. an env var removed: the list is written back without it..."
k -n "$APP_NS" set env "deployment/$DEPLOY" DEMO_REGION- >/dev/null
refresh_app
wait_for "proposal is Proposed (env var DEMO_REGION removed)" has_proposal "$APP" "$DEPLOY" Proposed "/spec/template/spec/containers/0/env:Replace:"
remember_proposals
P_C2="$(proposal_with "$APP" "$DEPLOY" Proposed "/spec/template/spec/containers/0/env:Replace:" | head -n 1)"
if [[ -n "$P_C2" ]]; then
  BRANCH_C2="$(branch_of "$P_C2")"
  IFS='|' read -r IID_C2 STATE_C2 _ _ <<<"$(mr_info "$BRANCH_C2")"
  expect_eq "merge request state" "opened" "$STATE_C2"
  expect_mapping_multi "$P_C2" "$VALUES" "extraEnvs"
  raw_file "$VALUES" "$BRANCH_C2" >"$WORKDIR/branch.txt"
  if grep -qxF '  - name: DEMO_MODE' "$WORKDIR/branch.txt"; then pass "the list is in block style"; else fail "the branch has no block style '  - name: DEMO_MODE'"; fi
  if grep -qxF '    value: "on"' "$WORKDIR/branch.txt"; then pass "on stays a quoted string (YAML 1.1 reads a plain on as a boolean)"; else fail "the branch has no '    value: \"on\"'"; fi
  if grep -q 'DEMO_REGION' "$WORKDIR/branch.txt"; then fail "the env var DEMO_REGION is still on the branch"; else pass "the env var DEMO_REGION is gone from the branch"; fi
  expect_comments_kept "$BRANCH_C2" "$VALUES"
  wait_for "no scratch branch is left in the demo repository" no_scratch_branches
  close_and_reset "$IID_C2" "$P_C2"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted d; then
step "d. ReportOnly: Unmapped with ScratchBranchesDisabled, and no scratch branch is pushed"
apply_policy ReportOnly
wait_for "policy is Ready in ReportOnly mode" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
BEFORE_D="$(proposal_count "$APP")"
drift_replicas d
wait_for "proposal is Unmapped (replicas $GIT_REPLICAS -> $NEW_REPLICAS)" has_proposal "$APP" "$DEPLOY" Unmapped "/spec/replicas:Replace:$GIT_REPLICAS>$NEW_REPLICAS"
remember_proposals
P_D="$(proposal_with "$APP" "$DEPLOY" Unmapped "$NEW_REPLICAS" | head -n 1)"
if [[ -n "$P_D" ]]; then
  expect_unmapped "$P_D" ScratchBranchesDisabled "spec.mapping.scratchBranches"
  expect_eq "proposals for $APP (one new)" "$((BEFORE_D + 1))" "$(proposal_count "$APP")"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
  sleep "$QUIET"
  expect_eq "scratch branches ever pushed" "" "$(scratch_branches)"
fi
apply_policy MergeRequest
wait_for "policy is Ready in MergeRequest mode again" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
reset_cluster

fi

# ---------------------------------------------------------------------------
if section_wanted e; then
step "e. Scratch branches are deleted after a mapping, and no pipeline ran for one"
drift_replicas e
wait_for "proposal is Proposed (replicas $GIT_REPLICAS -> $NEW_REPLICAS)" has_proposal "$APP" "$DEPLOY" Proposed "/spec/replicas:Replace:$GIT_REPLICAS>$NEW_REPLICAS"
remember_proposals
P_E="$(proposal_with "$APP" "$DEPLOY" Proposed "$NEW_REPLICAS" | head -n 1)"
wait_for "no scratch branch is left in the demo repository" no_scratch_branches
refresh_log
if [[ -f "$LOG_FILE" ]]; then
  expect_contains "operator log (the verification, with its renders)" "Verification finished" "$(grep "Verification finished" "$LOG_FILE" | tail -n 1)"
  note "$(grep "Verification finished" "$LOG_FILE" | tail -n 1 | cut -c1-300)"
fi
# The scratch commits carry [skip ci] (and GitLab gets the ci.skip push option), so
# no pipeline (GitLab) or workflow run (GitHub) may have started for them.
CODE_E="$(gl_status GET "$(project_path "/pipelines?per_page=1")")"
if [[ "$CODE_E" == "200" ]]; then
  PIPES_E="$(gl GET "$(project_path "/pipelines?updated_after=$START_TS&per_page=100")" |
    jx 'len([p for p in d if p.get("ref", "").startswith("'"$SCRATCH_PREFIX"'")])')"
  expect_eq "pipelines or workflow runs that ran for a scratch branch" "0" "$PIPES_E"
elif [[ "$SCM" == "github" ]]; then
  note "SKIPPED the workflow runs check: GitHub answered HTTP $CODE_E (the token needs Actions: read to list them); only the branches are checked"
else
  note "the pipelines of the project cannot be read (HTTP $CODE_E): only the branches are checked"
fi
if [[ -n "$P_E" ]]; then
  IFS='|' read -r IID_E _ _ _ <<<"$(mr_info "$(branch_of "$P_E")")"
  close_and_reset "$IID_E" "$P_E"
else
  reset_cluster
fi

fi

# ---------------------------------------------------------------------------
if section_wanted f; then
step "f. Git ahead: a commit to the value file pauses $APP; a chart version bump is a change of the Application"
wait_idle_at "$(main_sha)" || fail "$APP is not idle and synced at main before the commits: $(app_state)"
if disable_autosync; then pass "automated sync of $APP switched off"; else fail "could not switch automated sync of $APP off"; fi

echo "  f1. a commit to the value file ($VALUES)..."
GIT_REPLICAS_F="$(yaml_value "$VALUES" main replicaCount)"
NEW_REPLICAS_F="$((GIT_REPLICAS_F + 1))"
AHEAD_BEFORE="$(event_count GitAhead)"
PROPOSALS_BEFORE="$(proposal_count "$APP")"
SHA_F1="$(commit_edit "$VALUES" "^replicaCount: .*" "replicaCount: $NEW_REPLICAS_F" "test: the value file of the multi-source demo (Backflow e2e)")" || SHA_F1=""
expect_nonempty "commit to $VALUES" "$SHA_F1"
if [[ -n "$SHA_F1" ]]; then
  wait_for "$APP is OutOfSync, comparing \$values against the commit but not synced to it" app_ahead_at "$SHA_F1"
  wait_for "an Event says that detection for $APP is paused" event_exceeds GitAhead "$AHEAD_BEFORE"
  expect_contains "Event text" "its Helm chart renders from changed" "$(events_of GitAhead | grep -- " $APP " | tail -n 1)"
  expect_eq "proposals for $APP (none new)" "$PROPOSALS_BEFORE" "$(proposal_count "$APP")"
  sync_app "$SHA_F1" || fail "$APP did not become Synced after f1: $(app_state)"
  wait_settled "$SHA_F1" || fail "Argo CD did not settle at ${SHA_F1:0:8}: $(app_state)"
fi

echo "  f2. a commit to a file the render does not read (helm/demo/values-dev.yaml)..."
AHEAD_BEFORE="$(event_count GitAhead)"
SHA_F2="$(commit_edit helm/demo/values-dev.yaml "^# Development environment.*" "# Development environment (edited by the Backflow e2e test)." "test: a file the multi-source demo does not read (Backflow e2e)")" || SHA_F2=""
expect_nonempty "commit to helm/demo/values-dev.yaml" "$SHA_F2"
if [[ -n "$SHA_F2" ]]; then
  wait_for "$APP compares \$values against the commit" app_compared_at "$SHA_F2"
  expect_eq "$APP is not synced to it (manual sync)" "true" "$([[ "$(synced_values)" != "$SHA_F2" ]] && echo true || echo false)"
  GIT_R="$(yaml_value "$VALUES" main replicaCount)"
  NEW_R="$((GIT_R + 2))"
  require_drift f2 "$GIT_R" "$(live_replicas)" "$NEW_R"
  k -n "$APP_NS" scale "deployment/$DEPLOY" --replicas="$NEW_R" >/dev/null
  refresh_app
  wait_for "a real drift is proposed although Git is ahead (replicas $GIT_R -> $NEW_R)" has_proposal "$APP" "$DEPLOY" 'Detected|Mapping|Proposed' "/spec/replicas:Replace:$GIT_R>$NEW_R"
  remember_proposals
  P_F2="$(proposal_with "$APP" "$DEPLOY" 'Detected|Mapping|Proposed' "$NEW_R" | head -n 1)"
  expect_eq "new GitAhead events for $APP (none)" "$AHEAD_BEFORE" "$(event_count GitAhead)"
  k -n "$APP_NS" scale "deployment/$DEPLOY" --replicas="$GIT_R" >/dev/null
  if [[ -n "$P_F2" ]]; then
    wait_for "and retired when the live value goes back" phase_is "$P_F2" Reverted
    expect_cleaned_and_closed "$P_F2"
  fi
  sync_app "$SHA_F2" || fail "$APP did not become Synced after f2: $(app_state)"
  wait_settled "$SHA_F2" || fail "Argo CD did not settle at ${SHA_F2:0:8}: $(app_state)"
fi

echo "  f3. a new chart version in the Application..."
CHANGED_BEFORE="$(event_count ApplicationChanged)"
AHEAD_BEFORE="$(event_count GitAhead)"
PROPOSALS_BEFORE="$(proposal_count "$APP")"
NEW_CHART="6.7.0"
[[ "$CHART_ORIG" != "$NEW_CHART" ]] || NEW_CHART="6.6.0"
RESTORE_TARGET="$CHART_ORIG"
k -n "$ARGOCD_NS" patch application "$APP" --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/sources/0/targetRevision\",\"value\":\"$NEW_CHART\"}]" >/dev/null ||
  fail "could not change the chart version of the Application"
refresh_app hard
chart_is() { refresh_app hard; [[ "$(chart_version)" == "$1" ]]; }
wait_for "Argo CD compares against chart $NEW_CHART" chart_is "$NEW_CHART"
wait_for "an Event reports a change of the Application" event_exceeds ApplicationChanged "$CHANGED_BEFORE"
expect_contains "Event text" "moved from $CHART_ORIG to $NEW_CHART" "$(events_of ApplicationChanged | grep -- " $APP " | tail -n 1)"
expect_eq "GitAhead events for $APP (no new one: it is not Git being ahead)" "$AHEAD_BEFORE" "$(event_count GitAhead)"
expect_eq "proposals for $APP (none new while it is paused)" "$PROPOSALS_BEFORE" "$(proposal_count "$APP")"
k -n "$ARGOCD_NS" patch application "$APP" --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/sources/0/targetRevision\",\"value\":\"$CHART_ORIG\"}]" >/dev/null ||
  fail "could not restore the chart version of the Application"
RESTORE_TARGET=""
restore_autosync && pass "automated sync of $APP is on again" || fail "could not switch automated sync of $APP on again"
refresh_app hard
wait_for "Argo CD compares against chart $CHART_ORIG again" chart_is "$CHART_ORIG"
reset_cluster

fi

# ---------------------------------------------------------------------------
if section_wanted g; then
step "g. The $SCM_LABEL token appears nowhere that Backflow keeps or shows"
refresh_log
# token_in: reads stdin; succeeds when it holds the token. The token comes from
# the environment, never from an argument.
token_in() { python3 -c 'import os, sys; sys.exit(0 if os.environ[os.environ["SCM_TOKEN_VAR"]] in sys.stdin.read() else 1)'; }
if token_in <"$LOG_FILE" 2>/dev/null; then fail "the $SCM_LABEL token appears in the operator log"; else pass "the $SCM_LABEL token does not appear in the operator log"; fi
if k -n "$NS" get driftproposals -o yaml | token_in; then fail "the $SCM_LABEL token appears in a DriftProposal"; else pass "the $SCM_LABEL token does not appear in any DriftProposal"; fi
if k -n "$NS" get events -o yaml | token_in; then fail "the $SCM_LABEL token appears in an Event"; else pass "the $SCM_LABEL token does not appear in any Event"; fi
if k -n "$NS" get bfp,scm -o yaml | token_in; then fail "the $SCM_LABEL token appears in the policy or the connection"; else pass "the $SCM_LABEL token does not appear in the policy or the connection"; fi
if gl GET "$(project_path "/merge_requests?state=all&per_page=100")" | token_in; then fail "the $SCM_LABEL token appears in a merge request"; else pass "the $SCM_LABEL token does not appear in any merge request"; fi
# Secrets of the rendered manifests: none is rendered here, so only look that no
# line of the log quotes a manifest.
if grep -q 'kind: Secret\|"kind":"Secret"' "$LOG_FILE" 2>/dev/null; then fail "the operator log quotes a Secret manifest"; else pass "the operator log quotes no Secret manifest"; fi

fi

# ---------------------------------------------------------------------------
print_summary
if (( FAIL > 0 )); then
  exit 1
fi
