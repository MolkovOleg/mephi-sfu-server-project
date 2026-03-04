package sfu

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v3"
)

// =============================================================================
// Приемник входящего медия-трафика (Reciever)
// =============================================================================

// Настройка Reciver
type RecieverConfig struct {
	// Интервал автоматической отправки PLI-запросов
	PLIInterav time.Duration
}

// Установка настроек по умолчанию
func DefaultRecieverConfig() RecieverConfig {
	return RecieverConfig{
		PLIInterav: 2 * time.Second,
	}
}

// Тип callback-функции, вызываемой при получении RTP-пакета
type OnPAcketFunc func(buf *[]byte, n int)

type Reciver struct {
	trackID         string
	streamID        string
	trackType       webrtc.RTPCodecType
	codec           webrtc.RTPCodecParameters
	track           *webrtc.TrackRemote
	peerConnection  *webrtc.PeerConnection
	onPacket        OnPAcketFunc
	onPacketMu      sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	closeOnce       sync.Once
	packetsRecieved atomic.Uint64
	bytesRecieved   atomic.Uint64
	config          RecieverConfig
}
