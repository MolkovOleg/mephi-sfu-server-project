package sfu

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Helper-функции для тестов Room
// =============================================================================

func defaultTestRoomConfig() RoomConfig {
	return RoomConfig{MaxPeers: 0}
}

func newTestRoom(t *testing.T, config RoomConfig) *Room {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewRoom(ctx, "test-room", config, nil, nil)
}

func defaultTestPeerConfig() PeerConfig {
	return DefaultPeerConfig()
}

func joinPeer(t *testing.T, room *Room, peerID string) *Peer {
	t.Helper()
	peer, err := room.Join(peerID, defaultTestPeerConfig())
	require.NoError(t, err, "failed to join peer '%s'", peerID)
	require.NotNil(t, peer)
	return peer
}

// =============================================================================
// Тесты
// =============================================================================

// TestRoomJoin — успешное добавление пира, проверка PeerCount()
func TestRoomJoin(t *testing.T) {
	room := newTestRoom(t, defaultTestRoomConfig())

	assert.Equal(t, 0, room.PeerCount(), "initial peer count should be 0")

	peer1 := joinPeer(t, room, "peer-1")
	assert.Equal(t, "peer-1", peer1.ID())
	assert.Equal(t, PeerStateNew, peer1.State())
	assert.Equal(t, 1, room.PeerCount())

	joinPeer(t, room, "peer-2")
	assert.Equal(t, 2, room.PeerCount())

	// GetPeer
	retrieved := room.GetPeer("peer-1")
	require.NotNil(t, retrieved)
	assert.Equal(t, "peer-1", retrieved.ID())

	// Peers()
	allPeers := room.Peers()
	assert.Len(t, allPeers, 2)

	// Non-existent
	assert.Nil(t, room.GetPeer("non-existent"))

	// Cleanup
	room.Close()
}

// TestRoomJoin_LimitsAndDuplicates — table-driven тест лимитов и дубликатов
func TestRoomJoin_LimitsAndDuplicates(t *testing.T) {
	tests := []struct {
		name        string
		maxPeers    int
		peersToAdd  []string
		expectError error
	}{
		{
			name:        "ErrRoomFull when MaxPeers reached",
			maxPeers:    2,
			peersToAdd:  []string{"peer-1", "peer-2", "peer-3"},
			expectError: ErrRoomFull,
		},
		{
			name:        "ErrPeerAlreadyJoined on duplicate",
			maxPeers:    0,
			peersToAdd:  []string{"peer-1", "peer-1"},
			expectError: ErrPeerAlreadyJoined,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := RoomConfig{MaxPeers: tt.maxPeers}
			room := newTestRoom(t, config)

			for i, peerID := range tt.peersToAdd {
				_, err := room.Join(peerID, defaultTestPeerConfig())
				if i == len(tt.peersToAdd)-1 && tt.expectError != nil {
					assert.ErrorIs(t, err, tt.expectError)
				} else {
					assert.NoError(t, err)
				}
			}

			room.Close()
		})
	}
}

// TestRoomLeave — проверка удаления пира из мапы
func TestRoomLeave(t *testing.T) {
	room := newTestRoom(t, defaultTestRoomConfig())

	joinPeer(t, room, "peer-1")
	joinPeer(t, room, "peer-2")

	require.Equal(t, 2, room.PeerCount())

	room.Leave("peer-1")

	assert.Equal(t, 1, room.PeerCount())
	assert.Nil(t, room.GetPeer("peer-1"))
	require.NotNil(t, room.GetPeer("peer-2"))

	// Leave несуществующего — не паникует
	assert.NotPanics(t, func() {
		room.Leave("non-existent")
	})

	room.Close()
}

// TestRoomLeave_AutoClose — если комната очищается до 0, срабатывает Close() и onClose()
func TestRoomLeave_AutoClose(t *testing.T) {
	room := newTestRoom(t, defaultTestRoomConfig())

	var onCloseCalled atomic.Bool
	var onCloseRoomID string
	var onCloseMu sync.Mutex

	room.SetOnClose(func(roomID string) {
		onCloseCalled.Store(true)
		onCloseMu.Lock()
		onCloseRoomID = roomID
		onCloseMu.Unlock()
	})

	peer1 := joinPeer(t, room, "peer-1")
	joinPeer(t, room, "peer-2")

	require.Equal(t, 2, room.PeerCount())

	// Первый уход — комната жива
	room.Leave("peer-1")
	assert.False(t, onCloseCalled.Load(), "onClose should NOT fire while peers remain")
	assert.Equal(t, 1, room.PeerCount())

	// Второй уход — комната пуста → Close()
	room.Leave("peer-2")

	// Ждём асинхронное закрытие
	time.Sleep(100 * time.Millisecond)

	assert.True(t, onCloseCalled.Load(), "onClose should fire when room becomes empty")

	onCloseMu.Lock()
	gotID := onCloseRoomID
	onCloseMu.Unlock()
	assert.Equal(t, "test-room", gotID)

	// Контекст комнаты отменён
	select {
	case <-room.Context().Done():
		// ok
	default:
		t.Error("expected room context to be cancelled after auto-close")
	}

	assert.Equal(t, PeerStateClosed, peer1.State())
}

// TestRoom_Concurrency — множественный конкурентный Join и Leave
func TestRoom_Concurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	room := NewRoom(ctx, "concurrent-room", RoomConfig{MaxPeers: 50}, nil, nil)
	t.Cleanup(func() { room.Close() })

	const goroutines = 50
	var wg sync.WaitGroup
	var joinErrors atomic.Int32

	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			peerID := fmt.Sprintf("concurrent-peer-%d", idx)

			_, err := room.Join(peerID, defaultTestPeerConfig())
			if err != nil {
				if err != ErrRoomFull && err != ErrPeerAlreadyJoined {
					joinErrors.Add(1)
					t.Logf("unexpected join error for %s: %v", peerID, err)
				}
				return
			}

			time.Sleep(time.Duration(idx%10) * time.Millisecond)
			room.Leave(peerID)
		}(i)
	}

	// Ждём завершения с таймаутом
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-ctx.Done():
		t.Fatal("test timed out — possible deadlock")
	}

	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, int32(0), joinErrors.Load(), "no unexpected join errors expected")
}

// TestRoom_Stats — проверка корректности Room.Stats()
func TestRoom_Stats(t *testing.T) {
	room := newTestRoom(t, defaultTestRoomConfig())

	stats := room.Stats()
	assert.Equal(t, 0, stats.PeerCount)
	assert.Equal(t, 0, stats.ReceiverCount)
	assert.Equal(t, 0, stats.SenderCount)

	joinPeer(t, room, "peer-1")

	stats = room.Stats()
	assert.Equal(t, 1, stats.PeerCount)

	room.Close()
}

// TestRoom_Close_Idempotent — повторный Close не паникует
func TestRoom_Close_Idempotent(t *testing.T) {
	room := newTestRoom(t, defaultTestRoomConfig())
	joinPeer(t, room, "peer-1")

	assert.NotPanics(t, func() {
		room.Close()
		room.Close()
		room.Close()
	})
}
