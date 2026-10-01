.PHONY: build test lint fmt

build:
	go build -trimpath -o bin/forkden ./cmd/forkden

test:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w cmd api
