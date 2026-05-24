package sfu

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"

	"github.com/pion/webrtc/v3"
	"sfu-server/internal/cluster"
	"sfu-server/internal/kafka"
	appmetrics "sfu-server/internal/metrics"
)

// =============================================================================
// Менеджер конференц-комнаты (Room)
// =============================================================================
var (
	ErrRoomFull          = errors.New("room is full")
	ErrPeerAlreadyJoined = errors.New("peer already joined")
)

// RoomConfig — настройки комнаты.
type RoomConfig struct {
	// Максимальное кол-во участников в комнате
	MaxPeers int
}

// Установка настроек по умолчанию для комнаты
func DefaultRoomConfig() RoomConfig {
	return RoomConfig{
		MaxPeers: 0,
	}
}

type Room struct {
	id             string
	router         *Router
	peers          map[string]*Peer
	cascadeBridges map[string]*CascadeBridge
	cascadeMu      sync.RWMutex
	mu             sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	config         RoomConfig
	onClose        func(roomID string)
	metrics        *appmetrics.Metrics
	kafkaProducer  *kafka.AsyncProducer
}

// NewRoom создаёт инстанцию комнаты с маршрутизатором.
// m может быть nil — тогда метрики не собираются.
func NewRoom(ctx context.Context, id string, config RoomConfig, m *appmetrics.Metrics, kp *kafka.AsyncProducer) *Room {
	roomCtx, cancel := context.WithCancel(ctx)

	// Передаём метрики в Router — он инструментирован для hot-path
	router := NewRouter(roomCtx, m)

	room := &Room{
		id:             id,
		router:         router,
		peers:          make(map[string]*Peer),
		cascadeBridges: make(map[string]*CascadeBridge),
		ctx:            roomCtx,
		cancel:         cancel,
		config:         config,
		metrics:        m,
		kafkaProducer:  kp,
	}

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

// Join добавляет нового участника (Peer) в комнату
//
// Выполняет следующие действия:
//  1. Проверяет лимиты комнаты
//  2. Инициализирует инстанс Peer'а (WebRTC PeerConnection)
//  3. Устанавливает слушатель отключения пира
//  4. Подписывает нового участника на ВСЕ уже существующие треки в комнате
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

	if r.kafkaProducer != nil {
		r.kafkaProducer.Emit(kafka.EventPeerJoined, r.id, peerID, nil)
	}

	log.Printf("[Room] peer joined: room=%s peer=%s peers=%d",
		r.id, peerID, r.PeerCount())

	return peer, nil
}

// Leave безопасно удаляет участника из комнаты и закрывает его соединение
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

	if r.kafkaProducer != nil {
		r.kafkaProducer.Emit(kafka.EventPeerLeft, r.id, peerID, nil)
	}

	// Обязательно снимаем lock ДО вызова peer.Close()
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

// Close закрывает комнату и принудительно отключает всех участников
func (r *Room) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()

		// Собираем всех Peer'ов во временный список и сразу очищаем мапу
		peers := r.peers
		r.peers = make(map[string]*Peer)

		onClose := r.onClose

		r.mu.Unlock()

		// Закрываем все Peer'ов ВНЕ блокировки
		// Архитектурная деталь для высоконагруженных систем:
		// вызов I/O или тяжелых операций (например, сетевых) не должен происходить под Lock
		for id, peer := range peers {
			peer.Close()
			log.Printf("[Room] peer closed (room close): room=%s peer=%s", r.id, id)
		}

		// Закрываем все каскадные мосты
		r.cascadeMu.Lock()
		bridges := r.cascadeBridges
		r.cascadeBridges = make(map[string]*CascadeBridge)
		r.cascadeMu.Unlock()

		for _, bridge := range bridges {
			bridge.Close()
		}

		// Останавливаем маршрутизатор
		r.router.Close()

		// Отменяем контекст (сигнал для завершения всех процессов, связанных с Room)
		r.cancel()

		log.Printf("[Room] closed: room=%s", r.id)

		// Уведомляем систему уровнем выше (SFU Server), что данная комната мертва
		if onClose != nil {
			onClose(r.id)
		}
	})
}

// Stats возвращает статистику комнаты (кол-во участников и метрики роутера)
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

// onReceiverAdded работает как callback, который передаётся в Router
// Срабатывает в тот момент, когда произвольный пользователь начал транслировать трек
// (микрофон, камеру, скриншар). Здесь мы подписываем остальных пользователей на этот трек
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

	if r.kafkaProducer != nil {
		r.kafkaProducer.Emit(kafka.EventTrackPublished, r.id, ownerID, map[string]string{
			"track_id":   trackID,
			"stream_id":  receiver.StreamID(),
			"track_kind": receiver.Kind().String(),
		})
	}

	// Ретранслируем трек на другие ноды через каскадные мосты
	r.cascadeMu.RLock()
	bridges := make([]*CascadeBridge, 0, len(r.cascadeBridges))
	for _, bridge := range r.cascadeBridges {
		bridges = append(bridges, bridge)
	}
	r.cascadeMu.RUnlock()

	for _, bridge := range bridges {
		// Предотвращаем петли: не отправляем трек обратно на ту же ноду, откуда он каскадирован
		if ownerID == "cascade-"+bridge.remoteNodeID {
			continue
		}
		_ = bridge.AddTrack(trackID)
	}
}

// AddCascadeBridge регистрирует новый каскадный мост для комнаты
func (r *Room) AddCascadeBridge(remoteNodeID string, bridge *CascadeBridge) {
	r.cascadeMu.Lock()
	defer r.cascadeMu.Unlock()
	r.cascadeBridges[remoteNodeID] = bridge
}

// RemoveCascadeBridge удаляет и закрывает каскадный мост
func (r *Room) RemoveCascadeBridge(remoteNodeID string) {
	r.cascadeMu.Lock()
	defer r.cascadeMu.Unlock()
	if bridge, ok := r.cascadeBridges[remoteNodeID]; ok {
		bridge.Close()
		delete(r.cascadeBridges, remoteNodeID)
	}
}

// GetCascadeBridges возвращает список всех каскадных мостов комнаты
func (r *Room) GetCascadeBridges() []*CascadeBridge {
	r.cascadeMu.RLock()
	defer r.cascadeMu.RUnlock()
	var list []*CascadeBridge
	for _, bridge := range r.cascadeBridges {
		list = append(list, bridge)
	}
	return list
}

// onReceiverRemoved — callback от Router, вызываемый при прекращении трансляции трека
// (например, пользователь выключил камеру)
// Отписывает всех активных пользователей от этого вымершего трека
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

	if r.kafkaProducer != nil {
		r.kafkaProducer.Emit(kafka.EventTrackUnpublished, r.id, ownerID, map[string]string{
			"track_id": trackID,
		})
	}
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

	if r.kafkaProducer != nil {
		r.kafkaProducer.Emit(kafka.EventPeerLeft, r.id, peerID, nil)
	}

	r.mu.Unlock()

	log.Printf("[Room] peer disconnected: room=%s peer=%s peers=%d",
		r.id, peerID, r.PeerCount())

	// Если комната опустела — убиваем её, чтобы сэкономить ресурсы памяти SFU
	if isEmpty {
		log.Printf("[Room] empty, closing: room=%s", r.id)
		r.Close()
	}
}

// HandleClusterMessage обрабатывает входящие каскадные сообщения от другой ноды
func (r *Room) HandleClusterMessage(msg cluster.ClusterMessage, localNodeID string, rdb *cluster.RedisClient) {
	r.cascadeMu.Lock()
	bridge, ok := r.cascadeBridges[msg.FromNodeID]
	r.cascadeMu.Unlock()

	switch msg.Type {
	case cluster.TypeCascadeOffer:
		if !ok {
			log.Printf("[Room] creating cascade bridge for incoming offer from node %s: room=%s",
				msg.FromNodeID, r.id)
			var err error
			// Создаем пассивный мост (isInitiator = false)
			bridge, err = NewCascadeBridge(r.ctx, r.id, localNodeID, msg.FromNodeID, rdb, r.router, false)
			if err != nil {
				log.Printf("[Room] failed to create cascade bridge: %v", err)
				return
			}
			r.AddCascadeBridge(msg.FromNodeID, bridge)

			// Подписываемся на ВСЕ существующие локальные треки в комнате, чтобы отправить их новой ноде!
			// ВАЖНО: это позволяет новой ноде получить все треки, которые уже были запущены!
			for _, recv := range r.router.GetReceivers() {
				if recv.OwnerPeerID() != "cascade-"+msg.FromNodeID {
					_ = bridge.AddTrack(recv.TrackID())
				}
			}
		}

		// Обрабатываем предложение (SDP Offer)
		answerSDP, err := bridge.HandleOffer(msg.SDP)
		if err != nil {
			log.Printf("[Room] HandleOffer error: %v", err)
			return
		}

		// Отправляем ответ (SDP Answer) обратно через Redis Pub/Sub
		replyMsg := cluster.ClusterMessage{
			Type:       cluster.TypeCascadeAnswer,
			RoomID:     r.id,
			FromNodeID: localNodeID,
			SDP:        answerSDP,
			SDPType:    "answer",
		}
		replyData, _ := json.Marshal(replyMsg)
		_ = rdb.PublishPubSubMessage(r.ctx, msg.FromNodeID, replyData)

	case cluster.TypeCascadeAnswer:
		if ok {
			err := bridge.HandleAnswer(msg.SDP)
			if err != nil {
				log.Printf("[Room] HandleAnswer error: %v", err)
			}
		}

	case cluster.TypeCascadeCandidate:
		if ok {
			candidateInit := webrtc.ICECandidateInit{
				Candidate:     msg.Candidate,
				SDPMid:        &msg.SDPMid,
				SDPMLineIndex: &msg.SDPMLineIdx,
			}
			err := bridge.HandleCandidate(candidateInit)
			if err != nil {
				log.Printf("[Room] HandleCandidate error: %v", err)
			}
		}
	}
}

