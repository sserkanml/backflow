#!/usr/bin/env bash
#
# Writes the file-based catalog (FBC) of the Backflow operator to catalog/out/backflow/catalog.yaml:
#   the package, with the default channel and the icon,
#   a bundle for every release before this one (rendered from its published image) and for the bundle in bundle/ (rendered
#   from the directory, with the image name of BUNDLE_IMG),
#   the channel, in which each version replaces the one before it.
#
#   OPM=bin/opm BUNDLE_IMG=ghcr.io/sserkanml/backflow-bundle:1.0.1 BUNDLE_VERSION=1.0.1 ./hack/olm-catalog.sh
#
# Which releases come before: the git tags. A release is a tag vX.Y.Z, and every release from OLM_FIRST_VERSION on has a bundle
# image ghcr.io/sserkanml/backflow-bundle:X.Y.Z (the release workflow pushes it). The list is not kept in a file that somebody must
# remember to edit: it is derived from the tags, which are the record of what was released, and every one of them is rendered
# from its image, so a release whose bundle was not published makes this fail, and with it the release that was being made. That
# is the point: a catalog without the version before would upgrade nobody to this one. For the same reason a clone without the
# tags (a shallow one) is refused: it would leave versions out without a word.
#
# Environment: OPM, BUNDLE_IMG, BUNDLE_VERSION (required), IMAGE_REPO (default ghcr.io/sserkanml), CHANNEL (default stable),
# OLM_FIRST_VERSION (default 1.0.0: the first release with a bundle), OUT (default catalog/out), and for a test with bundles
# that are not released: PREVIOUS_BUNDLES, a space separated list of VERSION=IMAGE that replaces the tags, and OPM_RENDER_FLAGS
# (for example --use-http, for a registry on localhost).
#
# bash 3.2 compatible.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OPM="${OPM:-$ROOT/bin/opm}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/sserkanml}"
CHANNEL="${CHANNEL:-stable}"
FIRST="${OLM_FIRST_VERSION:-1.0.0}"
OUT="${OUT:-$ROOT/catalog/out}"
RENDER_FLAGS="${OPM_RENDER_FLAGS:-}"
: "${BUNDLE_IMG:?BUNDLE_IMG is required}" "${BUNDLE_VERSION:?BUNDLE_VERSION is required}"
[[ -d "$ROOT/bundle" ]] || { echo "no bundle/: run make bundle first" >&2; exit 1; }

# version_lt A B: A < B as X.Y.Z.
version_lt() {
  local a b i
  IFS=. read -r -a a <<<"$1"
  IFS=. read -r -a b <<<"$2"
  for i in 0 1 2; do
    if ((${a[i]:-0} < ${b[i]:-0})); then return 0; fi
    if ((${a[i]:-0} > ${b[i]:-0})); then return 1; fi
  done
  return 1
}

# previous: lines VERSION=IMAGE, oldest first.
previous=""
if [[ -n "${PREVIOUS_BUNDLES+x}" ]]; then
  for entry in $PREVIOUS_BUNDLES; do previous="$previous$entry"$'\n'; done
else
  if [[ "$(git -C "$ROOT" rev-parse --is-shallow-repository 2>/dev/null)" != "false" ]]; then
    echo "this clone is shallow or is not a git clone: the tags of the releases may be missing, and the catalog would leave" >&2
    echo "versions out. Clone with all the tags (git fetch --tags --unshallow, or actions/checkout with fetch-depth: 0)." >&2
    exit 1
  fi
  for tag in $(git -C "$ROOT" tag --list 'v[0-9]*' --sort=version:refname); do
    v="${tag#v}"
    [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
    version_lt "$v" "$FIRST" && continue
    version_lt "$v" "$BUNDLE_VERSION" || continue
    previous="$previous$v=$IMAGE_REPO/backflow-bundle:$v"$'\n'
  done
fi

# opm pulls the bundle images with containers/image, which refuses to run without a signature policy file (a machine without
# podman or skopeo has none). Pulling a bundle by its tag is what the release does, so an accept-all policy is made when there is none.
if [[ ! -f /etc/containers/policy.json && ! -f "${XDG_CONFIG_HOME:-$HOME/.config}/containers/policy.json" ]]; then
  policy_dir="$(mktemp -d)"
  trap 'rm -rf "$policy_dir"' EXIT
  mkdir -p "$policy_dir/containers"
  printf '{"default":[{"type":"insecureAcceptAnything"}]}\n' >"$policy_dir/containers/policy.json"
  export XDG_CONFIG_HOME="$policy_dir"
fi

rm -rf "$OUT"
mkdir -p "$OUT/backflow"
file="$OUT/backflow/catalog.yaml"

"$OPM" init backflow --default-channel="$CHANNEL" --icon="$ROOT/docs/olm/icon.svg" --output yaml >"$file"

versions=""
while IFS='=' read -r v image; do
  [[ -z "$v" ]] && continue
  # shellcheck disable=SC2086 # the flags are words
  "$OPM" render $RENDER_FLAGS "$image" --output yaml >>"$file" \
    || { echo "cannot render the bundle of $v from $image: the release $v has no published bundle, and the catalog needs it" >&2; exit 1; }
  versions="$versions $v"
done <<<"$previous"
# The bundle of this build: rendered from its directory, which has no image name.
"$OPM" render "$ROOT/bundle" --output yaml | sed "s#^image: \"\"#image: $BUNDLE_IMG#" >>"$file"

{
  echo "---"
  echo "schema: olm.channel"
  echo "package: backflow"
  echo "name: $CHANNEL"
  echo "entries:"
  last=""
  for v in $versions $BUNDLE_VERSION; do
    echo "  - name: backflow.v$v"
    [[ -n "$last" ]] && echo "    replaces: backflow.v$last"
    last="$v"
  done
} >>"$file"

"$OPM" validate "$OUT"
echo "catalog: $file ($(grep -c '^schema: olm.bundle' "$file") bundle(s):${versions:+$versions} $BUNDLE_VERSION; channel $CHANNEL)"
