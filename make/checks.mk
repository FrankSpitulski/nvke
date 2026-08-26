# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# The merge-gate checks that had no Make target.
#
# Four of the gate's thirteen members existed only as inline workflow shell, so a
# developer could not run them without copying YAML. Every tool comes from PATH,
# and a missing one reports which tool and two ways to get it.

CHART ?= controller/deploy/charts/cloud-workflow-engine

define require_tool
command -v $(1) >/dev/null 2>&1 || { \
	echo "ERROR: $(1) not found (CI uses $(2))."; \
	echo "  with flox:  flox activate"; \
	echo "  without:    $(3)"; \
	exit 1; }
endef

##@ Checks

lint-workflows: ## Lint the GitHub Actions workflows. (run by the merge gate)
	@$(call require_tool,actionlint,$(ACTIONLINT_VERSION),brew install actionlint)
	@actionlint -color
	@echo "  workflows: no findings"

# --severity=error matches shellcheck.yml. The tree is clean at error and is not
# clean at warning, so raising it here would fail on a backlog the gate does not
# block on. xargs -0, never mapfile: see hack/shell-scripts.sh.
lint-shell: ## Lint every tracked shell script. (run by the merge gate)
	@$(call require_tool,shellcheck,0.11.0,brew install shellcheck)
	@n=$$(./hack/shell-scripts.sh | tr -dc '\0' | wc -c | tr -d ' '); \
	 echo "  checking $$n shell scripts at severity=error"; \
	 ./hack/shell-scripts.sh | xargs -0 shellcheck --severity=error --format=tty
	@echo "  shell scripts: no error-level findings"

# helm lint and helm template both pass on a manifest Kubernetes would reject, so
# kubeconform parses what was rendered. A chart that renders nothing also passes
# helm template silently, hence the count.
lint-chart: ## Lint and render the Helm chart, then validate it. (run by the merge gate)
	@$(call require_tool,helm,$(HELM_CHART_VERSION),brew install helm)
	@helm lint $(CHART)
	@# A directory, not `mktemp -t`: kubeconform selects files by extension, and
	@# macOS mktemp appends random characters after the template, so a file named
	@# nke-chart-XXXXXX.qlKjrjG4RL is skipped and the summary reads
	@# "0 resource found in 0 file" while the target reports success.
	@dir=$$(mktemp -d); out="$$dir/rendered.yaml"; \
	 helm template cwe $(CHART) > "$$out"; \
	 count=$$(grep -c '^kind:' "$$out" || true); \
	 if [ "$$count" -eq 0 ]; then \
		echo "ERROR: the chart rendered no resources."; rm -rf "$$dir"; exit 1; \
	 fi; \
	 echo "  rendered $$count resources"; \
	 if command -v kubeconform >/dev/null 2>&1; then \
		kubeconform -strict -summary -ignore-missing-schemas \
			-schema-location default \
			-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
			"$$out"; \
	 else \
		echo "  NOTE: kubeconform not on PATH, so the rendered manifests were not"; \
		echo "        validated. CI uses $(KUBECONFORM_VERSION). brew install kubeconform"; \
	 fi; \
	 rm -rf "$$dir"
	@./controller/hack/chart-auth-assertions.sh

# Not the workspace's Go. Under go.work's 1.26.5 controller reports 7 reachable
# findings and agent 4; under CI's pin both report none. Exit 3 means "found
# something", any other non-zero means it could not run, which is not a finding.
vulncheck: ## Report vulnerabilities reachable from each module. (run by the merge gate)
	@$(call require_tool,govulncheck,latest,go install golang.org/x/vuln/cmd/govulncheck@latest)
	@echo "- govulncheck under Go $(GOVULNCHECK_GO_VERSION), the version CI uses"
	@fail=0; \
	 for m in $(WORK_MODULES); do \
		echo "- $$m"; \
		( cd $$m && GOTOOLCHAIN=go$(GOVULNCHECK_GO_VERSION) govulncheck ./... ); \
		rc=$$?; \
		if [ $$rc -ne 0 ] && [ $$rc -ne 3 ]; then \
			echo "  ERROR: govulncheck could not run in $$m (exit $$rc)"; fail=1; \
		elif [ $$rc -eq 3 ]; then \
			echo "  reachable vulnerabilities reported in $$m"; fail=1; \
		fi; \
	 done; \
	 [ $$fail -eq 0 ] || exit 1
	@echo "  govulncheck: nothing reachable"

##@ Everything

# Every module already has fmt and lint. There was no way to run them across the
# repository: `make lint` at the root failed with "No rule to make target". CI
# still calls the module targets directly, so its seven lint jobs stay parallel.
fmt: ## Format every module.
	@for m in $(PREFLIGHT_MODULES); do \
		echo "- fmt $$m"; \
		$(MAKE) --no-print-directory -C $$m fmt || exit 1; \
	 done

lint: ## Lint every module. (run by the merge gate)
	@for m in $(PREFLIGHT_MODULES); do \
		echo "- lint $$m"; \
		$(MAKE) --no-print-directory -C $$m lint || exit 1; \
	 done

# Not every gate member: three need Docker, kind, PostgreSQL or Linux on amd64.
# They are named at the end rather than run, so this stays something a developer
# will wait for. `make preflight` is the change-scoped version.
gate: ## Run the merge-gate checks that work on a laptop.
	@$(MAKE) --no-print-directory build
	@$(MAKE) --no-print-directory test
	@$(MAKE) --no-print-directory lint
	@$(MAKE) --no-print-directory work-check-offline
	@$(MAKE) --no-print-directory licenses-check
	@$(MAKE) --no-print-directory license-header-check
	@$(MAKE) --no-print-directory lint-workflows
	@$(MAKE) --no-print-directory lint-shell
	@$(MAKE) --no-print-directory lint-chart
	@$(MAKE) --no-print-directory -C controller generate-check
	@echo
	@echo "Not run here, because they need more than a laptop:"
	@echo "  nkectl-bootstrap-e2e   make -C cli test-e2e-bootstrap   docker, kind"
	@echo "  gofail                 make -C controller e2e-gofail    postgres, envtest"
	@echo "  govulncheck            make vulncheck                   slow"
	@echo "  CAPD e2e               make test-e2e-backend-ci         Linux amd64 only"
