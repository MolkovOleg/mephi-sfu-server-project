package main

// =============================================================================
// Точка входа SFU-сервера
//
// Порядок запуска:
//  1. Загрузка конфигурации из configs/config.yml (+ ENV override)
//  2. Инициализация SFU-ядра (sfu.SFUServer)
//  3. Запуск сигнального HTTP/WebSocket сервера (signal.Server)
//  4. Ожидание сигнала ОС (SIGINT/SIGTERM)
//  5. Graceful shutdown: signal.Shutdown → sfu.Close
// =============================================================================

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	appmetrics "sfu-server/internal/metrics"
	appcfg "sfu-server/internal/config"
	"sfu-server/internal/sfu"
	sfusignal "sfu-server/internal/signal"
	"sfu-server/internal/cluster"
	"sfu-server/internal/kafka"
	"sfu-server/internal/turn"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
)

func main() {
	// Загрузка конфигурации
	cfgPath := "configs/config.yml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := appcfg.Load(cfgPath)
	if err != nil {
		log.Fatalf("[sfud] failed to load config %q: %v", cfgPath, err)
	}

	log.Printf("[sfud] SFU Server starting")
	log.Printf("[sfud] config: addr=%s log=%s cluster=%v metrics=%v",
		cfg.Server.GetServerAddr(), cfg.Log.Level,
		cfg.Cluster.Enabled, cfg.Metrics.Enabled)

	// Инициализация Prometheus-метрик
	// Даже если cfg.Metrics.Enabled == false, создаём метрики:
	// эндпоинт /metrics включается только если Enabled == true
	m := appmetrics.New()
	var activeMetrics *appmetrics.Metrics
	if cfg.Metrics.Enabled {
		activeMetrics = m
		log.Printf("[sfud] metrics enabled: http://%s/metrics", cfg.Server.GetServerAddr())
	}

	// Определяем ID текущей ноды (для логов/событий Kafka и кластеризации)
	nodeID := cfg.Cluster.NodeID
	if nodeID == "" {
		nodeID = "sfu-node-" + uuid.New().String()
	}

	// Контекст с отменой по сигналу ОС
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Инициализация асинхронного продюсера событий Kafka
	kafkaConfig := kafka.KafkaConfig{
		Enabled:    cfg.Kafka.Enabled,
		Brokers:    cfg.Kafka.Brokers,
		Topic:      cfg.Kafka.Topic,
		BufferSize: cfg.Kafka.BufferSize,
	}
	kafkaProducer := kafka.NewAsyncProducer(ctx, kafkaConfig, nodeID)
	kafkaProducer.Start()
	defer kafkaProducer.Close()

	// Инициализация встроенного TURN-сервера
	var turnServer *turn.Server
	if cfg.TurnServer.Enabled {
		turnCfg := turn.TurnServerConfig{
			Enabled:      cfg.TurnServer.Enabled,
			PublicIP:     cfg.TurnServer.PublicIP,
			Port:         cfg.TurnServer.Port,
			Realm:        cfg.TurnServer.Realm,
			StaticSecret: cfg.TurnServer.StaticSecret,
			MinPort:      cfg.TurnServer.MinPort,
			MaxPort:      cfg.TurnServer.MaxPort,
		}
		turnServer = turn.NewServer(turnCfg)
		if err := turnServer.Start(); err != nil {
			log.Fatalf("[sfud] failed to start embedded TURN server: %v", err)
		}
		defer func() {
			if err := turnServer.Close(); err != nil {
				log.Printf("[sfud] error closing TURN server: %v", err)
			}
		}()
	}

	var rdb *cluster.RedisClient
	var clusterNode *cluster.ClusterNode
	var roomRouter *cluster.RoomRouter

	if cfg.Cluster.Enabled {
		var err error
		rdb, err = cluster.NewRedisClient(cfg.Cluster)
		if err != nil {
			log.Fatalf("[sfud] failed to initialize Redis: %v", err)
		}
		defer rdb.Close()

		// Вычисляем адрес рекламы ноды
		advertiseHost := cfg.Server.Host
		if advertiseHost == "0.0.0.0" {
			advertiseHost = "127.0.0.1" // для локальных тестов
		}
		advertiseAddr := fmt.Sprintf("%s:%d", advertiseHost, cfg.Server.Port)

		clusterNode = cluster.NewClusterNode(nodeID, advertiseAddr, rdb)
		roomRouter = cluster.NewRoomRouter(rdb)

		log.Printf("[sfud] cluster node initialized: nodeID=%s addr=%s", nodeID, advertiseAddr)
	}

	// Инициализация SFU-ядра
	sfuServer := sfu.NewSFUServer(ctx, sfu.ServerConfig{
		MaxRooms:               cfg.SFU.MaxRooms,
		DefaultRoomConfig:      buildRoomConfig(cfg),
		DefaultPeerConfig:      buildPeerConfig(cfg),
		Metrics:                activeMetrics,
		KafkaProducer:          kafkaProducer,
		ClusterEnabled:         cfg.Cluster.Enabled,
		ClusterEnableCascading: cfg.Cluster.EnableCascading,
		ClusterNode:            clusterNode,
		RoomRouter:             roomRouter,
	})

	// Если кластеризация включена, запускаем Heartbeats, Keep-Alive комнат и PubSub слушатель
	if cfg.Cluster.Enabled {
		peerCountFn := func() int64 {
			return int64(sfuServer.Stats().TotalPeers)
		}
		roomCountFn := func() int64 {
			return int64(sfuServer.RoomsCount())
		}

		clusterNode.StartHeartbeat(ctx, cfg.Cluster.HeartbeatInterval, cfg.Cluster.NodeTTL, peerCountFn, roomCountFn)

		// Горутина подписки на Pub/Sub канал ноды для каскадирования
		pubsub := rdb.SubscribeToNodePubSub(ctx, clusterNode.NodeID())
		go func() {
			defer pubsub.Close()
			ch := pubsub.Channel()
			log.Printf("[sfud] subscribed to cluster signaling channel: nodeID=%s", clusterNode.NodeID())
			for {
				select {
				case <-ctx.Done():
					return
				case redisMsg, ok := <-ch:
					if !ok {
						return
					}
					var msg cluster.ClusterMessage
					if err := json.Unmarshal([]byte(redisMsg.Payload), &msg); err != nil {
						log.Printf("[sfud] failed to unmarshal cluster message: %v", err)
						continue
					}
					sfuServer.HandleClusterMessage(msg)
				}
			}
		}()

		// Горутина для периодического продления TTL комнат этой ноды в Redis
		go func() {
			ticker := time.NewTicker(cfg.Cluster.HeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					rooms := sfuServer.Rooms()
					for _, room := range rooms {
						_ = roomRouter.KeepRoomAlive(ctx, room.ID(), cfg.Cluster.NodeTTL)
					}
				}
			}
		}()
	}

	// Запуск сигнального сервера
	signalServer := sfusignal.NewServer(cfg, sfuServer, "web", activeMetrics)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- signalServer.ListenAndServe()
	}()

	log.Printf("[sfud] server started: addr=%s", cfg.Server.GetServerAddr())

	// Ожидание сигнала завершения или ошибки сервера
	select {
	case <-ctx.Done():
		log.Printf("[sfud] shutdown signal received")
	case err := <-serverErr:
		if err != nil {
			log.Printf("[sfud] signal server error: %v", err)
		}
	}

	// Gracefull shutdown
	log.Printf("[sfud] shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Останавливаем прием новых WebSocket-соединений
	if err := signalServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("[sfud] signal server shutdown error: %v", err)
	}

	// Каскадно закрываем все комнаты и пиры
	sfuServer.Close()
	log.Printf("[sfud] server stopped")
}

// =============================================================================
// Вспомогательные функции
// =============================================================================

// Метод формирования конфига комнаты из конфигурации приложения
func buildRoomConfig(cfg *appcfg.Config) sfu.RoomConfig {
	return sfu.RoomConfig{
		MaxPeers: cfg.SFU.MaxPeersPerRoom,
	}
}

// Метод формирования конфигурации WebRTC-пира, ICE-сервера,
// интервала PLI, размер буфера Sender
func buildPeerConfig(cfg *appcfg.Config) sfu.PeerConfig {
	peerCfg := sfu.DefaultPeerConfig()
	peerCfg.ICEServers = buildICEServers(cfg)
	peerCfg.ReceiverConfig.PLIInterval = cfg.SFU.PLIInterval
	peerCfg.SenderConfig.WriteChannelSize = cfg.SFU.SenderBufSize

	return peerCfg
}

// Метод преобразует ICEConfig в список pion-совместимых ICEServer
// Включает STUN-серверы и опциональные TURN-серверы из конфига
func buildICEServers(cfg *appcfg.Config) []webrtc.ICEServer {
	var servers []webrtc.ICEServer

	// STUN-серверы
	if len(cfg.ICE.STUNServers) > 0 {
		servers = append(servers, webrtc.ICEServer{
			URLs: cfg.ICE.STUNServers,
		})
	}

	// TURN-серверы (с аунтификацией)
	for _, t := range cfg.ICE.TURNServers {
		servers = append(servers, webrtc.ICEServer{
			URLs:           t.URLs,
			Username:       t.Username,
			Credential:     t.Credential,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}

	return servers
}
