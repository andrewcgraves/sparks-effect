BINARY_NAME := sparks-effect-routing-worker
BUILD_DIR := ./bin
CMD_DIR := ./cmd/worker

GOLANGCI_LINT_VERSION := v2.12.2
GOVULNCHECK_VERSION := v1.8.0

DOCKER := docker

RABBITMQ_IMAGE    := rabbitmq:4-alpine
TEST_MQ_CONTAINER := sparks-effect-worker-test-mq
TEST_MQ_PORT      := 5673
TEST_AMQP_URL     := amqp://guest:guest@localhost:$(TEST_MQ_PORT)/

API_DIR             ?= ../api
MESSAGE_FIXTURE     := internal/routing/testdata/message.golden.json
STORE_FIXTURE       := internal/store/testdata/worker-store.golden.json
API_MESSAGE_FIXTURE := $(API_DIR)/internal/routing/testdata/message.golden.json
API_STORE_FIXTURE   := $(API_DIR)/internal/handler/testdata/worker-store.golden.json

TEST_VALHALLA_URL ?=

.PHONY: all deps build run test test-integration itest acceptance check-config check-contract \
	mq-up mq-wait mq-down vet lint vulncheck tidy clean dev-workflow

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

# Diff both golden fixtures against the API's copies in ../api. The message
# and the worker-store envelope are two files; matching the message must not
# skip the store check.
#
# It is a diff between two directories of the same checkout: no network, and
# whatever branch is checked out is what gets compared, so a change to the
# contract lands on both sides in one pull request.
#
# The API keeps the store golden at internal/handler/; the worker keeps it
# next to the client that sends it. check-contract maps the paths. The API's
# check-contract reads this side's file at $(STORE_FIXTURE).
check-contract:
	@failed=0; \
	check_one() { \
		_local=$$1; _theirs=$$2; _name=$$3; \
		if [ ! -f "$$_theirs" ]; then \
			echo "the API's $$_name fixture is missing at $$_theirs" >&2; \
			return 1; \
		fi; \
		if diff -u "$$_theirs" "$$_local"; then \
			echo "$$_local matches $$_theirs"; \
			return 0; \
		fi; \
		echo >&2; \
		echo "the $$_name contract has drifted from the API's copy." >&2; \
		echo "'-' is the API's copy, '+' is the worker's." >&2; \
		echo "make the two fixtures identical and make the wire types agree with them." >&2; \
		echo >&2; \
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

# vulncheck fails on a known vulnerability that this code actually calls. Not
# part of `dev-workflow`: it needs the network (vuln.go.dev). CI runs it in the
# check job.
#
# Standard-library findings are reported against the `go` on PATH, not the
# version in go.mod, so a local run on an older toolchain can fail where CI,
# on the latest patch of GO_VERSION, passes. GOTOOLCHAIN=go<patch> make
# vulncheck reproduces CI's answer.
vulncheck:
	@gobin="$$(go env GOPATH)/bin"; \
	if [ ! -x "$$gobin/govulncheck" ]; then \
		GOBIN="$$gobin" go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); \
	fi; \
	"$$gobin/govulncheck" ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
