# ============================================================
# chess-server Dockerfile — PHASE_2.md Step 7
# ============================================================
# Multi-stage build: compile in a full Go image, run in a minimal one.
# First image any container in this project needs — Phase 1 never
# containerized the server itself (single instance, run via `go run` /
# `make run` directly against Docker-hosted Postgres). Phase 2's multi-
# instance stack (docker-up-cluster) is the first thing that actually needs
# this.
# ============================================================

FROM golang:1.25-alpine AS builder

WORKDIR /build

# Cache dependency downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: fully static binary, so the final stage can be a minimal
# base image with no libc dependency to match.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /chess-server ./cmd/server

# ------------------------------------------------------------

FROM alpine:3.20

# ca-certificates: needed for any outbound TLS (none today, but cheap
# insurance and standard practice). wget: used by this image's own
# HEALTHCHECK and by docker-compose.yml's service healthchecks — busybox's
# wget is already present in alpine by default, so no separate install is
# needed for that specifically; ca-certificates is the only real addition.
RUN apk add --no-cache ca-certificates

COPY --from=builder /chess-server /chess-server
COPY migrations /migrations

EXPOSE 8080

ENTRYPOINT ["/chess-server"]
