# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary

# Module-level targets, matching the other workspace members. The root Makefile
# builds nkectl into the repository's bin/ as part of `make build`; this file is
# for working inside controller/nkectl/ alone, and it is what the Lint workflow
# invokes.
#
# Every dependency here is public, so these targets need neither the workspace
# nor the committed vendor/ tree, and CI runs them with GOWORK=off.

GO ?= go
BINARY := bin/nkectl

# Go tool install location (GOPATH/bin, unless GOBIN is set); matches
# agent/Makefile and controller/forge-vms/Makefile.
GOBIN ?= $(shell $(GO) env GOBIN)
ifeq (,$(GOBIN))
GOBIN = $(shell $(GO) env GOPATH)/bin
endif

GOLANGCI_LINT_REPO    ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2

# Version stamping, mirroring controller/Makefile.
#
# --match is not optional. This is a monorepo whose tags are per-component
# (agent/v*, controller/v*), so a bare `git describe --tags` returns whichever
# family happens to be nearest and would stamp nkectl with, say, a controller
# version. That is not hypothetical: hack/capd-kindest-node/build-image.sh
# derives an agent version with an unmatched `git describe` and already
# produces the wrong string.
#
# nkectl versions independently of the controller it sits under, so it gets its
# own tag family. No nkectl/v* tag exists yet, hence the two fallbacks: a plain
# v* tag, then v0.0.0. The matched prefix is stripped so the binary reports
# v1.2.3 rather than nkectl/v1.2.3. --dirty marks a build from a modified tree.
_NKECTL_VERSION_RAW := $(shell git describe --tags --dirty --match 'nkectl/v[0-9]*' 2>/dev/null || git describe --tags --dirty --match 'v[0-9]*' 2>/dev/null || echo "v0.0.0")
NKECTL_VERSION  ?= $(patsubst nkectl/%,%,$(_NKECTL_VERSION_RAW))
NKECTL_COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
NKECTL_TREE     ?= $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo clean || echo dirty)
# The commit timestamp, not the wall clock: building the same commit twice
# should produce the same binary.
NKECTL_DATE     ?= $(shell git log -1 --format=%cI 2>/dev/null || echo "unknown")

# "main", not the full import path. The variables live in package main, and
# for that package the linker matches on the package NAME. Given the import
# path it finds no symbol and silently changes nothing -- no error, no warning,
# just a binary that still reports "dev". Hence the version assertion in
# `make build`.
NKECTL_PKG := main
NKECTL_LDFLAGS := \
	-X '$(NKECTL_PKG).version=$(NKECTL_VERSION)' \
	-X '$(NKECTL_PKG).gitCommit=$(NKECTL_COMMIT)' \
	-X '$(NKECTL_PKG).gitTreeState=$(NKECTL_TREE)' \
	-X '$(NKECTL_PKG).buildDate=$(NKECTL_DATE)'

.PHONY: build test fmt lint license file-license-check

build:
	$(GO) build -ldflags "$(NKECTL_LDFLAGS)" -o $(BINARY) ./cmd/nkectl
	@# A wrong -X symbol is silently ignored by the linker, so the only way to
	@# know the stamping worked is to ask the binary. Cheap, and it fails loudly.
	@out="$$($(BINARY) version)"; \
	case "$$out" in \
	  *" dev "*|*"unknown"*) \
	    echo "ERROR: nkectl was built without version information: $$out"; \
	    echo "       The -X symbols in NKECTL_LDFLAGS no longer match the variables in cmd/nkectl."; \
	    exit 1;; \
	  *) echo "  $$out";; \
	esac

test:
	$(GO) test ./...

# golangci-lint applies the formatters enabled in .golangci.yml (gci, gofumpt),
# so there is no separate gofmt step here.
fmt:
	$(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint fmt ./...

# One recipe for CI and local, pinned by version, as in the sibling modules.
lint:
	$(GO) install $(GOLANGCI_LINT_REPO)@$(GOLANGCI_LINT_VERSION)
	$(GOBIN)/golangci-lint run --timeout 10m --max-same-issues 0 --max-issues-per-linter 0 ./...

license: ## Ensure license headers at code.
	$(GO) run github.com/google/addlicense@v1.1.1 -f ./hack/boilerplate.addlicense.txt .

# nkectl is a redistributed binary, so it carries its own attribution. It reuses
# the controller's generator rather than copying it: the script discovers the
# vendor tree by walking up, and takes the module and its packages from the
# environment, so one implementation serves every module in the workspace.
.PHONY: notices notices-check
notices: ## Regenerate THIRD_PARTY_NOTICES.md for the nkectl binary.
	@$(MAKE) --no-print-directory -C .. bin/go-licenses
	@MODULE_ROOT="$(CURDIR)" \
		OUTPUT="$(CURDIR)/THIRD_PARTY_NOTICES.md" \
		PACKAGES="./cmd/nkectl" \
		GO_LICENSES="$(CURDIR)/../bin/go-licenses" \
		bash ../hack/generate-notices

notices-check: ## Verify THIRD_PARTY_NOTICES.md is up to date.
	@# git diff says nothing about an untracked path, so check tracking first --
	@# otherwise a deleted or never-committed notices file passes silently.
	@git ls-files --error-unmatch THIRD_PARTY_NOTICES.md > /dev/null 2>&1 \
		|| { echo "THIRD_PARTY_NOTICES.md is not tracked by git. Run 'make notices' and commit it."; exit 1; }
	@$(MAKE) --no-print-directory notices
	@if ! git diff --exit-code -- THIRD_PARTY_NOTICES.md > /dev/null 2>&1; then \
		echo "THIRD_PARTY_NOTICES.md is out of date. Please run 'make notices' locally and commit the changes."; \
		git diff --name-only -- THIRD_PARTY_NOTICES.md; \
		exit 1; \
	fi

# The name matters beyond this file. license.yml builds its header-check matrix
# by selecting the modules whose Makefile defines a `file-license-check:`
# target, so a module without one is silently absent from that matrix rather
# than reported as unchecked. Expects a clean working directory.
file-license-check: license ## Verify all files have license headers.
	@if ! git diff --exit-code -- . > /dev/null 2>&1; then \
		echo "License headers are missing or outdated. Please run 'make license' locally and commit the changes."; \
		echo "Files with missing/outdated license headers:"; \
		git diff --name-only -- .; \
		exit 1; \
	fi
