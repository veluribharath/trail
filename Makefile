VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed "s/^v//" || echo dev)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o trail .

test:
	go vet ./...
	go test ./...

# Each fuzz test for FUZZTIME (default 1m). Crashes are saved under testdata/fuzz/.
FUZZTIME ?= 1m
fuzz:
	for f in FuzzSQLite FuzzOpenCode FuzzKiro FuzzParsePatch FuzzTranscripts; do \
		go test -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) -fuzzminimizetime 5s . || exit 1; \
	done

run: build
	./trail -open

# The project site: site/ plus the app's fonts and the README screenshot.
site:
	rm -rf _site && mkdir -p _site/fonts
	cp site/* _site/
	cp web/fonts/* _site/fonts/
	cp docs/screenshot.png _site/

.PHONY: build test fuzz run site
