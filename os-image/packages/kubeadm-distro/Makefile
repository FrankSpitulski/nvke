# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

##@ Help

.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

GO ?= go

UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

# CRANE_VERSION/CRANE_LINUX_*_SHA256 and NFPM_VERSION/NFPM_LINUX_*_SHA256 live
# together in make/tool-versions.mk at the repository root, so a version bump
# and its digests move as one change. crane (from go-containerregistry)
# resolves image tags to manifest digests and pulls the pinned digests; nfpm
# packages the .deb. Neither is pinned for Darwin: a developer's own `crane`
# or `nfpm` (for example via `brew install crane`/`brew install nfpm`) is used
# instead.
include ../../../make/tool-versions.mk

# The Kubernetes releases bundled by `make debs`: the newest patch of each of the
# three newest minors. This is the single place to bump them; see README.md.
K8S_VERSIONS ?= v1.36.3 v1.35.3 v1.34.2

# Accept 1.36.3 as well as v1.36.3: the bundle directories and the download URLs
# are all leading-v, so normalise once here. Package names drop the v again.
K8S_VERSIONS_V := $(foreach v,$(K8S_VERSIONS),v$(patsubst v%,%,$(v)))

# Where the .deb files and the apt index are written. Required by `debs`.
OUT_DIR ?=

# Debian packaging revision. Bump when the package content changes but the
# Kubernetes patch does not.
DEB_REVISION ?= 1

# Downloaded upstream artifacts, reused across runs when they still verify.
DIST_DIR ?= $(CURDIR)/dist
BIN_DIR ?= $(CURDIR)/bin

# The committed lock files: one <version>.json per Kubernetes version, holding
# the sha256 of every artifact and the digest of every container image. The
# build refuses to run without them.
LOCK_DIR ?= $(CURDIR)/lock

##@ Build

# -trimpath and -s -w keep build-host paths and symbol tables out of the binaries
# that ship inside the .deb, matching agent/Makefile.
GO_BUILD_FLAGS := -trimpath -buildvcs=false -ldflags="-s -w"

.PHONY: debs
debs: check-host check-deb-tools bundle ## Build one .deb per K8S_VERSIONS into OUT_DIR, plus a flat apt index.
	@# Clear the previous run first: a .deb left from a different K8S_VERSIONS
	@# would be indexed here without appearing in any package's Conflicts.
	rm -f "$(OUT_DIR)"/nke-kubeadm-distro-*.deb "$(OUT_DIR)/Packages"
	@set -eu; \
	for v in $(K8S_VERSIONS_V); do \
		PEER_VERSIONS="$(K8S_VERSIONS_V)" DEB_REVISION="$(DEB_REVISION)" NFPM_BIN="$(BIN_DIR)/nfpm" \
			./packaging/build-deb.sh "$$v" "$(DIST_DIR)/$$v" "$(OUT_DIR)"; \
	done
	@cd "$(OUT_DIR)" && dpkg-scanpackages . /dev/null >Packages
	@echo "wrote $(OUT_DIR)/Packages"
ifneq ($(SKIP_CONTRACT_CHECK),1)
	$(MAKE) contract-check
endif

.PHONY: contract-check
# check-deb-tools is the OUT_DIR (non-empty, absolute) and tool-availability
# gate, shared with `debs`; contract-check itself is just the one debmanifest
# invocation below.
contract-check: check-deb-tools $(BIN_DIR)/debmanifest ## Compare debs in OUT_DIR against testdata/golden; strict once deviations.txt has content, advisory otherwise.
	$(BIN_DIR)/debmanifest check \
		-out-dir "$(OUT_DIR)" \
		-golden-dir "$(CURDIR)/testdata/golden" \
		-deviations "$(CURDIR)/testdata/golden/deviations.txt"

RUNC_KEYRING := $(CURDIR)/packaging/keyrings/runc-release.gpg

.PHONY: bundle
bundle: check-host $(BIN_DIR)/kubeadm-bundle $(BIN_DIR)/crane ## Download, verify and stage the upstream artifacts into DIST_DIR.
	$(BIN_DIR)/kubeadm-bundle -versions "$(K8S_VERSIONS_V)" -dist "$(DIST_DIR)" -lock "$(LOCK_DIR)" \
		-crane "$(BIN_DIR)/crane" -runc-keyring "$(RUNC_KEYRING)"

.PHONY: lock
lock: check-host $(BIN_DIR)/kubeadm-bundle $(BIN_DIR)/crane ## Regenerate lock/<version>.json from upstream. Review the diff, then commit it.
	@# Needs Linux and network: it downloads every artifact, runs the verified
	@# kubeadm for its image list, and resolves each tag to a manifest digest.
	$(BIN_DIR)/kubeadm-bundle -write-lock -versions "$(K8S_VERSIONS_V)" -dist "$(DIST_DIR)" -lock "$(LOCK_DIR)" \
		-crane "$(BIN_DIR)/crane" -runc-keyring "$(RUNC_KEYRING)"
	@echo
	@echo "Review the lock diff before committing it:"
	@echo "    git diff -- $(LOCK_DIR)"

$(BIN_DIR)/kubeadm-bundle: $(shell find . -name '*.go' -not -name '*_test.go')
	@mkdir -p $(@D)
	$(GO) build $(GO_BUILD_FLAGS) -o $@ ./cmd/kubeadm-bundle

# debmanifest runs on the build host, over the debs contract-check produces there.
$(BIN_DIR)/debmanifest: $(shell find . -name '*.go' -not -name '*_test.go')
	@mkdir -p $(@D)
	$(GO) build $(GO_BUILD_FLAGS) -o $@ ./cmd/debmanifest

$(BIN_DIR)/crane: ## Fetch the pinned crane (go-containerregistry) release for this host.
	@mkdir -p $(@D)
	@set -eu; \
	if [ "$(UNAME_S)" = "Darwin" ]; then \
		if command -v crane >/dev/null 2>&1; then \
			install -m 0755 "$$(command -v crane)" $@; \
		else \
			echo "kubeadm-distro: crane is not pinned for Darwin; install it (for example: brew install crane) so it is on PATH." >&2; \
			exit 1; \
		fi; \
	else \
		case "$(UNAME_M)" in \
			x86_64|amd64) arch=x86_64; sha=$(CRANE_LINUX_AMD64_SHA256) ;; \
			aarch64|arm64) arch=arm64; sha=$(CRANE_LINUX_ARM64_SHA256) ;; \
			*) echo "kubeadm-distro: no pinned crane release for $(UNAME_S)/$(UNAME_M)" >&2; exit 1 ;; \
		esac; \
		tmp=$$(mktemp -d); \
		trap 'rm -rf "$$tmp"' EXIT; \
		file="go-containerregistry_Linux_$${arch}.tar.gz"; \
		url="https://github.com/google/go-containerregistry/releases/download/v$(CRANE_VERSION)/$${file}"; \
		echo "downloading $${url}"; \
		curl --fail --silent --show-error --location \
			--connect-timeout 10 --max-time 120 \
			--retry 3 --retry-delay 5 --retry-connrefused \
			-o "$$tmp/$${file}" "$${url}"; \
		echo "$${sha}  $$tmp/$${file}" | sha256sum -c -; \
		tar -xzf "$$tmp/$${file}" -C "$$tmp" crane; \
		install -m 0755 "$$tmp/crane" $@; \
	fi

# Named alias, so ../../Makefile's `test` can ask for the binary by a phony
# name (`$(MAKE) -C packages/kubeadm-distro nfpm`) rather than the absolute
# $(BIN_DIR)/nfpm target path, matching go-licenses's alias in
# controller/Makefile.
.PHONY: nfpm
nfpm: $(BIN_DIR)/nfpm ## Fetch the pinned nfpm (goreleaser) release for this host.

$(BIN_DIR)/nfpm:
	@mkdir -p $(@D)
	@set -eu; \
	if [ "$(UNAME_S)" = "Darwin" ]; then \
		if command -v nfpm >/dev/null 2>&1; then \
			install -m 0755 "$$(command -v nfpm)" $@; \
		else \
			echo "kubeadm-distro: nfpm is not pinned for Darwin; install it (for example: brew install nfpm) so it is on PATH." >&2; \
			exit 1; \
		fi; \
	else \
		case "$(UNAME_M)" in \
			x86_64|amd64) arch=x86_64; sha=$(NFPM_LINUX_AMD64_SHA256) ;; \
			aarch64|arm64) arch=arm64; sha=$(NFPM_LINUX_ARM64_SHA256) ;; \
			*) echo "kubeadm-distro: no pinned nfpm release for $(UNAME_S)/$(UNAME_M)" >&2; exit 1 ;; \
		esac; \
		tmp=$$(mktemp -d); \
		trap 'rm -rf "$$tmp"' EXIT; \
		file="nfpm_$(NFPM_VERSION)_Linux_$${arch}.tar.gz"; \
		url="https://github.com/goreleaser/nfpm/releases/download/v$(NFPM_VERSION)/$${file}"; \
		echo "downloading $${url}"; \
		curl --fail --silent --show-error --location \
			--connect-timeout 10 --max-time 120 \
			--retry 3 --retry-delay 5 --retry-connrefused \
			-o "$$tmp/$${file}" "$${url}"; \
		echo "$${sha}  $$tmp/$${file}" | sha256sum -c -; \
		tar -xzf "$$tmp/$${file}" -C "$$tmp" nfpm; \
		install -m 0755 "$$tmp/nfpm" $@; \
	fi

##@ Checks

.PHONY: check-host
check-host: ## Fail early when this host cannot run the bundle step.
	@# The bundler runs the downloaded linux kubeadm binary to list the
	@# control-plane images, and stages the downloaded containerd/CNI/crictl/helm
	@# tarballs into an FHS tree -- both need a Linux host. There is no
	@# development shortcut that skips this: a .deb without images or without
	@# the staged runtime tree is not a product this build produces.
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "kubeadm-distro: the bundle step runs the downloaded linux kubeadm binary, so it needs a Linux host; this is $$(uname -s)." >&2; \
		exit 1; \
	fi
	@# crane itself is fetched by $(BIN_DIR)/crane; these are what fetch it with.
	@missing=""; \
	command -v curl >/dev/null 2>&1 || missing="$$missing curl"; \
	command -v sha256sum >/dev/null 2>&1 || missing="$$missing sha256sum"; \
	command -v tar >/dev/null 2>&1 || missing="$$missing tar"; \
	if [ -n "$$missing" ]; then \
		echo "kubeadm-distro: missing tools needed to fetch crane:$$missing" >&2; \
		exit 1; \
	fi

.PHONY: check-deb-tools
check-deb-tools: $(BIN_DIR)/nfpm ## Fail early when the Debian packaging tools are missing.
	@set -eu; \
	if [ "$$(uname -s)" != "Linux" ]; then \
		echo "kubeadm-distro: packages are built on a Linux host only; this is $$(uname -s)." >&2; \
		exit 1; \
	fi; \
	missing=""; \
	command -v dpkg-scanpackages >/dev/null 2>&1 || missing="$$missing dpkg-scanpackages(dpkg-dev)"; \
	command -v gpg >/dev/null 2>&1 || missing="$$missing gpg(gnupg)"; \
	if [ -n "$$missing" ]; then \
		echo "kubeadm-distro: missing build tools:$$missing" >&2; \
		echo "  install them with: apt install dpkg-dev gnupg" >&2; \
		exit 1; \
	fi
	@# Checked here rather than against MAKECMDGOALS, which `make clean debs`
	@# would defeat.
	@case "$(OUT_DIR)" in \
	  /*) ;; \
	  "") echo "kubeadm-distro: OUT_DIR is required, as an absolute path." >&2; exit 1 ;; \
	  *) echo "kubeadm-distro: OUT_DIR must be an absolute path, got '$(OUT_DIR)'." >&2; exit 1 ;; \
	esac

.PHONY: test
test: $(BIN_DIR)/nfpm ## Run this package's Go tests.
	NFPM_BIN="$(BIN_DIR)/nfpm" $(GO) test ./...

.PHONY: clean
clean: ## Remove the built binaries and the downloaded artifact tree.
	rm -rf $(BIN_DIR) $(DIST_DIR)
