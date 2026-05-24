.PHONY: build run-server run-loadtest observability smoke-test test lint

# ─── Сборка ───────────────────────────────────────────────────────────────────

build:
	go build ./cmd/sfud/...
	go build ./cmd/loadtest/...

# ─── Запуск SFU-сервера ───────────────────────────────────────────────────────

run-server:
	go run ./cmd/sfud/... configs/loadtest.yml

# ─── Нагрузочный тест ─────────────────────────────────────────────────────────
# Переменные можно переопределять: make run-loadtest CLIENTS=500 RAMP=20

ADDR     ?= ws://localhost:8080/ws
CLIENTS  ?= 100
RAMP     ?= 10
DURATION ?= 30s
ROOMS    ?= 1

run-loadtest:
	go run ./cmd/loadtest/... \
		-addr     $(ADDR) \
		-clients  $(CLIENTS) \
		-ramp     $(RAMP) \
		-duration $(DURATION) \
		-rooms    $(ROOMS)

# Быстрый smoke-тест (5 клиентов, 10 сек) — убедиться что всё работает
smoke-test:
	go run ./cmd/loadtest/... \
		-addr     $(ADDR) \
		-clients  5 \
		-ramp     2 \
		-duration 10s

# ─── Observability (Prometheus + Grafana) ─────────────────────────────────────

observability:
	docker compose -f deployments/docker-compose.yml up -d
	@echo ""
	@echo "✅ Prometheus: http://localhost:9090"
	@echo "✅ Grafana:    http://localhost:3000  (Dashboard: SFU Load Test — Single Node)"
	@echo ""

observability-down:
	docker compose -f deployments/docker-compose.yml down

observability-logs:
	docker compose -f deployments/docker-compose.yml logs -f

# ─── Тесты и линтинг ──────────────────────────────────────────────────────────

test:
	go test ./...

lint:
	golangci-lint run ./...
