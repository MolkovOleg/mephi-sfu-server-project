package sfu

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"sfu-server/internal/cluster"
)

func TestCascadeBridgeFlow(t *testing.T) {
	// Инициализируем клиент Redis для проверки
	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("local Redis is not running, skipping cascade integration test")
		return
	}
	defer rdb.Close()

	rc := &cluster.RedisClient{Client: rdb}

	// 1. Создаем два локальных SFU роутера (имитирующие две ноды)
	routerA := NewRouter(ctx, nil)
	routerB := NewRouter(ctx, nil)
	defer routerA.Close()
	defer routerB.Close()

	// 2. Создаем каскадные мосты между Node A и Node B
	bridgeA, err := NewCascadeBridge(ctx, "room-1", "node-A", "node-B", rc, routerA, true)
	if err != nil {
		t.Fatalf("failed to create bridge A: %v", err)
	}
	defer bridgeA.Close()

	bridgeB, err := NewCascadeBridge(ctx, "room-1", "node-B", "node-A", rc, routerB, false)
	if err != nil {
		t.Fatalf("failed to create bridge B: %v", err)
	}
	defer bridgeB.Close()

	// Проверяем успешное создание
	if bridgeA == nil || bridgeB == nil {
		t.Fatal("bridges should not be nil")
	}
}
