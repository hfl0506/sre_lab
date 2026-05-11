COMPOSE := docker compose

.PHONY: db-up db-down migrate-up migrate-down migrate-version

db-up:
	$(COMPOSE) up -d

db-down:
	$(COMPOSE) down

migrate-up:
	$(COMPOSE) --profile tools run --rm migrate

migrate-down:
	$(COMPOSE) --profile tools run --rm migrate -path /migrations -database "postgres://sre:sre@pg_db:5432/sre?sslmode=disable" down 1

migrate-version:
	$(COMPOSE) --profile tools run --rm migrate -path /migrations -database "postgres://sre:sre@pg_db:5432/sre?sslmode=disable" version
