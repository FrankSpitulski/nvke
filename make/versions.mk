# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Tool versions, in one place.
#
# Each of these was written only inside a workflow file, so a developer had no
# way to find out which version CI uses, and no way to match it. `make
# show-versions` prints the set, and the workflows read it back with
# `make print-<name>` rather than repeating the number.
#
# The values are plain make variables, not a YAML file parsed with yq. A sibling
# repository does the yq form, and it costs a parse-time dependency: every
# target, `make help` included, fails when yq is absent. Tools here are resolved
# when a recipe runs, never to load the Makefile.
#
# When bumping one of these, change it here and nowhere else, then run
# `make show-versions` to see the result.

# The Go that govulncheck and CodeQL run.
#
# NOT the workspace's Go, and the difference is not cosmetic. go.work asks for
# 1.26.5, and govulncheck.yml:167-169 records that the same controller/ tree
# reports 7 reachable standard-library vulnerabilities under 1.26.5 and 0 under
# 1.26.6. A developer running govulncheck with the workspace toolchain therefore
# sees findings CI does not, which is why this number has to be reachable from
# outside the workflow that hardcodes it.
GOVULNCHECK_GO_VERSION ?= 1.26.6

# Static analysis. actionlint.yml:77 and secrets.yml:88 install these by hand,
# each from a linux_amd64 tarball, which is why neither runs on a Mac. The Flox
# environment carries actionlint; gitleaks is not in it yet.
ACTIONLINT_VERSION ?= 1.7.12
GITLEAKS_VERSION   ?= 8.30.1

# Chart rendering and validation. chart.yml:111 downloads kubeconform inline.
KUBECONFORM_VERSION ?= v0.7.0

# Helm, and the one genuine disagreement in this file.
#
# backend-e2e.yml:113 and nkectl-bootstrap-e2e.yml:113 install v3.20.2, while
# chart.yml:74, chart-publish.yml:92 and chart-publish-nvcr.yml:68 install
# v3.19.0. Two versions of Helm render the charts this repository ships. The
# Flox environment pins 3.20.2, matching the e2e lanes.
#
# Unifying them is a behaviour change and belongs in its own commit, so both are
# recorded here rather than silently picking one.
HELM_VERSION       ?= v3.20.2
HELM_CHART_VERSION ?= v3.19.0

# The local cluster the e2e lanes create. backend-e2e.yml:118 and
# nkectl-bootstrap-e2e.yml:121.
KIND_VERSION    ?= v0.32.0
KUBECTL_VERSION ?= v1.36.0

# Artefact and image packaging. ORAS appears in four workflows, Packer in one.
ORAS_VERSION   ?= 1.3.3
PACKER_VERSION ?= 1.14.2

# Read one value from a workflow step, so a workflow never repeats a number:
#
#     V=$(make --no-print-directory print-KIND_VERSION)
#
# The pattern matches any variable, so it needs no upkeep as this file grows.
print-%:
	@echo "$($*)"

##@ Tool versions

show-versions: ## Print every pinned tool version.
	@echo "Tool versions (make/versions.mk):"
	@printf '  %-24s %s\n' \
		"GOVULNCHECK_GO_VERSION" "$(GOVULNCHECK_GO_VERSION)" \
		"ACTIONLINT_VERSION"     "$(ACTIONLINT_VERSION)" \
		"GITLEAKS_VERSION"       "$(GITLEAKS_VERSION)" \
		"KUBECONFORM_VERSION"    "$(KUBECONFORM_VERSION)" \
		"HELM_VERSION"           "$(HELM_VERSION)" \
		"HELM_CHART_VERSION"     "$(HELM_CHART_VERSION)" \
		"KIND_VERSION"           "$(KIND_VERSION)" \
		"KUBECTL_VERSION"        "$(KUBECTL_VERSION)" \
		"ORAS_VERSION"           "$(ORAS_VERSION)" \
		"PACKER_VERSION"         "$(PACKER_VERSION)"
	@echo
	@echo "Pinned in .flox/env/manifest.toml, and printed here from the live"
	@echo "environment rather than from a second copy of the number:"
	@printf '  %-24s %s\n' \
		"go"            "$$(command -v go >/dev/null && go version | awk '{print $$3}' || echo 'not on PATH')" \
		"golangci-lint" "$$(command -v golangci-lint >/dev/null && golangci-lint --version | awk '{print $$4}' || echo 'not on PATH')" \
		"make"          "$(MAKE_VERSION)"
