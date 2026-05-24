package sfu

import (
	"context"
	"testing"

	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Тесты Sender
// =============================================================================

func newTestSender(t *testing.T, ctx context.Context, peerID, trackID string, config SenderConfig) *Sender {
	t.Helper()
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}
	sender, err := NewSender(ctx, peerID, codec, trackID, "stream-1", config)
	require.NoError(t, err)
	require.NotNil(t, sender)
	return sender
}

// TestSender_Backpressure — проверка отбрасывания пакетов при переполнении
func TestSender_Backpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	config := SenderConfig{WriteChannelSize: 5}
	sender := newTestSender(t, ctx, "peer-1", "bp-track", config)

	// НЕ запускаем Start() — никто не читает из канала
	const totalPackets = 10
	for i := 0; i < totalPackets; i++ {
		buf := make([]byte, 100)
		sender.WriteRTP(&buf, 100)
	}

	stats := sender.Stats()
	assert.Equal(t, uint64(0), stats.PacketsSent)
	assert.Equal(t, uint64(5), stats.PacketsDropped, "5 packets should be dropped")
}

// TestSender_Start_Stop
func TestSender_Start_Stop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sender := newTestSender(t, ctx, "peer-1", "start-stop-track", DefaultSenderConfig())
	sender.Start()
	sender.Stop()

	assert.NotPanics(t, func() {
		sender.Stop()
		sender.Stop()
	})
}

// TestSender_Accessors
func TestSender_Accessors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sender := newTestSender(t, ctx, "test-peer", "test-track", DefaultSenderConfig())
	assert.Equal(t, "test-track", sender.ID())
	assert.Equal(t, "test-peer", sender.PeerID())
	assert.NotNil(t, sender.Track())
}

// TestSender_Stats_Initial
func TestSender_Stats_Initial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sender := newTestSender(t, ctx, "peer-1", "stats-track", DefaultSenderConfig())
	stats := sender.Stats()
	assert.Equal(t, uint64(0), stats.PacketsSent)
	assert.Equal(t, uint64(0), stats.BytesSent)
	assert.Equal(t, uint64(0), stats.PacketsDropped)
}

// =============================================================================
// Тесты Receiver — API-тесты (без loopback, т.к. OnTrack требует медиа-потока)
// =============================================================================

// TestReceiver_Stop_WithoutStart — Stop без Start безопасен
func TestReceiver_Stop_WithoutStart(t *testing.T) {
	// Для создания Receiver нужен реальный TrackRemote,
	// который недоступен без loopback с медиа-потоком.
	// Этот тест пропускаем — он покрыт в интеграционных тестах.
	t.Skip("requires real TrackRemote from loopback with media packets")
}

// TestReceiver_PLI_RequiresPeerConnection — PLI требует PC
func TestReceiver_PLI_RequiresPeerConnection(t *testing.T) {
	t.Skip("requires real TrackRemote and PeerConnection from loopback")
}
