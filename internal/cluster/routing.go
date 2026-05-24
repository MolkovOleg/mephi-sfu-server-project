package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrRoomMappingNotFound = errors.New("room mapping not found")
)

// RoomRouter отвечает за распределенную маршрутизацию комнат между нодами SFU
type RoomRouter struct {
	redisClient *RedisClient
}

// Метод создает экземпляр распределенного роутера комнат
func NewRoomRouter(redisClient *RedisClient) *RoomRouter {
	return &RoomRouter{
		redisClient: redisClient,
	}
}

// Метод привязывает комнату roomID к конкретной ноде nodeID
func (rr *RoomRouter) RegisterRoom(ctx context.Context, roomID, nodeID string, ttl time.Duration) error {
	key := "sfu:rooms:" + roomID
	err := rr.redisClient.Client.Set(ctx, key, nodeID, ttl).Err()
	if err != nil {
		return fmt.Errorf("redis: failed to register room %s to node %s: %w", roomID, nodeID, err)
	}
	return nil
}

// Метод удаляет привязку комнаты из реестра Redis
func (rr *RoomRouter) UnregisterRoom(ctx context.Context, roomID string) error {
	key := "sfu:rooms:" + roomID
	err := rr.redisClient.Client.Del(ctx, key).Err()
	if err != nil {
		return fmt.Errorf("redis: failed to unregister room %s: %w", roomID, err)
	}
	return nil
}

// Метод возвращает ID ноды, на которой запущена комната roomID
func (rr *RoomRouter) GetNodeForRoom(ctx context.Context, roomID string) (string, error) {
	key := "sfu:rooms:" + roomID
	nodeID, err := rr.redisClient.Client.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrRoomMappingNotFound
		}
		return "", fmt.Errorf("redis: failed to get node for room %s: %w", roomID, err)
	}
	return nodeID, nil
}

// Метод продлевает время жизни комнаты в реестре Redis
func (rr *RoomRouter) KeepRoomAlive(ctx context.Context, roomID string, ttl time.Duration) error {
	key := "sfu:rooms:" + roomID
	err := rr.redisClient.Client.Expire(ctx, key, ttl).Err()
	if err != nil {
		return fmt.Errorf("redis: failed to refresh room %s TTL: %w", roomID, err)
	}
	return nil
}
