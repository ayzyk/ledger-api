DATABASE_URL ?= postgres://ledger:ledger@localhost:5433/ledger?sslmode=disable

.PHONY: db run test

db:
	docker compose up -d --wait

run:
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/server

test:
	TEST_DATABASE_URL="$(DATABASE_URL)" go test -race -count=1 ./...
