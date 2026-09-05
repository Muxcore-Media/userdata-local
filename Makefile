.PHONY: build test lint clean fmt tidy docker proto ci help

GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
LDFLAGS ?= -s -w
BINARY ?= userdata-local

build:
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/module

test:
	$(GO) test -race -count=1 -timeout 60s ./...

lint:
	golangci-lint run --timeout 120s ./...

clean:
	rm -f $(BINARY)
	rm -f cmd/module/module
	rm -rf dist/

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

proto:
	protoc -I proto --go_out=proto/gen --go_opt=module=github.com/Muxcore-Media/userdata-local/proto/gen \
		--go-grpc_out=proto/gen --go-grpc_opt=module=github.com/Muxcore-Media/userdata-local/proto/gen \
		proto/muxcore/userdata/v1/userdata.proto

docker:
	docker build -f Dockerfile -t ghcr.io/muxcore-media/$(BINARY):$(VERSION) ..
	docker tag ghcr.io/muxcore-media/$(BINARY):$(VERSION) ghcr.io/muxcore-media/$(BINARY):latest

ci: lint test build

help:
	@echo "Targets:"
	@echo "  build  - compile the module binary"
	@echo "  test   - run tests with race detection"
	@echo "  lint   - golangci-lint"
	@echo "  proto  - regenerate protobuf stubs"
	@echo "  ci     - lint + test + build"
