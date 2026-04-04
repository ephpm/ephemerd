BINARY := ephemerd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build clean test lint

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/ephemerd/

clean:
	rm -f $(BINARY)

test:
	go test ./...

lint:
	golangci-lint run ./...
