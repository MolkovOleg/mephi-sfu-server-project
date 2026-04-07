package sfu

import (
	"context"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

// =============================================================================
// Приемник входящего медия-трафика (Receiver)
// =============================================================================

// Настройка Receiver
type ReceiverConfig struct {
	// Интервал автоматической отправки PLI-запросов
	PLIInterval time.Duration
}

// Установка настроек по умолчанию
func DefaultReceiverConfig() ReceiverConfig {
	return ReceiverConfig{
		PLIInterval: 2 * time.Second,
	}
}

// Тип callback-функции, вызываемой при получении RTP-пакета
type OnPacketFunc func(buf *[]byte, n int)

// Receiver — приемник входящего WebRTC медиа-трека
type Receiver struct {
	trackID         string
	streamID        string
	trackKind       webrtc.RTPCodecType
	ownerPeerID     string
	codec           webrtc.RTPCodecParameters
	track           *webrtc.TrackRemote
	peerConnection  *webrtc.PeerConnection
	onPacket        OnPacketFunc
	onPacketMu      sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	closeOnce       sync.Once
	packetsReceived atomic.Uint64
	bytesReceived   atomic.Uint64
	config          ReceiverConfig
}

// Создание нового Receiver для входящего трека
func NewReceiver(
	ctx context.Context,
	track *webrtc.TrackRemote,
	pc *webrtc.PeerConnection,
	ownerPeerID string,
	config ReceiverConfig,
) *Receiver {
	receiverCtx, cancel := context.WithCancel(ctx)

	return &Receiver{
		trackID:        track.ID(),
		streamID:       track.StreamID(),
		trackKind:      track.Kind(),
		ownerPeerID:    ownerPeerID,
		codec:          track.Codec(),
		track:          track,
		peerConnection: pc,
		ctx:            receiverCtx,
		cancel:         cancel,
		config:         config,
	}
}

// --- API методы ---

// Запуск горутин чтения RTP-пакетов и отправки PLI-запросов
func (r *Receiver) Start() {
	go r.readLoop()
	go r.pliLoop()
}

// Остановка всех горутин у Receiver (безопасен для повторного вызова sync.Once)
func (r *Receiver) Stop() {
	r.closeOnce.Do(func() {
		r.cancel()
	})
}

// Установка callback для получения RTP-пакетов
func (r *Receiver) SetOnPacket(fn OnPacketFunc) {
	r.onPacketMu.Lock()
	defer r.onPacketMu.Unlock()
	r.onPacket = fn
}

// Возвращает ID трека
func (r *Receiver) TrackID() string { return r.trackID }

// Возвращает ID потока
func (r *Receiver) StreamID() string { return r.streamID }

// Возвращает ID владельца трека (Peer, который публикует)
func (r *Receiver) OwnerPeerID() string { return r.ownerPeerID }

// Возвращает тип трека (аудио/видео)
func (r *Receiver) Kind() webrtc.RTPCodecType { return r.trackKind }

// Возвращает параметры кодека
func (r *Receiver) Codec() webrtc.RTPCodecParameters { return r.codec }

// Возвращает статистику приема
func (r *Receiver) Stats() ReceiverStats {
	return ReceiverStats{
		PacketsReceived: r.packetsReceived.Load(),
		BytesReceived:   r.bytesReceived.Load(),
	}
}

// Статистика приема по Receiver
type ReceiverStats struct {
	PacketsReceived uint64
	BytesReceived   uint64
}

// --- Внутренняя логика ---

// Основной цикл чтения RTP-пакетов
func (r *Receiver) readLoop() {
	defer log.Printf("[Receiver] readLoop stopped: track=%s stream=%s kind=%s",
		r.trackID, r.streamID, r.trackKind)

	// Проверка контекста Receiver
	for {
		select {
		case <-r.ctx.Done():
			return
		default:
		}

		buf := GetRTPBuffer()

		n, _, err := r.track.Read(*buf)

		if err != nil {
			PutRTPBuffer(buf)

			if err == io.EOF {
				log.Printf("[Receiver] track closed (EOF): track=%s", r.trackID)
				return
			}

			if r.ctx.Err() != nil {
				return
			}

			log.Printf("[Receiver] read error: track=%s err=%s", r.trackID, err)
			return
		}

		// Обновляем данные для статистики
		r.packetsReceived.Add(1)
		r.bytesReceived.Add(uint64(n))

		// Вызываем callback для передачи пакета в Router
		r.onPacketMu.RLock()
		onPacket := r.onPacket
		r.onPacketMu.RUnlock()

		if onPacket != nil {
			onPacket(buf, n)
		}

		PutRTPBuffer(buf)
	}
}

// Цикл переодической отправки PLI-запросов
func (r *Receiver) pliLoop() {
	// PLI-запросы нужны только для видео
	if r.Kind() != webrtc.RTPCodecTypeVideo {
		return
	}

	ticker := time.NewTicker(r.config.PLIInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.RequestKeyFrame()
		}
	}
}

// Функция отправки PLI-запроса
func (r *Receiver) RequestKeyFrame() {
	err := r.peerConnection.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{
			MediaSSRC: uint32(r.track.SSRC()),
		},
	})

	if err != nil {
		log.Printf("[Receiver] PLI send error: track=%s err=%v", r.trackID, err)
	}
}
