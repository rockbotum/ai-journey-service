# Toolchain. The pins keep local runs and CI identical.
GO ?= go
BIN ?= bin

# Coverage threshold for `make cover`. Enforced in CI so coverage cannot fall
# silently.
COVER_MIN ?= 70

.DEFAULT_GOAL := help

## help: list the available targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F: '{printf "  %-16s %s\n", $$1, $$2}'

## fmt: format all Go sources
.PHONY: fmt
fmt:
	$(GO) fmt ./...

## vet: run the standard static checks
.PHONY: vet
vet:
	$(GO) vet ./...

## build: compile every package and the server binary
.PHONY: build
build:
	$(GO) build ./...
	$(GO) build -o $(BIN)/server ./cmd/server

## test: run the test suite
.PHONY: test
test:
	$(GO) test ./...

## test-race: run the test suite with the race detector
##
## Requires cgo and a C compiler. On Windows install a toolchain first, e.g.
## `go install github.com/microsoft/go-winnarmor/...` is unrelated; use MinGW or
## clang and make sure gcc is on PATH.
.PHONY: test-race
test-race:
	$(GO) test -race ./...

## cover: run the suite and enforce a coverage threshold
.PHONY: cover
cover:
	$(GO) test -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1
	@$(GO) tool cover -func=coverage.out | awk -v min=$(COVER_MIN) '\
		{ gsub("%", "", $$NF); if ($$NF + 0 < min) { \
			printf "coverage %s%% is below the %s%% threshold\n", $$NF, min; exit 1 } } \
		END { print "coverage threshold met" }'

## lint: run golangci-lint when it is installed
##
## Skips with a clear message rather than failing, so a developer without the
## tool is not blocked, but CI requires the job to actually run.
.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 \
		&& golangci-lint run ./... \
		|| (echo "golangci-lint is not installed; run 'make lint-tools'"; exit 1)

## lint-tools: install the pinned linter
.PHONY: lint-tools
lint-tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6

## vuln: scan dependencies for known vulnerabilities
.PHONY: vuln
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

## check: everything CI runs, in the order CI runs it
.PHONY: check
check: fmt-check vet build test cover lint

## fmt-check: fail when a source file is not formatted
.PHONY: fmt-check
fmt-check:
	@out=$$($(GO) fmt ./...); \
	if [ -n "$$out" ]; then echo "not formatted:"; echo "$$out"; exit 1; fi; \
	echo "formatting ok"

## clean: remove build artefacts
.PHONY: clean
clean:
	rm -rf $(BIN) coverage.out

## run: start the server against the current environment
.PHONY: run
run:
	$(GO) run ./cmd/server

# ---------------------------------------------------------------------------
# Container
# ---------------------------------------------------------------------------
DOCKER    ?= docker
IMAGE     ?= ai-journey-service
TAG       ?= dev
# The Dockerfile lives in deployments/ while the build context stays the
# repository root, where go.mod and the sources are. -f is therefore required:
# a bare "docker build ." no longer finds a Dockerfile at the context root.
DOCKERFILE ?= deployments/Dockerfile

## docker-build: build the runtime image
##
## The image build runs vet and the full test suite, so a failing test fails
## the build rather than shipping a broken binary.
.PHONY: docker-build
docker-build:
	$(DOCKER) build -f $(DOCKERFILE) -t $(IMAGE):$(TAG) .

## docker-run: run the image on port 8081
##
## The service refuses to start without its configuration, so the variables must
## be provided. Real values are never baked into an image.
##
## For the full stack including postgres use "make compose-up".
.PHONY: docker-run
docker-run:
	$(DOCKER) run --rm -p 8081:8080 \
		-e DB_DSN="$$DB_DSN" \
		-e AUTH_CREDENTIALS="$$AUTH_CREDENTIALS" \
		-e SUPPLIER_BASE_URL="$$SUPPLIER_BASE_URL" \
		-e SUPPLIER_ID="$$SUPPLIER_ID" \
		-e SUPPLIER_API_KEY="$$SUPPLIER_API_KEY" \
		$(IMAGE):$(TAG)

## docker-shell: open a shell in the image
##
## The runtime image is distroless and has no shell. This target therefore
## starts a temporary container from the build stage instead, which is where
## tooling actually exists.
.PHONY: docker-shell
docker-shell:
	$(DOCKER) build -f $(DOCKERFILE) --target build -t $(IMAGE):build .
	$(DOCKER) run --rm -it --entrypoint /bin/sh $(IMAGE):build

# ---------------------------------------------------------------------------
# Compose
# ---------------------------------------------------------------------------
COMPOSE ?= docker compose -f deployments/docker-compose.yml

## compose-up: build and start the API together with postgres
##
## Requires deployments/.env, see deployments/.env.example. Containers are left
## running so they can be inspected in Docker Desktop.
.PHONY: compose-up
compose-up:
	$(COMPOSE) up -d --build

## compose-down: stop the stack, keeping the database volume
.PHONY: compose-down
compose-down:
	$(COMPOSE) down

## compose-logs: follow the API logs
.PHONY: compose-logs
compose-logs:
	$(COMPOSE) logs -f app

## compose-ps: show container state including health
.PHONY: compose-ps
compose-ps:
	$(COMPOSE) ps

## docker-clean: remove the built images
.PHONY: docker-clean
docker-clean:
	-$(DOCKER) image rm $(IMAGE):$(TAG) $(IMAGE):build

