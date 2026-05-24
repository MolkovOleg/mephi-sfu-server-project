package main

// =============================================================================
// CLI для нагрузочного тестирования SFU-кластера
//
// Использование:
//   go run ./cmd/loadtest/... [флаги]
//
// Примеры:
//   # 1. Нагрузка на одну ноду:
//   go run ./cmd/loadtest/... -clients 500 -ramp 20 -duration 60s
//
//   # 2. Нагрузка на фиксированный список нод:
//   go run ./cmd/loadtest/... -addrs "ws://localhost:8080/ws,ws://localhost:8081/ws" -clients 1000
//
//   # 3. Автоматическое обнаружение всех нод через Redis:
//   go run ./cmd/loadtest/... -redis-addr "localhost:6379" -clients 2000
//
// Метрики нагрузочного клиента доступны на :9092/metrics
// =============================================================================

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sfu-server/internal/cluster"
	"sfu-server/internal/config"
	"sfu-server/internal/loadtest"
)

func main() {
	// --- CLI флаги ---
	addr := flag.String("addr", "ws://localhost:8080/ws",
		"WebSocket-адрес SFU-сервера (фолбэк)")
	addrsStr := flag.String("addrs", "",
		"Список WebSocket-адресов SFU через запятую")
	clients := flag.Int("clients", 100,
		"Общее количество клиентов")
	ramp := flag.Int("ramp", 10,
		"Количество новых клиентов в секунду (ramp-up rate)")
	duration := flag.Duration("duration", 30*time.Second,
		"Как долго удерживать каждое соединение")
	rooms := flag.Int("rooms", 1,
		"Количество параллельных комнат")
	roomPrefix := flag.String("room-prefix", "loadtest",
		"Префикс имён комнат")
	metricsAddr := flag.String("metrics-addr", ":9092",
		"Адрес для Prometheus /metrics эндпоинта нагрузочного клиента")
	redisAddr := flag.String("redis-addr", "",
		"Адрес Redis для авто-обнаружения нод кластера (например, localhost:6379)")
	redisPass := flag.String("redis-pass", "",
		"Пароль для подключения к Redis")
	flag.Parse()

	// --- Инициализация метрик нагрузочного клиента ---
	m := loadtest.NewLoadTestMetrics()

	// Запускаем HTTP-сервер метрик в фоне
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	metricsServer := &http.Server{Addr: *metricsAddr, Handler: mux}
	go func() {
		log.Printf("[loadtest] metrics available at http://%s/metrics", *metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[loadtest] metrics server error: %v", err)
		}
	}()

	// --- Контекст с обработкой сигналов ОС ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Формирование списка адресов нод ---
	var targetAddrs []string

	// 1. Попытка авто-обнаружения нод через Redis
	if *redisAddr != "" {
		log.Printf("[loadtest] connecting to Redis for cluster node discovery: %s", *redisAddr)
		rdb, err := cluster.NewRedisClient(config.ClusterConfig{
			RedisAddr:     *redisAddr,
			RedisPassword: *redisPass,
			RedisDB:       0,
		})
		if err != nil {
			log.Fatalf("[loadtest] failed to connect to Redis: %v", err)
		}
		defer rdb.Close()

		nodes, err := cluster.GetAllNodes(ctx, rdb)
		if err != nil {
			log.Printf("[loadtest] warning: failed to fetch cluster nodes: %v", err)
		} else {
			for _, node := range nodes {
				// Преобразуем рекламный адрес ноды (например, 127.0.0.1:8080) в WebSocket URL
				wsURL := fmt.Sprintf("ws://%s/ws", node.Addr)
				targetAddrs = append(targetAddrs, wsURL)
				log.Printf("[loadtest] discovered cluster node: node_id=%s, ws_url=%s, active_peers=%d",
					node.NodeID, wsURL, node.ActivePeers)
			}
		}
	}

	// 2. Если авто-обнаружение ничего не нашло, смотрим на флаг -addrs
	if len(targetAddrs) == 0 && *addrsStr != "" {
		parts := strings.Split(*addrsStr, ",")
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				targetAddrs = append(targetAddrs, trimmed)
			}
		}
	}

	// 3. Если и там пусто, откатываемся к одиночному -addr
	if len(targetAddrs) == 0 {
		targetAddrs = append(targetAddrs, *addr)
	}

	// --- Конфигурация прогона ---
	cfg := loadtest.RunnerConfig{
		Addr:         targetAddrs[0],
		Addrs:        targetAddrs,
		TotalClients: *clients,
		RampPerSec:   *ramp,
		Duration:     *duration,
		Rooms:        *rooms,
		RoomPrefix:   *roomPrefix,
	}

	fmt.Printf("\n=== SFU Cluster Load Test ===\n")
	fmt.Printf("Nodes count: %d\n", len(cfg.Addrs))
	for i, u := range cfg.Addrs {
		fmt.Printf("  Node %d:    %s\n", i+1, u)
	}
	fmt.Printf("Clients:     %d (ramp: %d/sec)\n", cfg.TotalClients, cfg.RampPerSec)
	fmt.Printf("Duration:    %s per client\n", cfg.Duration)
	fmt.Printf("Rooms:       %d (prefix: %s)\n", cfg.Rooms, cfg.RoomPrefix)
	fmt.Printf("Metrics:     http://localhost%s/metrics\n\n", *metricsAddr)

	// --- Запуск теста ---
	runner := loadtest.NewRunner(cfg, m)
	report := runner.Run(ctx)

	// --- Вывод отчёта ---
	report.Print()

	// Завершаем сервер метрик
	_ = metricsServer.Close()

	// Ненулевой код выхода если error rate > 10%
	if report.ErrorRate > 10.0 {
		os.Exit(1)
	}
}
