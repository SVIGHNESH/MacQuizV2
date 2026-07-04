VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)

.PHONY: build test vet fmt run-server run-worker migrate up down web-dev web-build web-lint generate-api

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

migrate:
	cd server && go run ./cmd/macquiz migrate

up:
	docker compose up --build -d

down:
	docker compose down

web-dev:
	cd web && npm run dev

web-build:
	cd web && npm run build

web-lint:
	cd web && npm run lint

generate-api:
	cd web && npm run generate:api
