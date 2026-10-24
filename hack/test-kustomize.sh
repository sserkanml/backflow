#!/usr/bin/env bash
#
# Backflow - Kustomize mapping end-to-end test
#
# Runs the full path for the Kustomize demo Application against the real demo
# repository on gitlab.com. Backflow renders the overlay through Argo CD to
# verify every edit, so this needs the Argo CD of ./hack/dev-up.sh.
#
#   a. An image tag drift on demo-kustomize becomes a merge request that edits
#      images[0].newTag of the dev overlay (a setting of the kustomization, strategy
#      KustomizePatch). Merging it makes the proposal Merged; after Argo CD syncs the
#      live image is unchanged and there is no new proposal. main is restored.
#   b. A replicas drift (the overlay's strategic merge patch sets replicas) edits the
#      value inside replicas.yaml.
#   c. An env value from the shared base (REGION): by default Backflow does not
#      edit the base. The merge request adds a new Backflow patch to the overlay
#      (backflow-patches/deployment-demo-kustomize.yaml), creates the file and lists
#      it in kustomization.yaml, in one commit; the base is untouched.
#   d. With spec.kustomize.editBases: true, and no other Application building the
#      base, the base's deployment.yaml is edited (strategy Direct); the body warns
#      that the file is outside the Application's path.
#   e. The data of the hashed generator (kust-flags-<hash>) is Unmapped
#      (ValueAffectsOtherFields): the edit would change the name hash.
#   f. An Application-level spec.source.kustomize.images makes an image drift
#      Unmapped (SetByApplication).
#   g. Git ahead, with automated sync off: a commit to the shared base pauses
#      demo-kustomize (a file its kustomization builds from, although outside its
#      path); a commit to a file of another source type does not.
#   h. Secrets: the GitLab token never appears in the operator log or in a proposal.
#
# Requires the environment from ./hack/dev-up.sh (the Application demo-kustomize,
# label backflow.io/demo=kustomize; run dev-up.sh with GITLAB_TOKEN once so that the demo repository
# has kustomize/, copied from hack/demo/kustomize/) and GITLAB_TOKEN with the api
# and write_repository scopes (owner or maintainer of the demo project). Without
# GITLAB_TOKEN the test is skipped with a message and exits 0.
#
# Everything the script creates is removed on exit: branches, merge requests, the
# test label, the test namespace. Content that was merged or committed to main is
# restored with a follow-up commit through the API; those restore commits stay in
# the history of main. The Application's own options and automated sync are put
# back.
#
# The operator runs locally (bin/manager, polling merge requests every POLL)
# unless IN_CLUSTER=true, which uses the deployed operator.
#
# Usage (from the repository root):
#   GITLAB_TOKEN=... ./hack/test-kustomize.sh
#   SECTIONS=a,c ./hack/test-kustomize.sh   # only those sections (default: all);
#                                           # every section works on its own
#   WINDOW=10s POLL=3s QUIET=25              # batchWindow, merge request polling, quiet waits
#   KEEP=true ./hack/test-kustomize.sh      # keep the test namespace

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-backflow}"
NS="${NS:-backflow-kust-test}"
ARGOCD_NS="${ARGOCD_NS:-argocd}"
POLICY="kust-policy"
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
# Seconds to wait for Argo CD, the operator and GitLab to react.
if [[ "$IN_CLUSTER" == "true" ]]; then WAIT="${WAIT:-360}"; else WAIT="${WAIT:-180}"; fi
# Seconds to wait before asserting that nothing happened: long enough for the
# two observations a proposal needs, plus a margin.
QUIET="${QUIET:-$((2 * WINDOW_S + 5))}"

# The Application under test and what it renders.
APPS="demo-kustomize"
DEMO_APP="demo-kustomize"
DEMO_NS="demo-kustomize"
DEPLOY="demo-kustomize"
SETTINGS_CM="kust-settings"
OVERLAY="kustomize/overlays/dev"
OVERLAY_KUST="$OVERLAY/kustomization.yaml"
OVERLAY_PATCH="$OVERLAY/replicas.yaml"
BASE_DEPLOY="kustomize/base/deployment.yaml"
NEW_PATCH="$OVERLAY/backflow-patches/deployment-demo-kustomize.yaml"
# A file of another source type: committing to it must not pause the Application.
OTHER_FILE="helm/demo/values-prod.yaml"
IMAGE_PATH="/spec/template/spec/containers/0/image"

# SECTIONS=a,j runs only those sections (letters, comma separated) after the
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
LOG_FILE="$LOG_DIR/test-kustomize-manager.log"

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
# Set while spec.source.kustomize of the Application holds options of this test.
APP_OPTIONS_SET=false
MAIN_ORIG_SHA=""
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

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/backflow-kust-test.XXXXXX")"
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


def retriable(code, text):
    """gitlab.com sometimes answers 400, 502 or 503 with "Deadline Exceeded" when a
    commit takes too long on its side. The commit may or may not have landed:
    send() checks that before repeating a commit."""
    return code in RETRIABLE or (code in (400, 502, 503) and "deadline exceeded" in text.lower())


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
        if not retriable(code, text):
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
  k -n "$NS" get driftproposals -l "backflow.io/policy=$POLICY" -o jsonpath='{range .items[*]}{.metadata.name}{" phase="}{.status.phase}{" Mapped="}{.status.conditions[?(@.type=="Mapped")].reason}{" Proposed="}{.status.conditions[?(@.type=="Proposed")].reason}{" ("}{.status.conditions[?(@.type=="Proposed")].message}{")"}{"\n"}{end}' 2>/dev/null | cut -c1-300 | sed 's/^/      /'
  if [[ -f "$LOG_FILE" ]]; then
    echo "    last operator messages about mapping or proposing:"
    grep -E "Cannot (map|propose)|Proposal (failed|unmapped)|Reconciler error|Mapping again|Reusing the mapping|Mapping at the head|Commit built|Pushed the branch|declined" "$LOG_FILE" 2>/dev/null | tail -n 4 | cut -c1-400 | sed 's/^/      /'
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
# wait_all_settled <sha> [app...]: every Application (default: all) settled at
# the commit, within WAIT.
wait_all_settled() {
  local a waited=0 sha="$1" apps
  shift
  apps="${*:-$APPS}"
  for a in $apps; do
    while ! app_settled_at "$a" "$sha"; do
      waited=$((waited + 2))
      (( waited > WAIT )) && return 1
      sleep 2
    done
  done
}
app_state() {
  echo "revision=$(app_field "$1" '{.status.sync.revision}') sync=$(app_sync_status "$1") phase=$(app_field "$1" '{.status.operationState.phase}') operation=$(app_field "$1" '{.operation}') finishedAt=$(app_field "$1" '{.status.operationState.finishedAt}') reconciledAt=$(app_field "$1" '{.status.reconciledAt}')"
}

# app_idle_at <app> <sha>: the Application is Synced at the commit
# and has no operation requested or running. This is the state a
# section needs before it commits to the demo repository and expects the
# Application to become "Git ahead": a sync that is still pending would pick the
# new commit up and the Application would never be ahead.
app_idle_at() {
  local phase
  refresh_app "$1"
  [[ "$(app_sync_status "$1")" == "Synced" ]] || return 1
  [[ "$(app_field "$1" '{.status.sync.revision}')" == "$2" ]] || return 1
  [[ -z "$(app_field "$1" '{.operation}')" ]] || return 1
  phase="$(app_field "$1" '{.status.operationState.phase}')"
  [[ "$phase" != "Running" && "$phase" != "Terminating" ]]
}
# wait_all_idle_at <sha> [app...]: every Application (default: all) idle at the
# commit, within WAIT.
wait_all_idle_at() {
  local a waited=0 sha="$1" apps
  shift
  apps="${*:-$APPS}"
  for a in $apps; do
    while ! app_idle_at "$a" "$sha"; do
      waited=$((waited + 2))
      (( waited > WAIT )) && return 1
      sleep 2
    done
  done
}

# sync_app <app> [sha]: run one sync of the Application at a commit and wait
# until it is Synced at it. The revision is always pinned (operation.sync.revision):
# sha, else $SYNC_SHA, else the head of main now. An implicit revision would be
# resolved when Argo CD starts the operation, and a commit made in between would
# be synced instead. A sync that is already requested or running is waited for
# first. A sync operation also records the revision as the last synced one, which
# the Git-ahead check compares against; a Synced Application that only saw a new
# commit has no such record yet.
sync_app() {
  local sha="${2:-${SYNC_SHA:-}}" i
  [[ -n "$sha" ]] || sha="$(main_sha 2>/dev/null || true)"
  [[ -n "$sha" ]] || sha="$(app_field "$1" '{.status.sync.revision}')"
  [[ -n "$sha" ]] || return 1
  for i in $(seq 1 30); do
    [[ -z "$(app_field "$1" '{.operation}')" && "$(app_field "$1" '{.status.operationState.phase}')" != "Running" ]] && break
    sleep 2
  done
  k -n "$ARGOCD_NS" patch application "$1" --type merge \
    -p "{\"operation\":{\"initiatedBy\":{\"username\":\"backflow-test\"},\"sync\":{\"revision\":\"$sha\"}}}" >/dev/null 2>&1 || true
  for i in $(seq 1 60); do
    refresh_app "$1" hard
    [[ "$(app_sync_status "$1")" == "Synced" && -z "$(app_field "$1" '{.operation}')" &&
       "$(app_field "$1" '{.status.operationState.syncResult.revision}')" == "$sha" ]] && return 0
    sleep 2
  done
  return 1
}
sync_all() {
  local a ok=0 apps="${*:-$APPS}"
  for a in $apps; do sync_app "$a" || ok=1; done
  return "$ok"
}

# reset_cluster [app...]: Git is the truth; sync the Applications (default: all)
# and wait until Argo CD is idle at the head of main, so the next section starts
# from a known state. A section that drifted one Application resets only that
# one: syncing and settling the others is wasted time.
reset_cluster() {
  local head a apps="${*:-$APPS}"
  head="$(main_sha)"
  for a in $apps; do refresh_app "$a" hard; done
  sleep 3
  SYNC_SHA="$head" sync_all $apps || { fail "an Application did not become Synced: $(app_state "$DEMO_APP")"; return 0; }
  wait_all_settled "$head" $apps || fail "Argo CD did not settle at ${head:0:8} within ${WAIT}s: $(app_state "$DEMO_APP")"
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

# expect_mapping <proposal> <strategy> <files> <locations>: the proposal is mapped
# with the strategy, verified, to exactly these edited files and locations (space
# separated, in the order the status lists them).
expect_mapping() {
  expect_eq "strategy" "$2" "$(dp_field "$1" .status.mapping.strategy)"
  expect_eq "mapping verified" "true" "$(dp_field "$1" .status.mapping.verified)"
  expect_eq "edited files" "$3" "$(dp_field "$1" '.status.mapping.edits[*].file')"
  expect_eq "edited locations" "$4" "$(dp_field "$1" '.status.mapping.edits[*].location')"
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
  reset_cluster "$DEMO_APP"
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

  if [[ "$APP_OPTIONS_SET" == "true" ]]; then
    k -n "$ARGOCD_NS" patch application "$DEMO_APP" --type json -p '[{"op":"remove","path":"/spec/source/kustomize"}]' >/dev/null 2>&1 &&
      echo "  the Application-level kustomize options are removed again"
  fi
  if [[ -n "$AUTOSYNC_OFF" ]]; then
    if restore_autosync; then echo "  automated sync of the Applications is on again"; else
      echo "  ${RED}WARNING:${NC} automated sync is still off for:$AUTOSYNC_OFF. For each: kubectl -n $ARGOCD_NS patch application <name> --type merge -p '{\"spec\":{\"syncPolicy\":{\"automated\":{}}}}'" >&2
    fi
  fi
  if [[ "$RESTORE_CLUSTER" == "true" ]]; then
    echo "Restoring the Kustomize demo resources to their Git state..."
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
  if [[ -n "$WORKDIR" && "$WORKDIR" == *backflow-kust-test.* ]]; then
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
  [[ "$(app_field "$a" '{.metadata.labels.backflow\.io/demo}')" == "kustomize" ]] || die "Application $a has no label backflow.io/demo=kustomize. Run ./hack/dev-up.sh first."
  [[ "$(app_field "$a" '{.metadata.labels.env}')" != "test" ]] || die "Application $a has the label env=test, which the policies of the other tests select. Run ./hack/dev-up.sh first."
  [[ "$(app_field "$a" '{.status.sourceType}')" == "Kustomize" ]] || die "Application $a is not a Kustomize Application (sourceType '$(app_field "$a" '{.status.sourceType}')'). Does the demo repository have kustomize/? Run GITLAB_TOKEN=... ./hack/dev-up.sh."
  autosync_enabled "$a" || die "Application $a has no automated sync; this test expects the state ./hack/dev-up.sh creates."
  [[ -z "$(app_field "$a" '{.spec.source.kustomize}')" ]] || die "Application $a has spec.source.kustomize options; this test expects none."
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
for f in "$OVERLAY_KUST" "$OVERLAY_PATCH" "$BASE_DEPLOY" kustomize/base/kustomization.yaml "$OTHER_FILE"; do
  [[ -s "$WORKDIR/orig/$f" ]] || die "$f not found on main; run GITLAB_TOKEN=... ./hack/dev-up.sh to copy hack/demo/kustomize/ into the demo repository."
done
pass "main is at ${MAIN_ORIG_SHA:0:8}; $(find "$WORKDIR/orig" -type f | wc -l | tr -d ' ') file(s) saved for restoring"

# Start from a settled Argo CD (see test-helm.sh for why).
echo "  waiting for Argo CD to compare $DEMO_APP against main (${MAIN_ORIG_SHA:0:8}) and to be idle..."
RESTORE_CLUSTER=true
refresh_all hard
sleep 3
if ! wait_all_settled "$MAIN_ORIG_SHA"; then
  echo "  $DEMO_APP is not settled yet; syncing it first..."
  SYNC_SHA="$MAIN_ORIG_SHA" sync_all || die "$DEMO_APP did not become Synced: $(app_state "$DEMO_APP")"
  wait_all_settled "$MAIN_ORIG_SHA" || die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} within ${WAIT}s: $(app_state "$DEMO_APP")"
fi
[[ "$(main_sha)" == "$MAIN_ORIG_SHA" ]] || die "main moved while this test was starting; run it again"
[[ "$(app_sync_status "$DEMO_APP")" == "Synced" ]] || die "$DEMO_APP is not Synced at main: $(app_state "$DEMO_APP")"
pass "$DEMO_APP is Synced and idle at main ${MAIN_ORIG_SHA:0:8}"

# One sync of main spends the automated sync of this commit, or it would reset
# the first drift of the test within a second.
SYNCED_AT="$(app_field "$DEMO_APP" '{.status.operationState.syncResult.revision}')"
if [[ "$SYNCED_AT" != "$MAIN_ORIG_SHA" ]]; then
  echo "  the last sync of $DEMO_APP was of ${SYNCED_AT:0:8}, not of main; syncing it once..."
  sync_app "$DEMO_APP" "$MAIN_ORIG_SHA" || die "$DEMO_APP did not become Synced: $(app_state "$DEMO_APP")"
fi
wait_all_settled "$MAIN_ORIG_SHA" || die "Argo CD did not settle at main ${MAIN_ORIG_SHA:0:8} within ${WAIT}s: $(app_state "$DEMO_APP")"


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
        backflow.io/demo: kustomize
  kustomize:
    editBases: false
    addPatches: true
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
  [[ "$n" == "1" ]]
}
wait_for "policy tracks $DEMO_APP and matched its repository to the ScmConnection" policy_matched

# set_kustomize <editBases> <addPatches>: spec.kustomize of the policy.
set_kustomize() {
  k -n "$NS" patch bfp "$POLICY" --type merge -p "{\"spec\":{\"kustomize\":{\"editBases\":$1,\"addPatches\":$2}}}" >/dev/null
}
live_image() { k -n "$DEMO_NS" get deployment "$DEPLOY" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null; }
image_is() { [[ "$(live_image)" == "$1" ]]; }
live_replicas() { k -n "$DEMO_NS" get deployment "$DEPLOY" -o jsonpath='{.spec.replicas}' 2>/dev/null; }
# live_env <name>: the value of an env var of the container.
live_env() { k -n "$DEMO_NS" get deployment "$DEPLOY" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name==\"$1\")].value}" 2>/dev/null; }
set_image() { k -n "$DEMO_NS" set image "deployment/$DEPLOY" "web=$1" >/dev/null; refresh_app "$DEMO_APP"; }
set_replicas() { k -n "$DEMO_NS" scale "deployment/$DEPLOY" --replicas="$1" >/dev/null; refresh_app "$DEMO_APP"; }
set_env() { k -n "$DEMO_NS" set env "deployment/$DEPLOY" "$1=$2" >/dev/null; refresh_app "$DEMO_APP"; }
flags_cm() { k -n "$DEMO_NS" get configmap -o name 2>/dev/null | sed 's|^configmap/||' | grep '^kust-flags-' | head -n 1; }
# merge_mr <iid>: merge through the API, retrying while GitLab computes mergeability.
merge_mr() {
  local i merged=false
  for i in $(seq 1 20); do
    if gl PUT "$(project_path "/merge_requests/$1/merge")" '{"should_remove_source_branch":false}' >/dev/null 2>&1; then
      merged=true; break
    fi
    sleep 3
  done
  if [[ "$merged" == "true" ]]; then pass "merged !$1 through the API"; else fail "could not merge !$1"; fi
}
# mr_body <iid>
mr_body() { gl GET "$(project_path "/merge_requests/$1")" | jx 'd["description"]'; }
commit_message() { gl GET "$(project_path "/repository/commits/$(enc "$1")")" | jx 'd["message"]'; }

# ---------------------------------------------------------------------------
if section_wanted a; then
step "a. An image tag drift becomes a merge request that edits images[0].newTag of the overlay, and is merged"
GIT_IMAGE="$(live_image)"
NEW_IMAGE="nginxinc/nginx-unprivileged:1.28-alpine"
GIT_TAG="${GIT_IMAGE##*:}"
NEW_TAG="${NEW_IMAGE##*:}"
require_drift a "$GIT_IMAGE" "$(live_image)" "$NEW_IMAGE"
set_image "$NEW_IMAGE"
wait_for "proposal is Proposed (image $GIT_IMAGE -> $NEW_IMAGE)" has_proposal "$DEMO_APP" "$DEPLOY" Proposed "$IMAGE_PATH:Replace:\"$GIT_IMAGE\">\"$NEW_IMAGE\""
remember_proposals
P_A="$(proposal_with "$DEMO_APP" "$DEPLOY" Proposed "\"$NEW_IMAGE\"" | head -n 1)"
if [[ -n "$P_A" ]]; then
  BRANCH_A="$(branch_of "$P_A")"
  IFS='|' read -r IID_A STATE_A _ _ <<<"$(mr_info "$BRANCH_A")"
  expect_nonempty "merge request from $BRANCH_A" "$IID_A"
  expect_eq "merge request state" "opened" "$STATE_A"
  expect_eq "Mapped condition" "True" "$(condition "$P_A" Mapped status)"
  expect_mapping "$P_A" KustomizePatch "$OVERLAY_KUST" "/images/0/newTag"
  expect_branch_file "$BRANCH_A" "$OVERLAY_KUST" "s/^\\( *newTag:\\) $GIT_TAG/\\1 $NEW_TAG/"
  for f in "$BASE_DEPLOY" "$OVERLAY_PATCH"; do
    if [[ "$(raw_file "$f" main)" == "$(raw_file "$f" "$BRANCH_A")" ]]; then pass "$f is the same on the branch and on main"; else fail "$f differs between the branch and main"; fi
  done
  expect_eq "main was not touched" "$MAIN_ORIG_SHA" "$(main_sha)"
  BODY_A="$(mr_body "$IID_A")"
  expect_contains "description" "**Kustomize**" "$BODY_A"
  expect_contains "description (tier)" "a setting of the kustomization" "$BODY_A"
  expect_contains "description (setting)" "images[nginxinc/nginx-unprivileged].newTag" "$BODY_A"
  expect_contains "description (verified)" "Verified by rendering through Argo CD" "$BODY_A"
  expect_contains "description (drifted field)" "($IMAGE_PATH)" "$BODY_A"
  expect_contains "description (diff)" "+  newTag: $NEW_TAG" "$BODY_A"
  expect_not_contains "description (no warning for a file of the Application)" "Warning" "$BODY_A"
  MSG_A="$(commit_message "$BRANCH_A")"
  expect_contains "commit message" "a setting of the kustomization" "$MSG_A"
  expect_contains "commit message trailer" "Proposal: $P_A" "$MSG_A"

  merge_mr "$IID_A"
  MERGE_A="$(gl GET "$(project_path "/merge_requests/$IID_A")" | jx 'd.get("merge_commit_sha") or d.get("squash_commit_sha") or d["sha"]')"
  wait_for "proposal is Merged" phase_is "$P_A" Merged
  expect_eq "status.commitSHA is the merge commit" "$MERGE_A" "$(dp_field "$P_A" .status.commitSHA)"
  BEFORE_A="$(proposal_count "$DEMO_APP")"
  wait_for "$DEMO_APP is Synced at the merge commit" app_synced_at "$DEMO_APP" "$MERGE_A"
  expect_eq "live image is unchanged by the sync" "$NEW_IMAGE" "$(live_image)"
  if sync_app "$DEMO_APP"; then pass "one sync of $DEMO_APP at the merge commit"; else fail "$DEMO_APP did not stay Synced"; fi
  sleep "$QUIET"
  expect_eq "proposal stays Merged" "Merged" "$(dp_field "$P_A" .status.phase)"
  expect_eq "proposals for $DEMO_APP (no new one)" "$BEFORE_A" "$(proposal_count "$DEMO_APP")"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
fi
# Put main back, so the next sections start from the original content.
restore_main || fail "main could not be restored"
reset_cluster "$DEMO_APP"
expect_eq "live image is back to Git" "$GIT_IMAGE" "$(live_image)"

fi

# ---------------------------------------------------------------------------
if section_wanted b; then
step "b. A replicas drift is mapped to the value inside the overlay's patch"
GIT_REPLICAS="$(live_replicas)"
NEW_REPLICAS=$((GIT_REPLICAS + 2))
require_drift b "$GIT_REPLICAS" "$(live_replicas)" "$NEW_REPLICAS"
set_replicas "$NEW_REPLICAS"
wait_for "proposal is Proposed (replicas $GIT_REPLICAS -> $NEW_REPLICAS)" has_proposal "$DEMO_APP" "$DEPLOY" Proposed "/spec/replicas:Replace:$GIT_REPLICAS>$NEW_REPLICAS"
remember_proposals
P_B="$(proposal_with "$DEMO_APP" "$DEPLOY" Proposed ">$NEW_REPLICAS;" | head -n 1)"
if [[ -n "$P_B" ]]; then
  BRANCH_B="$(branch_of "$P_B")"
  IFS='|' read -r IID_B STATE_B _ _ <<<"$(mr_info "$BRANCH_B")"
  expect_eq "merge request state" "opened" "$STATE_B"
  expect_mapping "$P_B" KustomizePatch "$OVERLAY_PATCH" "/spec/replicas"
  expect_branch_file "$BRANCH_B" "$OVERLAY_PATCH" "s/^\\( *replicas:\\) $GIT_REPLICAS/\\1 $NEW_REPLICAS/"
  expect_contains "description (tier)" "the existing patch" "$(mr_body "$IID_B")"
  close_and_reset "$IID_B" "$P_B"
  expect_eq "live replicas after the sync" "$GIT_REPLICAS" "$(live_replicas)"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted c; then
step "c. An env value from the shared base becomes a new Backflow patch in the overlay"
GIT_REGION="$(live_env REGION)"
NEW_REGION="us-east-1"
require_drift c "$GIT_REGION" "$(live_env REGION)" "$NEW_REGION"
set_kustomize false true
set_env REGION "$NEW_REGION"
wait_for "proposal is Proposed (REGION $GIT_REGION -> $NEW_REGION)" has_proposal "$DEMO_APP" "$DEPLOY" Proposed "/value:Replace:\"$GIT_REGION\">\"$NEW_REGION\""
remember_proposals
P_C="$(proposal_with "$DEMO_APP" "$DEPLOY" Proposed "\"$NEW_REGION\"" | head -n 1)"
if [[ -n "$P_C" ]]; then
  BRANCH_C="$(branch_of "$P_C")"
  IFS='|' read -r IID_C STATE_C _ _ <<<"$(mr_info "$BRANCH_C")"
  expect_eq "merge request state" "opened" "$STATE_C"
  expect_eq "strategy" "KustomizePatch" "$(dp_field "$P_C" .status.mapping.strategy)"
  FILES_C="$(dp_field "$P_C" '.status.mapping.edits[*].file')"
  expect_contains "edited files" "$NEW_PATCH" "$FILES_C"
  expect_contains "edited files" "$OVERLAY_KUST" "$FILES_C"
  expect_not_contains "edited files (the base is not edited)" "kustomize/base" "$FILES_C"
  PATCH_C="$(raw_file "$NEW_PATCH" "$BRANCH_C")"
  expect_contains "new patch file" "name: REGION" "$PATCH_C"
  expect_contains "new patch file" "value: $NEW_REGION" "$PATCH_C"
  expect_contains "kustomization lists the patch" "path: backflow-patches/deployment-demo-kustomize.yaml" "$(raw_file "$OVERLAY_KUST" "$BRANCH_C")"
  expect_eq "main has no such file" "404" "$(gl_status GET "$(project_path "/repository/files/$(enc "$NEW_PATCH")?ref=main")")"
  if [[ "$(raw_file "$BASE_DEPLOY" main)" == "$(raw_file "$BASE_DEPLOY" "$BRANCH_C")" ]]; then pass "the base is the same on the branch and on main"; else fail "the base differs between the branch and main"; fi
  BODY_C="$(mr_body "$IID_C")"
  expect_contains "description" "A new patch file was added" "$BODY_C"
  expect_contains "description (file)" "$NEW_PATCH" "$BODY_C"
  expect_contains "commit message" "A new patch file was added" "$(commit_message "$BRANCH_C")"
  close_and_reset "$IID_C" "$P_C"
  expect_eq "live REGION after the sync" "$GIT_REGION" "$(live_env REGION)"
fi

fi

# ---------------------------------------------------------------------------
if section_wanted d; then
step "d. With editBases on, a base no other Application builds is edited directly"
GIT_GREETING="$(live_env GREETING)"
NEW_GREETING="howdy"
require_drift d "$GIT_GREETING" "$(live_env GREETING)" "$NEW_GREETING"
set_kustomize true true
set_env GREETING "$NEW_GREETING"
wait_for "proposal is Proposed (GREETING $GIT_GREETING -> $NEW_GREETING)" has_proposal "$DEMO_APP" "$DEPLOY" Proposed "/value:Replace:\"$GIT_GREETING\">\"$NEW_GREETING\""
remember_proposals
P_D="$(proposal_with "$DEMO_APP" "$DEPLOY" Proposed "\"$NEW_GREETING\"" | head -n 1)"
if [[ -n "$P_D" ]]; then
  BRANCH_D="$(branch_of "$P_D")"
  IFS='|' read -r IID_D STATE_D _ _ <<<"$(mr_info "$BRANCH_D")"
  expect_eq "merge request state" "opened" "$STATE_D"
  expect_mapping "$P_D" Direct "$BASE_DEPLOY" "/spec/template/spec/containers/0/env/0/value"
  expect_branch_file "$BRANCH_D" "$BASE_DEPLOY" "s/^\\( *value:\\) $GIT_GREETING/\\1 $NEW_GREETING/"
  BODY_D="$(mr_body "$IID_D")"
  expect_contains "description (warning)" "outside the Application's path" "$BODY_D"
  expect_contains "description (unknown users)" "Applications outside Backflow's policies may use this file" "$BODY_D"
  expect_not_contains "description (no other known Application)" "also changes:" "$BODY_D"
  close_and_reset "$IID_D" "$P_D"
  expect_eq "live GREETING after the sync" "$GIT_GREETING" "$(live_env GREETING)"
fi
set_kustomize false true

fi

# ---------------------------------------------------------------------------
if section_wanted e; then
step "e. The data of the hashed generator is Unmapped: the edit would change the name hash"
FLAGS="$(flags_cm)"
expect_nonempty "the hashed ConfigMap" "$FLAGS"
GIT_FEATURE="$(live_cm "$DEMO_NS" "$FLAGS" FEATURE)"
NEW_FEATURE="off"
require_drift e "$GIT_FEATURE" "$(live_cm "$DEMO_NS" "$FLAGS" FEATURE)" "$NEW_FEATURE"
set_cm "$DEMO_NS" "$FLAGS" "{\"FEATURE\":\"$NEW_FEATURE\"}" "$DEMO_APP"
wait_for "proposal is Unmapped (FEATURE $GIT_FEATURE -> $NEW_FEATURE)" has_proposal "$DEMO_APP" "$FLAGS" Unmapped "/data/FEATURE:Replace:\"$GIT_FEATURE\">\"$NEW_FEATURE\""
remember_proposals
P_E="$(proposal_with "$DEMO_APP" "$FLAGS" Unmapped "\"$NEW_FEATURE\"" | head -n 1)"
if [[ -n "$P_E" ]]; then
  expect_unmapped "$P_E" ValueAffectsOtherFields "name hash" "disableNameSuffixHash"
  expect_eq "strategy" "Unmapped" "$(dp_field "$P_E" .status.mapping.strategy)"
  expect_eq "no merge request" "" "$(dp_field "$P_E" .status.mergeRequest.url)"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
fi
reset_cluster "$DEMO_APP"

fi

# ---------------------------------------------------------------------------
if section_wanted f; then
step "f. An Application-level images override: an image drift is Unmapped (SetByApplication)"
APP_IMAGE="nginxinc/nginx-unprivileged:1.29-alpine"
OVERRIDE_TO="nginxinc/nginx-unprivileged:1.30-alpine"
APP_OPTIONS_SET=true
k -n "$ARGOCD_NS" patch application "$DEMO_APP" --type merge \
  -p "{\"spec\":{\"source\":{\"kustomize\":{\"images\":[\"nginxinc/nginx-unprivileged=$APP_IMAGE\"]}}}}" >/dev/null
refresh_app "$DEMO_APP" hard
wait_for "$DEMO_APP renders the image of the Application option ($APP_IMAGE)" image_is "$APP_IMAGE"
sleep 3
wait_all_settled "$(main_sha)" || fail "Argo CD did not settle with the Application option: $(app_state "$DEMO_APP")"
require_drift f "$APP_IMAGE" "$(live_image)" "$OVERRIDE_TO"
set_image "$OVERRIDE_TO"
wait_for "proposal is Unmapped (image $APP_IMAGE -> $OVERRIDE_TO)" has_proposal "$DEMO_APP" "$DEPLOY" Unmapped "$IMAGE_PATH:Replace:\"$APP_IMAGE\">\"$OVERRIDE_TO\""
remember_proposals
P_F="$(proposal_with "$DEMO_APP" "$DEPLOY" Unmapped "\"$OVERRIDE_TO\"" | head -n 1)"
if [[ -n "$P_F" ]]; then
  expect_unmapped "$P_F" SetByApplication "spec.source.kustomize.images of Application $DEMO_APP; Git cannot change it"
  expect_eq "no merge request" "" "$(dp_field "$P_F" .status.mergeRequest.url)"
  expect_eq "open merge requests" "$OPEN_AT_START" "$(open_mrs)"
fi
k -n "$ARGOCD_NS" patch application "$DEMO_APP" --type json -p '[{"op":"remove","path":"/spec/source/kustomize"}]' >/dev/null
APP_OPTIONS_SET=false
reset_cluster "$DEMO_APP"

fi

# ---------------------------------------------------------------------------
if section_wanted g; then
step "g. Git ahead: a commit to the shared base pauses $DEMO_APP, a commit to a file elsewhere does not"
reset_cluster "$DEMO_APP"
if disable_autosync "$DEMO_APP"; then pass "automated sync of $DEMO_APP switched off"; else fail "could not switch automated sync of $DEMO_APP off"; fi

echo "  g1. a commit to a file of another source type ($OTHER_FILE)..."
wait_all_idle_at "$(main_sha)" || fail "$DEMO_APP is not idle and synced at main before the commit: $(app_state "$DEMO_APP")"
EVENTS_G="$(git_ahead_count "$DEMO_APP")"
GIT_PROD="$(yaml_value "$OTHER_FILE" main region)"
NEW_PROD="us-east-2"
[[ "$GIT_PROD" != "$NEW_PROD" ]] || NEW_PROD="us-east-3"
SHA_G1="$(commit_edit "$OTHER_FILE" "^  region: .*" "  region: $NEW_PROD" "test: a file the kustomization does not build from (Backflow e2e)")" || SHA_G1=""
expect_nonempty "commit to $OTHER_FILE" "$SHA_G1"
if [[ -n "$SHA_G1" ]]; then
  wait_for "$DEMO_APP compares against the commit" app_compared_at "$DEMO_APP" "$SHA_G1"
  GIT_REGION="$(live_env REGION)"
  NEW_REGION="ap-south-1"
  require_drift g1 "$GIT_REGION" "$(live_env REGION)" "$NEW_REGION"
  set_env REGION "$NEW_REGION"
  wait_for "a real drift is proposed although Git is ahead (REGION $GIT_REGION -> $NEW_REGION)" has_proposal "$DEMO_APP" "$DEPLOY" 'Detected|Mapping|Proposed' "/value:Replace:\"$GIT_REGION\">\"$NEW_REGION\""
  remember_proposals
  P_G1="$(proposal_with "$DEMO_APP" "$DEPLOY" 'Detected|Mapping|Proposed' "\"$NEW_REGION\"" | head -n 1)"
  expect_eq "new GitAhead events for $DEMO_APP (none)" "$EVENTS_G" "$(git_ahead_count "$DEMO_APP")"
  set_env REGION "$GIT_REGION"
  if [[ -n "$P_G1" ]]; then
    wait_for "and retired when the live value goes back" phase_is "$P_G1" Reverted
    expect_cleaned_and_closed "$P_G1"
  fi
  sync_all || fail "$DEMO_APP did not become Synced after g1"
  wait_all_settled "$SHA_G1" || fail "Argo CD did not settle at ${SHA_G1:0:8}: $(app_state "$DEMO_APP")"
fi

echo "  g2. a commit to the shared base ($BASE_DEPLOY), outside the Application's path..."
wait_all_idle_at "$(main_sha)" || fail "$DEMO_APP is not idle and synced at main before the commit: $(app_state "$DEMO_APP")"
EVENTS_G="$(git_ahead_count "$DEMO_APP")"
PROPOSALS_G="$(proposal_count "$DEMO_APP")"
OPEN_G="$(open_mrs)"
SHA_G2="$(commit_edit "$BASE_DEPLOY" "^kind: Deployment" "kind: Deployment # edited by the Backflow e2e test" "test: a commit to the shared base that the Application has not synced yet (Backflow e2e)")" || SHA_G2=""
expect_nonempty "commit to $BASE_DEPLOY" "$SHA_G2"
if [[ -n "$SHA_G2" ]]; then
  wait_for "$DEMO_APP compares against the commit" app_compared_at "$DEMO_APP" "$SHA_G2"
  expect_eq "$DEMO_APP is not synced to it (manual sync)" "true" "$([[ "$(app_field "$DEMO_APP" '{.status.operationState.syncResult.revision}')" != "$SHA_G2" ]] && echo true || echo false)"
  GIT_REGION="$(live_env REGION)"
  NEW_REGION="ap-south-1"
  require_drift g2 "$GIT_REGION" "$(live_env REGION)" "$NEW_REGION"
  set_env REGION "$NEW_REGION"
  wait_for "an Event says that detection for $DEMO_APP is paused" git_ahead_exceeds "$DEMO_APP" "$EVENTS_G"
  expect_contains "Event text" "its kustomization builds from changed" "$(git_ahead_notes | grep -- " $DEMO_APP " | tail -n 1)"
  echo "  waiting ${QUIET}s: no proposal may be made for $DEMO_APP while the commit is pending..."
  sleep "$QUIET"
  expect_eq "proposals for $DEMO_APP (none new)" "$PROPOSALS_G" "$(proposal_count "$DEMO_APP")"
  expect_eq "open merge requests" "$OPEN_G" "$(open_mrs)"
  if sync_app "$DEMO_APP"; then pass "synced $DEMO_APP"; else fail "$DEMO_APP did not become Synced"; fi
  wait_for "$DEMO_APP is Synced at the commit" app_synced_at "$DEMO_APP" "$SHA_G2"
  expect_eq "live REGION after the sync" "$GIT_REGION" "$(live_env REGION)"
  expect_eq "proposals for $DEMO_APP (still none new)" "$PROPOSALS_G" "$(proposal_count "$DEMO_APP")"
fi

if restore_autosync; then pass "automated sync switched back on"; else fail "could not switch automated sync back on for:$AUTOSYNC_OFF"; fi
if autosync_enabled "$DEMO_APP"; then pass "$DEMO_APP has automated sync again"; else fail "$DEMO_APP has no automated sync"; fi

fi

# ---------------------------------------------------------------------------
if section_wanted h; then
step "h. Secrets"
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

fi

# ---------------------------------------------------------------------------
print_summary
if (( FAIL > 0 )); then
  exit 1
fi
