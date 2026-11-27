# Each service keeps its own Makefile and is built, tested and released on its
# own. This file fans the common targets out to all three, for a check of the
# whole repository before a push.
#
#   make                 # list the targets
#   make prepush         # before every push
#   make -C api itest    # anything service-specific: use the service Makefile

SERVICES := api worker website

.DEFAULT_GOAL := help

.PHONY: help all build test lint check-contract prepush ci doctor \
	$(SERVICES:%=build-%) $(SERVICES:%=test-%) $(SERVICES:%=lint-%) $(SERVICES:%=prepush-%)

help: ## List targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo
	@echo "  One service only: build-<service>, test-<service>, lint-<service>"
	@echo "  ($(SERVICES)), or make -C <service> <target>."

all: build test ## Build and unit-test every service

build: $(SERVICES:%=build-%) ## Build every service
test: $(SERVICES:%=test-%) ## Unit tests for every service (no Docker)
lint: $(SERVICES:%=lint-%) ## Lint every service

$(SERVICES:%=build-%): build-%:
	$(MAKE) -C $* build

$(SERVICES:%=test-%): test-%:
	$(MAKE) -C $* test

$(SERVICES:%=lint-%): lint-%:
	$(MAKE) -C $* lint

# The queue message and worker-store fixtures exist in both Go services and
# must be identical.
check-contract: ## Fail if the API and worker contract fixtures differ
	$(MAKE) -C api check-contract
	$(MAKE) -C worker check-contract

# Each service's dev-workflow is its own pre-push check: lint, unit tests and
# build (plus vet for the Go services and typecheck for the website).
prepush: check-contract $(SERVICES:%=prepush-%) ## Run before every push: check-contract, then lint, unit tests and build for every service (no Docker)

$(SERVICES:%=prepush-%): prepush-%:
	$(MAKE) -C $* dev-workflow

# The Docker integration suites are what CI gates the Go services on.
ci: prepush ## prepush plus the API and worker Docker integration tests (needs Docker)
	$(MAKE) -C api itest
	$(MAKE) -C worker itest

# Go, Node and npm are required: a missing one fails. Docker is only needed by
# `make ci`, so a missing Docker is a warning. The Go check runs `go version`
# inside each module, which is the toolchain the build really uses (with
# GOTOOLCHAIN=auto, go downloads the go.mod version if PATH has an older one).
doctor: ## Check Go, Node, npm and Docker against go.mod and package.json
	@fail=0; \
	ok()   { printf '  ok    %s\n' "$$1"; }; \
	warn() { printf '  warn  %s\n' "$$1"; }; \
	bad()  { printf '  FAIL  %s\n' "$$1"; fail=1; }; \
	atleast() { [ "$$(printf '%s\n%s\n' "$$2" "$$1" | sort -V | head -n1)" = "$$2" ]; }; \
	if command -v go >/dev/null 2>&1; then \
		for mod in api worker; do \
			want=$$(awk '$$1 == "go" {print $$2; exit}' $$mod/go.mod); \
			have=$$(cd $$mod && go env GOVERSION 2>/dev/null | sed 's/^go//'); \
			if [ -z "$$have" ]; then \
				bad "go: cannot run Go $$want for $$mod/ (GOTOOLCHAIN=$$(go env GOTOOLCHAIN)); install Go $$want or allow the toolchain download"; \
			elif atleast "$$have" "$$want"; then \
				ok "go $$have for $$mod/ (go.mod wants $$want)"; \
			else \
				bad "go $$have for $$mod/, but $$mod/go.mod wants $$want; install Go $$want or set GOTOOLCHAIN=auto"; \
			fi; \
		done; \
	else \
		bad "go: not found on PATH; install Go $$(awk '$$1 == "go" {print $$2; exit}' api/go.mod) (api/ and worker/ need it)"; \
	fi; \
	want=$$(sed -n 's/.*"node": *">= *\([0-9.]*\)".*/\1/p' website/package.json | head -n1); \
	nvmrc=$$(cat website/.nvmrc 2>/dev/null); \
	if command -v node >/dev/null 2>&1; then \
		have=$$(node --version | sed 's/^v//'); \
		if atleast "$$have" "$$want"; then \
			ok "node $$have (website/package.json wants >= $$want)"; \
		else \
			bad "node $$have, but website/package.json wants >= $$want"; \
		fi; \
		if [ -n "$$nvmrc" ] && [ "$${have%%.*}" != "$${nvmrc%%.*}" ]; then \
			warn "node $$have differs from website/.nvmrc ($$nvmrc), which CI uses"; \
		fi; \
	else \
		bad "node: not found on PATH; install Node $${nvmrc:-$$want} (website/ needs it)"; \
	fi; \
	if command -v npm >/dev/null 2>&1; then \
		ok "npm $$(npm --version)"; \
	else \
		bad "npm: not found on PATH; it ships with Node"; \
	fi; \
	if ! command -v docker >/dev/null 2>&1; then \
		warn "docker: not found on PATH; make prepush works without it, make ci needs it"; \
	elif ! docker info >/dev/null 2>&1; then \
		warn "docker: installed, but the daemon is not reachable; make ci needs it"; \
	else \
		ok "docker $$(docker version --format '{{.Server.Version}}' 2>/dev/null)"; \
	fi; \
	if [ $$fail -ne 0 ]; then \
		echo; echo "doctor: a required tool is missing or too old (see FAIL above)" >&2; exit 1; \
	fi; \
	echo; echo "doctor: everything make prepush needs is installed"
