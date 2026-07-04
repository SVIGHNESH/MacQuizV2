VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)

.PHONY: build test vet fmt run-server run-worker up down

build:
	cd server && go build -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/macquiz ./cmd/macquiz

test:
	cd server && go test ./...

vet:
	cd server && go vet ./...

fmt:
	cd server && gofmt -l -w .

run-server:
	cd server && go run ./cmd/macquiz serve

run-worker:
	cd server && go run ./cmd/macquiz worker

up:
	docker compose up --build -d

down:
	docker compose down
