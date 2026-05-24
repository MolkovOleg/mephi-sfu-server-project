package sfu

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"sfu-server/internal/cluster"
	"sfu-server/internal/kafka"
	appmetrics "sfu-server/internal/metrics"
)

// =============================================================================
// Центральный SFU-сервер
// =============================================================================

var (
	ErrRoomAlreadyExists = errors.New("room already exists")
	ErrRoomNotFound      = errors.New("room not found")
	ErrServerClosed      = errors.New("server is closed")
	ErrMaxRoomLimit      = errors.New("max rooms limit reached")
)

// Конфигурацяи сервера SFU
type ServerConfig struct {
	MaxRooms          int                 // Максимальное кол-во комнат на данном сервере
	DefaultRoomConfig RoomConfig          // Настройка комнаты по умолчанию
	DefaultPeerConfig PeerConfig          // Настройка пиров по умолчанию
	Metrics           *appmetrics.Metrics // nil — метрики отключены
	KafkaProducer     *kafka.AsyncProducer

	ClusterEnabled         bool
	ClusterEnableCascading bool
	ClusterNode            *cluster.ClusterNode
	RoomRouter             *cluster.RoomRouter
}

// Установка конфигурации сервера SFU по умолчанию
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		MaxRooms:          0,
		DefaultRoomConfig: DefaultRoomConfig(),
		DefaultPeerConfig: DefaultPeerConfig(),
	}
}

// Центральный объект медиасервера (SFUServer)
// Управляет реестром комнат и обеспечивает общий жизненный цикл
type SFUServer struct {
	rooms map[string]*Room
	mu    sync.RWMutex

	ctx                    context.Context
	cancel                 context.CancelFunc
	closeOnce              sync.Once
	closed                 bool
	config                 ServerConfig
	metrics                *appmetrics.Metrics
	kafkaProducer          *kafka.AsyncProducer
	clusterEnabled         bool
	clusterEnableCascading bool
	clusterNode            *cluster.ClusterNode
	roomRouter             *cluster.RoomRouter
}

// Создание нового сервера SFU
func NewSFUServer(ctx context.Context, config ServerConfig) *SFUServer {
	serverCtx, cancel := context.WithCancel(ctx)

	server := &SFUServer{
		rooms:                  make(map[string]*Room),
		ctx:                    serverCtx,
		cancel:                 cancel,
		config:                 config,
		metrics:                config.Metrics,
		kafkaProducer:          config.KafkaProducer,
		clusterEnabled:         config.ClusterEnabled,
		clusterEnableCascading: config.ClusterEnableCascading,
		clusterNode:            config.ClusterNode,
		roomRouter:             config.RoomRouter,
	}

	log.Printf("[SFUServer] created: maxRooms=%d clusterEnabled=%v enableCascading=%v",
		config.MaxRooms, config.ClusterEnabled, config.ClusterEnableCascading)

	return server
}

// --- API методы ---

// Возвращение контекста сервера SFU
func (s *SFUServer) Context() context.Context { return s.ctx }

// Создание новой конференц-комнаты с заданным ID
//
// Выполняет следующие действия:
//  1. Проверяет, не закрыт ли сервер.
//  2. Проверяет лимит на кол-во комнат.
//  3. Проверяет отсутствие дубликата по roomID.
//  4. Создаёт инстанс Room с внутренним Router и привязывает к нему callback
//     автоматического удаления при закрытии (onRoomClosed).
func (s *SFUServer) CreateRoom(roomID string, config RoomConfig) (*Room, error) {
	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()
		return nil, ErrServerClosed
	}

	if s.config.MaxRooms > 0 && len(s.rooms) >= s.config.MaxRooms {
		s.mu.Unlock()
		return nil, ErrMaxRoomLimit
	}

	if _, exists := s.rooms[roomID]; exists {
		s.mu.Unlock()
		return nil, ErrRoomAlreadyExists
	}

	// Создаем комнату с метриками
	room := NewRoom(s.ctx, roomID, config, s.metrics, s.kafkaProducer)

	room.SetOnClose(func(id string) {
		s.onRoomClosed(id)
	})

	if s.clusterEnabled && s.roomRouter != nil && s.clusterNode != nil {
		// Проверяем владельца в Redis перед регистрацией
		ownerNodeID, err := s.roomRouter.GetNodeForRoom(s.ctx, roomID)
		if err == nil && ownerNodeID != s.clusterNode.NodeID() {
			// Мы выступаем в роли Guest Node
			log.Printf("[SFUServer] room %s belongs to remote node %s, creating Guest Room and initiating cascade", roomID, ownerNodeID)
			bridge, err := NewCascadeBridge(s.ctx, roomID, s.clusterNode.NodeID(), ownerNodeID, s.clusterNode.RedisClient(), room.router, true)
			if err != nil {
				log.Printf("[SFUServer] failed to create active cascade bridge to %s: %v", ownerNodeID, err)
			} else {
				room.AddCascadeBridge(ownerNodeID, bridge)
				// Запускаем переговорный процесс
				go bridge.negotiate()
			}
		} else {
			// Мы владелец комнаты, регистрируем на себя
			err := s.roomRouter.RegisterRoom(s.ctx, roomID, s.clusterNode.NodeID(), 2*time.Minute)
			if err != nil {
				log.Printf("[SFUServer] failed to register room %s in cluster: %v", roomID, err)
			}
		}
	}

	s.rooms[roomID] = room
	s.mu.Unlock()

	if s.metrics != nil {
		s.metrics.RoomCreated()
	}

	if s.kafkaProducer != nil {
		s.kafkaProducer.Emit(kafka.EventRoomCreated, roomID, "", nil)
	}

	log.Printf("[SFUServer] room created: room=%s rooms=%d", roomID, s.RoomsCount())

	return room, nil
}

// Возвращения существующей комнаты или создание новой
//
// Удобный метод для сценария, когда клиент подключается по roomID
// и комната должна быть создана автоматически при первом подключении
func (s *SFUServer) GetOrCreateRoom(roomID string, config RoomConfig) (*Room, error) {
	s.mu.RLock()
	if room, ok := s.rooms[roomID]; ok {
		s.mu.RUnlock()
		return room, nil
	}
	s.mu.RUnlock()

	// Комнаты нет, создаем новую
	return s.CreateRoom(roomID, config)
}

// Закрытие комнаты по ID
// При вызове данной функции каскадно отключаются все пиры
// останавливает Router и вызывает onRoomClosed callback
func (s *SFUServer) CloseRoom(roomID string) error {
	s.mu.RLock()

	room, ok := s.rooms[roomID]
	if !ok {
		s.mu.RUnlock()
		return ErrRoomNotFound
	}

	s.mu.RUnlock()

	// Закрываем комнату ВНЕ блокировка
	room.Close()

	log.Printf("[SFUServer] room closed: room=%s", roomID)

	return nil
}

// Получение списка всех актинвных комнат
func (s *SFUServer) Rooms() []*Room {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rooms := make([]*Room, 0, len(s.rooms))
	for _, room := range s.rooms {
		rooms = append(rooms, room)
	}

	return rooms
}

// Возврат кол-ва актвиных комнат
func (s *SFUServer) RoomsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.rooms)
}

// Завершение работы сервера SFU и закрытие всех комнат
//
// Порядок завершения:
//  1. Защита от повторного вызова (sync.Once).
//  2. Копирование списка комнат и очистка реестра (под Lock).
//  3. Закрытие каждой комнаты ВНЕ блокировки (deadlock-free pattern).
//  4. Отмена корневого контекста (каскадный сигнал завершения).
func (s *SFUServer) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()

		// Помечаем сервер закрытым
		s.closed = true

		// Очищаем все комнаты и реестр
		rooms := s.rooms
		s.rooms = make(map[string]*Room)

		s.mu.Unlock()

		// Закрываем все комнаты ВНЕ блокировки каскадно
		for id, room := range rooms {
			room.Close()
			log.Printf("[SFUServer] room closed (server close): room=%s", id)
		}

		// Отменяем корневой контекст - финальный сигнал для всех
		s.cancel()

		log.Printf("[SFUServer] closed: rooms closed count=%d", len(rooms))
	})
}

// Возврат статистики по всем комнатам сервера
func (s *SFUServer) Stats() ServerStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := ServerStats{
		RoomsCount: len(s.rooms),
	}

	// Агрегируем метрики по всем комнатам
	for _, room := range s.rooms {
		roomStats := room.Stats()
		stats.TotalPeers += roomStats.PeerCount
		stats.TotalReceivers += roomStats.ReceiverCount
		stats.TotalSenders += roomStats.SenderCount
	}

	return stats
}

// Агрегировнная статистика сервера SFU
type ServerStats struct {
	RoomsCount     int // Кол-во активных комнат
	TotalPeers     int // Суммарное кол-во подключенных участников
	TotalReceivers int // Суммарное кол-во входящих треков
	TotalSenders   int // Суммарное кол-во исходящих подписок
}

// --- Внутренняя логика ---

// callback, вызываемый при закрытии комнаты.
// Автоматически удаляет комнату из реестра сервре SFU
//
// Может быть вызван в двух сценариях:
//  1. Автоматическое закрытие — последний Peer покинул комнату.
//  2. Принудительное закрытие — вызов CloseRoom() или Close() на сервере.
func (s *SFUServer) onRoomClosed(roomID string) {
	s.mu.Lock()

	_, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return
	}

	delete(s.rooms, roomID)
	s.mu.Unlock()

	if s.clusterEnabled && s.roomRouter != nil {
		// Разрегистрируем комнату в Redis
		err := s.roomRouter.UnregisterRoom(s.ctx, roomID)
		if err != nil {
			log.Printf("[SFUServer] failed to unregister room %s from cluster: %v", roomID, err)
		}
	}

	if s.metrics != nil {
		s.metrics.RoomClosed()
	}

	if s.kafkaProducer != nil {
		s.kafkaProducer.Emit(kafka.EventRoomClosed, roomID, "", nil)
	}

	log.Printf("[SFUServer] room removed from registry: room=%s rooms=%d", roomID, s.RoomsCount())
}

// Метод сообщает, включена ли кластеризация на сервере
func (s *SFUServer) ClusterEnabled() bool {
	return s.clusterEnabled
}

// ClusterEnableCascading сообщает, включено ли каскадирование медиа-потоков
func (s *SFUServer) ClusterEnableCascading() bool {
	return s.clusterEnableCascading
}

// Метод ищет комнату в кластере и возвращает ее адрес перенаправления,
// флаг isLocal (находится ли комната на текущей ноде) и ошибку
func (s *SFUServer) LookupRoomNode(roomID string) (addr string, isLocal bool, err error) {
	if !s.clusterEnabled || s.roomRouter == nil || s.clusterNode == nil {
		return "", true, nil
	}

	nodeID, err := s.roomRouter.GetNodeForRoom(s.ctx, roomID)
	if err != nil {
		if errors.Is(err, cluster.ErrRoomMappingNotFound) {
			// Комнаты еще нет в реестре, значит, она будет создана локально
			return "", true, nil
		}
		return "", false, err
	}

	if nodeID == s.clusterNode.NodeID() {
		// Комната на текущей ноде
		return "", true, nil
	}

	// Комната на другой ноде! Запрашиваем статус всех нод из Redis
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()

	nodes, err := cluster.GetAllNodes(ctx, s.clusterNode.RedisClient())
	if err != nil {
		return "", false, err
	}

	for _, node := range nodes {
		if node.NodeID == nodeID {
			return node.Addr, false, nil
		}
	}

	// Если нода не найдена (например, упала и ключ сгнил), разрешаем создать локально
	log.Printf("[SFUServer] room %s registered on expired node %s, treating as local", roomID, nodeID)
	return "", true, nil
}

// HandleClusterMessage распределяет входящие межсерверные сообщения по комнатам
func (s *SFUServer) HandleClusterMessage(msg cluster.ClusterMessage) {
	s.mu.RLock()
	room, ok := s.rooms[msg.RoomID]
	s.mu.RUnlock()

	if !ok {
		log.Printf("[SFUServer] HandleClusterMessage: room %s not found locally", msg.RoomID)
		return
	}

	room.HandleClusterMessage(msg, s.clusterNode.NodeID(), s.clusterNode.RedisClient())
}
