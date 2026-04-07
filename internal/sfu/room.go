package sfu

import (
	"context"
	"errors"
	"log"
	"sync"
)

// =============================================================================
// room.go — Менеджер конференц-комнаты (Room)
// =============================================================================
var (
	ErrRoomFull          = errors.New("room is full")
	ErrPeerAlreadyJoined = errors.New("peer already joined")
)

// RoomConfig — настройки комнаты.
type RoomConfig struct {
	// Максимальное кол-во участников в комнате (0 — без ограничения).
	MaxPeers int
}

// Установка настроек по умолчанию для комнаты
func DefaultRoomConfig() RoomConfig {
	return RoomConfig{
		MaxPeers: 0,
	}
}

// Room — представление одной конференции (комнаты) в SFU сервере.
type Room struct {
	id        string
	router    *Router
	peers     map[string]*Peer
	mu        sync.RWMutex
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	config    RoomConfig
	onClose   func(roomID string)
}

// NewRoom создаёт новую инстанцию комнаты.
func NewRoom(ctx context.Context, id string, config RoomConfig) *Room {
	roomCtx, cancel := context.WithCancel(ctx)

	// Создаём отдельный маршрутизатор для этой комнаты
	router := NewRouter(roomCtx)

	room := &Room{
		id:     id,
		router: router,
		peers:  make(map[string]*Peer),
		ctx:    roomCtx,
		cancel: cancel,
		config: config,
	}

	// Настраиваем логику автоподписки/отписки:
	router.SetOnReceiverAdded(room.onReceiverAdded)
	router.SetOnReceiverRemoved(room.onReceiverRemoved)

	return room
}

// --- API-методы ---

// Возвращение ID комнаты
func (r *Room) ID() string { return r.id }

// Возврат контекста комнаты
func (r *Room) Context() context.Context { return r.ctx }

// Установка callback при закрытии комнаты
func (r *Room) SetOnClose(fn func(string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onClose = fn
}

// Join добавляет нового участника (Peer) в комнату.
//
// Выполняет следующие действия:
//  1. Проверяет лимиты комнаты.
//  2. Инициализирует инстанс Peer'а (WebRTC PeerConnection).
//  3. Устанавливает слушатель отключения пира.
//  4. Подписывает нового участника на ВСЕ уже существующие треки в комнате.
func (r *Room) Join(peerID string, peerConfig PeerConfig) (*Peer, error) {
	r.mu.Lock()

	// Проверяем лимит участников
	if r.config.MaxPeers > 0 && len(r.peers) >= r.config.MaxPeers {
		r.mu.Unlock()
		return nil, ErrRoomFull
	}

	// Проверяем дубликат
	if _, exists := r.peers[peerID]; exists {
		r.mu.Unlock()
		return nil, ErrPeerAlreadyJoined
	}

	// Создаём Peer с закреплением за общим Router комнаты
	peer, err := NewPeer(r.ctx, peerID, r.router, peerConfig)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}

	// Устанавливаем callback отключения, чтобы вовремя вычищать мапу peers
	peer.SetOnClose(func(id string) {
		r.onPeerClosed(id)
	})

	// Сохраняем Peer
	r.peers[peerID] = peer

	// Получаем текущие Receiver'ы до снятия лока
	receivers := r.router.GetReceivers()

	r.mu.Unlock()

	// Подписываем нового участника на ВСЕ существующие треки:
	for _, recv := range receivers {
		// Мы не подписываем пира на треки, которые он гипотетически мог создать сам
		if recv.OwnerPeerID() == peerID {
			continue
		}
		if err := peer.SubscribeToTrack(recv.TrackID()); err != nil {
			log.Printf("[Room] subscribe error on join: room=%s peer=%s track=%s err=%v",
				r.id, peerID, recv.TrackID(), err)
		}
	}

	log.Printf("[Room] peer joined: room=%s peer=%s peers=%d",
		r.id, peerID, r.PeerCount())

	return peer, nil
}

// Leave безопасно удаляет участника из комнаты и закрывает его соединение.
func (r *Room) Leave(peerID string) {
	r.mu.Lock()

	peer, ok := r.peers[peerID]
	if !ok {
		r.mu.Unlock()
		return
	}

	// Удаляем пира из стейта комнаты
	delete(r.peers, peerID)
	isEmpty := len(r.peers) == 0

	// Обязательно снимаем lock ДО вызова peer.Close(),
	r.mu.Unlock()

	// Закрываем Peer (также закроет PeerConnection и отправит клиенту уведомление)
	peer.Close()

	log.Printf("[Room] peer left: room=%s peer=%s peers=%d",
		r.id, peerID, r.PeerCount())

	// Паттерн "авто-очистки": если больше никого нет — закрываем всю комнату
	if isEmpty {
		log.Printf("[Room] empty after leave, closing: room=%s", r.id)
		r.Close()
	}
}

// Получение Peer'а по ID
func (r *Room) GetPeer(peerID string) *Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.peers[peerID]
}

// Возвращение списка всех Peer'ов
func (r *Room) Peers() []*Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Peer, 0, len(r.peers))
	for _, peer := range r.peers {
		result = append(result, peer)
	}
	return result
}

// Возвращение кол-ва участников в комнате
func (r *Room) PeerCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.peers)
}

// Close закрывает комнату и принудительно отключает всех участников.
func (r *Room) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()

		// Собираем всех Peer'ов во временный список и сразу очищаем мапу
		peers := r.peers
		r.peers = make(map[string]*Peer)

		onClose := r.onClose

		r.mu.Unlock()

		// Закрываем всех Peer'ов ВНЕ блокировки
		// Архитектурная деталь для высоконагруженных систем:
		// вызов I/O или тяжелых операций (например, сетевых) не должен происходить под Lock.
		for id, peer := range peers {
			peer.Close()
			log.Printf("[Room] peer closed (room close): room=%s peer=%s", r.id, id)
		}

		// Останавливаем маршрутизатор
		r.router.Close()

		// Отменяем контекст (сигнал для завершения всех процессов, связанных с Room)
		r.cancel()

		log.Printf("[Room] closed: room=%s", r.id)

		// Уведомляем систему уровнем выше (SFU Server), что данная комната мертва.
		if onClose != nil {
			onClose(r.id)
		}
	})
}

// Stats возвращает статистику комнаты (кол-во участников и метрики роутера).
func (r *Room) Stats() RoomStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	routerStats := r.router.Stats()

	return RoomStats{
		PeerCount:     len(r.peers),
		ReceiverCount: routerStats.ReceiverCount,
		SenderCount:   routerStats.SenderCount,
	}
}

// Статистика потоков и участников данной комнаты
type RoomStats struct {
	PeerCount     int // Кол-во участников (Peers)
	ReceiverCount int // Кол-во входящих треков (публикаций)
	SenderCount   int // Кол-во исходящих подписок (доставки медиа)
}

// --- Внутренняя логика ---

// onReceiverAdded работает как callback, который передаётся в Router.
// Срабатывает в тот момент, когда произвольный пользователь начал транслировать трек
// (микрофон, камеру, скриншар). Здесь мы подписываем остальных пользователей на этот трек.
func (r *Room) onReceiverAdded(receiver *Receiver) {
	r.mu.RLock()

	ownerID := receiver.OwnerPeerID()

	// Собираем Peer'ов для подписки.
	// Обязательно ИСКЛЮЧАЕМ владельца, иначе он будет получать своё эхо
	var peersToSubscribe []*Peer
	for id, peer := range r.peers {
		if id != ownerID {
			peersToSubscribe = append(peersToSubscribe, peer)
		}
	}

	r.mu.RUnlock()

	// Подписываем всех на новый трек (вне лока комнаты)
	trackID := receiver.TrackID()
	for _, peer := range peersToSubscribe {
		// Подписка триггерит SDP renegotiation у конкретного Peer'а
		if err := peer.SubscribeToTrack(trackID); err != nil {
			log.Printf("[Room] auto-subscribe error: room=%s peer=%s track=%s err=%v",
				r.id, peer.ID(), trackID, err)
		}
	}

	log.Printf("[Room] auto-subscribed %d peers to new track: room=%s track=%s owner=%s",
		len(peersToSubscribe), r.id, trackID, ownerID)
}

// onReceiverRemoved — callback от Router, вызываемый при прекращении трансляции трека
// (например, пользователь выключил камеру).
// Отписывает всех активных пользователей от этого вымершего трека.
func (r *Room) onReceiverRemoved(receiver *Receiver) {
	r.mu.RLock()

	ownerID := receiver.OwnerPeerID()

	// Собираем список, кого необходимо отписать
	var peersToUnsubscribe []*Peer
	for id, peer := range r.peers {
		if id != ownerID {
			peersToUnsubscribe = append(peersToUnsubscribe, peer)
		}
	}

	r.mu.RUnlock()

	// Отписываем от удалённого трека
	trackID := receiver.TrackID()
	for _, peer := range peersToUnsubscribe {
		if err := peer.UnsubscribeFromTrack(trackID); err != nil {
			log.Printf("[Room] auto-unsubscribe error: room=%s peer=%s track=%s err=%v",
				r.id, peer.ID(), trackID, err)
		}
	}

	log.Printf("[Room] auto-unsubscribed %d peers from removed track: room=%s track=%s",
		len(peersToUnsubscribe), r.id, trackID)
}

// onPeerClosed — служебный callback, вызываемый когда Peer(клиент) отваливается
// по какой-либо причине (сетевая ошибка, ручное отключение, failed WebRTC).
func (r *Room) onPeerClosed(peerID string) {
	r.mu.Lock()

	// Проверяем, есть ли такой Peer. Он мог быть успешно удален в Leave(),
	// в таком случае ничего делать не надо.
	_, ok := r.peers[peerID]
	if !ok {
		r.mu.Unlock()
		return
	}

	// Вычищаем из локального стейта
	delete(r.peers, peerID)
	isEmpty := len(r.peers) == 0

	r.mu.Unlock()

	log.Printf("[Room] peer disconnected: room=%s peer=%s peers=%d",
		r.id, peerID, r.PeerCount())

	// Если комната опустела — убиваем её, чтобы сэкономить ресурсы памяти SFU.
	if isEmpty {
		log.Printf("[Room] empty, closing: room=%s", r.id)
		r.Close()
	}
}
