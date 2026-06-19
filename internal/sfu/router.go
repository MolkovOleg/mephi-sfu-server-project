package sfu

import (
	"context"
	"log"
	"sync"
	"time"

	appmetrics "sfu-server/internal/metrics"
)

// =============================================================================
// Маршрутизатор медиа-потоков внутри комнат
// =============================================================================
type Router struct {
	receivers map[string]*Receiver
	senders   map[string]map[string]*Sender
	// kinds хранит строковый тип трека ("audio"|"video") для hot-path метрик
	// без повторных вызовов String() и map-lookup по receiver
	kinds map[string]string

	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	metrics *appmetrics.Metrics // nil если метрики отключены

	// --- Callbacks ---
	onReceiverAdded   func(receiver *Receiver)
	onReceiverRemoved func(receiver *Receiver)
}

// NewRouter создаёт маршрутизатор для одной комнаты.
// m может быть nil — тогда метрики не собираются.
func NewRouter(ctx context.Context, m *appmetrics.Metrics) *Router {
	routerCtx, cancel := context.WithCancel(ctx)

	return &Router{
		receivers: make(map[string]*Receiver),
		senders:   make(map[string]map[string]*Sender),
		kinds:     make(map[string]string),
		ctx:       routerCtx,
		cancel:    cancel,
		metrics:   m,
	}
}

// =============================================================================
// Управление Receiver через Router
// =============================================================================

// Добавление входящего трека в Router и настройка forwarding
func (r *Router) AddReceiver(receiver *Receiver) {
	r.mu.Lock()

	trackID := receiver.TrackID()
	r.receivers[trackID] = receiver
	r.kinds[trackID] = receiver.trackKind.String() // кешируем вид трека для hot-path

	if r.senders[trackID] == nil {
		r.senders[trackID] = make(map[string]*Sender)
	}

	receiver.SetOnPacket(func(buf *[]byte, n int) {
		r.forward(trackID, buf, n)
	})

	onAdded := r.onReceiverAdded
	m := r.metrics

	r.mu.Unlock()

	if m != nil {
		m.TrackAdded()
	}

	receiver.Start()

	log.Printf("[Router] receiver added: track=%s stream=%s kind=%s",
		receiver.trackID, receiver.streamID, receiver.trackKind)

	if onAdded != nil {
		onAdded(receiver)
	}
}

// Удаление входящего трека и его senders
func (r *Router) RemoveReceiver(trackID string) {
	r.mu.Lock()

	receiver, ok := r.receivers[trackID]
	if !ok {
		r.mu.Unlock()
		return
	}

	if senders, ok := r.senders[trackID]; ok {
		for peerID, sender := range senders {
			sender.Stop()
			if r.metrics != nil {
				r.metrics.SubscriptionRemoved()
			}
			log.Printf("[Router] sender removed (receiver gone): track=%s peer=%s",
				trackID, peerID)
		}
		delete(r.senders, trackID)
	}

	receiver.Stop()
	delete(r.receivers, trackID)
	delete(r.kinds, trackID)

	onRemoved := r.onReceiverRemoved
	m := r.metrics

	r.mu.Unlock()

	if m != nil {
		m.TrackRemoved()
	}

	log.Printf("[Router] receiver removed: track=%s", trackID)

	if onRemoved != nil {
		onRemoved(receiver)
	}
}

// =============================================================================
// Управление Sender через Router
// =============================================================================

// Подписываем пира на получение трека
func (r *Router) Subscribe(
	trackID string,
	peerID string,
	peerCtx context.Context,
	config SenderConfig,
) (*Sender, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Ищем receiver по треку
	receiver, ok := r.receivers[trackID]
	if !ok {
		log.Printf("[Router] subscribe failed: track=%s not found", trackID)
		return nil, nil
	}

	// Проверяем нет ли уже подписки
	if senders, ok := r.senders[trackID]; ok {
		if _, exists := senders[peerID]; exists {
			log.Printf("[Router] already subscribed: track=%s peer=%s", trackID, peerID)
			return senders[peerID], nil
		}
	}

	// ВАЖНАЯ ЧАСТЬ! Создания Sender с теми же параметрами кодека, что и у Receiver
	codec := receiver.Codec().RTPCodecCapability

	sender, err := NewSender(peerCtx, peerID, codec, trackID, receiver.StreamID(), config)
	if err != nil {
		return nil, err
	}

	// Сохраняем Sender в мапу
	if r.senders[trackID] == nil {
		r.senders[trackID] = make(map[string]*Sender)
	}
	r.senders[trackID][peerID] = sender

	// Запуск записи пакетов в трек
	sender.Start()

	if r.metrics != nil {
		r.metrics.SubscriptionAdded()
	}

	// Запрашиваем также ключевой I-frame для ноовго подписчика
	receiver.RequestKeyFrame()

	log.Printf("[Router] subscribed: track=%s peer=%s codec=%s",
		trackID, peerID, codec.MimeType)

	return sender, nil
}

// Отписываем пир от трека
func (r *Router) Unsubscribe(trackID string, peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	senders, ok := r.senders[trackID]
	if !ok {
		return
	}

	sender, ok := senders[peerID]
	if !ok {
		return
	}

	// Останавливаем Sender
	sender.Stop()
	delete(senders, peerID)

	if r.metrics != nil {
		r.metrics.SubscriptionRemoved()
	}

	log.Printf("[Router] unsubscribed: track=%s peer=%s", trackID, peerID)
}

// Отписание пира от всех треков
func (r *Router) UnsubscribeAll(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for trackID, senders := range r.senders {
		if sender, ok := senders[peerID]; ok {
			sender.Stop()
			delete(senders, peerID)

			if r.metrics != nil {
				r.metrics.SubscriptionRemoved()
			}

			log.Printf("[Router] unsubscribed from all: track=%s peer=%s", trackID, peerID)
		}
	}
}

// =============================================================================
// Пересылка пакетов (Forwarding)
// =============================================================================

// forward рассылает RTP-пакет всем Sender'ам, подписанным на трек.
// Вызывается из горутины Receiver'а — критический hot-path.
func (r *Router) forward(trackID string, buf *[]byte, n int) {
	r.mu.RLock()

	senders, ok := r.senders[trackID]
	if !ok || len(senders) == 0 {
		r.mu.RUnlock()
		return
	}

	// Замеряем время пересылки для Prometheus histogram (RTP Forward Latency)
	start := time.Now()

	// Рассылаем пакеты всем подписчикам этого трека
	for _, sender := range senders {
		sender.WriteRTP(buf, n)
	}

	// Метрики обновляем раз на весь forward-цикл (не N раз)
	if m := r.metrics; m != nil {
		kind := r.kinds[trackID]
		senderCount := len(senders)
		m.RTPPacketsForwarded.WithLabelValues(kind).Add(float64(senderCount))
		m.RTPBytesForwarded.WithLabelValues(kind).Add(float64(n * senderCount))
		m.RTPForwardDuration.Observe(time.Since(start).Seconds())
	}

	r.mu.RUnlock()
}

// =============================================================================
// Вспомогательные методы
// =============================================================================

// Возвращает список всех Receiver
func (r *Router) GetReceivers() []*Receiver {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Receiver, 0, len(r.receivers))
	for _, recv := range r.receivers {
		result = append(result, recv)
	}
	return result
}

// Возвращает список всех Sender
func (r *Router) GetSendersByPeer(peerID string) []*Sender {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*Sender
	for _, senders := range r.senders {
		if sender, ok := senders[peerID]; ok {
			result = append(result, sender)
		}
	}
	return result
}

// Установка callback при добавлении нового Receiver'а
func (r *Router) SetOnReceiverAdded(fn func(*Receiver)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onReceiverAdded = fn
}

// Установка callback при удалении Receiver'а
func (r *Router) SetOnReceiverRemoved(fn func(*Receiver)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onReceiverRemoved = fn
}

// Остановка всех Receiver'ов и Sender'ов
func (r *Router) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for trackID, senders := range r.senders {
		for peerID, sender := range senders {
			sender.Stop()
			if r.metrics != nil {
				r.metrics.SubscriptionRemoved()
			}
			log.Printf("[Router] sender stopped (close): track=%s peer=%s", trackID, peerID)
		}
	}

	for trackID, receiver := range r.receivers {
		receiver.Stop()
		log.Printf("[Router] receiver stopped (close) track=%s", trackID)
	}

	// Очищаем мапы
	r.senders = make(map[string]map[string]*Sender)
	r.receivers = make(map[string]*Receiver)

	// Отменяем контекст
	r.cancel()

	log.Printf("[Router] closed...")
}

// Возвращает статистику Router'а
func (r *Router) Stats() RouterStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := RouterStats{
		ReceiverCount: len(r.receivers),
	}

	for _, senders := range r.senders {
		stats.SenderCount += len(senders)
	}

	return stats
}

// Статистика собранная Router'ом
type RouterStats struct {
	ReceiverCount int // Кол-во входящих треков
	SenderCount   int // Кол-во исходящих треков (подписок)
}
