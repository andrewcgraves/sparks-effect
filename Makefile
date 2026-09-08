BINARY_NAME := sparks-effect-routing-worker
BUILD_DIR := ./bin
CMD_DIR := ./cmd/worker

GOLANGCI_LINT_VERSION := v2.12.2
GOBIN := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(GOBIN)/golangci-lint

DOCKER := docker

RABBITMQ_IMAGE    := rabbitmq:4-alpine
TEST_MQ_CONTAINER := sparks-effect-worker-test-mq
TEST_MQ_PORT      := 5673
TEST_AMQP_URL     := amqp://guest:guest@localhost:$(TEST_MQ_PORT)/

CONTRACT_FIXTURE := internal/routing/testdata/message.golden.json
CONTRACT_REPO    := andrewcgraves/sparks-effect-api
CONTRACT_REF     ?= main
CONTRACT_CANDIDATE_REFS ?= cursor/spa-264-trip-progress-d573 andrew/spa-264-trip-progress
CONTRACT_URL     := https://raw.githubusercontent.com/$(CONTRACT_REPO)/$(CONTRACT_REF)/$(CONTRACT_FIXTURE)

TEST_VALHALLA_URL ?=

.PHONY: all deps build run test test-integration itest acceptance check-contract \
	mq-up mq-wait mq-down vet lint tidy clean dev-workflow

all: build

dev-workflow: test vet lint build

deps:
	go mod download

build: deps
	go build -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

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

check-contract:
	@theirs=$$(mktemp); trap 'rm -f "$$theirs"' EXIT; \
	echo "fetching $(CONTRACT_URL)"; \
	curl -fsSL "$(CONTRACT_URL)" -o "$$theirs" || { \
		echo "could not fetch the API's fixture from $(CONTRACT_URL)" >&2; exit 1; }; \
	if diff -q "$$theirs" $(CONTRACT_FIXTURE) >/dev/null; then \
		echo "$(CONTRACT_FIXTURE) matches sparks-effect-api@$(CONTRACT_REF)"; \
		exit 0; \
	fi; \
	for ref in $(CONTRACT_CANDIDATE_REFS); do \
		url="https://raw.githubusercontent.com/$(CONTRACT_REPO)/$$ref/$(CONTRACT_FIXTURE)"; \
		echo "fetching $$url"; \
		if ! curl -fsSL "$$url" -o "$$theirs"; then \
			echo "skipping sparks-effect-api@$$ref (not fetchable)"; \
			continue; \
		fi; \
		if diff -q "$$theirs" $(CONTRACT_FIXTURE) >/dev/null; then \
			echo "$(CONTRACT_FIXTURE) matches sparks-effect-api@$$ref (in-flight; $(CONTRACT_REF) does not have it yet)"; \
			exit 0; \
		fi; \
	done; \
	echo "fetching $(CONTRACT_URL)"; \
	curl -fsSL "$(CONTRACT_URL)" -o "$$theirs" || { \
		echo "could not fetch the API's fixture from $(CONTRACT_URL)" >&2; exit 1; }; \
	diff -u "$$theirs" $(CONTRACT_FIXTURE) || true; \
	echo >&2; \
	echo "the message contract has drifted from sparks-effect-api@$(CONTRACT_REF)." >&2; \
	echo "'-' is the API's copy, '+' is this repository's." >&2; \
	echo "copy the API's file over $(CONTRACT_FIXTURE) and make the struct in" >&2; \
	echo "internal/routing/message.go agree with it." >&2; \
	exit 1

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

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

$(GOLANGCI_LINT):
	GOBIN=$(GOBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
