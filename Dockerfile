# syntax=docker/dockerfile:1
# Multi-stage build for the LoopSandbox control plane: the API, the worker, the
# sandboxctl admin CLI and the sandbox agent in one image. Dependencies are vendored (vendor/), so the
# build needs no network access.
# Base images pinned by digest (immutable) — a tag is mutable, a digest is not.
# Refresh with: docker buildx imagetools inspect <ref> | grep Digest
FROM golang:1.26.11-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/sandboxctl ./cmd/sandboxctl && \
    CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/lsb-agent ./cmd/lsb-agent && \
    CGO_ENABLED=0 GOOS=linux go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/lsb ./cmd/lsb

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b
COPY --from=build /out/api /api
COPY --from=build /out/worker /worker
COPY --from=build /out/sandboxctl /sandboxctl
COPY --from=build /out/lsb /lsb
# The agent the container driver runs inside each sandbox.
COPY --from=build /out/lsb-agent /lsb-agent
ENV LOOPSANDBOX_AGENT_BIN=/lsb-agent
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]
