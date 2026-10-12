# smartclips developer tasks. Usage: make <target>

GO      ?= go
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
IMAGE   ?= smartclips:dev

.PHONY: build test lint fmt image e2e fixtures clean

build:
	$(GO) build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/smartclips ./cmd/smartclips

test:
	$(GO) test -race ./...

lint:
	golangci-lint run
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

image:
	docker build -f build/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE) .

e2e: image
	scripts/e2e.sh $(IMAGE)

fixtures:
	scripts/make-fixtures.sh

clean:
	rm -rf bin
