BINARY_NAME := sparks-effect-routing-worker
BUILD_DIR := ./bin
CMD_DIR := ./cmd/worker

GOLANGCI_LINT_VERSION := v2.12.2
GOBIN := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(GOBIN)/golangci-lint

# Container tool: docker by default, override with `make mq-up DOCKER=podman`.
DOCKER := docker

# --- Throwaway RabbitMQ for queue integration tests ---
# Acknowledgement, redelivery and prefetch are properties of the broker
# conversation, so a fake proves nothing about them. The worker HTTP
# contract is exercised in-process (see internal/testsupport/api.go); there
# is no throwaway Postgres here because this worker no longer connects to
# one (SPA-273).
RABBITMQ_IMAGE    := rabbitmq:4-alpine
TEST_MQ_CONTAINER := sparks-effect-worker-test-mq
TEST_MQ_PORT      := 5673
TEST_AMQP_URL     := amqp://guest:guest@localhost:$(TEST_MQ_PORT)/

# Port is deliberately one above sparks-effect-api's 5672, so both
# repositories' throwaway brokers can be up at once on one machine without
# either quietly connecting to the other's.

# --- The cross-repo message contract ---
# The golden fixture is meant to be the same file, byte for byte, in both
# repositories. `make check-contract` is what makes that true rather than
# aspirational; see the target below.
CONTRACT_FIXTURE := internal/routing/testdata/message.golden.json
CONTRACT_REF     := main
CONTRACT_URL     := https://raw.githubusercontent.com/andrewcgraves/sparks-effect-api/$(CONTRACT_REF)/$(CONTRACT_FIXTURE)

# --- Acceptance checks against a real Valhalla ---
# The transit checks need tiles, which a fake cannot stand in for: the failure
# they exist to catch is a tileset whose GTFS calendars have expired, and
# Valhalla answers that with a 200 and a walking itinerary. Point this at a
# port-forward of the cluster's valhalla service.
TEST_VALHALLA_URL ?=

.PHONY: all deps build run test test-integration itest acceptance check-contract \
	mq-up mq-wait mq-down vet lint tidy clean dev-workflow

all: build

# dev-workflow runs everything a change needs to pass before it's pushed:
# tests, vet, lint, and a build. Meant to be run end-to-end by humans or
# agents as a single verification step.
dev-workflow: test vet lint build

deps:
	go mod download

build: deps
	go build -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

# test runs the full suite. The RabbitMQ integration tests skip themselves
# unless TEST_AMQP_URL is set, so this stays green with no broker running.
test: deps
	go test ./... -race -cover

# test-integration runs the full suite with TEST_AMQP_URL exported, so the
# broker integration tests actually execute. Point it at any broker via
# TEST_AMQP_URL; defaults to the `make mq-up` container. Used by both
# `make itest` and CI.
test-integration: deps
	TEST_AMQP_URL="$(TEST_AMQP_URL)" go test ./... -race -cover

# itest brings up a throwaway RabbitMQ, runs the integration suite, and tears
# it down again — the one-command local equivalent of the CI job.
#
# Bring-up and the test run share one recipe line so teardown is reached even
# when bring-up fails.
itest:
	$(MAKE) mq-up && $(MAKE) test-integration; \
	status=$$?; $(MAKE) mq-down; exit $$status

# acceptance asks a real Valhalla whether it will actually put a rider on a
# train, over a known pair on a rail line that is in the tiles.
#
# Not part of dev-workflow, and not part of `test`: it needs a server with
# transit tiles, which is a multi-hour build and tens of gigabytes. Without
# TEST_VALHALLA_URL the checks skip themselves, so this target refuses rather
# than reporting a green run that asserted nothing.
#
#	make acceptance TEST_VALHALLA_URL=http://localhost:8002
#
# TEST_TRANSIT_DEPART_AT=YYYY-MM-DDTHH:MM asks about a specific service date,
# which is how "do the feeds on the volume still cover next week?" gets checked
# before the answer turns into an undersized isochrone.
acceptance: deps
	@if [ -z "$(TEST_VALHALLA_URL)" ]; then \
		echo "set TEST_VALHALLA_URL to a Valhalla server with transit tiles, e.g." >&2; \
		echo "  make acceptance TEST_VALHALLA_URL=http://localhost:8002" >&2; \
		exit 1; \
	fi
	TEST_VALHALLA_URL="$(TEST_VALHALLA_URL)" \
	TEST_TRANSIT_DEPART_AT="$(TEST_TRANSIT_DEPART_AT)" \
		go test ./internal/valhalla/ -run Acceptance -count=1 -v

# check-contract diffs this repository's golden message fixture against
# sparks-effect-api's copy of the same file.
#
# The Message struct is hand-copied into both repositories and the fixture is
# documented as the only thing keeping the copies honest — but until this
# existed, nothing checked that the two files were still the same file. They
# drifted for two releases without anyone noticing, and trace ids were dropped
# on this side the whole time (SPA-227). A human remembering to copy a file
# across repositories is not an invariant.
#
# It fetches from the API's main rather than a pinned commit, because what has
# to hold is agreement with what the API publishes *now*. A failure here is
# therefore not always this repository's fault: usually it means the API added
# a field and this side has not caught up, and the fix is to copy the fetched
# file in and follow the failing tests until the struct agrees with it.
#
# Not part of dev-workflow: it needs the network, and the rest of that target
# deliberately does not.
check-contract:
	@echo "fetching $(CONTRACT_URL)"
	@theirs=$$(mktemp); trap 'rm -f "$$theirs"' EXIT; \
	curl -fsSL "$(CONTRACT_URL)" -o "$$theirs" || { \
		echo "could not fetch the API's fixture from $(CONTRACT_URL)" >&2; exit 1; }; \
	if diff -u "$$theirs" $(CONTRACT_FIXTURE); then \
		echo "$(CONTRACT_FIXTURE) matches sparks-effect-api@$(CONTRACT_REF)"; \
	else \
		echo >&2; \
		echo "the message contract has drifted from sparks-effect-api@$(CONTRACT_REF)." >&2; \
		echo "'-' is the API's copy, '+' is this repository's." >&2; \
		echo "copy the API's file over $(CONTRACT_FIXTURE) and make the struct in" >&2; \
		echo "internal/routing/message.go agree with it." >&2; \
		exit 1; \
	fi

# mq-up starts (or reuses) a throwaway RabbitMQ and waits until it is running.
# Idempotent: a running container is left in place.
#
# /var/lib/rabbitmq is a tmpfs. Nothing here is meant to outlive the test run,
# and on some Docker installations the image's data directory lands with
# permissions Erlang refuses ("Error when reading .erlang.cookie: eacces"),
# which crashes the node on boot. An in-memory data directory sidesteps that and
# matches what a throwaway broker is for.
mq-up:
	@if [ -z "$$($(DOCKER) ps -q -f name=^/$(TEST_MQ_CONTAINER)$$)" ]; then \
		if [ -n "$$($(DOCKER) ps -aq -f name=^/$(TEST_MQ_CONTAINER)$$)" ]; then \
			$(DOCKER) rm -f $(TEST_MQ_CONTAINER) >/dev/null; \
		fi; \
		echo "starting $(RABBITMQ_IMAGE) as $(TEST_MQ_CONTAINER)"; \
		$(DOCKER) run -d --name $(TEST_MQ_CONTAINER) \
			--tmpfs /var/lib/rabbitmq \
			-p $(TEST_MQ_PORT):5672 \
			$(RABBITMQ_IMAGE) >/dev/null; \
	else \
		echo "$(TEST_MQ_CONTAINER) already running"; \
	fi
	$(MAKE) mq-wait

# mq-wait blocks until RabbitMQ reports startup complete, by watching its log
# rather than probing it. RabbitMQ takes an appreciable moment to boot, hence
# the 60s ceiling.
#
# The obvious probe — `rabbitmq-diagnostics` inside the container — cannot be
# used here. Every diagnostics invocation starts its own Erlang node, and one
# started as root mid-boot writes /var/lib/rabbitmq/.erlang.cookie root-owned,
# after which the booting server cannot read its own cookie and dies with
# "eacces". The probe would be what broke the thing it was checking on. Reading
# the log costs nothing and cannot perturb the server.
mq-wait:
	@echo "waiting for RabbitMQ to accept connections..."
	@for i in $$(seq 1 60); do \
		if $(DOCKER) logs $(TEST_MQ_CONTAINER) 2>&1 | grep -q "Server startup complete"; then \
			echo "RabbitMQ is ready"; exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "RabbitMQ did not become ready in time" >&2; \
	$(DOCKER) logs --tail 20 $(TEST_MQ_CONTAINER) >&2; exit 1

mq-down:
	@$(DOCKER) rm -f $(TEST_MQ_CONTAINER) >/dev/null 2>&1 || true

vet: deps
	go vet ./...

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

$(GOLANGCI_LINT):
	GOBIN=$(GOBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
