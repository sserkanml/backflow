##@ OLM (Operator Lifecycle Manager: the bundle and the catalog)

# operator-sdk and opm are release binaries, downloaded for this platform and checked against the SHA-256 written
# here (the ones the projects publish in checksums.txt, taken when the version was pinned).
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
OPERATOR_SDK_VERSION ?= v1.42.3
OPM ?= $(LOCALBIN)/opm
OPM_VERSION ?= v1.74.0
OLM_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
OLM_ARCH := $(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
OLM_PLATFORM := $(OLM_OS)_$(OLM_ARCH)
OPERATOR_SDK_SHA256_darwin_arm64 = 098ae8b9dbe7dfd557e8e7ed0f1996736922dd4b984621df2aa033f225cae161
OPERATOR_SDK_SHA256_darwin_amd64 = 7cb0f24bb63b6383a117291ee4c808953c5dd789d5877da98051aa68b41f40ac
OPERATOR_SDK_SHA256_linux_amd64 = 887a3bb0d63ccc4ca47a522d0c8ffac56d9d5246f6a2bd886b4ed23eb2e2672f
OPM_SHA256_darwin_arm64 = 0ce2671c543e637ae24cb98dfeeec1a9554e3955a72c59eb872750eaf915627f
OPM_SHA256_darwin_amd64 = a448e5972689036cb1168b83e18195b193bb513399c234d402c425b073a52ea6
OPM_SHA256_linux_amd64 = cf1bd699be72e0a4511208afdf45444fc47d21eb2ffed0c204520ff50b5a4691

# download-checked <binary> <version> <url> <sha256>
define download-checked
@[ -f "$(1)-$(2)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(2)" ] || { \
	set -e; \
	want="$(4)"; \
	[ -n "$$want" ] || { echo "no checksum for $(OLM_PLATFORM) of $(notdir $(1)) $(2)" >&2; exit 1; }; \
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	echo "Downloading $(3)"; \
	curl -sSfL --retry 3 -o "$$tmp/bin" "$(3)"; \
	got="$$( (sha256sum "$$tmp/bin" 2>/dev/null || shasum -a 256 "$$tmp/bin") | cut -d' ' -f1)"; \
	[ "$$got" = "$$want" ] || { echo "$(3): checksum $$got, want $$want" >&2; exit 1; }; \
	chmod +x "$$tmp/bin"; rm -f "$(1)"; mv "$$tmp/bin" "$(1)-$(2)"; \
	} ; \
	ln -sf "$$(realpath "$(1)-$(2)")" "$(1)"
endef

.PHONY: operator-sdk
operator-sdk: $(OPERATOR_SDK) ## Download operator-sdk (pinned, checksummed) if necessary.
$(OPERATOR_SDK): $(LOCALBIN)
	$(call download-checked,$(OPERATOR_SDK),$(OPERATOR_SDK_VERSION),https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$(OLM_PLATFORM),$(OPERATOR_SDK_SHA256_$(OLM_PLATFORM)))

.PHONY: opm
opm: $(OPM) ## Download opm (pinned, checksummed) if necessary.
$(OPM): $(LOCALBIN)
	$(call download-checked,$(OPM),$(OPM_VERSION),https://github.com/operator-framework/operator-registry/releases/download/$(OPM_VERSION)/$(OLM_OS)-$(OLM_ARCH)-opm,$(OPM_SHA256_$(OLM_PLATFORM)))

BUNDLE_VERSION ?= $(shell sed -n 's/^appVersion: *//p' charts/backflow/Chart.yaml | tr -d '"')
IMAGE_REPO ?= ghcr.io/sserkanml
# The operator image keeps the name and the tag of config/manager (the release bumps it); OPERATOR_IMAGE overrides it, for
# a test with an image that was built here.
OPERATOR_IMAGE ?= $(IMAGE_REPO)/backflow:$(BUNDLE_VERSION)
BUNDLE_IMG ?= $(IMAGE_REPO)/backflow-bundle:$(BUNDLE_VERSION)
CATALOG_IMG ?= $(IMAGE_REPO)/backflow-catalog:$(BUNDLE_VERSION)
OLM_CHANNEL ?= stable
# The versions of the bundles that were published before this one, one per line: the catalog has them, and the channel
# replaces them in this order.
CATALOG_BUNDLES ?= catalog/bundles.txt

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle/ (the CSV, the CRDs, the RBAC) and bundle.Dockerfile from config/.
	"$(OPERATOR_SDK)" generate kustomize manifests --interactive=false --package backflow --apis-dir api -q
	rm -rf bundle bundle.Dockerfile
	tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	cp -R config "$$tmp/config"; \
	( cd "$$tmp/config/manager" && "$(abspath $(KUSTOMIZE))" edit set image controller=$(OPERATOR_IMAGE) ); \
	"$(KUSTOMIZE)" build "$$tmp/config/manifests" | "$(OPERATOR_SDK)" generate bundle -q --overwrite \
		--version $(BUNDLE_VERSION) --package backflow --channels $(OLM_CHANNEL) --default-channel $(OLM_CHANNEL) \
		--kustomize-dir "$$tmp/config/manifests"
	@# relatedImages: the operator image, which is the only one (RELATED_IMAGE_BACKFLOW of config/manager)
	sed -i.bak 's#controller:latest#$(OPERATOR_IMAGE)#' bundle/manifests/backflow.clusterserviceversion.yaml && rm -f bundle/manifests/*.bak
	sed -i.bak 's#containerImage: .*#containerImage: $(OPERATOR_IMAGE)#' bundle/manifests/backflow.clusterserviceversion.yaml && rm -f bundle/manifests/*.bak
	"$(OPERATOR_SDK)" bundle validate ./bundle

.PHONY: bundle-validate
bundle-validate: operator-sdk ## Validate bundle/ with the operatorframework and OperatorHub suites (run make bundle first).
	"$(OPERATOR_SDK)" bundle validate ./bundle
	"$(OPERATOR_SDK)" bundle validate ./bundle --select-optional suite=operatorframework --optional-values=k8s-version=1.30
	"$(OPERATOR_SDK)" bundle validate ./bundle --select-optional name=good-practices

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: catalog
catalog: opm ## Generate the file-based catalog in catalog/out from catalog/bundles.txt and bundle/ (run make bundle first).
	OPM="$(OPM)" BUNDLE_IMG="$(BUNDLE_IMG)" BUNDLE_VERSION="$(BUNDLE_VERSION)" IMAGE_REPO="$(IMAGE_REPO)" CHANNEL="$(OLM_CHANNEL)" ./hack/olm-catalog.sh

.PHONY: catalog-build
catalog-build: ## Build the catalog image from catalog/out.
	$(CONTAINER_TOOL) build -f catalog/catalog.Dockerfile -t $(CATALOG_IMG) catalog

.PHONY: olm-actor-webhook
olm-actor-webhook: ## Generate docs/olm/actor-webhook.yaml (the optional actor webhook for OLM) from the chart.
	./hack/olm-actor-webhook.sh
