# Multi-stage build of the birdman C++ SDK example "game": compile sdk/core +
# the example into one fully static binary (musl, zero runtime deps) and ship
# it on distroless/static — the C++ twin of examples/stub-server's image. The
# container behaves like the stub (UDP JOIN/SAY/LEAVE/PING) but is driven by
# the frozen C++ SDK (sdk/core), not the Go liba: running it as a real fleet
# version is what proves the C++ contract end-to-end against the real agent.
#
# Build context is sdk/ (needs the top-level CMakeLists + core + example):
#   docker build -f sdk/example/Dockerfile --build-arg APP_VERSION=0.9.0 \
#     -t ghcr.io/ufna/birdman-sdk-example:0.9.0 sdk
ARG APP_VERSION=dev

FROM alpine:3.20 AS build
RUN apk add --no-cache build-base cmake
WORKDIR /src
# Only what the birdman_example target needs (mockagent/scripts/unreal are not
# copied; the Go smoke test auto-disables when `go` is absent from the image).
COPY CMakeLists.txt ./
COPY core ./core
COPY example ./example
ARG APP_VERSION
# Fully static link so the binary runs on distroless/static (no libc there).
RUN cmake -S . -B /build -DCMAKE_BUILD_TYPE=Release \
      -DBIRDMAN_EXAMPLE_VERSION="${APP_VERSION}" \
      -DCMAKE_EXE_LINKER_FLAGS="-static" \
 && cmake --build /build --target birdman_example -j \
 && strip /build/example/birdman_example \
 && scanelf -n /build/example/birdman_example   # prints deps; static => none

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /build/example/birdman_example /birdman-example
# Runs standalone (SDK no-op) with no env; the agent injects BIRDMAN_SOCKET,
# BIRDMAN_PORT (host-network), BIRDMAN_SERVER_ID when it starts a dedik.
ENTRYPOINT ["/birdman-example"]
