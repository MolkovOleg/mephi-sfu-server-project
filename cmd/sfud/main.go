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
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	appcfg "sfu-server/internal/config"
	"sfu-server/internal/sfu"
	sfusignal "sfu-server/internal/signal"

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

	// Контекст с отменой по сигналу ОС
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Инициализация SFU-ядра
	sfuServer := sfu.NewSFUServer(ctx, sfu.ServerConfig{
		MaxRooms:          cfg.SFU.MaxRooms,
		DefaultRoomConfig: buildRoomConfig(cfg),
		DefaultPeerConfig: buildPeerConfig(cfg),
	})

	// Запуск сигнального сервера
	signalServer := sfusignal.NewServer(cfg.Server, sfuServer, "web")

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
