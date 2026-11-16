# Each service keeps its own Makefile and is built, tested and released on its
# own. This file only fans a few common targets out to all three, for a quick
# check of the whole repository before a change that crosses services.
#
#   make test            # every service
#   make -C api itest    # anything service-specific: use the service Makefile

SERVICES := api worker website

.PHONY: all build test lint check-contract $(SERVICES:%=build-%) $(SERVICES:%=test-%) $(SERVICES:%=lint-%)

all: build test

build: $(SERVICES:%=build-%)
test: $(SERVICES:%=test-%)
lint: $(SERVICES:%=lint-%)

$(SERVICES:%=build-%): build-%:
	$(MAKE) -C $* build

$(SERVICES:%=test-%): test-%:
	$(MAKE) -C $* test

$(SERVICES:%=lint-%): lint-%:
	$(MAKE) -C $* lint

# The queue message and worker-store fixtures exist in both Go services and
# must be identical.
check-contract:
	$(MAKE) -C api check-contract
	$(MAKE) -C worker check-contract
