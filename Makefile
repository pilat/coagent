.PHONY: help build test tests test.integration test.live harness-e2e lint lint.fix fmt all check ci long-fuzz race stress ci.mutation arch semgrep mutation tools post-stop-hook

.DEFAULT_GOAL := help

# Gremlins temporarily rewrites production files. Even when a caller passes
# `-j`, the composed CI prerequisites must finish sequentially so mutation never
# overlaps formatting, compilation, fuzzing, or another test process.
.NOTPARALLEL: ci

# Go and golangci-lint versions are pinned in mise.toml (golangci-lint 2.12.2 is
# the floor for the `modernize` linter in .golangci.yml).
GO_ARCH_LINT_VERSION ?= v1.16.0
GREMLINS_VERSION ?= v0.6.0
SEMGREP_VERSION ?= 1.168.0

GOLANGCI_RUN = golangci-lint run ./...
SEMGREP ?= uv tool run --from semgrep==$(SEMGREP_VERSION) semgrep

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
	@echo "  all              fmt + build + lint + arch + semgrep + tests"
	@echo "                   (hermetic: no network, no external binaries)"
	@echo "  check            all + integration tests      (needs local git/gopls)"
	@echo "  ci               slow local CI: all + integration + harness E2E + long fuzz + race"
	@echo "                   + protocol stress + scoped mutation testing"
	@echo ""
	@echo "Pieces:"
	@echo "  fmt              apply the formatters"
	@echo "  build            compile the binary"
	@echo "  tests            go test ./... (build-tagged files excluded)"
	@echo "  test.integration go test -tags=integration ./..."
	@echo "  lint             golangci-lint only"
	@echo "  lint.fix         apply every golangci-lint autofix"
	@echo "  arch             go-arch-lint only"
	@echo "  semgrep          project invariants only"
	@echo ""
	@echo "Opt-in (slow):"
	@echo "  harness-e2e      compiled daemon + socket + fake LLM process tests"
	@echo "  test.live        credentialed network smoke tests (not part of CI)"
	@echo "  long-fuzz        model-based protocol fuzzing (CI_FUZZ_TIME=5m)"
	@echo "  race             full default suite under Go's race detector"
	@echo "  stress           repeat/shuffle critical protocol tests"
	@echo "  ci.mutation      mutate harness-critical execution and delivery boundaries"
	@echo "  mutation MUTATION_PATH=./internal/session"
	@echo "  tools            install/verify the toolchain"

# Everything that must be green before a commit, and nothing that needs the
# network. Every gate is listed by name: burying arch/semgrep inside `lint` made
# people read `all` and conclude they were missing.
all: fmt build lint arch semgrep tests

# `all` plus hermetic suites that exercise installed programs (real gopls and
# local-only git clone/pull). They never clone a network repository.
check: all test.integration

# Slow, reproducible, local pre-merge gate. Its test workloads are hermetic: the
# compiled-process E2E uses a local fake LLM server, Git clones only temporary
# local repositories, and the stress suite needs no external services. Local dev
# tools are still required; the mutation target installs pinned Gremlins if it is
# absent. Budgets are variables so a developer can run a short smoke first
# without changing the canonical defaults used for the final local CI run.
ci: all test.integration harness-e2e long-fuzz race stress ci.mutation

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

long-fuzz:
	go test ./internal/sessionstore -run '^$$' -fuzz '^FuzzHarnessProtocol$$' -fuzztime=$(CI_FUZZ_TIME)

race:
	go test -race -count=1 ./...

CI_STRESS_COUNT ?= 25
CI_STRESS_TIMEOUT ?= 15m
CI_STRESS_PACKAGES := ./internal/session ./internal/sessionstore ./internal/daemon ./internal/schedule ./internal/managers/telegram ./internal/migrate
CI_STRESS_RUN := Test(Harness|ExecuteToolCalls_(RejectsSleepAlongside|RejectedSleepDoesNotSkip)|Integration_(StressBlockingNoDeadlock|BackgroundTaskRejectsCompetingSleepProtocol|ScatterGatherBlockingTasks|OneShotAckFailureRedeliversWithoutDuplicateTranscriptOrPublication|FreshScheduleDuplicateDoesNotResetOrRunTwice)|Executor_CronAckRetryKeepsCanonicalIdentityAndPayload|ScheduledDeliveryStore_ContextResetRollsBackClaimAndTranscriptOnInsertFailure|SendMessage_DoesNotDuplicateOnRateLimitOrAmbiguousTransportFailure|FollowUpAcceptedBeforeTerminalBoundaryStaysInSameActivation|Stop(ParksWholeTreeAndExplicitFollowUpResumesOnlyChild|DirectChildParksItsOwnLinkWithoutStoppingParent)|StartFinishesInterruptedStopBeforeRecoverySweep|SubagentWaitGuardRejectsSleepUntilCompletionDelivered|OpenDB_ExplicitTransactionsReserveWriterAtBegin)

stress:
	go test -shuffle=on -count=$(CI_STRESS_COUNT) -timeout=$(CI_STRESS_TIMEOUT) -run '$(CI_STRESS_RUN)' $(CI_STRESS_PACKAGES)

# Ensure dev tools are present. golangci-lint comes from mise, gremlins/go-arch-lint
# from go install; semgrep runs through uv so it needs no permanent install.
tools:
	golangci-lint version
	go install github.com/fe3dback/go-arch-lint@$(GO_ARCH_LINT_VERSION)
	go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)
	@command -v uv >/dev/null 2>&1 || { echo "✋ uv missing (semgrep runs through it): curl -LsSf https://astral.sh/uv/install.sh | sh"; exit 1; }
	$(SEMGREP) --version

# Architecture boundary check (.go-arch-lint.yml). Installs the pinned version
# when the binary on PATH is missing or a different one — presence alone isn't enough.
arch:
	@go-arch-lint version 2>/dev/null | grep -q "$(GO_ARCH_LINT_VERSION)" || go install github.com/fe3dback/go-arch-lint@$(GO_ARCH_LINT_VERSION)
	@go-arch-lint self-inspect --json >/dev/null
	go-arch-lint check

# Project invariants that are not expressible as a Go linter (.semgrep/). Every
# rule there is at zero violations, so this is a hard gate with no baseline.
semgrep:
	@command -v uv >/dev/null 2>&1 || { echo "✋ uv missing (semgrep runs through it): curl -LsSf https://astral.sh/uv/install.sh | sh"; exit 1; }
	$(SEMGREP) scan --config .semgrep/ --error --metrics=off --quiet .

lint:
	golangci-lint config verify
	$(GOLANGCI_RUN)

# Apply every autofix golangci-lint offers, then report what still needs a human.
lint.fix:
	golangci-lint cache clean
	$(GOLANGCI_RUN) --fix

fmt:
	golangci-lint fmt

# Mutation testing (.gremlins.yaml): asks whether the tests would actually FAIL if
# the logic were wrong — the question coverage cannot answer. MUTATION_PATH is
# required because every mutant reruns the suite; scope it deliberately.
MUTATION_WORKERS ?= 4
# A surviving mutant runs the whole suite; killed ones exit early. Too tight a
# coefficient turns survivors into TIMED OUT and silently hides the real debt.
MUTATION_TIMEOUT_COEFFICIENT ?= 30

# The local CI mutation gate is intentionally narrower than the exploratory
# `mutation` target. Mutating whole packages makes the everyday gate unusable;
# these files are the load-bearing concurrency, schedule-delivery and durable
# idempotency boundaries exercised by the harness regressions. The thresholds
# are an explicit baseline, not Gremlins' default "always success".
CI_MUTATION_DIR := internal/session
CI_MUTATION_FILES := toolexec.go message_persist.go
CI_MUTATION_EFFICACY ?= 80
CI_MUTATION_COVERAGE ?= 90
CI_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CI_MUTATION_FILES),$(notdir $(wildcard $(CI_MUTATION_DIR)/*.go))),--exclude-files '$(file)')
CI_SCHEDULE_MUTATION_FILES := executor.go service.go store.go
CI_SCHEDULE_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CI_SCHEDULE_MUTATION_FILES),$(notdir $(wildcard internal/schedule/*.go))),--exclude-files '$(file)')
CI_STORE_MUTATION_FILES := scheduled_delivery_store.go
CI_STORE_MUTATION_EXCLUDES = $(foreach file,$(filter-out $(CI_STORE_MUTATION_FILES),$(notdir $(wildcard internal/sessionstore/*.go))),--exclude-files '$(file)')

ci.mutation:
	@command -v gremlins >/dev/null 2>&1 || go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)
	gremlins unleash ./$(CI_MUTATION_DIR) \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(CI_MUTATION_EFFICACY) \
		--threshold-mcover $(CI_MUTATION_COVERAGE) \
		$(CI_MUTATION_EXCLUDES)
	gremlins unleash ./internal/schedule \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(CI_MUTATION_EFFICACY) \
		--threshold-mcover $(CI_MUTATION_COVERAGE) \
		$(CI_SCHEDULE_MUTATION_EXCLUDES)
	gremlins unleash ./internal/sessionstore \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		--threshold-efficacy $(CI_MUTATION_EFFICACY) \
		--threshold-mcover $(CI_MUTATION_COVERAGE) \
		$(CI_STORE_MUTATION_EXCLUDES)

mutation:
	@if [ -z "$(MUTATION_PATH)" ]; then \
		echo "✋ MUTATION_PATH is required — each mutant reruns the whole suite for that path."; \
		echo "   Scope it:      make mutation MUTATION_PATH=./internal/session"; \
		echo "   Whole module:  make mutation MUTATION_PATH=./...   (many minutes)"; \
		exit 1; \
	fi
	@command -v gremlins >/dev/null 2>&1 || go install github.com/go-gremlins/gremlins/cmd/gremlins@$(GREMLINS_VERSION)
	gremlins unleash \
		--workers $(MUTATION_WORKERS) \
		--timeout-coefficient $(MUTATION_TIMEOUT_COEFFICIENT) \
		$(MUTATION_FLAGS) $(MUTATION_PATH)

# Stop hook (.claude/settings.json): static gates, but only when source or a lint
# config changed.
LINT_DEPS := $(shell find ./cmd ./internal -type f -name '*.go') \
	     .golangci.yml .go-arch-lint.yml $(shell find .semgrep -type f)

.lint.stamp: $(LINT_DEPS)
	$(MAKE) lint arch semgrep
	@touch $@

post-stop-hook: .lint.stamp
