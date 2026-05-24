package sfu

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// Helper-функции для тестов
// =============================================================================

// defaultTestConfig возвращает стандартную конфигурацию для тестов
func defaultTestServerConfig() ServerConfig {
	cfg := DefaultServerConfig()
	cfg.MaxRooms = 100 // разумный лимит для тестов
	return cfg
}

// newTestSFUServer создаёт SFUServer с контекстом, отменяемым после теста
func newTestSFUServer(t *testing.T, config ServerConfig) *SFUServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewSFUServer(ctx, config)
}

// =============================================================================
// Тесты
// =============================================================================

// TestNewSFUServer — проверка применения дефолтных и кастомных настроек
func TestNewSFUServer(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		cfg := DefaultServerConfig()
		server := newTestSFUServer(t, cfg)

		if server == nil {
			t.Fatal("expected non-nil server")
		}
		if server.ctx == nil {
			t.Error("expected non-nil context")
		}
		if server.cancel == nil {
			t.Error("expected non-nil cancel func")
		}
		if server.rooms == nil {
			t.Error("expected non-nil rooms map")
		}
		if server.config.MaxRooms != 0 {
			t.Errorf("expected MaxRooms=0 for default config, got %d", server.config.MaxRooms)
		}
		if server.config.DefaultRoomConfig.MaxPeers != 0 {
			t.Errorf("expected DefaultRoomConfig.MaxPeers=0, got %d",
				server.config.DefaultRoomConfig.MaxPeers)
		}
	})

	t.Run("custom config", func(t *testing.T) {
		cfg := ServerConfig{
			MaxRooms: 50,
			DefaultRoomConfig: RoomConfig{
				MaxPeers: 10,
			},
			DefaultPeerConfig: DefaultPeerConfig(),
		}
		server := newTestSFUServer(t, cfg)

		if server.config.MaxRooms != 50 {
			t.Errorf("expected MaxRooms=50, got %d", server.config.MaxRooms)
		}
		if server.config.DefaultRoomConfig.MaxPeers != 10 {
			t.Errorf("expected DefaultRoomConfig.MaxPeers=10, got %d",
				server.config.DefaultRoomConfig.MaxPeers)
		}
	})
}

// TestCreateRoom — успешное создание комнаты и проверка счётчика
func TestCreateRoom(t *testing.T) {
	server := newTestSFUServer(t, defaultTestServerConfig())

	room, err := server.CreateRoom("room-1", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if room == nil {
		t.Fatal("expected non-nil room")
	}
	if room.ID() != "room-1" {
		t.Errorf("expected room ID 'room-1', got '%s'", room.ID())
	}
	if count := server.RoomsCount(); count != 1 {
		t.Errorf("expected RoomsCount=1, got %d", count)
	}

	// Создаём вторую комнату
	room2, err := server.CreateRoom("room-2", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if room2 == nil {
		t.Fatal("expected non-nil room2")
	}
	if count := server.RoomsCount(); count != 2 {
		t.Errorf("expected RoomsCount=2, got %d", count)
	}
}

// TestCreateRoom_Errors — проверка возврата ошибок при лимите и дубликате
func TestCreateRoom_Errors(t *testing.T) {
	t.Run("ErrRoomAlreadyExists", func(t *testing.T) {
		server := newTestSFUServer(t, defaultTestServerConfig())

		_, err := server.CreateRoom("room-1", server.config.DefaultRoomConfig)
		if err != nil {
			t.Fatalf("first CreateRoom failed: %v", err)
		}

		_, err = server.CreateRoom("room-1", server.config.DefaultRoomConfig)
		if err != ErrRoomAlreadyExists {
			t.Errorf("expected ErrRoomAlreadyExists, got %v", err)
		}
	})

	t.Run("ErrMaxRoomLimit", func(t *testing.T) {
		cfg := ServerConfig{
			MaxRooms:          2,
			DefaultRoomConfig: DefaultRoomConfig(),
			DefaultPeerConfig: DefaultPeerConfig(),
		}
		server := newTestSFUServer(t, cfg)

		// Заполняем лимит
		_, err := server.CreateRoom("room-1", server.config.DefaultRoomConfig)
		if err != nil {
			t.Fatalf("first CreateRoom failed: %v", err)
		}
		_, err = server.CreateRoom("room-2", server.config.DefaultRoomConfig)
		if err != nil {
			t.Fatalf("second CreateRoom failed: %v", err)
		}

		// Превышаем лимит
		_, err = server.CreateRoom("room-3", server.config.DefaultRoomConfig)
		if err != ErrMaxRoomLimit {
			t.Errorf("expected ErrMaxRoomLimit, got %v", err)
		}
	})

	t.Run("ErrServerClosed", func(t *testing.T) {
		server := newTestSFUServer(t, defaultTestServerConfig())
		server.Close()

		_, err := server.CreateRoom("room-1", server.config.DefaultRoomConfig)
		if err != ErrServerClosed {
			t.Errorf("expected ErrServerClosed, got %v", err)
		}
	})
}

// TestGetOrCreateRoom — проверка возврата существующей комнаты
func TestGetOrCreateRoom(t *testing.T) {
	server := newTestSFUServer(t, defaultTestServerConfig())

	// Первый вызов — создание комнаты
	room1, err := server.GetOrCreateRoom("room-1", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("first GetOrCreateRoom failed: %v", err)
	}
	if count := server.RoomsCount(); count != 1 {
		t.Errorf("expected RoomsCount=1, got %d", count)
	}

	// Второй вызов — должна вернуть ту же комнату
	room2, err := server.GetOrCreateRoom("room-1", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("second GetOrCreateRoom failed: %v", err)
	}
	if room1 != room2 {
		t.Error("expected GetOrCreateRoom to return the same room instance")
	}
	if count := server.RoomsCount(); count != 1 {
		t.Errorf("expected RoomsCount still 1, got %d", count)
	}

	// Другой roomID — новая комната
	room3, err := server.GetOrCreateRoom("room-2", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("GetOrCreateRoom for room-2 failed: %v", err)
	}
	if room1 == room3 {
		t.Error("expected different room instances for different IDs")
	}
	if count := server.RoomsCount(); count != 2 {
		t.Errorf("expected RoomsCount=2, got %d", count)
	}
}

// TestCloseRoom — проверка удаления комнаты из реестра после Close
func TestCloseRoom(t *testing.T) {
	server := newTestSFUServer(t, defaultTestServerConfig())

	_, err := server.CreateRoom("room-1", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("CreateRoom failed: %v", err)
	}
	if count := server.RoomsCount(); count != 1 {
		t.Fatalf("expected RoomsCount=1, got %d", count)
	}

	err = server.CloseRoom("room-1")
	if err != nil {
		t.Fatalf("CloseRoom failed: %v", err)
	}

	// CloseRoom вызывает Room.Close(), который триггерит onRoomClosed callback.
	// Даём небольшое время на асинхронное удаление из реестра.
	time.Sleep(50 * time.Millisecond)

	if count := server.RoomsCount(); count != 0 {
		t.Errorf("expected RoomsCount=0 after CloseRoom, got %d", count)
	}

	// Повторный CloseRoom должен вернуть ErrRoomNotFound
	err = server.CloseRoom("room-1")
	if err != ErrRoomNotFound {
		t.Errorf("expected ErrRoomNotFound, got %v", err)
	}
}

// TestServerClose — проверка graceful shutdown
func TestServerClose(t *testing.T) {
	server := newTestSFUServer(t, defaultTestServerConfig())

	// Создаём несколько комнат
	_, err := server.CreateRoom("room-1", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("CreateRoom room-1 failed: %v", err)
	}
	_, err = server.CreateRoom("room-2", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("CreateRoom room-2 failed: %v", err)
	}

	// Закрываем сервер
	server.Close()

	// Контекст должен быть отменён
	select {
	case <-server.ctx.Done():
		// ok
	default:
		t.Error("expected server context to be cancelled after Close")
	}

	// Реестр должен быть пустым
	if count := server.RoomsCount(); count != 0 {
		t.Errorf("expected RoomsCount=0 after Close, got %d", count)
	}

	// Повторный вызов Close не должен вызывать панику (sync.Once)
	server.Close()
}

// TestServerStats — проверка корректного сложения счётчиков
func TestServerStats(t *testing.T) {
	server := newTestSFUServer(t, defaultTestServerConfig())

	// Создаём 3 комнаты
	_, err := server.CreateRoom("room-1", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("CreateRoom room-1 failed: %v", err)
	}
	_, err = server.CreateRoom("room-2", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("CreateRoom room-2 failed: %v", err)
	}
	_, err = server.CreateRoom("room-3", server.config.DefaultRoomConfig)
	if err != nil {
		t.Fatalf("CreateRoom room-3 failed: %v", err)
	}

	stats := server.Stats()

	if stats.RoomsCount != 3 {
		t.Errorf("expected RoomsCount=3, got %d", stats.RoomsCount)
	}
	// На данный момент нет Peer'ов, Receivers, Senders
	if stats.TotalPeers != 0 {
		t.Errorf("expected TotalPeers=0, got %d", stats.TotalPeers)
	}
	if stats.TotalReceivers != 0 {
		t.Errorf("expected TotalReceivers=0, got %d", stats.TotalReceivers)
	}
	if stats.TotalSenders != 0 {
		t.Errorf("expected TotalSenders=0, got %d", stats.TotalSenders)
	}
}

// TestServer_Race — конкурентное CreateRoom и CloseRoom из 100 горутин
// Запускать с флагом -race: go test -race -run TestServer_Race
func TestServer_Race(t *testing.T) {
	cfg := ServerConfig{
		MaxRooms:          1000,
		DefaultRoomConfig: DefaultRoomConfig(),
		DefaultPeerConfig: DefaultPeerConfig(),
	}
	server := newTestSFUServer(t, cfg)

	const goroutines = 100
	var wg sync.WaitGroup
	var createdCount atomic.Int32
	var closedCount atomic.Int32
	var errCount atomic.Int32

	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			roomID := fmt.Sprintf("race-room-%d", idx)

			// Создаём комнату
			_, err := server.CreateRoom(roomID, server.config.DefaultRoomConfig)
			if err != nil {
				if err != ErrRoomAlreadyExists && err != ErrMaxRoomLimit {
					errCount.Add(1)
				}
				return
			}
			createdCount.Add(1)

			// Закрываем комнату
			if err := server.CloseRoom(roomID); err != nil {
				// Если комната уже удалена callback'ом, это допустимо
				if err != ErrRoomNotFound {
					errCount.Add(1)
				}
			} else {
				closedCount.Add(1)
			}
		}(i)
	}

	wg.Wait()

	// Даём время на асинхронное удаление из реестра через onRoomClosed
	time.Sleep(100 * time.Millisecond)

	t.Logf("Race test: created=%d, closed=%d, errors=%d",
		createdCount.Load(), closedCount.Load(), errCount.Load())

	if errCount.Load() > 0 {
		t.Errorf("unexpected errors during race: %d", errCount.Load())
	}

	// После закрытия всех комнат реестр должен стать пустым
	if count := server.RoomsCount(); count > 0 {
		t.Errorf("expected RoomsCount=0 after all rooms closed, got %d", count)
	}
}
