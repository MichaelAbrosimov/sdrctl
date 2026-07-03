BINARY  := sdrctl
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/MichaelAbrosimov/sdrctl/internal/version.Version=$(VERSION)

.PHONY: build build-linux test vet fmt clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/sdrctl

# Target node wyse-sdr-01: Dell Wyse 3040, x86_64, DietPi/Debian.
# Deploy: scp bin/$(BINARY)-linux-amd64 root@wyse-sdr-01:/usr/local/bin/sdrctl
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-amd64 ./cmd/sdrctl

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin
