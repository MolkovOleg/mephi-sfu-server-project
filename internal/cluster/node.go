package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime"
	"syscall"
	"time"
)

// Отображение показателей производительности и активности ноды
type NodeStats struct {
	NodeID      string    `json:"node_id"`
	Addr        string    `json:"addr"`
	ActivePeers int64     `json:"active_peers"`
	ActiveRooms int64     `json:"active_rooms"`
	CPUUsage    float64   `json:"cpu_usage"`  // в процентах (0-100)
	MemoryMB    float64   `json:"memory_mb"`  // потребление памяти Alloc в МБ
	Goroutines  int       `json:"goroutines"` // количество горутин
	UpdatedAt   time.Time `json:"updated_at"`
}

// Представление локальной ноды в кластере
type ClusterNode struct {
	nodeID      string
	addr        string
	redisClient *RedisClient
	stats       *NodeStats

	// Переменные для расчета CPU
	lastCPUTime time.Duration
	lastTime    time.Time
}

// Метод инициализирует объект локальной ноды
func NewClusterNode(nodeID, addr string, redisClient *RedisClient) *ClusterNode {
	cn := &ClusterNode{
		nodeID:      nodeID,
		addr:        addr,
		redisClient: redisClient,
		stats: &NodeStats{
			NodeID:    nodeID,
			Addr:      addr,
			UpdatedAt: time.Now(),
		},
		lastTime: time.Now(),
	}
	cn.lastCPUTime = cn.getCPUTime()
	return cn
}

// Метод возвращает уникальный идентификатор ноды
func (cn *ClusterNode) NodeID() string {
	return cn.nodeID
}

// Метод возвращает клиент Redis ноды
func (cn *ClusterNode) RedisClient() *RedisClient {
	return cn.redisClient
}

// Метод возвращает адрес сигнализации ноды
func (cn *ClusterNode) Addr() string {
	return cn.addr
}

// Метод запускает фоновую горутину для отправки heartbeat-статуса
func (cn *ClusterNode) StartHeartbeat(
	ctx context.Context,
	interval time.Duration,
	ttl time.Duration,
	peerCountFn func() int64,
	roomCountFn func() int64,
) {
	log.Printf("[ClusterNode] starting heartbeat: nodeID=%s addr=%s interval=%s ttl=%s",
		cn.nodeID, cn.addr, interval, ttl)

	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// При завершении работы ноды — удаляем ее из реестра в Redis (graceful shutdown)
				log.Printf("[ClusterNode] stopping heartbeat and deregistering nodeID=%s", cn.nodeID)
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				if err := cn.redisClient.Client.Del(cleanupCtx, "sfu:nodes:"+cn.nodeID).Err(); err != nil {
					log.Printf("[ClusterNode] failed to deregister: %v", err)
				}
				cancel()
				return

			case <-ticker.C:
				cn.updateStats(peerCountFn(), roomCountFn())

				data, err := json.Marshal(cn.stats)
				if err != nil {
					log.Printf("[ClusterNode] stats marshal error: %v", err)
					continue
				}

				// Обновляем ключ ноды с заданным TTL
				err = cn.redisClient.Client.Set(ctx, "sfu:nodes:"+cn.nodeID, data, ttl).Err()
				if err != nil {
					log.Printf("[ClusterNode] failed to send heartbeat to Redis: %v", err)
				}
			}
		}
	}()
}

// Метод собирает текущие метрики системы и процесса
func (cn *ClusterNode) updateStats(activePeers, activeRooms int64) {
	cn.stats.ActivePeers = activePeers
	cn.stats.ActiveRooms = activeRooms
	cn.stats.Goroutines = runtime.NumGoroutine()

	// Получаем использование памяти в МБ
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	cn.stats.MemoryMB = float64(m.Alloc) / 1024 / 1024

	// Расчет CPU
	now := time.Now()
	cpuTime := cn.getCPUTime()
	timeDelta := now.Sub(cn.lastTime)

	if timeDelta > 0 {
		cpuDelta := cpuTime - cn.lastCPUTime
		// Расчет процента использования одного ядра
		percentage := (float64(cpuDelta) / float64(timeDelta)) * 100.0
		// Нормализуем по количеству ядер CPU
		cn.stats.CPUUsage = percentage / float64(runtime.NumCPU())
	}

	cn.lastCPUTime = cpuTime
	cn.lastTime = now
	cn.stats.UpdatedAt = now
}

// Метод возвращает суммарное процессорное время (user + system) текущего процесса
func (cn *ClusterNode) getCPUTime() time.Duration {
	var rusage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &rusage); err != nil {
		return 0
	}
	userTime := time.Duration(rusage.Utime.Sec)*time.Second + time.Duration(rusage.Utime.Usec)*time.Microsecond
	sysTime := time.Duration(rusage.Stime.Sec)*time.Second + time.Duration(rusage.Stime.Usec)*time.Microsecond
	return userTime + sysTime
}

// Метод возвращает список всех активных нод в кластере
func GetAllNodes(ctx context.Context, rdb *RedisClient) ([]NodeStats, error) {
	keys, err := rdb.Client.Keys(ctx, "sfu:nodes:*").Result()
	if err != nil {
		return nil, fmt.Errorf("redis: failed to list nodes: %w", err)
	}

	nodes := make([]NodeStats, 0, len(keys))
	for _, key := range keys {
		val, err := rdb.Client.Get(ctx, key).Result()
		if err != nil {
			continue // Нода могла истечь между вызовами Keys и Get
		}

		var stats NodeStats
		if err := json.Unmarshal([]byte(val), &stats); err != nil {
			continue
		}
		nodes = append(nodes, stats)
	}

	return nodes, nil
}
