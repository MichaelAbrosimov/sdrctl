BINARY  := sdrctl
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/MichaelAbrosimov/sdrctl/internal/version.Version=$(VERSION)

.PHONY: build build-linux test vet fmt clean deploy

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/sdrctl

# Target node wyse-sdr-01: Dell Wyse 3040, x86_64, DietPi/Debian.
# Deploy: scp bin/$(BINARY)-linux-amd64 root@wyse-sdr-01:/usr/local/bin/sdrctl
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-amd64 ./cmd/sdrctl

# Deploy to the node: build, stage over ssh, install idempotently.
# Tests run first — hardware is a poor place to discover a broken build.
#   make deploy            (HOST defaults to wyse-sdr)
#   make deploy HOST=other
deploy: test
	@scripts/deploy.sh

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin
