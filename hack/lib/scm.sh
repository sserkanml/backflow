# Which Git host the e2e scripts run against. Source it, do not run it.
#
#   SCM=gitlab|github   the host (default gitlab)
#   DEMO_REPO           the demo repository: owner/name, or its https URL.
#                       Default sserkanml/backflow-demo (gitlab),
#                       sserkanml/backflow-demo-gh (github).
#   GITLAB_TOKEN | GITHUB_TOKEN   the token of the host; read from the
#                       environment only, never printed.
#
# It sets (bash 3.2 compatible): SCM, SCM_LABEL (GitLab, GitHub), SCM_HOST,
# SCM_URL (https://host), SCM_PROJECT (owner/name), DEMO_REPO_URL (https, .git),
# SCM_MIN_LEVEL (the access level the token needs, in GitLab's numbers: 30 Developer,
# 40 Maintainer), SCM_TOKEN_VAR (the name of the variable that holds the token), SCM_TOKEN and
# SCM_CLUSTER (the default kind cluster name: GitLab and GitHub demos live in
# separate clusters, so they can coexist; their Applications have the same names).
# hack/scm.py, which the scripts call, reads SCM and the token itself.

SCM_HACK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCM="${SCM:-gitlab}"
case "$SCM" in
  gitlab)
    SCM_LABEL="GitLab"; SCM_TOKEN_VAR="GITLAB_TOKEN"; SCM_CLUSTER="backflow"
    SCM_DEFAULT_REPO="${GITLAB_PROJECT:-sserkanml/backflow-demo}"
    SCM_DEFAULT_HOST="gitlab.com"
    SCM_TOKEN_SCOPES="a token with the api and write_repository scopes"
    SCM_MIN_LEVEL=40  # Maintainer: main is protected, and the scripts restore it
    ;;
  github)
    SCM_LABEL="GitHub"; SCM_TOKEN_VAR="GITHUB_TOKEN"; SCM_CLUSTER="backflow-gh"
    SCM_DEFAULT_REPO="sserkanml/backflow-demo-gh"
    SCM_DEFAULT_HOST="github.com"
    SCM_MIN_LEVEL=30  # write access: main of the demo repository is not protected
    SCM_TOKEN_SCOPES="a fine-grained token for the demo repository only (Contents: read and write, Pull requests: read and write, Metadata: read)"
    ;;
  *)
    echo "SCM must be gitlab or github, not '$SCM'" >&2
    exit 1
    ;;
esac

_scm_repo="${DEMO_REPO:-$SCM_DEFAULT_REPO}"
case "$_scm_repo" in
  http://*|https://*)
    DEMO_REPO_URL="$_scm_repo"
    _scm_rest="${_scm_repo#*://}"
    SCM_HOST="${_scm_rest%%/*}"
    SCM_URL="${_scm_repo%%://*}://$SCM_HOST"
    SCM_PROJECT="${_scm_rest#*/}"
    SCM_PROJECT="${SCM_PROJECT%.git}"
    ;;
  *)
    SCM_HOST="$SCM_DEFAULT_HOST"
    SCM_URL="https://$SCM_HOST"
    SCM_PROJECT="${_scm_repo%.git}"
    DEMO_REPO_URL="$SCM_URL/$SCM_PROJECT.git"
    ;;
esac
unset _scm_repo _scm_rest

SCM_TOKEN="${!SCM_TOKEN_VAR:-}"
export SCM SCM_PROJECT SCM_TOKEN_VAR
