package sfu

import (
	"context"
	"errors"
	"log"
	"sync"
)

// =============================================================================
// server.go — Центральный SFU-сервер (SFUServer)
// =============================================================================

var (
	ErrRoomAlreadyExists = errors.New("room already exists")
	ErrRoomNotFound      = errors.New("room not found")
	ErrServerClosed      = errors.New("server is closed")
	ErrMaxRoomLimit      = errors.New("max rooms limit reached")
)

// Конфигурацяи сервера SFU
type ServerConfig struct {
	MaxRooms          int        // Максимальное кол-во комнат на данном сервере
	DefaultRoomConfig RoomConfig // Настройка комнаты по умолчанию
	DefaultPeerConfig PeerConfig // Настройка пиров по умолчанию
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
	rooms map[string]*Room // реестр активных комнат
	mu    sync.RWMutex     // потокобезопасность для работы с комнатами

	// для работы с контекстом
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closed    bool
	config    ServerConfig
}

// Создание нового сервера SFU
func NewSFUServer(ctx context.Context, config ServerConfig) *SFUServer {
	serverCtx, cancel := context.WithCancel(ctx)

	server := &SFUServer{
		rooms:  make(map[string]*Room),
		ctx:    serverCtx,
		cancel: cancel,
		config: config,
	}

	log.Printf("[SFUServer] created: maxRooms=%d", config.MaxRooms)

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

	// Создаем комнату
	room := NewRoom(s.ctx, roomID, config)

	// Устанавливаем callback для автоматического удаления комнаты из реестра
	// при ее закрытии (последний участник вышел или прнудительное завершение)
	room.SetOnClose(func(id string) {
		s.onRoomClosed(id)
	})

	// Регистрируем комнату
	s.rooms[roomID] = room

	s.mu.Unlock()

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

// Возврат статистики по всес комнатам сервера
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

	// Проверка наличия данной комнаты
	_, ok := s.rooms[roomID]
	if !ok {
		s.mu.Unlock()
		return
	}

	// Удаляем из реестра
	delete(s.rooms, roomID)

	s.mu.Unlock()

	log.Printf("[SFUServer] room removed from registry: room=%s rooms=%d", roomID, s.RoomsCount())
}
