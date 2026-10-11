BIN := bin/blastguard
TEST_DSN ?= postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable

.PHONY: build build-all test test-integration testdb testdb-rm fmt vet

build:
	go build -o $(BIN) ./cmd/blastguard

# macOS universal targets
build-all:
	GOOS=darwin GOARCH=arm64 go build -o bin/blastguard-darwin-arm64 ./cmd/blastguard
	GOOS=darwin GOARCH=amd64 go build -o bin/blastguard-darwin-amd64 ./cmd/blastguard

test:
	go test ./...

# Throwaway Postgres for integration tests (never point this at a real database).
testdb:
	docker run -d --name blastguard-testdb -e POSTGRES_PASSWORD=postgres -p 127.0.0.1:55432:5432 postgres:16
	@echo "waiting for postgres…"; until docker exec blastguard-testdb pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done

testdb-rm:
	docker rm -f blastguard-testdb

test-integration:
	BLASTGUARD_TEST_DSN='$(TEST_DSN)' go test ./... -count=1

fmt:
	gofmt -w cmd internal

vet:
	go vet ./...
