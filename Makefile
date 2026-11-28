VERSION  ?= 0.5.0
LDFLAGS  := -s -w -X main.version=$(VERSION)
GO_IMAGE ?= golang:1.25

# Runs a Go command in the official image, as the current user, with the caches in .cache.
DOCKER_GO = docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/tmp \
	-e GOCACHE=/src/.cache/go-build -e GOMODCACHE=/src/.cache/gomod \
	-e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
	-v "$(CURDIR):/src" -w /src $(GO_IMAGE)

.PHONY: build test bench docker-build docker-test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o stt-router ./cmd/stt-router

test:
	go vet ./...
	test -z "$$(gofmt -l .)"
	go test ./...

bench:
	go test -run '^$$' -bench . -benchtime 3s ./internal/server/

docker-build:
	$(DOCKER_GO) go build -trimpath -ldflags "$(LDFLAGS)" -o stt-router ./cmd/stt-router

docker-test:
	$(DOCKER_GO) sh -c 'go vet ./... && test -z "$$(gofmt -l .)" && go test ./...'
	docker run --rm -u "$$(id -u):$$(id -g)" -e HOME=/tmp \
		-e GOCACHE=/src/.cache/go-build -e GOMODCACHE=/src/.cache/gomod \
		-e GOFLAGS=-buildvcs=false -e CGO_ENABLED=1 \
		-v "$(CURDIR):/src" -w /src $(GO_IMAGE) go test -race ./...
