BINARY     := bin/nimlet
MODULE     := github.com/thiagoribeiro/nimlet
GOFLAGS    := -race

.PHONY: all build test test-unit test-integration lint fmt tidy \
        docker-up docker-down migrate-up migrate-down migrate-status \
        bench clean

all: tidy build test-unit

## Build
build:
	@mkdir -p bin
	go build -o $(BINARY) ./cmd/nimlet

## Testing
test: test-unit

test-unit:
	go test $(GOFLAGS) -count=1 ./...

test-integration:
	go test $(GOFLAGS) -count=1 -tags integration -timeout 2m ./...

## Code quality
lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

## Local infra
docker-up:
	docker compose -f deploy/docker-compose.dev.yml up -d

docker-down:
	docker compose -f deploy/docker-compose.dev.yml down -v

## Migration helpers (needs NIMLET_DB_DSN only)
migrate-up:
	NIMLET_DB_DSN=$(NIMLET_DB_DSN) go run ./cmd/nimlet migrate up

migrate-down:
	NIMLET_DB_DSN=$(NIMLET_DB_DSN) go run ./cmd/nimlet migrate down

migrate-status:
	NIMLET_DB_DSN=$(NIMLET_DB_DSN) go run ./cmd/nimlet migrate status

## Benchmarks
bench:  # see bench/README.md; needs TEST_POSTGRES_DSN (bench/infra.sh up)
	go test -run '^$$' -bench=. -benchmem -count=6 -benchtime=2s -tags integration ./internal/adapter/postgres/...
	go test -run '^$$' -bench=. -benchmem -count=6 -benchtime=300ms ./internal/sync/...

## Run server locally (requires docker-up first)
run:
	NIMLET_DB_DSN="postgres://nimlet:nimlet@localhost:5432/nimlet?sslmode=disable" \
	NIMLET_REDIS_ADDR="localhost:6379" \
	NIMLET_AUTH_HMAC_SECRET="dev-secret-at-least-32-chars-long!!" \
	NIMLET_MEDIA_DRIVER=filesystem \
	NIMLET_MEDIA_PATH=./media \
	go run ./cmd/nimlet server

clean:
	rm -rf bin/ media/
