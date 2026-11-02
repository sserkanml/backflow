#!/usr/bin/env bash
#
# Writes the file-based catalog (FBC) of the Backflow operator to catalog/out/backflow/catalog.yaml:
#   the package, with the default channel and the icon,
#   a bundle for every version in catalog/bundles.txt (rendered from its image) and for the bundle in bundle/ (rendered from
#   the directory, with the image name of BUNDLE_IMG),
#   the channel, in which each version replaces the one before it.
#
#   OPM=bin/opm BUNDLE_IMG=ghcr.io/sserkanml/backflow-bundle:0.19.0 BUNDLE_VERSION=0.19.0 ./hack/olm-catalog.sh
#
# Environment: OPM, BUNDLE_IMG, BUNDLE_VERSION (required), IMAGE_REPO (default ghcr.io/sserkanml), CHANNEL (default stable),
# BUNDLES (default catalog/bundles.txt), OUT (default catalog/out).
#
# bash 3.2 compatible.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OPM="${OPM:-$ROOT/bin/opm}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/sserkanml}"
CHANNEL="${CHANNEL:-stable}"
BUNDLES="${BUNDLES:-$ROOT/catalog/bundles.txt}"
OUT="${OUT:-$ROOT/catalog/out}"
: "${BUNDLE_IMG:?BUNDLE_IMG is required}" "${BUNDLE_VERSION:?BUNDLE_VERSION is required}"
[[ -d "$ROOT/bundle" ]] || { echo "no bundle/: run make bundle first" >&2; exit 1; }

rm -rf "$OUT"
mkdir -p "$OUT/backflow"
file="$OUT/backflow/catalog.yaml"

"$OPM" init backflow --default-channel="$CHANNEL" --icon="$ROOT/docs/olm/icon.svg" --output yaml >"$file"

versions=""
if [[ -f "$BUNDLES" ]]; then
  while IFS= read -r line; do
    line="${line%%#*}"
    line="$(echo "$line" | tr -d '[:space:]')"
    [[ -z "$line" ]] && continue
    [[ "$line" == "$BUNDLE_VERSION" ]] && continue
    versions="$versions $line"
  done <"$BUNDLES"
fi

for v in $versions; do
  "$OPM" render "$IMAGE_REPO/backflow-bundle:$v" --output yaml >>"$file"
done
# The bundle of this build: rendered from its directory, which has no image name.
"$OPM" render "$ROOT/bundle" --output yaml | sed "s#^image: \"\"#image: $BUNDLE_IMG#" >>"$file"

{
  echo "---"
  echo "schema: olm.channel"
  echo "package: backflow"
  echo "name: $CHANNEL"
  echo "entries:"
  previous=""
  for v in $versions $BUNDLE_VERSION; do
    echo "  - name: backflow.v$v"
    [[ -n "$previous" ]] && echo "    replaces: backflow.v$previous"
    previous="$v"
  done
} >>"$file"

"$OPM" validate "$OUT"
echo "catalog: $file ($(grep -c '^schema: olm.bundle' "$file") bundle(s), channel $CHANNEL)"
