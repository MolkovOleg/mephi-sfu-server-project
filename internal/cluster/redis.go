package cluster

import (
	"context"
	"fmt"
	"time"

	"sfu-server/internal/config"

	"github.com/redis/go-redis/v9"
)

// Представляет обертку вокруг клиента Redis
type RedisClient struct {
	Client *redis.Client
}

// Метод создает и проверяет подключение к Redis
func NewRedisClient(cfg config.ClusterConfig) (*RedisClient, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	// Проверяем подключение с таймаутом
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis: connection failed: %w", err)
	}

	return &RedisClient{Client: rdb}, nil
}

// Close закрывает соединение с Redis
func (r *RedisClient) Close() error {
	if r.Client != nil {
		return r.Client.Close()
	}
	return nil
}

// Метод отправляет сообщение в Pub/Sub канал указанной ноды
func (r *RedisClient) PublishPubSubMessage(ctx context.Context, targetNodeID string, msg []byte) error {
	channel := "sfu:node:pubsub:" + targetNodeID
	return r.Client.Publish(ctx, channel, msg).Err()
}

// Метод подписывается на Pub/Sub канал ноды
func (r *RedisClient) SubscribeToNodePubSub(ctx context.Context, nodeID string) *redis.PubSub {
	channel := "sfu:node:pubsub:" + nodeID
	return r.Client.Subscribe(ctx, channel)
}
