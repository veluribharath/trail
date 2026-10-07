VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed "s/^v//" || echo dev)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o trail .

test:
	go vet ./...
	go test ./...

run: build
	./trail -open

.PHONY: build test run
