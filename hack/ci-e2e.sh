#!/usr/bin/env bash
#
# The e2e scripts in CI (.github/workflows/e2e.yml). It runs on the runner, but
# nothing here is specific to GitHub Actions, so it can be tried locally.
#
#   ./hack/ci-e2e.sh plan       print the scripts that would run (one per line)
#   ./hack/ci-e2e.sh run        run them one after the other, each into its own log;
#                               go on after a failed script and fail at the end
#   ./hack/ci-e2e.sh cleanup    close the open merge requests of Backflow in the demo
#                               repository and delete its backflow/ branches (never main)
#   ./hack/ci-e2e.sh scrub DIR  fail if a token value is in a file below DIR, and
#                               replace it with *** there anyway
#
# Environment:
#   SCM        gitlab (default) or github: the host, as for the scripts
#   DEMO_REPO  the demo repository, as for the scripts
#   SCRIPTS    comma separated: policy, drift, mapping, application-values, scm,
#              merge-request, helm, kustomize, multi-source (test- and .sh are
#              accepted). Empty: all of them for the host. The ones that need no
#              token (the first four) belong to the gitlab run only.
#   SECTIONS   passed to the script when exactly one script is selected
#   E2E_LOG_DIR  where the logs go (default tmp/e2e-logs)
#   E2E_SCRIPT_TIMEOUT  seconds one script may take (default 5400)
#   GITLAB_TOKEN | GITHUB_TOKEN  the token of the host; it is never printed.
#
# bash 3.2 compatible, like the scripts it runs.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export SCM="${SCM:-gitlab}"
SCRIPTS="${SCRIPTS:-}"
# shellcheck source=lib/scm.sh
source "$ROOT/hack/lib/scm.sh"

LOG_DIR="${E2E_LOG_DIR:-$ROOT/tmp/e2e-logs}"
SCRIPT_TIMEOUT="${E2E_SCRIPT_TIMEOUT:-5400}"
TOKENLESS="backflow-policy drift mapping application-values"
TOKENED="scm merge-request helm kustomize multi-source"

# normalise <name>: backflow-policy from "test-backflow-policy.sh", "policy" and the like.
normalise() {
  local n="${1// /}"
  n="${n#test-}"
  n="${n%.sh}"
  [[ "$n" == "policy" ]] && n="backflow-policy"
  echo "$n"
}

# plan: the scripts to run, one per line.
plan() {
  local all want n known item
  if [[ "$SCM" == "gitlab" ]]; then all="$TOKENLESS $TOKENED"; else all="$TOKENED"; fi
  if [[ -z "${SCRIPTS// /}" ]]; then
    for n in $all; do echo "$n"; done
    return 0
  fi
  for item in $(echo "$SCRIPTS" | tr ',' ' '); do
    n="$(normalise "$item")"
    known=false
    for want in $TOKENLESS $TOKENED; do [[ "$want" == "$n" ]] && known=true; done
    if [[ "$known" != "true" ]]; then echo "unknown script '$item'" >&2; return 1; fi
    case " $all " in
      *" $n "*) echo "$n" ;;
      *) echo "skipping $n: it needs no token and runs in the gitlab job only" >&2 ;;
    esac
  done
}

summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then cat >>"$GITHUB_STEP_SUMMARY"; else cat; fi
}

fmt_duration() {
  printf '%dm%02ds' "$(($1 / 60))" "$(($1 % 60))"
}

run() {
  local scripts n count=0 failed=0 start secs rc log counts
  scripts="$(plan)" || exit 1
  for n in $scripts; do count=$((count + 1)); done
  if (( count == 0 )); then
    echo "nothing to run for SCM=$SCM"
    printf '### e2e on %s\n\nNothing to run for this host.\n' "$SCM_LABEL" | summary
    return 0
  fi
  mkdir -p "$LOG_DIR"
  local sections=""
  if [[ -n "${SECTIONS:-}" ]]; then
    if (( count == 1 )); then
      sections="$SECTIONS"
    else
      echo "::warning::SECTIONS is ignored: $count scripts are selected, it applies to one"
    fi
  fi

  local rows="" details=""
  for n in $scripts; do
    log="$LOG_DIR/test-$n.log"
    echo "::group::test-$n.sh ($SCM_LABEL)"
    start=$SECONDS
    # timeout sends TERM, which the scripts' EXIT traps turn into their usual cleanup.
    SECTIONS="$sections" timeout --foreground -k 120 "$SCRIPT_TIMEOUT" "$ROOT/hack/test-$n.sh" 2>&1 | tee "$log"
    rc=${PIPESTATUS[0]}
    secs=$((SECONDS - start))
    echo "::endgroup::"
    # The operator logs of the scripts, which they keep in tmp/.
    cp "$ROOT"/tmp/*-manager.log "$LOG_DIR"/ 2>/dev/null || true

    counts="$(grep -a 'Summary:' "$log" | tail -n 1 | sed -E 's/\x1b\[[0-9;]*m//g' | sed -nE 's/.*Summary: *([0-9]+) passed, *([0-9]+) failed.*/\1|\2/p')"
    local passed="" nfail="" result
    if [[ -n "$counts" ]]; then passed="${counts%%|*}"; nfail="${counts##*|}"; fi
    if grep -aq '^SKIPPED:' "$log"; then
      # A script that skips itself found no token: in CI that is a setup error, not a pass.
      result="skipped"
      if [[ "${CI:-}" == "true" ]]; then
        result="FAILED (skipped itself: no token)"
        failed=$((failed + 1))
        echo "::error::test-$n.sh skipped itself: the token of $SCM_LABEL is not available"
      fi
    elif (( rc == 0 )); then
      result="passed"
    else
      result="FAILED"
      failed=$((failed + 1))
      echo "::error::test-$n.sh failed (exit code $rc) on $SCM_LABEL"
    fi
    rows="$rows| test-$n.sh | $result | ${passed:--} | ${nfail:--} | $(fmt_duration "$secs") |
"
    local timing
    timing="$(sed -E 's/\x1b\[[0-9;]*m//g' "$log" | awk '/Time per section:/ {p=1; next} p && /^ *[0-9]+s / {print; next} p {p=0}')"
    if [[ -n "$timing" ]]; then
      details="$details<details><summary>test-$n.sh: time per section</summary>

\`\`\`
$timing
\`\`\`

</details>
"
    fi
  done

  {
    printf '### e2e on %s (%s)\n\n' "$SCM_LABEL" "$SCM_PROJECT"
    printf '| Script | Result | Passed | Failed | Duration |\n|---|---|---|---|---|\n'
    printf '%s\n' "$rows"
    printf '%s\n' "$details"
  } | summary
  if (( failed > 0 )); then
    echo "$failed of $count script(s) failed"
    return 1
  fi
  echo "all $count script(s) passed"
}

cleanup() {
  if [[ -z "$SCM_TOKEN" ]]; then echo "$SCM_TOKEN_VAR is not set: nothing to clean up"; return 0; fi
  python3 "$SCM_HACK_DIR/scm.py" cleanup
}

# scrub DIR: look for the values of the tokens in the environment, plain and as
# base64 (how a Secret keeps them), in every file below DIR. Reports file names
# only. A value is replaced by *** in the file; the exit status says it was there.
scrub() {
  local dir="${1:?scrub needs a directory}"
  [[ -d "$dir" ]] || { echo "no logs in $dir"; return 0; }
  python3 - "$dir" <<'PY'
import base64, os, sys

secrets = []
for name in ("GITLAB_TOKEN", "GITHUB_TOKEN", "E2E_GITHUB"):
    v = os.environ.get(name, "")
    if len(v) >= 8:
        secrets += [v, base64.b64encode(v.encode()).decode()]
found = []
for root, _, files in os.walk(sys.argv[1]):
    for name in files:
        path = os.path.join(root, name)
        with open(path, "rb") as f:
            data = f.read()
        text = data
        hit = False
        for s in secrets:
            if s.encode() in text:
                hit = True
                text = text.replace(s.encode(), b"***")
        if hit:
            found.append(path)
            with open(path, "wb") as f:
                f.write(text)
for path in found:
    print("::error::a token value was in %s (it has been replaced by ***)" % path)
sys.exit(1 if found else 0)
PY
}

case "${1:-}" in
  plan) plan ;;
  run) run ;;
  cleanup) cleanup ;;
  scrub) scrub "${2:-}" ;;
  *) sed -n '3,30p' "${BASH_SOURCE[0]}"; exit 2 ;;
esac
