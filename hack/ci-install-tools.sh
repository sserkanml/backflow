#!/usr/bin/env bash
#
# Installs pinned versions of kind and kubectl for a GitHub Actions runner
# (linux/amd64), each checked against a SHA-256 that is written down here, and
# puts them first on the PATH of the later steps. Versions are bumped by
# changing the version and the checksum together; the checksums are the ones the
# projects publish next to the binaries.
#
#   ./hack/ci-install-tools.sh            # kind and kubectl
#   ./hack/ci-install-tools.sh kind
#
# KIND_NODE_IMAGE, below, is the node image the e2e workflow asks kind for.

set -euo pipefail

KIND_VERSION="v0.33.0"
KIND_SHA256="aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d"
# kubectl may differ by one minor version from the cluster: this is the cluster's.
KUBECTL_VERSION="v1.35.10"
KUBECTL_SHA256="c49a28f1f4efc4d475ed05f47f2ce8e563bf7bef69ed77fe1b924ea6d83b6327"

[[ "$(uname -s)-$(uname -m)" == "Linux-x86_64" ]] || { echo "only linux/amd64 is supported, this is $(uname -s)-$(uname -m)" >&2; exit 1; }

BIN="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/e2e-bin"
mkdir -p "$BIN"

# install <name> <url> <sha256>
install_checked() {
  local name="$1" url="$2" sha="$3"
  curl -sSfL --retry 3 -o "$BIN/$name" "$url"
  echo "$sha  $BIN/$name" | sha256sum --check --status || { echo "$name: the checksum of $url does not match" >&2; rm -f "$BIN/$name"; exit 1; }
  chmod +x "$BIN/$name"
  echo "installed $name"
}

want="${*:-kind kubectl}"
for tool in $want; do
  case "$tool" in
    kind) install_checked kind "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-amd64" "$KIND_SHA256" ;;
    kubectl) install_checked kubectl "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/amd64/kubectl" "$KUBECTL_SHA256" ;;
    *) echo "unknown tool '$tool'" >&2; exit 1 ;;
  esac
done

if [[ -n "${GITHUB_PATH:-}" ]]; then echo "$BIN" >> "$GITHUB_PATH"; fi
