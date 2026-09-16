BINARY_NAME := sparks-effect-routing-worker
BUILD_DIR := ./bin
CMD_DIR := ./cmd/worker

GOLANGCI_LINT_VERSION := v2.12.2

DOCKER := docker

RABBITMQ_IMAGE    := rabbitmq:4-alpine
TEST_MQ_CONTAINER := sparks-effect-worker-test-mq
TEST_MQ_PORT      := 5673
TEST_AMQP_URL     := amqp://guest:guest@localhost:$(TEST_MQ_PORT)/

CONTRACT_REPO    := andrewcgraves/sparks-effect-api
CONTRACT_REF     ?= main
CONTRACT_CANDIDATE_REFS ?=
MESSAGE_FIXTURE     := internal/routing/testdata/message.golden.json
STORE_FIXTURE       := internal/store/testdata/worker-store.golden.json
API_MESSAGE_FIXTURE := internal/routing/testdata/message.golden.json
API_STORE_FIXTURE   := internal/handler/testdata/worker-store.golden.json

TEST_VALHALLA_URL ?=

.PHONY: all deps build run test test-integration itest acceptance check-config check-contract \
	mq-up mq-wait mq-down vet lint tidy clean dev-workflow

all: build

dev-workflow: test vet lint build

deps:
	go mod download

build: deps
	go build -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

# Validate process env / .env without connecting to AMQP, the API, or Valhalla.
# Same rules as startup. Local without a tileset: VALHALLA_SYNTHETIC=true.
check-config: build
	$(BUILD_DIR)/$(BINARY_NAME) -check

test: deps
	go test ./... -race -cover

test-integration: deps
	TEST_AMQP_URL="$(TEST_AMQP_URL)" go test ./... -race -cover

itest:
	$(MAKE) mq-up && $(MAKE) test-integration; \
	status=$$?; $(MAKE) mq-down; exit $$status

acceptance: deps
	@if [ -z "$(TEST_VALHALLA_URL)" ]; then \
		echo "set TEST_VALHALLA_URL to a Valhalla server with transit tiles, e.g." >&2; \
		echo "  make acceptance TEST_VALHALLA_URL=http://localhost:8002" >&2; \
		exit 1; \
	fi
	TEST_VALHALLA_URL="$(TEST_VALHALLA_URL)" \
	TEST_TRANSIT_DEPART_AT="$(TEST_TRANSIT_DEPART_AT)" \
		go test ./internal/valhalla/ -run Acceptance -count=1 -v

# Diff both golden fixtures against the API's copies. The message and the
# worker-store envelope are two files; matching the message must not skip
# the store check. Fetching unpinned main is deliberate — see the API Makefile.
#
# The API keeps the store golden at internal/handler/; this repo keeps it
# next to the client that sends it. check-contract maps the paths. The API's
# job curls this file at $(STORE_FIXTURE).
check-contract:
	@theirs=$$(mktemp); trap 'rm -f "$$theirs"' EXIT; \
	failed=0; \
	check_one() { \
		_local=$$1; _remote=$$2; _name=$$3; \
		_url="https://raw.githubusercontent.com/$(CONTRACT_REPO)/$(CONTRACT_REF)/$$_remote"; \
		echo "fetching $$_url"; \
		if ! curl -fsSL "$$_url" -o "$$theirs"; then \
			echo "could not fetch the API's $$_name fixture from $$_url" >&2; \
			return 1; \
		fi; \
		if diff -q "$$theirs" "$$_local" >/dev/null; then \
			echo "$$_local matches sparks-effect-api@$(CONTRACT_REF)"; \
			return 0; \
		fi; \
		set -- $(CONTRACT_CANDIDATE_REFS); \
		for _ref in "$$@"; do \
			_curl="https://raw.githubusercontent.com/$(CONTRACT_REPO)/$$_ref/$$_remote"; \
			echo "fetching $$_curl"; \
			if ! curl -fsSL "$$_curl" -o "$$theirs"; then \
				echo "skipping sparks-effect-api@$$_ref (not fetchable)"; \
				continue; \
			fi; \
			if diff -q "$$theirs" "$$_local" >/dev/null; then \
				echo "$$_local matches sparks-effect-api@$$_ref (in-flight; $(CONTRACT_REF) does not have it yet)"; \
				return 0; \
			fi; \
		done; \
		_url="https://raw.githubusercontent.com/$(CONTRACT_REPO)/$(CONTRACT_REF)/$$_remote"; \
		curl -fsSL "$$_url" -o "$$theirs" || { \
			echo "could not fetch the API's $$_name fixture from $$_url" >&2; \
			return 1; \
		}; \
		diff -u "$$theirs" "$$_local" || true; \
		echo >&2; \
		echo "the $$_name contract has drifted from sparks-effect-api@$(CONTRACT_REF)." >&2; \
		echo "'-' is the API's copy, '+' is this repository's." >&2; \
		echo "copy the API's file over $$_local and make the wire types agree with it." >&2; \
		echo >&2; \
		echo "if the API's side is still in flight, name its branch to compare" >&2; \
		echo "against that instead:" >&2; \
		echo "  make check-contract CONTRACT_CANDIDATE_REFS=their/branch" >&2; \
		return 1; \
	}; \
	check_one "$(MESSAGE_FIXTURE)" "$(API_MESSAGE_FIXTURE)" "message" || failed=1; \
	check_one "$(STORE_FIXTURE)" "$(API_STORE_FIXTURE)" "worker-store" || failed=1; \
	exit $$failed

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

lint:
	@gobin="$$(go env GOPATH)/bin"; \
	if [ ! -x "$$gobin/golangci-lint" ]; then \
		GOBIN="$$gobin" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi; \
	"$$gobin/golangci-lint" run ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
