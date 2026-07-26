GO_LDFLAGS := -X main.Tag=$(shell git describe --exact-match --tags 2>/dev/null) \
              -X main.Commit=$(shell git rev-parse HEAD 2>/dev/null) \
              -X "main.BuildTime=$(shell date -Iseconds)"

# goolm selects mautrix's pure-Go Olm implementation. Without it the build needs
# libolm's C headers, which is an avoidable dependency for this bridge.
GO_TAGS := goolm

.PHONY: all build test lint fmt example-config clean

all: build

build:
	go build -tags '$(GO_TAGS)' -ldflags '$(GO_LDFLAGS)' -o matrix-nctalk ./cmd/matrix-nctalk

test:
	go test -tags '$(GO_TAGS)' ./...

lint:
	go vet -tags '$(GO_TAGS)' ./...
	gofmt -l pkg cmd

fmt:
	gofmt -w pkg cmd

example-config: build
	./matrix-nctalk -e -c example-config.yaml

clean:
	rm -f matrix-nctalk
