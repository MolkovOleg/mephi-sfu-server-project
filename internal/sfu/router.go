package sfu

import (
	"context"
	"log"
	"sync"
)

// =============================================================================
// Маршрутизатор медиа-потоков внутри комнат
// =============================================================================
type Router struct {
	// Структура:
	//   receivers["trackA"] = {
	//       ReceiverA
	//   }
	receivers map[string]*Receiver

	// Структура:
	//   senders["trackA"] = {
	//       "peerB": SenderAB,  // Peer B получает трек A
	//       "peerC": SenderAC,  // Peer C получает трек A
	//   }
	senders map[string]map[string]*Sender

	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	// --- Callbacks ---
	onReceiverAdded   func(receiver *Receiver)
	onReceiverRemoved func(receiver *Receiver)
}

// Создание нового Router
func NewRouter(ctx context.Context) *Router {
	routerCtx, cancel := context.WithCancel(ctx)

	return &Router{
		receivers: make(map[string]*Receiver),
		senders:   make(map[string]map[string]*Sender),
		ctx:       routerCtx,
		cancel:    cancel,
	}
}

// =============================================================================
// Управление Receiver через Router
// =============================================================================

// Добавление входящего трека в Router и настройка forwarding
func (r *Router) AddReceiver(receiver *Receiver) {
	r.mu.Lock()

	// Сохраняем receiver в мапу
	trackID := receiver.TrackID()
	r.receivers[trackID] = receiver

	// Создаем мапу senders для данного трека, если еще нет
	if r.senders[trackID] == nil {
		r.senders[trackID] = make(map[string]*Sender)
	}

	receiver.SetOnPacket(func(buf *[]byte, n int) {
		r.forward(trackID, buf, n)
	})

	onAdded := r.onReceiverAdded

	r.mu.Unlock()

	receiver.Start()

	log.Printf("[Router] receiver added: track=%s stram=%s king=%s",
		receiver.trackID, receiver.streamID, receiver.trackKind)

	// Вызываем вне блокировки для избежания DEADLOCK
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

	// Останавливаем все senders и очищаем мапу
	if senders, ok := r.senders[trackID]; ok {
		for peerID, sender := range senders {
			sender.Stop()
			log.Printf("[Router] sender removed (receiver gone): track=%s peer=%s",
				trackID, peerID)
		}
		delete(r.senders, trackID)
	}

	// Останаваливаем receiver
	receiver.Stop()
	delete(r.receivers, trackID)

	onRemoved := r.onReceiverRemoved

	r.mu.Unlock()

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
		log.Printf("[Router] subcribe failed: track=%s not found", trackID)
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
			log.Printf("[Router] unsubscribed from all: track=%s peer=%s", trackID, peerID)
		}
	}
}

// =============================================================================
// Пересылка пакетов (Forwarding)
// =============================================================================

// Рассылка RTP-пакетов всем Sender, подписанным на трек
func (r *Router) forward(trackID string, buf *[]byte, n int) {
	r.mu.RLock()

	senders, ok := r.senders[trackID]
	if !ok || len(senders) == 0 {
		r.mu.RUnlock()
		return
	}

	// Рассылаем пакеты всем подписчикам этого трека
	for _, sender := range senders {
		sender.WriteRTP(buf, n)
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
		RecieverCount: len(r.receivers),
	}

	for _, senders := range r.senders {
		stats.SenderCount += len(senders)
	}

	return stats
}

// Статистика собранная Router'ом
type RouterStats struct {
	RecieverCount int // Кол-во входящих треков
	SenderCount   int // Кол-во исходящих треков (подписок)
}
