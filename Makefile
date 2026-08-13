# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Root Makefile: manages the Go workspace and builds the commands in it.
# The per-module Makefiles under agent/ and controller/ are unchanged and still
# work on their own. This one only covers what spans modules.
#
# See docs/go-workspace.md.

# The docs teach `GOWORK=off` for the modules outside the workspace. Inherited
# here it would make every workspace target fail with "no go.work file found",
# so it is dropped from the environment the recipes pass on.
unexport GOWORK

GO      ?= go
BIN_DIR ?= $(CURDIR)/bin

# Modules in go.work. Every one builds from public dependencies, so `work-sync`
# and `build` need no NVIDIA credentials and give an outside contributor the
# same result as CI.
WORK_MODULES := agent \
                controller/api \
                controller/hack/capd-reboot-controller \
                controller/hack/nico-mock \
                controller/test/e2e

# The subset that holds released commands, built from ./cmd. controller/api and
# controller/test/e2e carry library and test code with no ./cmd. The agent's
# sample clients under samples/ and sample-clients/ are package main too, and
# are deliberately not built: they are examples, not artefacts.
CMD_MODULES := agent \
               controller/hack/capd-reboot-controller \
               controller/hack/nico-mock

# Kept out of go.work on purpose: both require gitlab-master.nvidia.com, and one
# private member makes every module in the workspace unbuildable without
# credentials. `work-full` adds them for work inside NVIDIA.
PRIVATE_MODULES  := controller controller/forge-vms
CARBIDE_SDK      := gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go
CARBIDE_OVERRIDE := gitlab-master.nvidia.com/nvmetal/carbide-rest-ngc-sdk-go@v0.0.14

.DEFAULT_GOAL := help
.PHONY: help work work-sync work-check work-full build clean

help: ## Show the available targets.
	@echo "Usage: make <target>"
	@echo
	@awk 'BEGIN { FS = ":.*## " } /^[a-z][a-z-]*:.*## / { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
	@echo

work: ## Reset go.work to exactly the public modules, creating it if absent.
	@test -f go.work || $(GO) work init
	@# `go work use` only ever adds, so on its own this could not undo
	@# `make work-full` -- it left seven modules listed and still reported five.
	@for d in $$($(GO) work edit -json | grep -o '"DiskPath": "[^"]*"' | cut -d'"' -f4); do \
		case " $(addprefix ./,$(WORK_MODULES)) " in \
			*" $$d "*) ;; \
			*) $(GO) work edit -dropuse $$d ;; \
		esac; \
	done
	@for o in $$($(GO) work edit -json | grep -A1 '"Old"' | grep -o '"Path": "[^"]*"' | cut -d'"' -f4); do \
		$(GO) work edit -dropreplace $$o; \
	done
	@$(GO) work use $(addprefix ./,$(WORK_MODULES))
	@echo "go.work lists $$($(GO) work edit -json | grep -c '"DiskPath"') modules."

work-sync: work ## Refresh go.work.sum and the modules' go.mod and go.sum.
	@# A workspace builds every member against one unified version of each
	@# dependency, whether or not that version is written down. `go work sync`
	@# writes it down: each go.mod then states the version its module is really
	@# built with, so Dependabot, THIRD_PARTY_NOTICES.md and a reader all see the
	@# same thing. Leaving it out does not avoid the upgrade, it only hides it.
	@$(GO) work sync
	@# go work sync updates go.mod but not go.sum, which leaves each module
	@# unbuildable on its own -- and the modules ARE built on their own, by their
	@# own Makefiles, by the image builds and by every per-module CI job. Tidy
	@# each one with the workspace off to complete it.
	@for m in $(WORK_MODULES); do \
		(cd $$m && GOWORK=off $(GO) mod tidy) || exit 1; \
	done
	@# Deleted first: `go mod download all` only adds hashes, so without this
	@# go.work.sum accumulates entries for versions nothing references any more
	@# and stops being a function of the modules it describes.
	@rm -f go.work.sum
	@$(GO) mod download all
	@# The vendor tree is a function of the go.mod files above it, so it has to
	@# be regenerated here rather than by hand. A root vendor/ puts the whole
	@# workspace in -mod=vendor: leave it behind and every build stops with
	@# "is marked as explicit in vendor/modules.txt, but not explicitly
	@# required in a go.mod". That is how main broke on 2026-08-13.
	@$(GO) work vendor
	@echo "Workspace refreshed. Commit go.work, go.work.sum, vendor/ and every"
	@echo "go.mod and go.sum this changed, together."

work-check: ## Verify go.work and go.work.sum are complete and current.
	@# git diff reports nothing for an untracked path, so an uncommitted
	@# workspace file would otherwise pass this check in silence.
	@git ls-files --error-unmatch go.work go.work.sum >/dev/null 2>&1 \
		|| { echo "ERROR: go.work and go.work.sum are not both tracked. Run 'make work-sync' and commit them."; exit 1; }
	@echo "- Checking go.work lists the expected modules..."
	@listed=$$($(GO) work edit -json | grep -o '"DiskPath": "[^"]*"' | cut -d'"' -f4 | sed 's|^\./||' | sort); \
	 wanted=$$(printf '%s\n' $(WORK_MODULES) | sort); \
	 if [ "$$listed" != "$$wanted" ]; then \
	   echo "ERROR: go.work does not list the expected modules."; \
	   echo "  in go.work:"; printf '%s\n' "$$listed" | sed 's/^/    /'; \
	   echo "  expected:";   printf '%s\n' "$$wanted" | sed 's/^/    /'; \
	   echo "Run 'make work' and commit go.work."; exit 1; \
	 fi
	@echo "- Checking go.work carries no replace directive..."
	@# `make work-full` writes one, for work inside NVIDIA. Committed, it would
	@# silently repoint a dependency for every module in the workspace, and the
	@# module list above would still look correct.
	@[ "$$($(GO) work edit -json | grep -c '"Old"')" = "0" ] \
		|| { echo "ERROR: go.work carries a replace directive. That is 'make work-full' output and is local only. Undo it with 'git checkout -- go.work'."; exit 1; }
	@echo "- Checking every module in the tree is accounted for..."
	@dup=$$(printf '%s\n' $(WORK_MODULES) $(PRIVATE_MODULES) | sort | uniq -d); \
	 [ -z "$$dup" ] || { echo "ERROR: listed in both WORK_MODULES and PRIVATE_MODULES:"; printf '%s\n' "$$dup" | sed 's/^/    /'; exit 1; }
	@# A module added to the repository and to neither list is the failure this
	@# catches: it would never be built, linted or synced, and nothing else
	@# would report it.
	@found=$$(find . -name go.mod -not -path './vendor/*' -not -path '*/vendor/*' -not -path '*/testdata/*' | sed 's|^\./||; s|/go\.mod$$||' | sort); \
	 known=$$(printf '%s\n' $(WORK_MODULES) $(PRIVATE_MODULES) | sort); \
	 added=$$(printf '%s\n' "$$found" "$$known" "$$known" | sort | uniq -u); \
	 gone=$$(printf '%s\n' "$$known" "$$found" "$$found" | sort | uniq -u); \
	 if [ -n "$$added" ]; then \
	   echo "ERROR: these modules exist in the tree but are in neither WORK_MODULES nor PRIVATE_MODULES:"; \
	   printf '%s\n' "$$added" | sed 's/^/    /'; \
	   echo "Add each to the right list in the Makefile, then run 'make work-sync'."; exit 1; \
	 fi; \
	 if [ -n "$$gone" ]; then \
	   echo "ERROR: these modules are listed in the Makefile but no longer exist:"; \
	   printf '%s\n' "$$gone" | sed 's/^/    /'; exit 1; \
	 fi
	@echo "- Checking the workspace, go.work.sum and every go.mod agree..."
	@$(GO) work sync
	@for m in $(WORK_MODULES); do \
		(cd $$m && GOWORK=off $(GO) mod tidy) || exit 1; \
	done
	@rm -f go.work.sum
	@$(GO) mod download all
	@# Regenerated, then diffed with everything else. vendor/ is derived from the
	@# go.mod files, and a root vendor tree puts the workspace in -mod=vendor, so
	@# a stale one does not degrade the build, it stops it. main broke this way on
	@# 2026-08-13 and no job reported it, because this target did not look here.
	@$(GO) work vendor
	@# go.mod and go.sum are in the diff on purpose. The failure this catches is
	@# a dependency bump landing in one module without the workspace being
	@# refreshed: the other modules then build against a version none of their
	@# go.mod files admits to.
	@#
	@# --exit-code alone would miss a package that vendoring ADDS, because git
	@# does not diff an untracked path. vendor/ gains directories whenever a
	@# dependency does, so the untracked check below is not optional here.
	@git diff --exit-code -- go.work go.work.sum '*/go.mod' '*/go.sum' vendor \
		|| { echo "ERROR: the workspace is stale. Run 'make work-sync' and commit every file it changes."; exit 1; }
	@untracked=$$(git ls-files --others --exclude-standard -- vendor); \
	 [ -z "$$untracked" ] \
		|| { echo "ERROR: vendoring added files that are not tracked. Run 'make work-sync' and commit every file it changes:"; \
		     printf '%s\n' "$$untracked" | sed 's/^/    /'; exit 1; }
	@echo "go.work, go.work.sum and vendor/ are up to date."

build: ## Build every workspace package, and the released commands into bin/.
	@# Compile everything first. Building only ./cmd/... left controller/api and
	@# controller/test/e2e with no compile coverage anywhere in CI, and this
	@# change re-versioned both of them.
	@for m in $(WORK_MODULES); do \
		echo "- checking $$m"; \
		$(GO) build -C $$m ./... || exit 1; \
	done
	@mkdir -p $(BIN_DIR)
	@for m in $(CMD_MODULES); do \
		echo "- building $$m"; \
		$(GO) build -C $$m -o $(BIN_DIR)/ ./cmd/... || exit 1; \
	done
	@echo "Built into $(BIN_DIR):"
	@ls -1 $(BIN_DIR) | sed 's/^/    /'

work-full: ## NVIDIA-internal only: add controller and forge-vms. Do not commit the result.
	@$(GO) work use $(addprefix ./,$(WORK_MODULES) $(PRIVATE_MODULES))
	@# controller pins the carbide SDK at v0.0.14 and forge-vms at v0.0.9. A
	@# workspace holds one replacement, so without this override every module
	@# fails with "conflicting replacements". This forces forge-vms onto v0.0.14.
	@$(GO) work edit -replace $(CARBIDE_SDK)=$(CARBIDE_OVERRIDE)
	@echo
	@echo "go.work now includes $(PRIVATE_MODULES)."
	@echo "It needs gitlab-master.nvidia.com credentials, and forge-vms is forced"
	@echo "onto $(CARBIDE_OVERRIDE)."
	@echo "DO NOT COMMIT go.work in this state. Restore it with:  git checkout -- go.work"

clean: ## Remove the built binaries.
	@rm -rf $(BIN_DIR)
