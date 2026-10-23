.PHONY: tidy test test-race test-short test-e2e build seed reseed run mock-provider

tidy:
	go mod tidy

test:
	go test -count=1 ./...

test-race:
	CGO_ENABLED=1 go test -race -count=1 ./...

test-short:
	go test -count=1 -short ./...

test-e2e:
	go test -count=1 -v -run TestEndToEnd ./internal/api

build:
	go build -o bin/ ./cmd/...

seed:
	go run ./cmd/seed

reseed:
	go run ./cmd/seed -reset

run:
	go run ./cmd/server

mock-provider:
	go run ./cmd/mock-provider