SHELL := /bin/bash
GO ?= go
PYTHON ?= python3
TEST_TIMEOUT ?= 5m
HOSTCHECK_TIMEOUT ?= 45s
PLUGIN_DIR ?= build
VERSION ?= 0.0.0
BUILD_TAG ?= dev
VERSION_LDFLAGS = -X github.com/megumin31/cpa-opencode-zen/internal/buildinfo.Version=$(VERSION) -X github.com/megumin31/cpa-opencode-zen/internal/buildinfo.BuildTag=$(BUILD_TAG)

.PHONY: check-offline test test-main test-host fixtures preflight race vet build build-hostcheck hostcheck hostcheck-known-bug sdkrepro check-plugin-dir metadata

# One complete, network-independent verification entry point, also used by CI.
# Populate both modules' dependency caches beforehand; see docs/BUILD.md.
check-offline:
	GO="$(GO)" PYTHON="$(PYTHON)" bash scripts/check-offline.sh

test: test-main test-host fixtures preflight

test-main:
	$(GO) test ./... -count=1 -timeout=$(TEST_TIMEOUT)

test-host:
	$(GO) -C cmd/hostcheck test ./... -count=1 -timeout=$(TEST_TIMEOUT)

fixtures:
	$(PYTHON) -m unittest discover -s tools/officialwire -p 'test_*.py' -v
	$(PYTHON) -m unittest discover -s scripts -p 'test_*.py' -v

preflight:
	@for script in scripts/*.sh cmd/hostcheck/servicecheck/run.sh; do bash -n "$$script" || exit; done
	GO="$(GO)" bash scripts/check-plugin-directory-test.sh

race:
	$(GO) test -race ./... -count=1 -timeout=$(TEST_TIMEOUT)
	$(GO) -C cmd/hostcheck test -race ./... -count=1 -timeout=$(TEST_TIMEOUT)

vet:
	$(GO) vet ./...
	$(GO) -C cmd/hostcheck vet ./...

metadata:
	@$(GO) run -ldflags "$(VERSION_LDFLAGS)" ./cmd/buildmeta

build:
	@set -eu; filename="$$($(GO) run -ldflags "$(VERSION_LDFLAGS)" ./cmd/buildmeta --field filename)"; \
	mkdir -p build; \
	CGO_ENABLED=1 $(GO) build -ldflags "$(VERSION_LDFLAGS)" -buildmode=c-shared -o "build/$$filename" .; \
	$(GO) run -ldflags "$(VERSION_LDFLAGS)" ./cmd/buildmeta > build/plugin-metadata.json

build-hostcheck:
	@mkdir -p build
	CGO_ENABLED=1 $(GO) -C cmd/hostcheck build -o "$(abspath build/hostcheck)" .

hostcheck: build build-hostcheck
	timeout --signal=TERM --kill-after=5s $(HOSTCHECK_TIMEOUT) build/hostcheck --quiesce "$(abspath build)"
	timeout --signal=TERM --kill-after=5s $(HOSTCHECK_TIMEOUT) build/hostcheck --cancel-first "$(abspath build)"
	timeout --signal=TERM --kill-after=5s $(HOSTCHECK_TIMEOUT) build/hostcheck --reconfigure "$(abspath build)"
	timeout --signal=TERM --kill-after=5s $(HOSTCHECK_TIMEOUT) build/hostcheck --reenable "$(abspath build)"
	@set -eu; filename="$$($(GO) run -ldflags "$(VERSION_LDFLAGS)" ./cmd/buildmeta --field filename)"; \
	for scenario in success truncated error blocked empty-feedback responses-tools responses-tools-error responses-tools-incomplete responses-tools-cancel \
	  chat-gemini-combined chat-gemini-reasoning chat-gemini-split-usage chat-gemini-no-usage chat-gemini-partial-usage chat-gemini-zero-usage \
	  chat-gemini-length chat-gemini-filtered chat-gemini-tools chat-gemini-error chat-gemini-truncated chat-gemini-cancel chat-gemini-refusal chat-gemini-legacy-function-call; do \
	  $(GO) -C cmd/hostcheck run ./httpframing -plugin "$(abspath build)/$$filename" -matrix -scenario "$$scenario"; \
	done

# Deliberately outside check-offline: the pinned host's disable-path regression
# remains a strict, separately reportable failure. Never suppress its exit code.
hostcheck-known-bug: build build-hostcheck
	timeout --signal=TERM --kill-after=5s $(HOSTCHECK_TIMEOUT) build/hostcheck --disable "$(abspath build)"

sdkrepro:
	$(GO) run ./cmd/sdkrepro

# Read-only deployment preflight; override PLUGIN_DIR for the host directory.
check-plugin-dir:
	@set -eu; filename="$$($(GO) run -ldflags "$(VERSION_LDFLAGS)" ./cmd/buildmeta --field filename)"; \
	bash scripts/check-plugin-directory.sh "$(PLUGIN_DIR)" "$$filename"
