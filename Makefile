# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest kustomize ## Run tests (the Kustomize mapping tests render with kustomize v5.8.1).
	KUSTOMIZE_BIN="$(abspath $(KUSTOMIZE))" KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# To use a different vendor for e2e tests, modify the setup under 'test/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= backflow-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: helm-sync
helm-sync: manifests ## Copy the CRDs and regenerate the RBAC rules of the Helm chart from config/.
	./hack/helm-sync.sh

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	helm lint charts/backflow
	helm lint charts/backflow --set metrics.enabled=true --set serviceMonitor.enabled=true --set networkPolicy.enabled=true
	helm lint charts/backflow --set prometheusRule.enabled=true --set grafanaDashboard.enabled=true
	helm template backflow charts/backflow --set prometheusRule.enabled=true --set grafanaDashboard.enabled=true > /dev/null

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

# shellcheck is a release binary, not a Go program: it is downloaded for this platform
# and checked against the SHA-256 written here (the ones of the release assets, taken
# when the version was pinned), so that a machine without shellcheck lints the same as CI.
SHELLCHECK ?= $(LOCALBIN)/shellcheck
SHELLCHECK_VERSION ?= v0.11.0
SHELLCHECK_PLATFORM := $(shell uname -s | tr '[:upper:]' '[:lower:]').$(shell uname -m | sed 's/arm64/aarch64/')
SHELLCHECK_SHA256_linux.x86_64 = 8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198
SHELLCHECK_SHA256_linux.aarch64 = 12b331c1d2db6b9eb13cfca64306b1b157a86eb69db83023e261eaa7e7c14588
SHELLCHECK_SHA256_darwin.x86_64 = 3c89db4edcab7cf1c27bff178882e0f6f27f7afdf54e859fa041fca10febe4c6
SHELLCHECK_SHA256_darwin.aarch64 = 56affdd8de5527894dca6dc3d7e0a99a873b0f004d7aabc30ae407d3f48b0a79

$(SHELLCHECK): $(LOCALBIN)
	@[ -f "$(SHELLCHECK)-$(SHELLCHECK_VERSION)" ] && [ "$$(readlink -- "$(SHELLCHECK)" 2>/dev/null)" = "$(SHELLCHECK)-$(SHELLCHECK_VERSION)" ] || { \
	set -e; \
	want="$(SHELLCHECK_SHA256_$(SHELLCHECK_PLATFORM))"; \
	[ -n "$$want" ] || { echo "no shellcheck checksum for $(SHELLCHECK_PLATFORM); install shellcheck $(SHELLCHECK_VERSION) and set SHELLCHECK=<path>" >&2; exit 1; }; \
	archive="shellcheck-$(SHELLCHECK_VERSION).$(SHELLCHECK_PLATFORM).tar.xz"; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	echo "Downloading $$archive"; \
	curl -sSfL --retry 3 -o "$$tmp/$$archive" "https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/$$archive"; \
	got="$$( (sha256sum "$$tmp/$$archive" 2>/dev/null || shasum -a 256 "$$tmp/$$archive") | cut -d' ' -f1)"; \
	[ "$$got" = "$$want" ] || { echo "$$archive: checksum $$got, want $$want" >&2; exit 1; }; \
	tar -xJf "$$tmp/$$archive" -C "$$tmp"; \
	rm -f "$(SHELLCHECK)"; \
	mv "$$tmp/shellcheck-$(SHELLCHECK_VERSION)/shellcheck" "$(SHELLCHECK)-$(SHELLCHECK_VERSION)"; \
	} ; \
	ln -sf "$$(realpath "$(SHELLCHECK)-$(SHELLCHECK_VERSION)")" "$(SHELLCHECK)"

.PHONY: shellcheck
shellcheck: $(SHELLCHECK) ## Lint the shell scripts of hack/ with shellcheck.
	"$(SHELLCHECK)" -x -P SCRIPTDIR hack/*.sh hack/lib/*.sh

# actionlint checks the workflows, and with -shellcheck it checks the shell of every
# run: block with the pinned shellcheck above, never with whatever the machine has.
ACTIONLINT ?= $(LOCALBIN)/actionlint
ACTIONLINT_VERSION ?= v1.7.12

.PHONY: actionlint
actionlint: $(ACTIONLINT) $(SHELLCHECK) ## Lint the GitHub Actions workflows, and the shell in them.
	"$(ACTIONLINT)" -color -shellcheck="$(SHELLCHECK)"

$(ACTIONLINT): $(LOCALBIN)
	$(call go-install-tool,$(ACTIONLINT),github.com/rhysd/actionlint/cmd/actionlint,$(ACTIONLINT_VERSION))

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

# The version and the commit the binary reports (backflow_build_info): the
# nearest tag without its v (and -dirty for a modified tree) and the short commit.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
LDFLAGS = -X github.com/sserkanml/backflow/internal/version.Version=$(VERSION) -X github.com/sserkanml/backflow/internal/version.Commit=$(COMMIT)

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -ldflags "$(LDFLAGS)" -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(COMMIT)" -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name backflow-builder
	$(CONTAINER_TOOL) buildx use backflow-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --build-arg VERSION="$(VERSION)" --build-arg COMMIT="$(COMMIT)" --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm backflow-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

# v2.13 could not read the export data of Go 1.27.3 (Go 1.27.1 still worked) ("export data version
# 5 is greater than maximum supported version 4"), so it failed type-checking on a
# machine with a current Go; v2.15 reads it, and still builds with the Go of go.mod.
GOLANGCI_LINT_VERSION ?= v2.15.0
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

.PHONY: coverage
coverage: kustomize ## Measure Helm and Kustomize mapping coverage (needs the helm binary; installs kustomize v5.8.1, the version of Argo CD; writes test/coverage/REPORT.md).
	KUSTOMIZE_BIN="$(abspath $(KUSTOMIZE))" go test -tags coverage ./test/coverage/ -run TestCoverage -count=1 -v -timeout 45m

.PHONY: coverage-update
coverage-update: kustomize ## Accept the current mapping coverage as the golden file.
	KUSTOMIZE_BIN="$(abspath $(KUSTOMIZE))" go test -tags coverage ./test/coverage/ -run TestCoverage -count=1 -v -timeout 45m -args -update
