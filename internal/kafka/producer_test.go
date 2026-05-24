package kafka

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestKafkaAsyncProducerNonBlocking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := KafkaConfig{
		Enabled:    false, // Mock mode
		BufferSize: 10,
	}

	producer := NewAsyncProducer(ctx, cfg, "test-node")
	producer.Start()
	defer producer.Close()

	// Направляем 100 событий в буфер размером 10
	// Если бы продюсер блокировал поток, это заняло бы вечность или вызвало бы дедлок.
	// В неблокирующем режиме это должно завершиться мгновенно!
	start := time.Now()
	for i := 0; i < 100; i++ {
		producer.Emit(EventPeerJoined, "room-x", "peer-y", nil)
	}
	duration := time.Since(start)

	assert.Less(t, duration, 50*time.Millisecond, "producer Emit must be completely non-blocking")

	sent, dropped := producer.Stats()
	assert.Greater(t, sent+dropped, uint64(0))
}

func TestKafkaConsumerMock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := KafkaConfig{
		Enabled: false, // Mock mode
	}

	var eventCalled atomic.Bool

	consumer := NewConsumer(ctx, cfg, "test-group", func(event *SFUEvent) {
		eventCalled.Store(true)
	})
	consumer.Start()
	defer consumer.Close()

	// В mock режиме ридер nil, поэтому цикл не запускается, ошибок нет
	assert.False(t, eventCalled.Load())
}
