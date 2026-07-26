GO_LDFLAGS := -X main.Tag=$(shell git describe --exact-match --tags 2>/dev/null) \
              -X main.Commit=$(shell git rev-parse HEAD 2>/dev/null) \
              -X "main.BuildTime=$(shell date -Iseconds)"

# goolm selects mautrix's pure-Go Olm implementation. Without it the build needs
# libolm's C headers, which is an avoidable dependency for this bridge.
GO_TAGS := goolm

.PHONY: all build test cover lint fmt example-config clean

all: build

build:
	go build -tags '$(GO_TAGS)' -ldflags '$(GO_LDFLAGS)' -o matrix-nctalk ./cmd/matrix-nctalk

test:
	go test -tags '$(GO_TAGS)' -race ./...

# The project holds itself to 85% statement coverage.
COVERAGE_MIN := 85

cover:
	go test -tags '$(GO_TAGS)' -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -1
	@pct=$$(go tool cover -func=coverage.out | tail -1 | grep -oE '[0-9]+\.[0-9]+'); \
	  awk -v p="$$pct" -v m="$(COVERAGE_MIN)" 'BEGIN { if (p+0 < m+0) { printf "coverage %.1f%% is below the %d%% minimum\n", p, m; exit 1 } printf "coverage %.1f%% meets the %d%% minimum\n", p, m }'

lint:
	go vet -tags '$(GO_TAGS)' ./...
	gofmt -l pkg cmd

fmt:
	gofmt -w pkg cmd

example-config: build
	./matrix-nctalk -e -c example-config.yaml

clean:
	rm -f matrix-nctalk coverage.out
