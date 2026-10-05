# syntax=docker/dockerfile:1
# Generated from Muxcore-Media/umbrella scripts/ci-templates/go-module-Dockerfile.
# Standalone build (context = this repository). Private Muxcore-Media modules are
# fetched with an optional BuildKit secret:
#   docker build --secret id=gh_token,env=GH_TOKEN -t muxcore/<module> .
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine AS builder
RUN apk add --no-cache git ca-certificates
ENV CGO_ENABLED=0 GOPRIVATE=github.com/Muxcore-Media/* GONOSUMDB=github.com/Muxcore-Media/*
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=gh_token,required=false \
    if [ -s /run/secrets/gh_token ]; then \
      git config --global url."https://x-access-token:$(cat /run/secrets/gh_token)@github.com/Muxcore-Media/".insteadOf "https://github.com/Muxcore-Media/"; \
    fi && go mod download
COPY . .
RUN entry=./cmd/module; [ -d "$entry" ] || entry=.; \
    go build -trimpath -ldflags="-s -w" -o /out/module "$entry"

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -h /data app
USER app
WORKDIR /app
COPY --from=builder /out/module ./module
ENTRYPOINT ["./module"]
