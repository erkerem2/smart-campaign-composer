.PHONY: tidy test test-race build seed reseed run

tidy:
	go mod tidy

test:
	go test -count=1 ./...

test-race:
	CGO_ENABLED=1 go test -race -count=1 ./...

build:
	go build -o bin/ ./cmd/...

seed:
	go run ./cmd/seed

reseed:
	go run ./cmd/seed -reset

run:
	go run ./cmd/server