FROM golang:1.26-alpine AS builder
COPY core/ /build/core/
COPY userdata-local/ /build/userdata-local/
WORKDIR /build/userdata-local
RUN go mod download
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /module ./cmd/module
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /module /
ENTRYPOINT ["/module"]
