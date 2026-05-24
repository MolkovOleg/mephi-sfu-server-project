package kafka

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/segmentio/kafka-go"
)

// AsyncProducer представляет асинхронный неблокирующий продюсер событий
type AsyncProducer struct {
	config       KafkaConfig
	nodeID       string
	ch           chan *SFUEvent
	writer       *kafka.Writer
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	droppedCount atomic.Uint64
	sentCount    atomic.Uint64
}

// Создание нового инстанс продюсера
func NewAsyncProducer(ctx context.Context, config KafkaConfig, nodeID string) *AsyncProducer {
	pCtx, cancel := context.WithCancel(ctx)

	var writer *kafka.Writer
	if config.Enabled {
		writer = &kafka.Writer{
			Addr:         kafka.TCP(config.Brokers...),
			Topic:        config.Topic,
			Balancer:     &kafka.LeastBytes{},
			BatchSize:    100,
			BatchTimeout: 50 * time.Millisecond,
			Async:        false, // Буферизация уже сделана на уровне Go-канала
		}
		log.Printf("[KafkaProducer] initialized: brokers=%v topic=%s bufSize=%d",
			config.Brokers, config.Topic, config.BufferSize)
	} else {
		log.Printf("[KafkaProducer] running in local mock/log mode (Kafka disabled)")
	}

	return &AsyncProducer{
		config: config,
		nodeID: nodeID,
		ch:     make(chan *SFUEvent, config.BufferSize),
		writer: writer,
		ctx:    pCtx,
		cancel: cancel,
	}
}

// Запуск цикла отправки событий в фоновом режиме
func (ap *AsyncProducer) Start() {
	ap.wg.Add(1)
	go ap.writeLoop()
}

// Принимает событие в неблокирующем режиме
func (ap *AsyncProducer) Emit(evtType EventType, roomID string, peerID string, payload interface{}) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[KafkaProducer] failed to marshal payload: %v", err)
		return
	}

	event := &SFUEvent{
		Type:      evtType,
		RoomID:    roomID,
		PeerID:    peerID,
		NodeID:    ap.nodeID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payloadBytes,
	}

	// Неблокирующая запись в канал
	select {
	case ap.ch <- event:
	default:
		ap.droppedCount.Add(1)
		if ap.droppedCount.Load()%100 == 1 {
			log.Printf("[KafkaProducer] warning: channel buffer is full, events dropped: total_dropped=%d",
				ap.droppedCount.Load())
		}
	}
}

// Возвращает статистику отправки
func (ap *AsyncProducer) Stats() (sent uint64, dropped uint64) {
	return ap.sentCount.Load(), ap.droppedCount.Load()
}

// Остановка продюсера и сброс оставшихся сообщений
func (ap *AsyncProducer) Close() {
	ap.cancel()
	ap.wg.Wait()

	if ap.writer != nil {
		_ = ap.writer.Close()
	}
	log.Printf("[KafkaProducer] stopped: sent=%d dropped=%d", ap.sentCount.Load(), ap.droppedCount.Load())
}

// Фоновый цикл отправки событий
func (ap *AsyncProducer) writeLoop() {
	defer ap.wg.Done()

	for {
		select {
		case <-ap.ctx.Done():
			// Сбрасываем оставшиеся сообщения из канала перед выходом
			ap.drain()
			return

		case event := <-ap.ch:
			ap.send(event)
		}
	}
}

// Отправка одного события
func (ap *AsyncProducer) send(event *SFUEvent) {
	if ap.writer == nil {
		// Mock режим: просто выводим в лог при уровне debug
		ap.sentCount.Add(1)
		return
	}

	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("[KafkaProducer] marshal event error: %v", err)
		return
	}

	// Пишем в Kafka с таймаутом
	writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = ap.writer.WriteMessages(writeCtx, kafka.Message{
		Key:   []byte(event.RoomID),
		Value: data,
	})

	if err != nil {
		ap.droppedCount.Add(1)
		log.Printf("[KafkaProducer] failed to write message to Kafka: %v", err)
	} else {
		ap.sentCount.Add(1)
	}
}

// Очистка оставшихся в канале событий при выключении
func (ap *AsyncProducer) drain() {
	close(ap.ch)
	for event := range ap.ch {
		ap.send(event)
	}
}
