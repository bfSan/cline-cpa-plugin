.PHONY: build test lint clean

GO ?= go
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

build:
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -ldflags "$(LDFLAGS)" -o cline.so .

test:
	$(GO) test -race -count=1 ./...

lint:
	@test -z "$$($(GO)fmt -l .)" || ($(GO)fmt -l . && exit 1)
	$(GO) vet ./...

clean:
	rm -f cline.so cline.h
	rm -rf dist/ coverage.out coverage.html
