.PHONY: help build test tests test.integration test.live harness-e2e lint lint.paths lint.fix fmt fmt.check all verify verify-offline check ci long-fuzz race stress arch semgrep secrets mutation mutation.critical mutation.nightly tools workflow.check

.DEFAULT_GOAL := help

# Go and golangci-lint versions are pinned in mise.toml (golangci-lint 2.12.2 is
# the floor for the `modernize` linter in .golangci.yml).
GO_ARCH_LINT_VERSION ?= v1.16.0
GREMLINS_VERSION ?= v0.6.0
GOPLS_VERSION ?= v0.20.0
ACTIONLINT_VERSION ?= v1.7.12
SEMGREP_VERSION ?= 1.168.0
GITLEAKS_VERSION ?= v8.30.1

GOLANGCI_RUN = golangci-lint run ./...
SEMGREP ?= uv tool run --offline --from semgrep==$(SEMGREP_VERSION) semgrep

# Verification consumes only dependencies prepared by `make tools`. Target-
# specific exports flow into prerequisites and subprocesses (including go list
# and mutation workers) without disabling the explicitly online bootstrap.
OFFLINE_TARGETS := all verify verify-offline check ci build test tests \
	test.integration harness-e2e long-fuzz race stress mutation mutation.critical mutation.nightly \
	lint lint.paths arch semgrep secrets
$(OFFLINE_TARGETS): export GOPROXY := off
$(OFFLINE_TARGETS): export GOSUMDB := off
$(OFFLINE_TARGETS): export GOTOOLCHAIN := local

# The binary version is stamped from git. The fallback is "dev", not a
# plausible-looking number: a build without tags must be obvious in a version-skew
# report, not silently claim to be a release.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_PKG := github.com/pilat/coagent/internal/version
GO_LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION)

# Service installation is `coagent daemon install`, not a target here: it runs on
# the target machine and has to pick the unit scope and copy the binary itself.

help:
	@echo "Everyday gate:"
	@echo "  all / verify     format check + build + lint + arch + semgrep + secret scan + tests"
	@echo "  verify-offline   verify with Go/uv network resolution disabled"
	@echo "  check            all + integration tests      (needs local git/gopls)"
	@echo "  ci               slow local CI: all + integration + harness E2E + long fuzz + race"
	@echo "                   + protocol stress"
	@echo ""
	@echo "Pieces:"
	@echo "  fmt              apply the formatters"
	@echo "  fmt.check        report formatting drift without modifying files"
	@echo "  build            compile the binary"
	@echo "  tests            go test ./... (build-tagged files excluded)"
	@echo "  test.integration go test -tags=integration ./..."
	@echo "  lint             golangci-lint over the whole module"
	@echo "  lint.paths       scoped lint (LINT_PATHS=./internal/session/...)"
	@echo "  lint.fix         apply every golangci-lint autofix"
	@echo "  arch             go-arch-lint only"
	@echo "  semgrep          project invariants only"
	@echo "  secrets          scan Git history and working tree for committed credentials"
	@echo "  workflow.check   validate GitHub Actions workflows with actionlint"
	@echo "  tools            online bootstrap for modules and pinned dev tools"
	@echo ""
	@echo "Opt-in (slow):"
	@echo "  harness-e2e      compiled daemon + socket + fake LLM process tests"
	@echo "  test.live        credentialed network smoke tests (not part of CI)"
	@echo "  long-fuzz        model-based protocol fuzzing (CI_FUZZ_TIME=5m)"
	@echo "  race             full default suite under Go's race detector"
	@echo "  stress           repeat/shuffle critical protocol tests"
	@echo ""
	@echo "Mutation diagnostics (never gates):"
	@echo "  mutation MUTATION_PATH=./internal/session"
	@echo "  mutation.critical  manual curated critical-file diagnostic"
	@echo "  mutation.nightly   scheduled workflow shard; do not run as a handoff gate"

# Everything that must be green before a commit, and nothing that needs the
# network. Every gate is listed by name: burying arch/semgrep inside `lint` made
# people read `all` and conclude they were missing.
all verify: fmt.check build lint arch semgrep secrets tests

# Prove the warmed checkout does not need module or Python-package resolution.
# Missing modules or uv tool state fail closed; only `tools` may populate them.
verify-offline:
	UV_OFFLINE=1 $(MAKE) verify

# `all` plus hermetic suites that exercise installed programs (real gopls and
# local-only git clone/pull). They never clone a network repository.
check: all test.integration

# Slow, reproducible, local pre-merge gate. Its test workloads are hermetic: the
# compiled-process E2E uses a local fake LLM server, Git clones only temporary
# local repositories, and the stress suite needs no external services. Local dev
# tools are still required and are provisioned explicitly by `make tools`.
# Budgets are variables so a developer can run a short smoke first
# without changing the canonical defaults used for the final local CI run.
ci: all test.integration harness-e2e long-fuzz race stress

build:
	go build -ldflags "$(GO_LDFLAGS)" -o coagent ./cmd/coagent

test tests:
	go test ./...

# Integration tests are behind //go:build integration: they drive real local
# programs such as gopls and git, so they stay out of the default run. Git tests
# clone only temporary local repositories; no mutable network repository is a
# test dependency. `lint` still compiles and lints these files (run.build-tags in
# .golangci.yml).
test.integration:
	go test -tags=integration -count=1 ./...

# Credentialed provider checks are intentionally outside every quality gate.
# They require network access and explicit provider environment variables; the
# `live` tag keeps them out of otherwise hermetic test discovery.
test.live:
	go test -tags=live -count=1 -v ./internal/llm

CI_E2E_COUNT ?= 3

harness-e2e:
	go test -tags=integration -count=$(CI_E2E_COUNT) ./cmd/coagent -run '^TestHarnessE2E_'

CI_FUZZ_TIME ?= 5m

# The cached corpus replays as fuzz baseline before a single new input runs.
# Each protocol exec drives two full SQLite suites (~0.4s), so an accumulated
# cache starves generation of the whole budget. Clearing it per run keeps the
# baseline at the checked-in seeds; regression corpus lives in testdata/fuzz.
long-fuzz:
	go clean -fuzzcache
	go test ./internal/sessionstore -run '^$$' -fuzz '^FuzzHarnessProtocol$$' -fuzztime=$(CI_FUZZ_TIME)
	go test ./internal/sessionstore -run '^$$' -fuzz '^FuzzManagerOutputProtocol$$' -fuzztime=$(CI_FUZZ_TIME)

race:
	go test -race -count=1 -timeout=20m ./...

CI_STRESS_COUNT ?= 25
CI_STRESS_TIMEOUT ?= 15m
CI_STRESS_PACKAGES := ./internal/session ./internal/sessionstore ./internal/daemon ./internal/schedule ./internal/managerdelivery ./internal/managers/cli ./internal/managers/telegram ./internal/migrate
CI_STRESS_RUN := Test(Harness|Worker|OutputTransport|ExecuteToolCalls_(RejectsSleepAlongside|RejectedSleepDoesNotSkip)|Integration_(StressBlockingNoDeadlock|BackgroundTaskRejectsCompetingSleepProtocol|ScatterGatherBlockingTasks|OneShotAckFailureRedeliversWithoutDuplicateTranscriptOrPublication|FreshScheduleDuplicateDoesNotResetOrRunTwice)|Executor_CronAckRetryKeepsCanonicalIdentityAndPayload|ScheduledDeliveryStore_ContextResetRollsBackClaimAndTranscriptOnInsertFailure|SendMessage_DoesNotDuplicateOnRateLimitOrAmbiguousTransportFailure|FollowUpAcceptedBeforeTerminalBoundaryStaysInSameActivation|Stop(ParksWholeTreeAndExplicitFollowUpResumesOnlyChild|DirectChildParksItsOwnLinkWithoutStoppingParent)|StartFinishesInterruptedStopBeforeRecoverySweep|SubagentWaitGuardRejectsSleepUntilCompletionDelivered|OpenDB_ExplicitTransactionsReserveWriterAtBegin|BudgetStore_ArmFireAndReplayAreAtomic)

stress:
	go test -shuffle=on -count=$(CI_STRESS_COUNT) -timeout=$(CI_STRESS_TIMEOUT) -run '$(CI_STRESS_RUN)' $(CI_STRESS_PACKAGES)

# The only online bootstrap target. Go and uv themselves come from mise; all Go
# helpers are pinned here, and the online Semgrep invocation warms uv's tool cache.
tools:
	go mod download
	golangci-lint version
	go install golang.org/x/tools/gopls@$(GOPLS_VERSION)
	go install github.com/fe3dback/go-arch-lint@$(GO_ARCH_LINT_VERSION)
	go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)
	go install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	go install github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)
	@command -v uv >/dev/null 2>&1 || { echo "✋ uv missing (semgrep runs through it): curl -LsSf https://astral.sh/uv/install.sh | sh"; exit 1; }
	uv tool run --from semgrep==$(SEMGREP_VERSION) semgrep --version

# Architecture boundary and documentation-contract checks. Gates never install.
arch:
	@go-arch-lint version 2>/dev/null | grep -q "$(GO_ARCH_LINT_VERSION)" || { echo "✋ go-arch-lint $(GO_ARCH_LINT_VERSION) required; run make tools"; exit 1; }
	@go-arch-lint self-inspect --json >/dev/null
	go-arch-lint check
	./scripts/check-architecture.sh

# Project invariants that are not expressible as a Go linter (.semgrep/). Every
# rule there is at zero violations, so this is a hard gate with no baseline.
semgrep:
	@command -v uv >/dev/null 2>&1 || { echo "✋ uv missing (semgrep runs through it): curl -LsSf https://astral.sh/uv/install.sh | sh"; exit 1; }
	$(SEMGREP) scan --config .semgrep/ --error --metrics=off --quiet .

secrets:
	@command -v gitleaks >/dev/null 2>&1 || { echo "✋ gitleaks missing; run make tools"; exit 1; }
	@gitleaks_bin="$$(command -v gitleaks)"; \
		go version -m "$$gitleaks_bin" | awk '$$1 == "mod" && $$2 == "github.com/zricethezav/gitleaks/v8" && $$3 == "$(GITLEAKS_VERSION)" { found = 1 } END { exit !found }' || { echo "✋ gitleaks $(GITLEAKS_VERSION) required; run make tools"; exit 1; }
	gitleaks git --redact=100 .
	gitleaks dir --redact=100 .

workflow.check:
	@actionlint -version 2>/dev/null | grep -q "$(patsubst v%,%,$(ACTIONLINT_VERSION))" || { echo "✋ actionlint $(ACTIONLINT_VERSION) required; run make tools"; exit 1; }
	actionlint

lint:
	golangci-lint config verify
	$(GOLANGCI_RUN)

lint.paths:
	@if [ -z "$(strip $(LINT_PATHS))" ]; then echo "✋ LINT_PATHS is required; for example ./internal/session/..."; exit 1; fi
	golangci-lint config verify
	golangci-lint run $(LINT_PATHS)

# Apply every autofix golangci-lint offers, then report what still needs a human.
lint.fix:
	golangci-lint cache clean
	$(GOLANGCI_RUN) --fix

fmt:
	golangci-lint fmt

fmt.check:
	@output="$$(mktemp)"; trap 'rm -f "$$output"' EXIT HUP INT TERM; \
		if ! golangci-lint fmt --diff >"$$output"; then cat "$$output"; exit 1; fi; \
		if [ -s "$$output" ]; then cat "$$output"; exit 1; fi

# Mutation testing (.gremlins.yaml) is diagnostic, never a commit, PR, pre-merge,
# or handoff gate. Do not add any mutation target to all/check/ci. The generic
# target requires an explicit filesystem scope because every mutant reruns tests.
MUTATION_WORKERS ?= 4
MUTATION_EFFICACY ?= 0
MUTATION_COVERAGE ?= 0
# A surviving mutant runs the whole suite; killed ones exit early. Too tight a
# coefficient turns survivors into TIMED OUT and silently hides the real debt.
MUTATION_TIMEOUT_COEFFICIENT ?= 30

# This manual diagnostic keeps a convenient curated scope for targeted test
# audits. It is deliberately not a prerequisite of any verification target.
CRITICAL_MUTATION_DIR := internal/session
CRITICAL_MUTATION_FILES := toolexec.go message_persist.go loop_boundary.go todo_replace.go
# Gremlins patterns are unanchored regexps, so a bare basename like "store.go"
# would also match every *_store.go — anchor each exclude to the basename.
CRITICAL_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CRITICAL_MUTATION_FILES),$(filter-out %_test.go,$(notdir $(wildcard $(CRITICAL_MUTATION_DIR)/*.go)))),--exclude-files '(^|/)$(file)$$')
CRITICAL_SCHEDULE_MUTATION_FILES := executor.go service.go store.go
CRITICAL_SCHEDULE_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CRITICAL_SCHEDULE_MUTATION_FILES),$(filter-out %_test.go,$(notdir $(wildcard internal/schedule/*.go)))),--exclude-files '(^|/)$(file)$$')
CRITICAL_STORE_MUTATION_FILES := scheduled_delivery_store.go output_delivery_store.go output_lifecycle_store.go output_message_store.go activation_store.go budget_state_store.go budget_output_store.go direct_output_store.go progress_store.go readiness_store.go
CRITICAL_STORE_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CRITICAL_STORE_MUTATION_FILES),$(filter-out %_test.go,$(notdir $(wildcard internal/sessionstore/*.go)))),--exclude-files '(^|/)$(file)$$')
CRITICAL_TELEGRAM_MUTATION_FILES := delivery.go delivery_errors.go
CRITICAL_TELEGRAM_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CRITICAL_TELEGRAM_MUTATION_FILES),$(filter-out %_test.go,$(notdir $(wildcard internal/managers/telegram/*.go)))),--exclude-files '(^|/)$(file)$$')
CRITICAL_DAEMON_MUTATION_FILES := budget_park.go progress.go readiness.go
CRITICAL_DAEMON_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CRITICAL_DAEMON_MUTATION_FILES),$(filter-out %_test.go,$(notdir $(wildcard internal/daemon/*.go)))),--exclude-files '(^|/)$(file)$$')

mutation.critical:
	@go version -m "$$(command -v gremlins)" 2>/dev/null | grep -q "github.com/go-gremlins/gremlins[[:space:]]*$(GREMLINS_VERSION)" || { echo "✋ gremlins $(GREMLINS_VERSION) required; run make tools"; exit 1; }
	gremlins unleash ./$(CRITICAL_MUTATION_DIR) \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(MUTATION_EFFICACY) \
		--threshold-mcover $(MUTATION_COVERAGE) \
		$(CRITICAL_MUTATION_EXCLUDES)
	gremlins unleash ./internal/schedule \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(MUTATION_EFFICACY) \
		--threshold-mcover $(MUTATION_COVERAGE) \
		$(CRITICAL_SCHEDULE_MUTATION_EXCLUDES)
	gremlins unleash ./internal/sessionstore \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(MUTATION_EFFICACY) \
		--threshold-mcover $(MUTATION_COVERAGE) \
		$(CRITICAL_STORE_MUTATION_EXCLUDES)
	gremlins unleash ./internal/managers/telegram \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(MUTATION_EFFICACY) \
		--threshold-mcover $(MUTATION_COVERAGE) \
		$(CRITICAL_TELEGRAM_MUTATION_EXCLUDES)
	gremlins unleash ./internal/budget \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(MUTATION_EFFICACY) \
		--threshold-mcover $(MUTATION_COVERAGE)
	gremlins unleash ./internal/daemon \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(MUTATION_EFFICACY) \
		--threshold-mcover $(MUTATION_COVERAGE) \
		$(CRITICAL_DAEMON_MUTATION_EXCLUDES)

# NIGHTLY DIAGNOSTIC ONLY. These shards cover the production Go module while
# keeping the slow daemon package below the hosted-runner job limit. Survivors
# are report data (thresholds stay zero); execution and tooling errors still
# fail the shard. Never add this target to all/check/ci or branch protection.
NIGHTLY_MUTATION_SHARDS := commands runtime persistence async managers models tooling config support daemon-lifecycle daemon-subagents daemon-ops daemon-output
NIGHTLY_MUTATION_PATHS_commands := ./cmd/coagent ./cmd/releasebuilder
NIGHTLY_MUTATION_PATHS_runtime := ./internal/session
NIGHTLY_MUTATION_PATHS_persistence := ./internal/sessionstore
NIGHTLY_MUTATION_PATHS_async := ./internal/admission ./internal/budget ./internal/inputruntime ./internal/migrate ./internal/progress ./internal/progressruntime ./internal/schedule ./internal/sessionbus ./internal/sessionevent ./internal/sessionlifecycle ./internal/subagent
NIGHTLY_MUTATION_PATHS_managers := ./internal/controllerapi ./internal/ctl ./internal/managercontrol ./internal/managerdelivery ./internal/managerdiscovery ./internal/managers ./internal/managers/cli ./internal/managers/telegram
NIGHTLY_MUTATION_PATHS_models := ./internal/catalog ./internal/llm ./internal/llmwire ./internal/registry
NIGHTLY_MUTATION_PATHS_tooling := ./internal/bashsandbox ./internal/lsp ./internal/mcp ./internal/mcpstore ./internal/shellenv ./internal/tool ./internal/tool/builtin
NIGHTLY_MUTATION_PATHS_config := ./internal/config ./internal/configapply ./internal/configops ./internal/configtools ./internal/loader ./internal/memory
NIGHTLY_MUTATION_PATHS_support := ./internal/coagenthome ./internal/git ./internal/humanize ./internal/id ./internal/install ./internal/logger ./internal/projectpath ./internal/todo ./internal/transcript ./internal/version
NIGHTLY_MUTATION_PATHS_daemon-lifecycle := ./internal/daemon
NIGHTLY_MUTATION_FILES_daemon-lifecycle := admission.go budget_park.go finalize.go input_recovery.go manager.go pending_runner.go queued_stop.go runner.go session_input.go
NIGHTLY_MUTATION_PATHS_daemon-subagents := ./internal/daemon
NIGHTLY_MUTATION_FILES_daemon-subagents := completion.go completion_wiring.go orphan_calls.go spawner.go subagent.go subagent_result.go subagent_send.go subagent_wait_guard.go task.go
NIGHTLY_MUTATION_PATHS_daemon-ops := ./internal/daemon
NIGHTLY_MUTATION_FILES_daemon-ops := budget_tool.go compaction_defer.go config_gate.go config_tools.go mcp_tools.go mcp_tools_schema.go secret_tool.go staged.go
NIGHTLY_MUTATION_PATHS_daemon-output := ./internal/daemon
NIGHTLY_MUTATION_FILES_daemon-output := progress.go project.go publish.go readiness.go store.go

NIGHTLY_MUTATION_PATHS = $(NIGHTLY_MUTATION_PATHS_$(NIGHTLY_MUTATION_SHARD))
NIGHTLY_MUTATION_FILES = $(NIGHTLY_MUTATION_FILES_$(NIGHTLY_MUTATION_SHARD))
NIGHTLY_MUTATION_REPORT_DIR ?= mutation-reports/$(NIGHTLY_MUTATION_SHARD)
NIGHTLY_MUTATION_WORKERS ?= 4
# Each shard baseline includes its slowest selected package, so a small
# coefficient gives each mutant several package-suite runtimes without turning
# one survivor into a multi-hour timeout.
NIGHTLY_MUTATION_TIMEOUT_COEFFICIENT ?= 3
NIGHTLY_MUTATION_FLAGS ?=

mutation.nightly:
	@if [ -z "$(NIGHTLY_MUTATION_SHARD)" ] || [ -z "$(filter $(NIGHTLY_MUTATION_SHARD),$(NIGHTLY_MUTATION_SHARDS))" ]; then \
		echo "✋ NIGHTLY_MUTATION_SHARD must name one declared nightly shard."; \
		echo "   Valid shards: $(NIGHTLY_MUTATION_SHARDS)"; \
		exit 1; \
	fi
	@go version -m "$$(command -v gremlins)" 2>/dev/null | grep -q "github.com/go-gremlins/gremlins[[:space:]]*$(GREMLINS_VERSION)" || { echo "✋ gremlins $(GREMLINS_VERSION) required; run make tools"; exit 1; }
	@mkdir -p "$(NIGHTLY_MUTATION_REPORT_DIR)"
	@set -eu; \
	for mutation_path in $(NIGHTLY_MUTATION_PATHS); do \
		set --; \
		if [ -n "$(strip $(NIGHTLY_MUTATION_FILES))" ]; then \
			for source_file in "$$mutation_path"/*.go; do \
				[ -f "$$source_file" ] || continue; \
				source_name=$${source_file##*/}; \
				case "$$source_name" in *_test.go) continue ;; esac; \
				keep=false; \
				for mutation_file in $(NIGHTLY_MUTATION_FILES); do \
					if [ "$$source_name" = "$$mutation_file" ]; then keep=true; break; fi; \
				done; \
				if [ "$$keep" = false ]; then \
					set -- "$$@" --exclude-files "(^|/)$$source_name$$"; \
				fi; \
			done; \
		fi; \
		report_name=$$(echo "$$mutation_path" | sed -e 's#^\./##' -e 's#[/.]#-#g'); \
		gremlins unleash \
			--workers $(NIGHTLY_MUTATION_WORKERS) \
			--timeout-coefficient $(NIGHTLY_MUTATION_TIMEOUT_COEFFICIENT) \
			--threshold-efficacy 0 \
			--threshold-mcover 0 \
			--output "$(NIGHTLY_MUTATION_REPORT_DIR)/$$report_name.json" \
			$(NIGHTLY_MUTATION_FLAGS) "$$@" "$$mutation_path"; \
	done

mutation:
	@if [ -z "$(MUTATION_PATH)" ]; then \
		echo "✋ MUTATION_PATH is required — each mutant reruns the whole suite for that path."; \
		echo "   Scope it:      make mutation MUTATION_PATH=./internal/session"; \
		echo "   Whole module runs only in the Nightly Mutation workflow."; \
		exit 1; \
	fi
	@go version -m "$$(command -v gremlins)" 2>/dev/null | grep -q "github.com/go-gremlins/gremlins[[:space:]]*$(GREMLINS_VERSION)" || { echo "✋ gremlins $(GREMLINS_VERSION) required; run make tools"; exit 1; }
	gremlins unleash \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(MUTATION_EFFICACY) \
		--threshold-mcover $(MUTATION_COVERAGE) \
		$(MUTATION_FLAGS) $(MUTATION_PATH)
