.PHONY: build run-server run-loadtest observability smoke-test test lint demo demo-down docker-loadtest docker-loadtest-10k

# ─── Сборка ───────────────────────────────────────────────────────────────────

build:
	go build ./cmd/sfud/...
	go build ./cmd/loadtest/...

# ─── Запуск SFU-сервера ───────────────────────────────────────────────────────

run-server:
	go run ./cmd/sfud/... configs/loadtest.yml

# ─── Нагрузочный тест (локально) ─────────────────────────────────────────────
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

# ─── Нагрузочный тест в Docker (с Prometheus-метриками) ────────────────────────
# Запускает loadtest как именованный контейнер в сети Docker Compose,
# чтобы Prometheus мог скрейпить его метрики на :9099/metrics

DOCKER_CLIENTS  ?= 105
DOCKER_RAMP     ?= 2
DOCKER_DURATION ?= 90s

docker-loadtest:
	-docker rm -f sfu-loadtest 2>/dev/null
	docker run --name sfu-loadtest \
		--network deployments_default \
		-v $(shell pwd):/app -w /app \
		golang:1.25-alpine \
		go run ./cmd/loadtest/main.go \
			-addrs ws://sfu-node-1:8080/ws,ws://sfu-node-2:8080/ws \
			-clients $(DOCKER_CLIENTS) \
			-ramp $(DOCKER_RAMP) \
			-duration $(DOCKER_DURATION)
	-docker rm -f sfu-loadtest 2>/dev/null

# ─── 🎯 Демонстрация 10 000 видеопотоков (основная цель проекта) ──────────────
# Запускает 105 клиентов на ОДНУ ноду → 105 × 104 = 10 920 видеопотоков
# Одна нода = чистый Full-Mesh без накладных расходов каскадирования.
# Результат фиксируется в Grafana (http://localhost:3000)

docker-loadtest-10k:
	@echo ""
	@echo "🚀 Запуск демонстрации 10 000+ видеопотоков (single-node)"
	@echo "   105 клиентов × 104 подписки = 10 920 видеопотоков"
	@echo "   Grafana: http://localhost:3000"
	@echo ""
	-docker rm -f sfu-loadtest 2>/dev/null
	docker run --name sfu-loadtest \
		--network deployments_default \
		-v $(shell pwd):/app -w /app \
		golang:1.25-alpine \
		go run ./cmd/loadtest/main.go \
			-addr ws://sfu-node-1:8080/ws \
			-clients 105 \
			-ramp 2 \
			-duration 120s
	-docker rm -f sfu-loadtest 2>/dev/null

# ─── Демонстрация кластеризации (2 ноды + каскад) ─────────────────────────
# Показывает горизонтальное масштабирование: 210 клиентов = 2× нагрузка single-node.
# ~105 клиентов на ноду → каждая нода обслуживает 105×104 = 10 920 потоков локально.
# Суммарно по кластеру: ~21 000+ видеопотоков = доказательство горизонтального масштабирования.
# duration=240s: обеспечивает ~100s-окно полного перекрытия (все 210 клиентов одновременно).
# Расчёт: ramp=105s + p50 latency≈41s → последний клиент подключается ~t=146s;
#          первый клиент отключается ~t=0+240=240s → окно ≈240-146=94s без потерь.
docker-loadtest-cluster:
	@echo ""
	@echo "Запуск кластерной демонстрации (2 ноды, 210 клиентов)"
	@echo "  210 клиентов / 2 ноды = ~105 на ноду"
	@echo "  Каждая нода: 105 x 104 = 10920 потоков (x2 по кластеру = ~21K)"
	@echo "  Grafana: http://localhost:3000"
	@echo ""
	-docker rm -f sfu-loadtest 2>/dev/null
	docker run --name sfu-loadtest \
		--network deployments_default \
		-v $(shell pwd):/app -w /app \
		golang:1.25-alpine \
		go run ./cmd/loadtest/main.go \
			-addrs ws://sfu-node-1:8080/ws,ws://sfu-node-2:8080/ws \
			-clients 160 \
			-ramp 2 \
			-duration 130s
	-docker rm -f sfu-loadtest 2>/dev/null

# ─── 🎬 DEMO: Полный запуск стека одной командой ──────────────────────────────
# Использование: make demo
# Останавливает старые контейнеры → собирает новые образы → запускает все сервисы

demo:
	@echo ""
	@echo "═══════════════════════════════════════════════════"
	@echo "  MEPhI SFU — WebRTC 10K Streams Demo"
	@echo "═══════════════════════════════════════════════════"
	@echo ""
	docker compose -f deployments/docker-compose.yml up -d --build
	@echo ""
	@echo "✅ Кластер запущен!"
	@echo ""
	@echo "📊 Мониторинг:"
	@echo "   Grafana:    http://localhost:3000"
	@echo "   Prometheus: http://localhost:9090"
	@echo "   SFU Node 1: http://localhost:8080/metrics"
	@echo "   SFU Node 2: http://localhost:8081/metrics"
	@echo "   Web Client: http://localhost:8080"
	@echo ""
	@echo "🧪 Затем запустите нагрузочный тест:"
	@echo "   make docker-loadtest-10k"
	@echo ""

demo-down:
	@echo "Останавливаю все контейнеры..."
	docker compose -f deployments/docker-compose.yml down -v
	-docker rm -f sfu-loadtest 2>/dev/null
	@echo "✅ Все сервисы остановлены."

# ─── Тесты и линтинг ──────────────────────────────────────────────────────────

test:
	go test ./...

lint:
	golangci-lint run ./...
