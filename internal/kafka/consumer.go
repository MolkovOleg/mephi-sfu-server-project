package kafka

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
)

// Обработчика событий консьюмера
type OnEventFunc func(event *SFUEvent)

// Легковесный подписчик на аналитические события Kafka
type Consumer struct {
	config  KafkaConfig
	reader  *kafka.Reader
	handler OnEventFunc
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// Создание нового инстанс консьюмера аналитики
func NewConsumer(ctx context.Context, config KafkaConfig, groupID string, handler OnEventFunc) *Consumer {
	pCtx, cancel := context.WithCancel(ctx)

	var reader *kafka.Reader
	if config.Enabled {
		reader = kafka.NewReader(kafka.ReaderConfig{
			Brokers:        config.Brokers,
			Topic:          config.Topic,
			GroupID:        groupID,
			MinBytes:       10e3, // 10KB
			MaxBytes:       10e6, // 10MB
			CommitInterval: 1 * time.Second,
		})
		log.Printf("[KafkaConsumer] initialized: brokers=%v topic=%s group=%s",
			config.Brokers, config.Topic, groupID)
	}

	return &Consumer{
		config:  config,
		reader:  reader,
		handler: handler,
		ctx:     pCtx,
		cancel:  cancel,
	}
}

// Запуск цикла чтения сообщений из Kafka в фоновом режиме
func (c *Consumer) Start() {
	if c.reader == nil {
		log.Printf("[KafkaConsumer] running in mock mode: reader is disabled")
		return
	}

	c.wg.Add(1)
	go c.readLoop()
}

// Остановка чтения и освобождение ресурсов
func (c *Consumer) Close() {
	c.cancel()
	c.wg.Wait()

	if c.reader != nil {
		_ = c.reader.Close()
	}
	log.Printf("[KafkaConsumer] stopped")
}

// Фоновый цикл чтения сообщений
func (c *Consumer) readLoop() {
	defer c.wg.Done()

	log.Printf("[KafkaConsumer] starting message read loop")
	for {
		// Читаем сообщение из топика с блокировкой
		msg, err := c.reader.ReadMessage(c.ctx)
		if err != nil {
			select {
			case <-c.ctx.Done():
				return
			default:
				log.Printf("[KafkaConsumer] read error: %v", err)
				// Небольшая пауза при ошибке, чтобы избежать спама
				time.Sleep(1 * time.Second)
				continue
			}
		}

		var event SFUEvent
		if err := json.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("[KafkaConsumer] failed to unmarshal event value: %v", err)
			continue
		}

		// Вызываем пользовательский callback
		if c.handler != nil {
			c.handler(&event)
		}
	}
}
