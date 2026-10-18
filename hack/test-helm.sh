#!/usr/bin/env bash
#
# Backflow - Helm values mapping end-to-end test
#
# Runs the full path for the Helm demo Applications against the real demo
# repository on gitlab.com. Backflow renders the chart through Argo CD to
# verify every edit, so this needs the Argo CD of ./hack/dev-up.sh.
#
#   a. A REGION drift on demo-helm becomes a merge request with strategy
#      HelmValues. The branch edits only global.region in
#      helm/demo/values-dev.yaml (every other line and comment is unchanged);
#      the body says the edit was verified by rendering through Argo CD and
#      lists REGION. Merging it makes the proposal Merged; after Argo CD syncs
#      the live value is unchanged and there is no new proposal.
#   b. LOG_LEVEL and LOG_LEVEL_UP changed together (one key feeds both): the
#      merge request edits config.logLevel. Closing it makes the proposal
#      Rejected; a sync reverts the cluster.
#   c. LOG_LEVEL alone is Unmapped (NoVerifiedCandidate): the only candidate,
#      config.logLevel, also moves LOG_LEVEL_UP, which did not drift. The
#      message names that near miss.
#   d. A key of the subchart: TIMEOUT_SECONDS in demo-helm-sidecar is mapped to
#      demo-sidecar.timeoutSeconds in the parent chart's values file.
#   e. EQUAL_A has two candidates with the same value (colorA and colorB); only
#      config.colorA verifies.
#   f. AMBIGUOUS is fed by two keys that give the same result: Unmapped
#      (AmbiguousCandidates), naming minA and minB.
#   g. TIMEOUT exists only in the chart's own values.yaml, and FEATURE is set by
#      a helm parameter of the Application: both Unmapped (NoVerifiedCandidate).
#   h. A Deployment: scaling demo-helm is mapped to replicaCount and stays an
#      integer. Changing the container image with kubectl set image changes only
#      a part of the image string ("repository:tag"); the key image.tag holds that
#      part. With the default policy the edit is unmapped, because the same tag
#      also feeds the version label of the Deployment and of its pods: the
#      message names image.tag as the nearest edit and the two labels as what it
#      would also change. With spec.helm.allowCollateralChanges: [Labels] the
#      merge request edits only image.tag, and its body lists the label changes.
#      Changing the version label alone finds image.tag too, and the image is
#      what the edit would also change.
#   i. Git ahead, with automated sync off for the three Applications: a commit
#      to another service's values file (helm/shared/services/beta/dev.yaml)
#      pauses only shared-beta, not shared-alpha; a commit to another
#      environment's file (helm/demo/values-prod.yaml) does not pause demo-helm;
#      a commit to helm/demo/values-dev.yaml does, until it is synced.
#
#   j. Partial proposals: scaling demo-helm and changing its image at the same
#      time. With the default policy the proposal is Unmapped (the image has no
#      verified edit). With spec.mapping.allowPartial: true the merge request is
#      marked "(partial)" and edits only replicaCount; its body lists the image
#      under "Not included in this merge request". After it is merged and Argo CD
#      compares against the merge commit (automated sync off), the image alone is
#      a new Unmapped proposal, and no duplicate of the merged one appears.
#
# Requires the environment from ./hack/dev-up.sh (the Applications demo-helm,
# shared-alpha and shared-beta, label env=helm-test) and GITLAB_TOKEN with the
# api and write_repository scopes (owner or maintainer of the demo project).
# Without GITLAB_TOKEN the test is skipped with a message and exits 0.
#
# Everything the script creates is removed on exit: branches, merge requests,
# the test label, the test namespace. Content that was merged or committed to
# main is restored with a follow-up commit through the API. Because commits
# cannot be undone, those restore commits stay in the history of main.
#
# The operator runs locally (bin/manager, polling merge requests every POLL)
# unless IN_CLUSTER=true, which uses the deployed operator (polling every two
# minutes by default; raise WAIT accordingly).
#
# Usage (from the repository root):
#   GITLAB_TOKEN=... ./hack/test-helm.sh
#   KEEP=true ./hack/test-helm.sh        # keep the test namespace

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
NS="${NS:-backflow-helm-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
POLICY="helm-policy"
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
# A drift becomes a proposal after it has been stable for the policy's
# batchWindow (default 30s, which this test uses), across two observations.
WINDOW="${WINDOW:-30s}"
# Seconds to wait for Argo CD, the operator and GitLab to react.
if [[ "$IN_CLUSTER" == "true" ]]; then WAIT="${WAIT:-360}"; else WAIT="${WAIT:-180}"; fi
# Seconds to wait before asserting that nothing happened.
QUIET="${QUIET:-25}"

# The Applications under test, their namespaces and the resources they render.
APPS="demo-helm shared-alpha shared-beta"
DEMO_APP="demo-helm"
ALPHA_APP="shared-alpha"
BETA_APP="shared-beta"
DEMO_NS="demo-helm"
ALPHA_NS="shared-alpha"
CM="demo-helm-config"
SIDECAR_CM="demo-helm-sidecar"
ALPHA_CM="shared-alpha-config"
VALUES_DEV="helm/demo/values-dev.yaml"
VALUES_PROD="helm/demo/values-prod.yaml"
ALPHA_VALUES="helm/shared/services/alpha/dev.yaml"
BETA_VALUES="helm/shared/services/beta/dev.yaml"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG_DIR="$ROOT/tmp"
LOG_FILE="$LOG_DIR/test-helm-manager.log"

if [[ -z "${GITLAB_TOKEN:-}" ]]; then
  echo "SKIPPED: GITLAB_TOKEN is not set."
  echo "This test pushes branches and merges into $GITLAB_PROJECT on gitlab.com and needs a"
  echo "token with the api and write_repository scopes: GITLAB_TOKEN=... $0"
  exit 0
fi

MANAGER_PID=""
PF_PID=""
WORKDIR=""
RESTORE_CLUSTER=false
LABEL_PREEXISTED=true
# Applications whose automated sync this run switched off (space separated).
AUTOSYNC_OFF=""
MAIN_ORIG_SHA=""
PASS=0
FAIL=0

if [[ -t 1 ]]; then
  GREEN=$'\e[32m'; RED=$'\e[31m'; BOLD=$'\e[1m'; NC=$'\e[0m'
else
  GREEN=""; RED=""; BOLD=""; NC=""
fi

CURRENT_STEP=""
step() { CURRENT_STEP="$*"; echo; echo "${BOLD}▶ $*${NC}"; }
pass() { echo "  ${GREEN}✔${NC} $*"; PASS=$((PASS + 1)); }
fail() { echo "  ${RED}✘${NC} $*"; FAIL=$((FAIL + 1)); }
die()  { echo "${RED}ERROR:${NC} $*" >&2; exit 1; }
note() { echo "    $*"; }

k() { kubectl --context "kind-$CLUSTER_NAME" "$@"; }

# ---------------------------------------------------------------------------
# GitLab API. The token travels in the environment only: never in an argument
# list, a log or a file other than the 0600 Secret input.
#
# gitlab.com is sometimes slow or down for a minute, so gl.py retries: 4 attempts
# with a 60s read timeout and a growing delay (GITLAB_ATTEMPTS, GITLAB_TIMEOUT,
# GITLAB_BACKOFF). Reads and idempotent writes (closing a merge request,
# deleting a branch) are repeated freely. A write whose outcome is unknown after
# a timeout or a 5xx is only repeated after checking that it did not happen: a
# commit is looked for at the head of its branch (same message, on top of the
# head it was made against), a merge by the state of the merge request.
# ---------------------------------------------------------------------------

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/backflow-helm-test.XXXXXX")"
chmod 700 "$WORKDIR"
mkdir -p "$WORKDIR/orig"

cat >"$WORKDIR/gl.py" <<'PY'
import http.client, json, os, re, sys, time, urllib.error, urllib.parse, urllib.request

base = os.environ.get("GITLAB_API", "https://gitlab.com/api/v4")
token = os.environ["GITLAB_TOKEN"]
# gitlab.com is sometimes slow or unavailable for a minute. Reads and
# idempotent writes are retried with a growing delay; a write whose outcome is
# unknown (a commit, a merge) is only repeated after checking that it did not
# happen. GITLAB_ATTEMPTS, GITLAB_TIMEOUT and GITLAB_BACKOFF tune this.
ATTEMPTS = int(os.environ.get("GITLAB_ATTEMPTS", "4"))
TIMEOUT = float(os.environ.get("GITLAB_TIMEOUT", "60"))
BACKOFF = float(os.environ.get("GITLAB_BACKOFF", "3"))
RETRIABLE = (0, 429, 500, 502, 503, 504)


def log(msg):
    sys.stderr.write("gitlab: " + msg + "\n")


def once(method, path, body=None):
    """One attempt. Returns (status, text, retry_after); status 0 is a network
    error or a timeout (text says which)."""
    req = urllib.request.Request(base + path, data=body.encode() if body else None, method=method,
                                 headers={"PRIVATE-TOKEN": token, "Content-Type": "application/json"})
    try:
        resp = urllib.request.urlopen(req, timeout=TIMEOUT)
        return resp.status, resp.read().decode(), None
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(), e.headers.get("Retry-After")
    except (urllib.error.URLError, TimeoutError, OSError, http.client.HTTPException) as e:
        return 0, "%s: %s" % (type(e).__name__, e), None


def get_json(path):
    """A GET with retries; None when it cannot be read."""
    code, text = send("GET", path)
    if code >= 400 or code == 0:
        return None
    return json.loads(text)


def commit_check(pid, body):
    """For POST .../repository/commits: remember the head of the branch, and
    return a function that says whether this very commit has since landed on it."""
    spec = json.loads(body)
    branch, message = spec["branch"], spec["commit_message"].rstrip("\n")
    head_path = "/projects/%s/repository/branches/%s" % (pid, urllib.parse.quote(branch, safe=""))
    before = get_json(head_path)
    pre = before["commit"]["id"] if before else None

    def landed():
        now = get_json(head_path)
        if not now or not pre or now["commit"]["id"] == pre:
            return None
        info = get_json("/projects/%s/repository/commits/%s" % (pid, now["commit"]["id"]))
        if info and info.get("message", "").rstrip("\n") == message and pre in info.get("parent_ids", []):
            return 201, json.dumps(info)
        return None
    return landed


def merge_check(pid, iid):
    def merged():
        mr = get_json("/projects/%s/merge_requests/%s" % (pid, iid))
        if mr and mr.get("state") == "merged":
            return 200, json.dumps(mr)
        return None
    return merged


def send(method, path, body=None):
    """The request with retries. Returns (status, text)."""
    check = None
    if method == "POST" and (m := re.fullmatch(r"/projects/([^/]+)/repository/commits", path)):
        check = commit_check(m.group(1), body)
    elif method == "PUT" and (m := re.fullmatch(r"/projects/([^/]+)/merge_requests/(\d+)/merge", path)):
        check = merge_check(m.group(1), m.group(2))
    code, text, retry_after = 0, "", None
    for attempt in range(ATTEMPTS):
        if attempt:
            delay = BACKOFF * 2 ** (attempt - 1)
            if retry_after and retry_after.isdigit():
                delay = max(delay, min(int(retry_after), 60))
            time.sleep(delay)
            if check:  # the previous attempt may have worked although we did not see its answer
                done = check()
                if done:
                    log("%s %s: the earlier attempt had worked" % (method, path.split("?")[0]))
                    return done
        code, text, retry_after = once(method, path, body)
        if code not in RETRIABLE:
            # A DELETE that finds the thing gone after an unseen first attempt is done.
            if method == "DELETE" and attempt and code == 404:
                return 204, ""
            return code, text
        log("%s %s: attempt %d of %d failed (%s)" % (method, path.split("?")[0], attempt + 1, ATTEMPTS,
                                                      "HTTP %d" % code if code else text[:120]))
    if check:
        done = check()
        if done:
            log("%s %s: the last attempt had worked" % (method, path.split("?")[0]))
            return done
    return code, text


if __name__ == "__main__":
    method, path = sys.argv[1], sys.argv[2]
    body = sys.argv[3] if len(sys.argv) > 3 else None
    status_only = method.startswith("STATUS-")
    if status_only:
        method = method[len("STATUS-"):]
    code, text = send(method, path, body)
    if status_only:
        print(code)
        sys.exit(0)
    if code >= 400 or code == 0:
        sys.stderr.write("HTTP %d: %s\n" % (code, text[:300]))
        sys.exit(22)
    sys.stdout.write(text)
PY

# edit.py <project id> <file> <regex> <replacement> <message>: commit to main
# the file with the first match of the regex (multi-line mode) replaced. Prints
# the new commit's SHA. The token is read by gl.py from the environment.
cat >"$WORKDIR/edit.py" <<'PY'
import json, os, re, subprocess, sys, urllib.parse

pid, path, pattern, repl, message = sys.argv[1:6]
gl = os.path.join(os.path.dirname(os.path.abspath(__file__)), "gl.py")

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

PID_ENC="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$GITLAB_PROJECT")"
enc() { python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$1"; }
gl() { python3 "$WORKDIR/gl.py" "$@"; }
# gl_status <METHOD> <path>: HTTP status code only.
gl_status() { python3 "$WORKDIR/gl.py" "STATUS-$1" "$2"; }
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
# Cluster helpers. Each takes the Application it is about.
# ---------------------------------------------------------------------------

refresh_log() {
  if [[ "$IN_CLUSTER" == "true" ]]; then
    mkdir -p "$LOG_DIR"
    k -n "$MANAGER_NS" logs "deploy/$MANAGER_DEPLOY" >"$LOG_FILE" 2>&1 || true
  fi
}

# show_state: what the policy's proposals are doing, and the last thing the
# operator said about failing to map or propose. Printed when a wait times out,
# so the log says why without a second look.
show_state() {
  refresh_log
  echo "    proposals of the policy:"
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" -o jsonpath='{range .items[*]}{.metadata.name}{" phase="}{.status.phase}{" Mapped="}{.status.conditions[?(@.type=="Mapped")].reason}{" Proposed="}{.status.conditions[?(@.type=="Proposed")].reason}{"\n"}{end}' 2>/dev/null | sed 's/^/      /'
  if [[ -f "$LOG_FILE" ]]; then
    echo "    last operator messages about mapping or proposing:"
    grep -E "Cannot (map|propose)|Proposal (failed|unmapped)|Reconciler error" "$LOG_FILE" 2>/dev/null | tail -n 3 | cut -c1-400 | sed 's/^/      /'
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
  k -n "$ARGOCD_NS" annotate application "$1" "argocd.argoproj.io/refresh=${2:-normal}" --overwrite >/dev/null 2>&1 || true
}
refresh_all() { local a; for a in $APPS; do refresh_app "$a" "${1:-normal}"; done; }
app_field() { k -n "$ARGOCD_NS" get application "$1" -o jsonpath="$2" 2>/dev/null; }
app_sync_status() { app_field "$1" '{.status.sync.status}'; }

# app_settled_at <app> <sha>: Argo CD compares against the commit, no operation
# is requested or running, and it has compared again since the last one
# finished (reconciledAt >= finishedAt; UTC RFC 3339 times compare as text).
app_settled_at() {
  local phase finished reconciled
  refresh_app "$1"
  [[ "$(app_field "$1" '{.status.sync.revision}')" == "$2" ]] || return 1
  [[ -z "$(app_field "$1" '{.operation}')" ]] || return 1
  phase="$(app_field "$1" '{.status.operationState.phase}')"
  [[ "$phase" != "Running" && "$phase" != "Terminating" ]] || return 1
  finished="$(app_field "$1" '{.status.operationState.finishedAt}')"
  reconciled="$(app_field "$1" '{.status.reconciledAt}')"
  [[ -z "$finished" ]] || { [[ -n "$reconciled" && ! "$reconciled" < "$finished" ]]; }
}
# wait_all_settled <sha>: every Application settled at the commit, within WAIT.
wait_all_settled() {
  local a waited=0
  for a in $APPS; do
    while ! app_settled_at "$a" "$1"; do
      waited=$((waited + 2))
      (( waited > WAIT )) && return 1
      sleep 2
    done
  done
}
app_state() {
  echo "revision=$(app_field "$1" '{.status.sync.revision}') sync=$(app_sync_status "$1") phase=$(app_field "$1" '{.status.operationState.phase}') operation=$(app_field "$1" '{.operation}') finishedAt=$(app_field "$1" '{.status.operationState.finishedAt}') reconciledAt=$(app_field "$1" '{.status.reconciledAt}')"
}

# sync_app <app>: run one sync of the Application and wait until it is Synced.
# A sync operation also records the revision as the last synced one, which the
# Git-ahead check compares against; a Synced Application that only saw a new
# commit has no such record yet.
sync_app() {
  k -n "$ARGOCD_NS" patch application "$1" --type merge \
    -p '{"operation":{"initiatedBy":{"username":"backflow-test"},"sync":{}}}' >/dev/null 2>&1 || true
  local i
  for i in $(seq 1 60); do
    refresh_app "$1" hard
    [[ "$(app_sync_status "$1")" == "Synced" && -z "$(app_field "$1" '{.operation}')" ]] && return 0
    sleep 2
  done
  return 1
}
sync_all() {
  local a ok=0
  for a in $APPS; do sync_app "$a" || ok=1; done
  return "$ok"
}

# reset_cluster: Git is the truth; sync everything and wait until Argo CD is idle
# at the head of main, so the next section starts from a known state.
reset_cluster() {
  local head
  head="$(main_sha)"
  refresh_all hard
  sleep 3
  sync_all || { fail "an Application did not become Synced: $(app_state "$DEMO_APP")"; return 0; }
  wait_all_settled "$head" || fail "Argo CD did not settle at ${head:0:8} within ${WAIT}s: $(app_state "$DEMO_APP")"
}

disable_autosync() {
  if k -n "$ARGOCD_NS" patch application "$1" --type json \
    -p '[{"op":"remove","path":"/spec/syncPolicy/automated"}]' >/dev/null 2>&1; then
    AUTOSYNC_OFF="$AUTOSYNC_OFF $1"
    return 0
  fi
  return 1
}
restore_autosync() {
  local a rest=""
  for a in $AUTOSYNC_OFF; do
    if k -n "$ARGOCD_NS" patch application "$a" --type merge \
      -p '{"spec":{"syncPolicy":{"automated":{}}}}' >/dev/null 2>&1; then :; else rest="$rest $a"; fi
  done
  AUTOSYNC_OFF="$rest"
  [[ -z "$rest" ]]
}
autosync_enabled() { [[ -n "$(app_field "$1" '{.spec.syncPolicy.automated}')" ]]; }

app_compared_at() {
  refresh_app "$1" hard
  [[ "$(app_field "$1" '{.status.sync.revision}')" == "$2" ]]
}
# app_ahead_at <app> <sha>: OutOfSync, comparing against sha, which has not been synced.
app_ahead_at() {
  refresh_app "$1" hard
  [[ "$(app_sync_status "$1")" == "OutOfSync" &&
     "$(app_field "$1" '{.status.sync.revision}')" == "$2" &&
     "$(app_field "$1" '{.status.operationState.syncResult.revision}')" != "$2" ]]
}
app_synced_at() {
  refresh_app "$1" hard
  [[ "$(app_sync_status "$1")" == "Synced" && "$(app_field "$1" '{.status.sync.revision}')" == "$2" ]]
}

# GitAhead Events of the policy: the text of each, and how many mention an Application.
git_ahead_notes() {
  k -n "$NS" get events -o json 2>/dev/null | python3 -c '
import sys, json
for e in json.load(sys.stdin)["items"]:
    if e.get("reason") == "GitAhead":
        print(e.get("note") or e.get("message") or "")'
}
git_ahead_count() { git_ahead_notes | grep -c -- " $1 " || true; }
git_ahead_exceeds() { [[ "$(git_ahead_count "$1")" -gt "$2" ]]; }

# live_cm <namespace> <configmap> <key>, set_cm <namespace> <configmap> <json data>
live_cm() { k -n "$1" get configmap "$2" -o jsonpath="{.data.$3}" 2>/dev/null; }
set_cm() {
  k -n "$1" patch configmap "$2" --type merge -p "{\"data\":$3}" >/dev/null
  refresh_app "$4"
}

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

# expect_mapping <proposal> <file> <location>: the proposal is mapped with the
# HelmValues strategy to exactly one edit.
expect_mapping() {
  expect_eq "strategy" "HelmValues" "$(dp_field "$1" .status.mapping.strategy)"
  expect_eq "mapping verified" "true" "$(dp_field "$1" .status.mapping.verified)"
  expect_eq "edited files" "$2" "$(dp_field "$1" '.status.mapping.edits[*].file')"
  expect_eq "edited keys" "$3" "$(dp_field "$1" '.status.mapping.edits[*].location')"
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
close_and_reset() {
  gl PUT "$(project_path "/merge_requests/$1")" '{"state_event":"close"}' >/dev/null || fail "could not close !$1"
  wait_for "proposal is Rejected" phase_is "$2" Rejected
  reset_cluster
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

  if [[ -n "$WORKDIR" && -f "$WORKDIR/gl.py" ]]; then
    echo
    echo "Cleaning up gitlab.com/$GITLAB_PROJECT..."
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
    if restore_autosync; then echo "  automated sync of the Applications is on again"; else
      echo "  ${RED}WARNING:${NC} automated sync is still off for:$AUTOSYNC_OFF. For each: kubectl -n $ARGOCD_NS patch application <name> --type merge -p '{\"spec\":{\"syncPolicy\":{\"automated\":{}}}}'" >&2
    fi
  fi
  if [[ "$RESTORE_CLUSTER" == "true" ]]; then
    echo "Restoring the Helm demo resources to their Git state..."
    local a
    refresh_all hard
    for a in $APPS; do
      if sync_app "$a"; then echo "  $a is Synced again"; else
        echo "  ${RED}WARNING:${NC} $a is not Synced; run: kubectl -n $ARGOCD_NS get application $a" >&2
      fi
    done
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
  if [[ -n "$WORKDIR" && "$WORKDIR" == *backflow-helm-test.* ]]; then
    rm -f "$WORKDIR/gl.py" "$WORKDIR/edit.py" "$WORKDIR/seen" "$WORKDIR/token" "$WORKDIR/paths"
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
for a in $APPS; do
  k -n "$ARGOCD_NS" get application "$a" >/dev/null 2>&1 || die "Application $a not found. Run ./hack/dev-up.sh first."
  [[ "$(app_field "$a" '{.metadata.labels.env}')" == "helm-test" ]] || die "Application $a has no label env=helm-test. Run ./hack/dev-up.sh first."
  [[ "$(app_field "$a" '{.status.sourceType}')" == "Helm" ]] || die "Application $a is not a Helm Application (sourceType '$(app_field "$a" '{.status.sourceType}')')."
  autosync_enabled "$a" || die "Application $a has no automated sync; this test expects the state ./hack/dev-up.sh creates."
  [[ "$(app_field "$a" '{.spec.source.repoURL}')" == *"gitlab.com/$GITLAB_PROJECT"* ]] || die "$a does not track gitlab.com/$GITLAB_PROJECT"
done
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

step "GitLab project: $GITLAB_PROJECT"
gl GET /user >/dev/null || die "GitLab rejected the token"
LEVEL="$(gl GET "$(project_path "")" | jx 'max(d["permissions"]["project_access"]["access_level"] if d["permissions"].get("project_access") else 0, d["permissions"]["group_access"]["access_level"] if d["permissions"].get("group_access") else 0)')" ||
  die "project $GITLAB_PROJECT is not reachable with this token"
(( LEVEL >= 40 )) || die "the token needs at least Maintainer access to restore main; it has level $LEVEL"
pass "token accepted, access level $LEVEL"

LEFTOVERS="$(gl GET "$(project_path "/repository/branches?search=backflow/&per_page=100")" | jx '" ".join(b["name"] for b in d if b["name"].startswith("backflow/"))')"
[[ -z "$LEFTOVERS" ]] || die "branches from an earlier run exist: $LEFTOVERS. Delete them (and their merge requests) first."
OPEN_AT_START="$(open_mrs)"
[[ "$OPEN_AT_START" == "0" ]] || die "$OPEN_AT_START merge request(s) are open in $GITLAB_PROJECT; close them first so this test can tell its own apart."
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
for f in "$VALUES_DEV" "$VALUES_PROD" "$ALPHA_VALUES" "$BETA_VALUES" helm/demo/values.yaml; do
  [[ -s "$WORKDIR/orig/$f" ]] || die "$f not found on main; has the Helm demo content been pushed to the demo repository?"
done
[[ "$(grep -c 'app.kubernetes.io/version' "$WORKDIR/orig/helm/demo/templates/deployment.yaml")" -ge 2 ]] ||
  die "the demo chart's Deployment does not put the image tag into the version label of its pod template as well. Push the demo repository's commit \"put the image tag into the version label of the pod template too\" first."
pass "main is at ${MAIN_ORIG_SHA:0:8}; $(find "$WORKDIR/orig" -type f | wc -l | tr -d ' ') file(s) saved for restoring"

# Start from a settled Argo CD. The Applications sync a new commit of main by
# themselves: a test that begins while that happens would see its first edit
# overwritten, and a drift that comes and goes is (rightly) never proposed.
echo "  waiting for Argo CD to compare the Applications against main (${MAIN_ORIG_SHA:0:8}) and to be idle..."
RESTORE_CLUSTER=true
refresh_all hard
sleep 3
if ! wait_all_settled "$MAIN_ORIG_SHA"; then
  echo "  an Application is not settled yet; syncing them first..."
  sync_all || die "an Application did not become Synced: $(app_state "$DEMO_APP")"
  wait_all_settled "$MAIN_ORIG_SHA" || die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} within ${WAIT}s: $(app_state "$DEMO_APP")"
fi
[[ "$(main_sha)" == "$MAIN_ORIG_SHA" ]] || die "main moved while this test was starting; run it again"
for a in $APPS; do
  [[ "$(app_sync_status "$a")" == "Synced" ]] || die "$a is not Synced at main: $(app_state "$a")"
done
pass "the three Applications are Synced and idle at main ${MAIN_ORIG_SHA:0:8}"

# Settled at main is not enough. Argo CD's automated sync applies a commit once,
# the first time an Application is OutOfSync at it, unless a sync of that commit
# already happened. If the last sync of an Application was of an older commit,
# the first drift of the test is reset by that automated sync within a second and
# never proposed. One sync of main for each Application spends that.
for a in $APPS; do
  SYNCED_AT="$(app_field "$a" '{.status.operationState.syncResult.revision}')"
  if [[ "$SYNCED_AT" != "$MAIN_ORIG_SHA" ]]; then
    echo "  the last sync of $a was of ${SYNCED_AT:0:8}, not of main; syncing it once so that its automated sync does not reset the first drift..."
    sync_app "$a" || die "$a did not become Synced: $(app_state "$a")"
  fi
done
wait_all_settled "$MAIN_ORIG_SHA" || die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} after the syncs within ${WAIT}s: $(app_state "$DEMO_APP")"
for a in $APPS; do
  [[ "$(app_field "$a" '{.status.operationState.syncResult.revision}')" == "$MAIN_ORIG_SHA" ]] ||
    die "the last sync of $a is still not of main: $(app_state "$a")"
done

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
  batchWindow: $WINDOW
  applications:
    selector:
      matchLabels:
        env: helm-test
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
policy_matched() {
  local n
  n="$(k -n "$NS" get bfp "$POLICY" -o jsonpath='{range .status.applications[*]}{.name}={.scmConnection}{"\n"}{end}' | grep -c "=$SCM_NAME\$" || true)"
  [[ "$n" == "3" ]]
}
wait_for "policy tracks the three Applications and matched their repository to the ScmConnection" policy_matched

# ---------------------------------------------------------------------------
step "a. A REGION drift becomes a merge request that edits global.region, and is merged"
GIT_REGION="$(yaml_value "$VALUES_DEV" main region)"
LIVE_REGION="$(live_cm "$DEMO_NS" "$CM" REGION)"
NEW_REGION="us-west-2"
require_drift a "$GIT_REGION" "$LIVE_REGION" "$NEW_REGION"
expect_eq "Git and live REGION agree before the drift" "$GIT_REGION" "$LIVE_REGION"
set_cm "$DEMO_NS" "$CM" "{\"REGION\":\"$NEW_REGION\"}" "$DEMO_APP"
wait_for "proposal is Proposed (REGION $GIT_REGION -> $NEW_REGION)" has_proposal "$DEMO_APP" "$CM" Proposed "/data/REGION:Replace:\"$GIT_REGION\">\"$NEW_REGION\""
remember_proposals
P_A="$(proposal_with "$DEMO_APP" "$CM" Proposed "\"$NEW_REGION\"" | head -n 1)"
if [[ -n "$P_A" ]]; then
  BRANCH_A="$(branch_of "$P_A")"
  IFS='|' read -r IID_A STATE_A _ _ <<<"$(mr_info "$BRANCH_A")"
  expect_nonempty "merge request from $BRANCH_A" "$IID_A"
  expect_eq "merge request state" "opened" "$STATE_A"
  expect_eq "Mapped condition" "True" "$(condition "$P_A" Mapped status)"
  expect_eq "Proposed condition" "True" "$(condition "$P_A" Proposed status)"
  expect_mapping "$P_A" "$VALUES_DEV" "global.region"
  expect_branch_file "$BRANCH_A" "$VALUES_DEV" "s/^\\( *region:\\) $GIT_REGION/\\1 $NEW_REGION/"
  if [[ "$(raw_file helm/demo/values.yaml main)" == "$(raw_file helm/demo/values.yaml "$BRANCH_A")" ]]; then
    pass "helm/demo/values.yaml is the same on the branch and on main"
  else
    fail "helm/demo/values.yaml differs between the branch and main"
  fi
  expect_eq "main was not touched" "$MAIN_ORIG_SHA" "$(main_sha)"
  MR_A="$(gl GET "$(project_path "/merge_requests/$IID_A")")"
  BODY_A="$(echo "$MR_A" | jx 'd["description"]')"
  expect_contains "description" "Verified by rendering through Argo CD" "$BODY_A"
  expect_contains "description (drifted fields)" "(/data/REGION)" "$BODY_A"
  expect_contains "description (value file)" "$VALUES_DEV" "$BODY_A"
  expect_contains "description (key and values)" "global.region" "$BODY_A"
  expect_contains "description (diff)" "+  region: $NEW_REGION" "$BODY_A"
  COMMIT_A="$(gl GET "$(project_path "/repository/commits/$(enc "$BRANCH_A")")")"
  expect_contains "commit message" "Value file: $VALUES_DEV" "$(echo "$COMMIT_A" | jx 'd["message"]')"
  expect_contains "commit message trailer" "Proposal: $P_A" "$(echo "$COMMIT_A" | jx 'd["message"]')"

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
  BEFORE_A="$(proposal_count "$DEMO_APP")"
  wait_for "$DEMO_APP is Synced at the merge commit" app_synced_at "$DEMO_APP" "$MERGE_A"
  expect_eq "live REGION is unchanged by the sync" "$NEW_REGION" "$(live_cm "$DEMO_NS" "$CM" REGION)"
  # One sync operation records the merge commit as the last synced revision;
  # without it the Application would still look behind and detection for it
  # would pause (the commit touched a file its render depends on).
  if sync_app "$DEMO_APP"; then pass "one sync of $DEMO_APP at the merge commit"; else fail "$DEMO_APP did not stay Synced"; fi
  sleep "$QUIET"
  expect_eq "proposal stays Merged" "Merged" "$(dp_field "$P_A" .status.phase)"
  expect_eq "proposals for $DEMO_APP (no new one)" "$BEFORE_A" "$(proposal_count "$DEMO_APP")"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
fi
reset_cluster

# ---------------------------------------------------------------------------
step "b. LOG_LEVEL and LOG_LEVEL_UP changed together: the merge request edits config.logLevel"
GIT_LEVEL="$(yaml_value "$VALUES_DEV" main logLevel)"
LIVE_LEVEL="$(live_cm "$DEMO_NS" "$CM" LOG_LEVEL)"
LIVE_LEVEL_UP="$(live_cm "$DEMO_NS" "$CM" LOG_LEVEL_UP)"
NEW_LEVEL="trace"
NEW_LEVEL_UP="TRACE"
require_drift b "$GIT_LEVEL" "$LIVE_LEVEL" "$NEW_LEVEL"
require_drift b "$(echo "$GIT_LEVEL" | tr '[:lower:]' '[:upper:]')" "$LIVE_LEVEL_UP" "$NEW_LEVEL_UP"
set_cm "$DEMO_NS" "$CM" "{\"LOG_LEVEL\":\"$NEW_LEVEL\",\"LOG_LEVEL_UP\":\"$NEW_LEVEL_UP\"}" "$DEMO_APP"
wait_for "proposal is Proposed (LOG_LEVEL $GIT_LEVEL -> $NEW_LEVEL)" has_proposal "$DEMO_APP" "$CM" Proposed "/data/LOG_LEVEL:Replace:\"$GIT_LEVEL\">\"$NEW_LEVEL\""
remember_proposals
P_B="$(proposal_with "$DEMO_APP" "$CM" Proposed "\"$NEW_LEVEL\"" | head -n 1)"
if [[ -n "$P_B" ]]; then
  BRANCH_B="$(branch_of "$P_B")"
  IFS='|' read -r IID_B STATE_B _ _ <<<"$(mr_info "$BRANCH_B")"
  expect_eq "merge request state" "opened" "$STATE_B"
  expect_contains "changed paths" "/data/LOG_LEVEL_UP" "$(dp_field "$P_B" '.spec.changes[*].path')"
  expect_mapping "$P_B" "$VALUES_DEV" "config.logLevel"
  expect_branch_file "$BRANCH_B" "$VALUES_DEV" "s/^\\( *logLevel:\\) $GIT_LEVEL/\\1 $NEW_LEVEL/"
  BODY_B="$(gl GET "$(project_path "/merge_requests/$IID_B")" | jx 'd["description"]')"
  expect_contains "description (both drifted fields)" "(/data/LOG_LEVEL, /data/LOG_LEVEL_UP)" "$BODY_B"
  close_and_reset "$IID_B" "$P_B"
  expect_eq "live LOG_LEVEL after the sync" "$GIT_LEVEL" "$(live_cm "$DEMO_NS" "$CM" LOG_LEVEL)"
fi

# ---------------------------------------------------------------------------
step "c. LOG_LEVEL alone is Unmapped: its key also feeds LOG_LEVEL_UP"
GIT_LEVEL="$(yaml_value "$VALUES_DEV" main logLevel)"
LIVE_LEVEL="$(live_cm "$DEMO_NS" "$CM" LOG_LEVEL)"
NEW_LEVEL="warn"
require_drift c "$GIT_LEVEL" "$LIVE_LEVEL" "$NEW_LEVEL"
set_cm "$DEMO_NS" "$CM" "{\"LOG_LEVEL\":\"$NEW_LEVEL\"}" "$DEMO_APP"
wait_for "proposal is Unmapped (LOG_LEVEL $GIT_LEVEL -> $NEW_LEVEL)" has_proposal "$DEMO_APP" "$CM" Unmapped "/data/LOG_LEVEL:Replace:\"$GIT_LEVEL\">\"$NEW_LEVEL\""
remember_proposals
P_C="$(proposal_with "$DEMO_APP" "$CM" Unmapped "\"$NEW_LEVEL\"" | head -n 1)"
if [[ -n "$P_C" ]]; then
  expect_unmapped "$P_C" NoVerifiedCandidate "config.logLevel" "would reproduce /data/LOG_LEVEL" "/data/LOG_LEVEL_UP"
  expect_eq "strategy" "Unmapped" "$(dp_field "$P_C" .status.mapping.strategy)"
  expect_eq "no merge request" "" "$(dp_field "$P_C" .status.mergeRequest.url)"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
fi
reset_cluster

# ---------------------------------------------------------------------------
step "d. A key of the subchart: TIMEOUT_SECONDS is mapped to demo-sidecar.timeoutSeconds"
GIT_TIMEOUT="$(yaml_value "$VALUES_DEV" main timeoutSeconds)"
LIVE_TIMEOUT="$(live_cm "$DEMO_NS" "$SIDECAR_CM" TIMEOUT_SECONDS)"
NEW_TIMEOUT="$((GIT_TIMEOUT + 15))"
require_drift d "$GIT_TIMEOUT" "$LIVE_TIMEOUT" "$NEW_TIMEOUT"
set_cm "$DEMO_NS" "$SIDECAR_CM" "{\"TIMEOUT_SECONDS\":\"$NEW_TIMEOUT\"}" "$DEMO_APP"
wait_for "proposal is Proposed (TIMEOUT_SECONDS $GIT_TIMEOUT -> $NEW_TIMEOUT)" has_proposal "$DEMO_APP" "$SIDECAR_CM" Proposed "/data/TIMEOUT_SECONDS:Replace:\"$GIT_TIMEOUT\">\"$NEW_TIMEOUT\""
remember_proposals
P_D="$(proposal_with "$DEMO_APP" "$SIDECAR_CM" Proposed "\"$NEW_TIMEOUT\"" | head -n 1)"
if [[ -n "$P_D" ]]; then
  BRANCH_D="$(branch_of "$P_D")"
  IFS='|' read -r IID_D STATE_D _ _ <<<"$(mr_info "$BRANCH_D")"
  expect_eq "merge request state" "opened" "$STATE_D"
  expect_mapping "$P_D" "$VALUES_DEV" "demo-sidecar.timeoutSeconds"
  expect_branch_file "$BRANCH_D" "$VALUES_DEV" "s/^\\( *timeoutSeconds:\\) $GIT_TIMEOUT/\\1 $NEW_TIMEOUT/"
  close_and_reset "$IID_D" "$P_D"
fi

# ---------------------------------------------------------------------------
step "e. EQUAL_A: two keys hold the value, only config.colorA verifies"
GIT_COLOR="$(yaml_value "$VALUES_DEV" main colorA)"
GIT_COLOR_B="$(yaml_value "$VALUES_DEV" main colorB)"
LIVE_COLOR="$(live_cm "$DEMO_NS" "$CM" EQUAL_A)"
NEW_COLOR="blue"
require_drift e "$GIT_COLOR" "$LIVE_COLOR" "$NEW_COLOR"
expect_eq "colorA and colorB are equal in Git (two candidates)" "$GIT_COLOR" "$GIT_COLOR_B"
set_cm "$DEMO_NS" "$CM" "{\"EQUAL_A\":\"$NEW_COLOR\"}" "$DEMO_APP"
wait_for "proposal is Proposed (EQUAL_A $GIT_COLOR -> $NEW_COLOR)" has_proposal "$DEMO_APP" "$CM" Proposed "/data/EQUAL_A:Replace:\"$GIT_COLOR\">\"$NEW_COLOR\""
remember_proposals
P_E="$(proposal_with "$DEMO_APP" "$CM" Proposed "\"$NEW_COLOR\"" | head -n 1)"
if [[ -n "$P_E" ]]; then
  BRANCH_E="$(branch_of "$P_E")"
  IFS='|' read -r IID_E STATE_E _ _ <<<"$(mr_info "$BRANCH_E")"
  expect_eq "merge request state" "opened" "$STATE_E"
  expect_mapping "$P_E" "$VALUES_DEV" "config.colorA"
  expect_branch_file "$BRANCH_E" "$VALUES_DEV" "s/^\\( *colorA:\\) $GIT_COLOR/\\1 $NEW_COLOR/"
  expect_contains "colorB on the branch" "colorB: $GIT_COLOR_B" "$(raw_file "$VALUES_DEV" "$BRANCH_E")"
  close_and_reset "$IID_E" "$P_E"
fi

# ---------------------------------------------------------------------------
step "f. AMBIGUOUS: two keys give the same result"
GIT_MIN="$(yaml_value "$VALUES_DEV" main minA)"
LIVE_AMB="$(live_cm "$DEMO_NS" "$CM" AMBIGUOUS)"
NEW_AMB="$((GIT_MIN + 2))"
require_drift f "$GIT_MIN" "$LIVE_AMB" "$NEW_AMB"
expect_eq "minA and minB are equal in Git" "$GIT_MIN" "$(yaml_value "$VALUES_DEV" main minB)"
set_cm "$DEMO_NS" "$CM" "{\"AMBIGUOUS\":\"$NEW_AMB\"}" "$DEMO_APP"
wait_for "proposal is Unmapped (AMBIGUOUS $GIT_MIN -> $NEW_AMB)" has_proposal "$DEMO_APP" "$CM" Unmapped "/data/AMBIGUOUS:Replace:\"$GIT_MIN\">\"$NEW_AMB\""
remember_proposals
P_F="$(proposal_with "$DEMO_APP" "$CM" Unmapped "\"$NEW_AMB\"" | head -n 1)"
if [[ -n "$P_F" ]]; then
  expect_unmapped "$P_F" AmbiguousCandidates "config.minA" "config.minB"
fi
reset_cluster

# ---------------------------------------------------------------------------
step "g. TIMEOUT (chart defaults) and FEATURE (helm parameter) are Unmapped"
GIT_TO="$(yaml_value helm/demo/values.yaml main timeout)"
LIVE_TO="$(live_cm "$DEMO_NS" "$CM" TIMEOUT)"
NEW_TO="$((GIT_TO + 15))"
require_drift g1 "$GIT_TO" "$LIVE_TO" "$NEW_TO"
GIT_FEATURE="$(k -n "$ARGOCD_NS" get application "$DEMO_APP" -o jsonpath='{.spec.source.helm.parameters[?(@.name=="config.feature")].value}')"
LIVE_FEATURE="$(live_cm "$DEMO_NS" "$CM" FEATURE)"
NEW_FEATURE="gamma"
require_drift g2 "$GIT_FEATURE" "$LIVE_FEATURE" "$NEW_FEATURE"
set_cm "$DEMO_NS" "$CM" "{\"TIMEOUT\":\"$NEW_TO\"}" "$DEMO_APP"
wait_for "proposal is Unmapped (TIMEOUT $GIT_TO -> $NEW_TO)" has_proposal "$DEMO_APP" "$CM" Unmapped "/data/TIMEOUT:Replace:\"$GIT_TO\">\"$NEW_TO\""
remember_proposals
P_G1="$(proposal_with "$DEMO_APP" "$CM" Unmapped "\"$NEW_TO\"" | head -n 1)"
if [[ -n "$P_G1" ]]; then
  expect_unmapped "$P_G1" NoVerifiedCandidate "chart's own defaults" "config.timeout"
fi
set_cm "$DEMO_NS" "$CM" "{\"TIMEOUT\":\"$LIVE_TO\",\"FEATURE\":\"$NEW_FEATURE\"}" "$DEMO_APP"
wait_for "proposal is Unmapped (FEATURE $GIT_FEATURE -> $NEW_FEATURE)" has_proposal "$DEMO_APP" "$CM" Unmapped "/data/FEATURE:Replace:\"$GIT_FEATURE\">\"$NEW_FEATURE\""
remember_proposals
P_G2="$(proposal_with "$DEMO_APP" "$CM" Unmapped "\"$NEW_FEATURE\"" | head -n 1)"
if [[ -n "$P_G2" ]]; then
  expect_unmapped "$P_G2" NoVerifiedCandidate "helm.parameters" "config.feature"
fi
reset_cluster

# ---------------------------------------------------------------------------
step "h. A Deployment: scaling is mapped to replicaCount; the image and the version label are not"
GIT_REPLICAS="$(yaml_value "$VALUES_DEV" main replicaCount)"
LIVE_REPLICAS="$(k -n "$DEMO_NS" get deployment "$DEMO_APP" -o jsonpath='{.spec.replicas}')"
NEW_REPLICAS="$((GIT_REPLICAS + 1))"
require_drift h1 "$GIT_REPLICAS" "$LIVE_REPLICAS" "$NEW_REPLICAS"
k -n "$DEMO_NS" scale deployment "$DEMO_APP" --replicas="$NEW_REPLICAS" >/dev/null
refresh_app "$DEMO_APP"
wait_for "proposal is Proposed (replicas $GIT_REPLICAS -> $NEW_REPLICAS)" has_proposal "$DEMO_APP" "$DEMO_APP" Proposed "/spec/replicas:Replace:$GIT_REPLICAS>$NEW_REPLICAS"
remember_proposals
P_H1="$(proposal_with "$DEMO_APP" "$DEMO_APP" Proposed "/spec/replicas" | head -n 1)"
if [[ -n "$P_H1" ]]; then
  BRANCH_H1="$(branch_of "$P_H1")"
  IFS='|' read -r IID_H1 STATE_H1 _ _ <<<"$(mr_info "$BRANCH_H1")"
  expect_eq "merge request state" "opened" "$STATE_H1"
  expect_mapping "$P_H1" "$VALUES_DEV" "replicaCount"
  expect_eq "the value is an integer in the status" "$NEW_REPLICAS" "$(dp_field "$P_H1" '.status.mapping.edits[0].value')"
  expect_branch_file "$BRANCH_H1" "$VALUES_DEV" "s/^replicaCount: $GIT_REPLICAS/replicaCount: $NEW_REPLICAS/"
  if raw_file "$VALUES_DEV" "$BRANCH_H1" | grep -q "^replicaCount: $NEW_REPLICAS\$"; then
    pass "replicaCount stays an unquoted integer on the branch"
  else
    fail "replicaCount is not an unquoted integer on the branch"
  fi
  close_and_reset "$IID_H1" "$P_H1"
fi

GIT_REPO_IMG="$(yaml_value "$VALUES_DEV" main repository)"
[[ -n "$GIT_REPO_IMG" ]] || GIT_REPO_IMG="$(yaml_value helm/demo/values.yaml main repository)"
GIT_TAG="$(yaml_value "$VALUES_DEV" main tag)"
NEW_TAG="1.28-alpine"
OLD_IMAGE="$GIT_REPO_IMG:$GIT_TAG"
NEW_IMAGE="$GIT_REPO_IMG:$NEW_TAG"
VERSION_LABEL='/metadata/labels/app.kubernetes.io~1version'
POD_LABEL='/spec/template/metadata/labels/app.kubernetes.io~1version'
LIVE_IMAGE="$(k -n "$DEMO_NS" get deployment "$DEMO_APP" -o jsonpath='{.spec.template.spec.containers[0].image}')"
require_drift h2 "$OLD_IMAGE" "$LIVE_IMAGE" "$NEW_IMAGE"

echo "  h2. kubectl set image, with the default policy..."
k -n "$DEMO_NS" set image "deployment/$DEMO_APP" "web=$NEW_IMAGE" >/dev/null
refresh_app "$DEMO_APP"
wait_for "proposal is Unmapped (image $OLD_IMAGE -> $NEW_IMAGE)" has_proposal "$DEMO_APP" "$DEMO_APP" Unmapped "containers/0/image:Replace:\"$OLD_IMAGE\">\"$NEW_IMAGE\""
remember_proposals
P_H2="$(proposal_with "$DEMO_APP" "$DEMO_APP" Unmapped "\"$NEW_IMAGE\"" | head -n 1)"
if [[ -n "$P_H2" ]]; then
  # No key holds the whole image. The key image.tag holds the part that changed,
  # so it is the nearest edit; the tag also goes to the labels, which the default
  # policy does not accept.
  expect_unmapped "$P_H2" NoVerifiedCandidate "image.tag" "to $NEW_TAG" "would reproduce" "containers/0/image" "$VERSION_LABEL" "$POD_LABEL"
fi
reset_cluster
if [[ -n "$P_H2" ]]; then
  wait_for "the unmapped proposal is retired once the cluster is back" phase_is "$P_H2" Reverted
fi

echo "  h2b. the same with spec.helm.allowCollateralChanges: [Labels]..."
if k -n "$NS" patch bfp "$POLICY" --type merge -p '{"spec":{"helm":{"allowCollateralChanges":["Labels"]}}}' >/dev/null; then
  pass "the policy allows label changes beside a Helm edit"
else
  fail "could not set spec.helm.allowCollateralChanges"
fi
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
require_drift h2b "$OLD_IMAGE" "$(k -n "$DEMO_NS" get deployment "$DEMO_APP" -o jsonpath='{.spec.template.spec.containers[0].image}')" "$NEW_IMAGE"
k -n "$DEMO_NS" set image "deployment/$DEMO_APP" "web=$NEW_IMAGE" >/dev/null
refresh_app "$DEMO_APP"
wait_for "proposal is Proposed (image $OLD_IMAGE -> $NEW_IMAGE)" has_proposal "$DEMO_APP" "$DEMO_APP" Proposed "containers/0/image:Replace:\"$OLD_IMAGE\">\"$NEW_IMAGE\""
remember_proposals
P_H2B="$(proposal_with "$DEMO_APP" "$DEMO_APP" Proposed "\"$NEW_IMAGE\"" | head -n 1)"
if [[ -n "$P_H2B" ]]; then
  BRANCH_H2B="$(branch_of "$P_H2B")"
  IFS='|' read -r IID_H2B STATE_H2B _ _ <<<"$(mr_info "$BRANCH_H2B")"
  expect_eq "merge request state" "opened" "$STATE_H2B"
  expect_mapping "$P_H2B" "$VALUES_DEV" "image.tag"
  expect_eq "the tag is the value of the edit" "\"$NEW_TAG\"" "$(dp_field "$P_H2B" '.status.mapping.edits[0].value')"
  expect_branch_file "$BRANCH_H2B" "$VALUES_DEV" "s/^\\( *tag:\\) $GIT_TAG/\\1 $NEW_TAG/"
  expect_eq "paths of the collateral changes in the status" "$VERSION_LABEL $POD_LABEL" "$(dp_field "$P_H2B" '.status.mapping.collateral[*].path' | tr ' ' '\n' | sort | tr '\n' ' ' | sed 's/ $//')"
  BODY_H2B="$(gl GET "$(project_path "/merge_requests/$IID_H2B")" | jx 'd["description"]')"
  expect_contains "description" "This edit also changes" "$BODY_H2B"
  expect_contains "description (label of the Deployment)" "$VERSION_LABEL" "$BODY_H2B"
  expect_contains "description (label of the pods)" "$POD_LABEL" "$BODY_H2B"
  expect_contains "description (values)" "\"$GIT_TAG\"" "$BODY_H2B"
  expect_contains "description (values)" "\"$NEW_TAG\"" "$BODY_H2B"
  expect_contains "description (drifted field)" "(/spec/template/spec/containers/0/image)" "$BODY_H2B"
  COMMIT_H2B="$(gl GET "$(project_path "/repository/commits/$(enc "$BRANCH_H2B")")" | jx 'd["message"]')"
  expect_contains "commit message" "This edit also changes" "$COMMIT_H2B"
  expect_contains "commit message" "Value file: $VALUES_DEV" "$COMMIT_H2B"
  close_and_reset "$IID_H2B" "$P_H2B"
fi
if k -n "$NS" patch bfp "$POLICY" --type merge -p '{"spec":{"helm":null}}' >/dev/null; then
  pass "the policy is back to the default (no collateral changes)"
else
  fail "could not remove spec.helm from the policy"
fi
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s

LIVE_VERSION="$(k -n "$DEMO_NS" get deployment "$DEMO_APP" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/version}')"
require_drift h3 "$GIT_TAG" "$LIVE_VERSION" "$NEW_TAG"
k -n "$DEMO_NS" label deployment "$DEMO_APP" "app.kubernetes.io/version=$NEW_TAG" --overwrite >/dev/null
refresh_app "$DEMO_APP"
wait_for "proposal is Unmapped (version label $GIT_TAG -> $NEW_TAG)" has_proposal "$DEMO_APP" "$DEMO_APP" Unmapped "\"$GIT_TAG\">\"$NEW_TAG\""
remember_proposals
P_H3="$(proposal_with "$DEMO_APP" "$DEMO_APP" Unmapped "\"$GIT_TAG\">\"$NEW_TAG\"" | head -n 1)"
if [[ -n "$P_H3" ]]; then
  # The tag feeds both the label and the image, so editing image.tag is a near
  # miss: it reproduces the label, and the image changes with it.
  expect_unmapped "$P_H3" NoVerifiedCandidate "image.tag" "would reproduce" "containers/0/image"
fi
reset_cluster

# ---------------------------------------------------------------------------
step "i. Git ahead: only commits to files a render depends on pause an Application"
reset_cluster
for a in $APPS; do
  if disable_autosync "$a"; then pass "automated sync of $a switched off"; else fail "could not switch automated sync of $a off"; fi
done

echo "  i1. a commit to another service's values file (helm/shared/services/beta/dev.yaml)..."
# The Events are counted before the commit: the operator reports shared-beta as
# soon as Argo CD compares it against the commit, which is what the next waits
# are for, so a count taken after them can already include the Event.
ALPHA_EVENTS="$(git_ahead_count "$ALPHA_APP")"
BETA_EVENTS="$(git_ahead_count "$BETA_APP")"
BETA_PROPOSALS="$(proposal_count "$BETA_APP")"
GIT_BETA="$(yaml_value "$BETA_VALUES" main logLevel)"
NEW_BETA="error"
[[ "$GIT_BETA" != "$NEW_BETA" ]] || NEW_BETA="trace"
SHA_I1="$(commit_edit "$BETA_VALUES" "^logLevel: .*" "logLevel: $NEW_BETA" "test: another service's values file (Backflow e2e)")" || SHA_I1=""
expect_nonempty "commit to $BETA_VALUES" "$SHA_I1"
if [[ -n "$SHA_I1" ]]; then
wait_for "$BETA_APP is OutOfSync, comparing against the commit but not synced to it" app_ahead_at "$BETA_APP" "$SHA_I1"
wait_for "$ALPHA_APP compares against the commit too" app_compared_at "$ALPHA_APP" "$SHA_I1"
expect_eq "$ALPHA_APP is still Synced (its render did not change)" "Synced" "$(app_sync_status "$ALPHA_APP")"
GIT_ALPHA="$(yaml_value "$ALPHA_VALUES" main logLevel)"
LIVE_ALPHA="$(live_cm "$ALPHA_NS" "$ALPHA_CM" LOG_LEVEL)"
NEW_ALPHA="trace"
[[ "$GIT_ALPHA" != "$NEW_ALPHA" ]] || NEW_ALPHA="warn"
require_drift i1 "$GIT_ALPHA" "$LIVE_ALPHA" "$NEW_ALPHA"
set_cm "$ALPHA_NS" "$ALPHA_CM" "{\"LOG_LEVEL\":\"$NEW_ALPHA\"}" "$ALPHA_APP"
refresh_app "$BETA_APP"
wait_for "a real drift on $ALPHA_APP is proposed although Git is ahead ($GIT_ALPHA -> $NEW_ALPHA)" has_proposal "$ALPHA_APP" "$ALPHA_CM" 'Detected|Mapping|Proposed' "/data/LOG_LEVEL:Replace:\"$GIT_ALPHA\">\"$NEW_ALPHA\""
remember_proposals
P_I1="$(proposal_with "$ALPHA_APP" "$ALPHA_CM" 'Detected|Mapping|Proposed' "\"$NEW_ALPHA\"" | head -n 1)"
wait_for "an Event says that detection for $BETA_APP is paused" git_ahead_exceeds "$BETA_APP" "$BETA_EVENTS"
expect_contains "Event text" "its Helm chart renders from changed" "$(git_ahead_notes | grep -- " $BETA_APP " | tail -n 1)"
expect_eq "new GitAhead events for $ALPHA_APP (none)" "$ALPHA_EVENTS" "$(git_ahead_count "$ALPHA_APP")"
expect_eq "proposals for $BETA_APP (none new)" "$BETA_PROPOSALS" "$(proposal_count "$BETA_APP")"
set_cm "$ALPHA_NS" "$ALPHA_CM" "{\"LOG_LEVEL\":\"$LIVE_ALPHA\"}" "$ALPHA_APP"
if [[ -n "$P_I1" ]]; then
  wait_for "and retired when the live value goes back" phase_is "$P_I1" Reverted
  expect_cleaned_and_closed "$P_I1"
fi
sync_all || fail "an Application did not become Synced after i1"
wait_all_settled "$SHA_I1" || fail "Argo CD did not settle at ${SHA_I1:0:8}: $(app_state "$BETA_APP")"
fi

echo "  i2. a commit to another environment's values file (helm/demo/values-prod.yaml)..."
DEMO_EVENTS="$(git_ahead_count "$DEMO_APP")"
GIT_PROD="$(yaml_value "$VALUES_PROD" main region)"
NEW_PROD="us-east-2"
[[ "$GIT_PROD" != "$NEW_PROD" ]] || NEW_PROD="us-east-3"
SHA_I2="$(commit_edit "$VALUES_PROD" "^  region: .*" "  region: $NEW_PROD" "test: another environment's values file (Backflow e2e)")" || SHA_I2=""
expect_nonempty "commit to $VALUES_PROD" "$SHA_I2"
if [[ -n "$SHA_I2" ]]; then
wait_for "$DEMO_APP compares against the commit" app_compared_at "$DEMO_APP" "$SHA_I2"
expect_eq "$DEMO_APP is not synced to it (manual sync)" "true" "$([[ "$(app_field "$DEMO_APP" '{.status.operationState.syncResult.revision}')" != "$SHA_I2" ]] && echo true || echo false)"
GIT_REGION="$(yaml_value "$VALUES_DEV" main region)"
LIVE_REGION="$(live_cm "$DEMO_NS" "$CM" REGION)"
NEW_REGION="ap-south-1"
require_drift i2 "$GIT_REGION" "$LIVE_REGION" "$NEW_REGION"
set_cm "$DEMO_NS" "$CM" "{\"REGION\":\"$NEW_REGION\"}" "$DEMO_APP"
wait_for "a real drift on $DEMO_APP is proposed although Git is ahead (REGION $GIT_REGION -> $NEW_REGION)" has_proposal "$DEMO_APP" "$CM" 'Detected|Mapping|Proposed' "/data/REGION:Replace:\"$GIT_REGION\">\"$NEW_REGION\""
remember_proposals
P_I2="$(proposal_with "$DEMO_APP" "$CM" 'Detected|Mapping|Proposed' "\"$NEW_REGION\"" | head -n 1)"
expect_eq "new GitAhead events for $DEMO_APP (none)" "$DEMO_EVENTS" "$(git_ahead_count "$DEMO_APP")"
set_cm "$DEMO_NS" "$CM" "{\"REGION\":\"$LIVE_REGION\"}" "$DEMO_APP"
if [[ -n "$P_I2" ]]; then
  wait_for "and retired when the live value goes back" phase_is "$P_I2" Reverted
  expect_cleaned_and_closed "$P_I2"
fi
sync_all || fail "an Application did not become Synced after i2"
wait_all_settled "$SHA_I2" || fail "Argo CD did not settle at ${SHA_I2:0:8}: $(app_state "$DEMO_APP")"
fi

echo "  i3. a commit to the Application's own values file (helm/demo/values-dev.yaml)..."
DEMO_EVENTS="$(git_ahead_count "$DEMO_APP")"
DEMO_PROPOSALS="$(proposal_count "$DEMO_APP")"
OPEN_I3="$(open_mrs)"
SHA_I3="$(commit_edit "$VALUES_DEV" "^# Development environment.*" "# Development environment (edited by the Backflow e2e test)." "test: a commit the Application has not synced yet (Backflow e2e)")" || SHA_I3=""
expect_nonempty "commit to $VALUES_DEV" "$SHA_I3"
if [[ -n "$SHA_I3" ]]; then
wait_for "$DEMO_APP compares against the commit" app_compared_at "$DEMO_APP" "$SHA_I3"
expect_eq "$DEMO_APP is not synced to it (manual sync)" "true" "$([[ "$(app_field "$DEMO_APP" '{.status.operationState.syncResult.revision}')" != "$SHA_I3" ]] && echo true || echo false)"
GIT_REGION="$(yaml_value "$VALUES_DEV" main region)"
LIVE_REGION="$(live_cm "$DEMO_NS" "$CM" REGION)"
NEW_REGION="ap-south-1"
require_drift i3 "$GIT_REGION" "$LIVE_REGION" "$NEW_REGION"
set_cm "$DEMO_NS" "$CM" "{\"REGION\":\"$NEW_REGION\"}" "$DEMO_APP"
wait_for "an Event says that detection for $DEMO_APP is paused" git_ahead_exceeds "$DEMO_APP" "$DEMO_EVENTS"
expect_contains "Event text" "its Helm chart renders from changed" "$(git_ahead_notes | grep -- " $DEMO_APP " | tail -n 1)"
echo "  waiting ${QUIET}s: no proposal may be made for $DEMO_APP while the commit is pending..."
sleep "$QUIET"
expect_eq "proposals for $DEMO_APP (none new)" "$DEMO_PROPOSALS" "$(proposal_count "$DEMO_APP")"
expect_eq "open merge requests" "$OPEN_I3" "$(open_mrs)"

if sync_app "$DEMO_APP"; then pass "synced $DEMO_APP"; else fail "$DEMO_APP did not become Synced"; fi
wait_for "$DEMO_APP is Synced at the commit" app_synced_at "$DEMO_APP" "$SHA_I3"
expect_eq "live REGION after the sync" "$GIT_REGION" "$(live_cm "$DEMO_NS" "$CM" REGION)"
expect_eq "proposals for $DEMO_APP (still none new)" "$DEMO_PROPOSALS" "$(proposal_count "$DEMO_APP")"

echo "  after the sync a real drift is detected again..."
set_cm "$DEMO_NS" "$CM" "{\"REGION\":\"$NEW_REGION\"}" "$DEMO_APP"
wait_for "proposal is made for the real drift (REGION $GIT_REGION -> $NEW_REGION)" has_proposal "$DEMO_APP" "$CM" 'Detected|Mapping|Proposed' "/data/REGION:Replace:\"$GIT_REGION\">\"$NEW_REGION\""
remember_proposals
P_I3="$(proposal_with "$DEMO_APP" "$CM" 'Detected|Mapping|Proposed' "\"$NEW_REGION\"" | head -n 1)"
set_cm "$DEMO_NS" "$CM" "{\"REGION\":\"$GIT_REGION\"}" "$DEMO_APP"
if [[ -n "$P_I3" ]]; then
  wait_for "and retired when the live value goes back" phase_is "$P_I3" Reverted
  expect_cleaned_and_closed "$P_I3"
fi
sync_all || fail "an Application did not become Synced after i3"
fi

if restore_autosync; then pass "automated sync switched back on"; else fail "could not switch automated sync back on for:$AUTOSYNC_OFF"; fi
for a in $APPS; do
  if autosync_enabled "$a"; then pass "$a has automated sync again"; else fail "$a has no automated sync"; fi
done

# ---------------------------------------------------------------------------
step "j. Partial proposals: replicas are mapped, an image Git cannot trace is left out (spec.mapping.allowPartial)"
# One drift of the Deployment has two fields: the replicas (replicaCount maps
# them) and the image (with the default policy no edit is accepted, see h2).
# Automated sync stays off for demo-helm: a sync of the merge commit would
# apply the Git image over the one that is left out.
reset_cluster
if disable_autosync "$DEMO_APP"; then pass "automated sync of $DEMO_APP switched off"; else fail "could not switch automated sync of $DEMO_APP off"; fi
J_GIT_REPLICAS="$(yaml_value "$VALUES_DEV" main replicaCount)"
J_NEW_REPLICAS="$((J_GIT_REPLICAS + 1))"
J_LIVE_IMAGE="$(k -n "$DEMO_NS" get deployment "$DEMO_APP" -o jsonpath='{.spec.template.spec.containers[0].image}')"
require_drift j "$OLD_IMAGE" "$J_LIVE_IMAGE" "$NEW_IMAGE"
require_drift j "$J_GIT_REPLICAS" "$(k -n "$DEMO_NS" get deployment "$DEMO_APP" -o jsonpath='{.spec.replicas}')" "$J_NEW_REPLICAS"
IMAGE_PATH='/spec/template/spec/containers/0/image'
drift_deployment() {
  k -n "$DEMO_NS" scale deployment "$DEMO_APP" --replicas="$J_NEW_REPLICAS" >/dev/null
  k -n "$DEMO_NS" set image "deployment/$DEMO_APP" "web=$NEW_IMAGE" >/dev/null
  refresh_app "$DEMO_APP"
}

echo "  j1. allowPartial off (the default): the whole proposal is Unmapped..."
drift_deployment
wait_for "proposal is Unmapped (replicas $J_GIT_REPLICAS -> $J_NEW_REPLICAS and the image)" has_proposal "$DEMO_APP" "$DEMO_APP" Unmapped "/spec/replicas:Replace:$J_GIT_REPLICAS>$J_NEW_REPLICAS"
remember_proposals
P_J1="$(proposal_with "$DEMO_APP" "$DEMO_APP" Unmapped "/spec/replicas:Replace:$J_GIT_REPLICAS>$J_NEW_REPLICAS" | head -n 1)"
if [[ -n "$P_J1" ]]; then
  expect_unmapped "$P_J1" NoVerifiedCandidate "image.tag"
  expect_eq "nothing is listed as left out" "" "$(dp_field "$P_J1" '.status.mapping.unmapped[*].path')"
fi
reset_cluster
if [[ -n "$P_J1" ]]; then
  wait_for "the unmapped proposal is retired once the cluster is back" phase_is "$P_J1" Reverted
fi

echo "  j2. allowPartial on: the replicas are proposed, the image is listed as not included..."
if k -n "$NS" patch bfp "$POLICY" --type merge -p '{"spec":{"mapping":{"allowPartial":true}}}' >/dev/null; then
  pass "the policy allows partial proposals"
else
  fail "could not set spec.mapping.allowPartial"
fi
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
drift_deployment
wait_for "proposal is Proposed (partial)" has_proposal "$DEMO_APP" "$DEMO_APP" Proposed "/spec/replicas:Replace:$J_GIT_REPLICAS>$J_NEW_REPLICAS"
remember_proposals
P_J2="$(proposal_with "$DEMO_APP" "$DEMO_APP" Proposed "/spec/replicas:Replace:$J_GIT_REPLICAS>$J_NEW_REPLICAS" | head -n 1)"
if [[ -n "$P_J2" ]]; then
  BRANCH_J2="$(branch_of "$P_J2")"
  IFS='|' read -r IID_J2 STATE_J2 _ _ <<<"$(mr_info "$BRANCH_J2")"
  expect_eq "merge request state" "opened" "$STATE_J2"
  expect_eq "Mapped condition" "True" "$(condition "$P_J2" Mapped status)"
  expect_eq "Mapped reason" "PartiallyMapped" "$(condition "$P_J2" Mapped reason)"
  expect_mapping "$P_J2" "$VALUES_DEV" "replicaCount"
  expect_eq "left out: path" "$IMAGE_PATH" "$(dp_field "$P_J2" '.status.mapping.unmapped[*].path')"
  expect_eq "left out: reason" "NoVerifiedCandidate" "$(dp_field "$P_J2" '.status.mapping.unmapped[*].reason')"
  expect_eq "the spec still lists both changes" "2" "$(dp_field "$P_J2" '.spec.changes[*].path' | wc -w | tr -d ' ')"
  expect_branch_file "$BRANCH_J2" "$VALUES_DEV" "s/^replicaCount: $J_GIT_REPLICAS/replicaCount: $J_NEW_REPLICAS/"
  MR_J2="$(gl GET "$(project_path "/merge_requests/$IID_J2")")"
  expect_contains "merge request title" "(partial)" "$(echo "$MR_J2" | jx 'd["title"]')"
  BODY_J2="$(echo "$MR_J2" | jx 'd["description"]')"
  expect_contains "description" "Not included in this merge request" "$BODY_J2"
  expect_contains "description (left-out path)" "$IMAGE_PATH" "$BODY_J2"
  expect_contains "description (live value)" "$NEW_IMAGE" "$BODY_J2"
  expect_contains "description (Git value)" "$OLD_IMAGE" "$BODY_J2"
  expect_contains "description" "stay different from Git after this merge request is merged" "$BODY_J2"
  expect_contains "description (verified fields)" "(/spec/replicas)" "$BODY_J2"
  COMMIT_J2="$(gl GET "$(project_path "/repository/commits/$(enc "$BRANCH_J2")")" | jx 'd["message"]')"
  expect_contains "commit message" "(partial)" "$COMMIT_J2"
  expect_contains "commit message" "Not included" "$COMMIT_J2"

  merged=false
  for _ in $(seq 1 20); do
    if gl PUT "$(project_path "/merge_requests/$IID_J2/merge")" '{"should_remove_source_branch":false}' >/dev/null 2>&1; then
      merged=true; break
    fi
    sleep 3
  done
  if [[ "$merged" == "true" ]]; then pass "merged !$IID_J2 through the API"; else fail "could not merge !$IID_J2"; fi
  MERGE_J2="$(gl GET "$(project_path "/merge_requests/$IID_J2")" | jx 'd.get("merge_commit_sha") or d.get("squash_commit_sha") or d["sha"]')"
  wait_for "proposal is Merged" phase_is "$P_J2" Merged
  echo "  j3. Argo CD compares against the merge commit: only the image is left, as a new proposal..."
  wait_for "$DEMO_APP compares against the merge commit" app_compared_at "$DEMO_APP" "$MERGE_J2"
  wait_for "a new proposal for the image alone is Unmapped" has_proposal "$DEMO_APP" "$DEMO_APP" Unmapped "$IMAGE_PATH:Replace:"
  remember_proposals
  P_J3="$(proposal_with "$DEMO_APP" "$DEMO_APP" Unmapped "$IMAGE_PATH:Replace:" | head -n 1)"
  if [[ -n "$P_J3" ]]; then
    if [[ "$P_J3" != "$P_J2" ]]; then pass "it is a new proposal, not the merged one"; else fail "the merged proposal was reused"; fi
    expect_eq "it has one change" "$IMAGE_PATH" "$(dp_field "$P_J3" '.spec.changes[*].path')"
    expect_eq "its revision is the merge commit" "$MERGE_J2" "$(dp_field "$P_J3" .spec.source.revision)"
  fi
  BEFORE_J="$(proposal_count "$DEMO_APP")"
  sleep "$QUIET"
  expect_eq "the merged proposal stays Merged" "Merged" "$(dp_field "$P_J2" .status.phase)"
  expect_eq "proposals for $DEMO_APP (no duplicate)" "$BEFORE_J" "$(proposal_count "$DEMO_APP")"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
fi
if k -n "$NS" patch bfp "$POLICY" --type merge -p '{"spec":{"mapping":null}}' >/dev/null; then
  pass "the policy is back to the default (no partial proposals)"
else
  fail "could not remove spec.mapping from the policy"
fi
wait_for "policy is Ready" k -n "$NS" wait "bfp/$POLICY" --for=condition=Ready --timeout=1s
reset_cluster
if [[ -n "${P_J3:-}" ]]; then
  wait_for "the unmapped proposal is retired once the cluster is back" phase_is "$P_J3" Reverted
fi
if restore_autosync; then pass "automated sync switched back on"; else fail "could not switch automated sync back on for:$AUTOSYNC_OFF"; fi

# ---------------------------------------------------------------------------
step "Secrets"
refresh_log
if grep -qF -- "$GITLAB_TOKEN" "$LOG_FILE" 2>/dev/null; then
  fail "the GitLab token appears in the operator log"
else
  pass "the GitLab token does not appear in the operator log"
fi
if k -n "$NS" get driftproposals -o yaml | grep -qF -- "$GITLAB_TOKEN"; then
  fail "the GitLab token appears in a DriftProposal"
else
  pass "the GitLab token does not appear in any DriftProposal"
fi

# ---------------------------------------------------------------------------
print_summary
if (( FAIL > 0 )); then
  exit 1
fi
