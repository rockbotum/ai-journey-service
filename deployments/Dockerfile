# Multi-stage build: the toolchain never reaches the runtime image.
#
# No `# syntax=` directive on purpose. The features used below are all supported
# by the built-in frontend, and pinning the syntax pulls a second image, which
# makes the build fail on a flaky network for no benefit.
#
# For reproducible production builds, pin both base images by digest as well as
# by tag.
ARG GO_IMAGE=golang:1.27-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
FROM ${GO_IMAGE} AS build

WORKDIR /src

# The module file is copied on its own so the dependency layer is reused whenever
# only source files change. Without this, every code edit re-downloads modules.
COPY go.mod ./

RUN go mod download

COPY . .

# CGO_ENABLED=0 yields a static binary, which is what the distroless base
# requires. -trimpath strips local filesystem paths from the binary, so a build
# does not leak the builder's directory layout. -s -w drop the symbol table and
# DWARF data.
#
# The build runs the tests first: a failing test must fail the image, not ship
# a broken binary to a registry.
RUN go vet ./... \
    && go test -count=1 ./... \
    && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -buildid=" \
        -o /out/server ./cmd/server

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS runtime

COPY --from=build /out/server /usr/local/bin/server

# The service must not run as root: a container escape then buys an attacker
# nothing. The nonroot variant already provides this user.
USER nonroot:nonroot

EXPOSE 8080

# The distroless image contains no shell and no HTTP client, so the health check
# is performed by the binary itself. It calls the real endpoint over HTTP, so a
# container that is listening but unable to serve is reported unhealthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/server", "-health-check"]

# The binary handles SIGTERM itself and drains in-flight requests, so it is
# safe to run as PID 1.
ENTRYPOINT ["/usr/local/bin/server"]
