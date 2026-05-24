package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"sfu-server/internal/config"
)

func TestClusterFlow(t *testing.T) {
	// Инициализируем конфиг для теста
	cfg := config.ClusterConfig{
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       0,
	}

	// Создаем клиент
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Проверяем, запущен ли Redis локально. Если нет — пропускаем тест
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("local Redis is not running, skipping integration test")
		return
	}
	defer rdb.Close()

	rc := &RedisClient{Client: rdb}

	t.Run("NodeRegistrationAndHeartbeat", func(t *testing.T) {
		nodeID := "test-node-1"
		addr := "127.0.0.1:8080"
		node := NewClusterNode(nodeID, addr, rc)

		// Запускаем heartbeat с коротким интервалом
		heartbeatCtx, heartbeatCancel := context.WithCancel(context.Background())
		defer heartbeatCancel()

		peerCount := int64(42)
		roomCount := int64(3)

		node.StartHeartbeat(
			heartbeatCtx,
			100*time.Millisecond,
			500*time.Millisecond,
			func() int64 { return peerCount },
			func() int64 { return roomCount },
		)

		// Ждем отправки первого heartbeat
		time.Sleep(150 * time.Millisecond)

		// Проверяем наличие ноды в Redis
		val, err := rdb.Get(context.Background(), "sfu:nodes:"+nodeID).Result()
		if err != nil {
			t.Fatalf("failed to get node stats: %v", err)
		}

		var stats NodeStats
		if err := json.Unmarshal([]byte(val), &stats); err != nil {
			t.Fatalf("failed to unmarshal stats: %v", err)
		}

		if stats.NodeID != nodeID {
			t.Errorf("expected nodeID %s, got %s", nodeID, stats.NodeID)
		}
		if stats.ActivePeers != peerCount {
			t.Errorf("expected peer count %d, got %d", peerCount, stats.ActivePeers)
		}
		if stats.ActiveRooms != roomCount {
			t.Errorf("expected room count %d, got %d", roomCount, stats.ActiveRooms)
		}

		// Проверяем получение всех нод
		nodes, err := GetAllNodes(context.Background(), rc)
		if err != nil {
			t.Fatalf("failed to get all nodes: %v", err)
		}

		found := false
		for _, n := range nodes {
			if n.NodeID == nodeID {
				found = true
				break
			}
		}
		if !found {
			t.Error("test node was not found in active nodes list")
		}

		// Останавливаем heartbeat и проверяем дерегистрацию
		heartbeatCancel()
		time.Sleep(100 * time.Millisecond)

		_, err = rdb.Get(context.Background(), "sfu:nodes:"+nodeID).Result()
		if !errors.Is(err, redis.Nil) {
			t.Errorf("expected key to be deleted, got err: %v", err)
		}
	})

	t.Run("RoomRouting", func(t *testing.T) {
		router := NewRoomRouter(rc)
		roomID := "test-room-xyz"
		nodeID := "test-node-99"

		// Регистрируем комнату
		err := router.RegisterRoom(context.Background(), roomID, nodeID, 1*time.Second)
		if err != nil {
			t.Fatalf("failed to register room: %v", err)
		}

		// Проверяем поиск комнаты
		targetNode, err := router.GetNodeForRoom(context.Background(), roomID)
		if err != nil {
			t.Fatalf("failed to get node for room: %v", err)
		}
		if targetNode != nodeID {
			t.Errorf("expected node %s, got %s", nodeID, targetNode)
		}

		// Продлеваем жизнь комнаты
		err = router.KeepRoomAlive(context.Background(), roomID, 5*time.Second)
		if err != nil {
			t.Fatalf("failed to keep room alive: %v", err)
		}

		// Удаляем комнату
		err = router.UnregisterRoom(context.Background(), roomID)
		if err != nil {
			t.Fatalf("failed to unregister room: %v", err)
		}

		// Проверяем, что комната удалена
		_, err = router.GetNodeForRoom(context.Background(), roomID)
		if !errors.Is(err, ErrRoomMappingNotFound) {
			t.Errorf("expected ErrRoomMappingNotFound, got: %v", err)
		}
	})
}
