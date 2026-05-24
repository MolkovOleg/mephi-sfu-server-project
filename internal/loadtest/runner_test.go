package loadtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRunnerConfigRoundRobin(t *testing.T) {
	cfg := RunnerConfig{
		Addr:         "ws://localhost:8080/ws",
		Addrs:        []string{"ws://node1/ws", "ws://node2/ws", "ws://node3/ws"},
		TotalClients: 6,
		RampPerSec:   100,
		Duration:     10 * time.Millisecond,
		Rooms:        2,
		RoomPrefix:   "test",
	}

	// Проверим правильность распределения адресов для 6 клиентов
	expectedAddrs := []string{
		"ws://node1/ws",
		"ws://node2/ws",
		"ws://node3/ws",
		"ws://node1/ws",
		"ws://node2/ws",
		"ws://node3/ws",
	}

	for n := 1; n <= cfg.TotalClients; n++ {
		targetAddr := cfg.Addr
		if len(cfg.Addrs) > 0 {
			targetAddr = cfg.Addrs[(n-1)%len(cfg.Addrs)]
		}
		assert.Equal(t, expectedAddrs[n-1], targetAddr)

		roomID := fmt.Sprintf("%s-%d", cfg.RoomPrefix, (n-1)%cfg.Rooms)
		expectedRoomID := fmt.Sprintf("test-%d", (n-1)%2)
		assert.Equal(t, expectedRoomID, roomID)
	}
}
