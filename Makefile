VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed "s/^v//" || echo dev)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o trail .

test:
	go vet ./...
	go test ./...

run: build
	./trail -open

# The project site: site/ plus the app's fonts and the README screenshot.
site:
	rm -rf _site && mkdir -p _site/fonts
	cp site/* _site/
	cp web/fonts/* _site/fonts/
	cp docs/screenshot.png _site/

.PHONY: build test run site
