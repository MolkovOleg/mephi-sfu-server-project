package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"sfu-server/internal/sfu"
)

func main() {
	// Создаём контекст с отменой по сигналу ОС (graceful shutdown)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Инициализация SFU сервера
	config := sfu.DefaultServerConfig()
	server := sfu.NewSFUServer(ctx, config)

	log.Printf("[sfud] MEPhI SFU Server started")

	// Блокировка до получения сигнала завершения
	<-ctx.Done()

	log.Printf("[sfud] shutting down...")

	// Graceful shutdown — закрываем все комнаты и ресурсы
	server.Close()

	log.Printf("[sfud] server stopped")
}
