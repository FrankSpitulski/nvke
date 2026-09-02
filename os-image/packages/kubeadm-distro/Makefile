# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

##@ Help

.PHONY: help
help: ## Show the available targets.
	@awk 'BEGIN { FS = ":.*## " } \
		/^##@ / { printf "\n%s\n", substr($$0, 5) } \
		/^[a-z][a-z0-9-]*:.*## / { printf "  %-22s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

# The Kubernetes releases bundled by `make debs`: the newest patch of each of the
# three newest minors. This is the single place to bump them; see README.md.
K8S_VERSIONS ?= v1.36.3 v1.35.3 v1.34.2

# Accept 1.36.3 as well as v1.36.3: versions.json keys and the download URLs are
# all leading-v, so normalise once here. Package names drop the v again.
K8S_VERSIONS_V := $(foreach v,$(K8S_VERSIONS),v$(patsubst v%,%,$(v)))

# Where the .deb files and the apt index are written. Required by `debs`.
OUT_DIR ?=

# The committed lock: every artifact URL and sha256, and every image ref,
# linux/amd64 digest and nix output hash. The build refuses to run without it.
LOCK ?= $(CURDIR)/versions.json

# Flakes are enabled per invocation so the host's nix.conf need not change.
NIX ?= nix
NIX_FLAGS := --extra-experimental-features 'nix-command flakes'
# path: rather than a bare '.': this directory sits inside a git work tree, and
# a bare path there resolves to git+file://<repo-root>?dir=..., which only sees
# committed files. `make debs` has to build what is on disk.
FLAKE := path:$(CURDIR)

##@ Build

.PHONY: debs
debs: check-nix check-out-dir ## Build one .deb per K8S_VERSIONS into OUT_DIR, plus a flat apt index.
	@mkdir -p "$(OUT_DIR)"
	@# Clear the previous run first: a .deb left from a different K8S_VERSIONS
	@# would be indexed here without being one of the packages just built.
	rm -f "$(OUT_DIR)"/nke-kubeadm-distro-*.deb "$(OUT_DIR)/Packages"
	@set -eu; \
	for v in $(K8S_VERSIONS_V); do \
		attr="deb-$$(echo "$${v#v}" | tr . _)"; \
		echo "building $$attr"; \
		out="$$($(NIX) $(NIX_FLAGS) build --no-link --print-out-paths "$(FLAKE)#$$attr")"; \
		install -m 0644 "$$out"/*.deb "$(OUT_DIR)/"; \
	done
	@# dpkg-scanpackages comes from the flake's own nixpkgs, so a bake host that
	@# has nix needs nothing else installed. It forks dpkg-deb by name, so the
	@# whole bin/ has to be on PATH, not just the one script.
	@set -eu; \
	dpkg="$$($(NIX) $(NIX_FLAGS) build --no-link --print-out-paths '$(FLAKE)#dpkg')"; \
	cd "$(OUT_DIR)" && PATH="$$dpkg/bin:$$PATH" dpkg-scanpackages . /dev/null >Packages
	@echo "wrote $(OUT_DIR)/Packages"
	@ls -l "$(OUT_DIR)"

.PHONY: lock
lock: check-nix ## Regenerate versions.json from upstream. Review the diff, then commit it.
	@# Needs Linux and network: it downloads every artifact, runs the verified
	@# kubeadm for its image list, and resolves each tag to a manifest digest.
	./prefetch.sh "$(LOCK)" $(K8S_VERSIONS_V)
	@echo
	@echo "Review the lock diff before committing it:"
	@echo "    git diff -- $(LOCK)"

##@ Checks

.PHONY: check
check: check-nix ## Evaluate every row and run the cheap gates (nix flake check).
	$(NIX) $(NIX_FLAGS) flake check "$(FLAKE)"

.PHONY: fmt
fmt: check-nix ## Format the nix files in place with the pinned nixfmt.
	@# Not `nix fmt`: on a path: flake that would format nix's own read-only
	@# copy of this directory in the store, not the files here.
	@set -eu; \
	fmt="$$($(NIX) $(NIX_FLAGS) build --no-link --print-out-paths '$(FLAKE)#formatter.x86_64-linux')"; \
	"$$fmt/bin/nixfmt" $(CURDIR)/*.nix

.PHONY: check-nix
check-nix: ## Fail early when this host cannot build the packages.
	@if ! command -v $(NIX) >/dev/null 2>&1; then \
		echo "kubeadm-distro: nix is required to build these packages." >&2; \
		echo "  Install it from https://nixos.org/download (the standard installer, not Determinate)." >&2; \
		exit 1; \
	fi
	@# x86_64-linux only: the .deb payload is linux/amd64 upstream binaries and
	@# fpm assembles them natively. There is no cross-build path here.
	@if [ "$$(uname -s)" != "Linux" ]; then \
		echo "kubeadm-distro: packages are built on an x86_64 Linux host only; this is $$(uname -s)." >&2; \
		echo "  On another platform, build inside a linux/amd64 container or a remote nix builder." >&2; \
		exit 1; \
	fi

.PHONY: check-out-dir
check-out-dir: ## Fail early when OUT_DIR is missing or relative.
	@case "$(OUT_DIR)" in \
	  /*) ;; \
	  "") echo "kubeadm-distro: OUT_DIR is required, as an absolute path." >&2; exit 1 ;; \
	  *) echo "kubeadm-distro: OUT_DIR must be an absolute path, got '$(OUT_DIR)'." >&2; exit 1 ;; \
	esac

.PHONY: clean
clean: ## Remove the local build symlinks. The nix store is managed by nix-collect-garbage.
	@# Only what this directory creates. OUT_DIR is caller-owned and may be
	@# anywhere, so `clean` does not touch it.
	rm -rf result result-*
