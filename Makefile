# Entry points for developing and for CI, which run the same commands.
#
# The split is by what a target needs, not by how long it takes: `check` and
# `test` need only Go, and `games` and `test-integration` need a Docker engine.

GO      ?= go
BIN     ?= bin/wge
GAMES   ?= $(wildcard games/*)
BASE    ?= bases/debian-13

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all check fmt vet tidy build test test-integration base games ci ci-integration dist install clean $(BIN)

all: check test

## check: everything that needs no engine and no network
check: fmt vet tidy build

fmt:
	@out=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$out" ]; then \
		echo "these files are not gofmt'd:"; echo "$$out"; exit 1; \
	fi

vet:
	$(GO) vet ./...

# A go.mod that does not match the imports is a build that works here and
# nowhere else.
tidy:
	$(GO) mod tidy -diff

build:
	$(GO) build ./...

# Phony on purpose: a rule with no prerequisites is never remade, which means
# every target that depends on it silently runs whatever was built first.
$(BIN):
	$(GO) build -o $(BIN) ./cmd/wge

## test: the tests that need nothing but Go. Fast enough to run on every save.
test:
	$(GO) test -race -short ./...

## test-integration: the tests that build images and boot containers.
##
## WGE_REQUIRE_DOCKER turns "no engine here" from a skip into a failure, so a
## run that proved nothing cannot report the same green as a run that did.
test-integration:
	WGE_REQUIRE_DOCKER=1 $(GO) test -count=1 -timeout 30m ./...

## base: the image games are built on
base: $(BIN)
	./$(BIN) base $(BASE)

## games: every game validates, compiles, and enforces its own level graph
games: $(BIN)
	@for g in $(GAMES); do \
		echo "--- $$g"; \
		./$(BIN) validate $$g || exit 1; \
		./$(BIN) build -q $$g || exit 1; \
		./$(BIN) test $$g || exit 1; \
	done

## dist: a static binary to put on a server
##
## CGO is off because nothing here needs it -- the SQLite driver is pure Go --
## which makes the result a single file that runs on any Linux of the same
## architecture, whatever libc it has.
dist:
	CGO_ENABLED=0 $(GO) build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o dist/wge ./cmd/wge
	@ls -lh dist/wge | awk '{print "  dist/wge", $$5}'

## install: install the binary, the unit and the configuration (needs root)
install: dist
	./deploy/install.sh

## ci: what runs on every push
ci: check test

## ci-integration: what runs where there is an engine
ci-integration: check base games test-integration

clean:
	rm -rf bin dist
