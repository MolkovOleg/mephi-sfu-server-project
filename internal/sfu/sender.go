package sfu

import (
	"context"
	"log"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v3"
)

// =============================================================================
// Отправитель исходящего медия-трафика (Sender)
// =============================================================================

// Настройка Sender
type SenderConfig struct {
	// Размер буферного канала для пакетов
	// Оптимальный размер 50-200 для видео и 20-50 для аудио
	WriteChannelSize int
}

// Установка настроек по умолчанию
func DefaultSenderConfig() SenderConfig {
	return SenderConfig{
		WriteChannelSize: 100,
	}
}

// Пакет передачи от Router к Sender
type senderPacket struct {
	data []byte
	n    int
}

// Пул для пакетов отправителя
var senderPacketPool = sync.Pool{
	New: func() interface{} {
		return &senderPacket{
			data: make([]byte, maxRTPPacketSize),
		}
	},
}

// Отправитель исходящего WebRTC медиа-трека
type Sender struct {
	id             string
	peerID         string
	track          *webrtc.TrackLocalStaticRTP
	ch             chan *senderPacket
	ctx            context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	packetsSent    atomic.Uint64
	bytesSent      atomic.Uint64
	packetsDropped atomic.Uint64
	config         SenderConfig
}

// Создание нового Sender для исходящего трека
func NewSender(
	ctx context.Context,
	peerID string,
	codec webrtc.RTPCodecCapability,
	trackID string,
	streamID string,
	config SenderConfig,
) (*Sender, error) {
	track, err := webrtc.NewTrackLocalStaticRTP(codec, trackID, streamID)
	if err != nil {
		return nil, err
	}

	senderCtx, cancel := context.WithCancel(ctx)

	return &Sender{
		id:     trackID,
		peerID: peerID,
		track:  track,
		ch:     make(chan *senderPacket, config.WriteChannelSize),
		ctx:    senderCtx,
		cancel: cancel,
		config: config,
	}, nil
}

// --- API методы ---

// Запуск горутины записи пакетов в трек
func (s *Sender) Start() {
	go s.writeLoop()
}

// Остановка Sender
func (s *Sender) Stop() {
	s.closeOnce.Do(func() {
		s.cancel()
	})
}

// Отправка RTP-пакета подписчику
func (s *Sender) WriteRTP(buf *[]byte, n int) {
	pkt := senderPacketPool.Get().(*senderPacket)

	// Копируем данные из буфера, так как после callback буфер будет возвращен обратно в пул
	pkt.n = copy(pkt.data, (*buf)[:n])

	// Неблокирующий паттерн горутины
	select {
	// Пакет успешно добавлен в очередь
	case s.ch <- pkt:
	default:
		// Канал переполнен, отбрасываем пакеты и возвращаем струтуру в пул
		s.packetsDropped.Add(1)
		senderPacketPool.Put(pkt)
	}
}

// Возвращает исходящий WebRTC-трек
func (s *Sender) Track() *webrtc.TrackLocalStaticRTP { return s.track }

func (s *Sender) ID() string { return s.id }

func (s *Sender) PeerID() string { return s.peerID }

func (s *Sender) Stats() SenderStats {
	return SenderStats{
		PacketsSent:    s.packetsSent.Load(),
		BytesSent:      s.bytesSent.Load(),
		PacketsDropped: s.packetsDropped.Load(),
	}
}

// Статистика отправки пакетов (Sender)
type SenderStats struct {
	PacketsSent    uint64
	BytesSent      uint64
	PacketsDropped uint64
}

// --- Внутренняя логика ---

// Основной цикл записи RTP-пакетов в WebRTC-трек
func (s *Sender) writeLoop() {
	defer log.Printf("[Sender] writeLoop stopped: tarck=%s peer=%s", s.id, s.peerID)

	for {
		select {
		case <-s.ctx.Done():
			s.drainChannel()
			return
		case pkt := <-s.ch:
			_, err := s.track.Write(pkt.data[:pkt.n])
			senderPacketPool.Put(pkt)

			if err != nil {
				select {
				case <-s.ctx.Done():
					return
				default:
				}
				log.Printf("[Sender] write error: track=%s peer=%s err=%v",
					s.id, s.peerID, err)
				continue
			}

			// Обновляем статистику
			s.packetsSent.Add(1)
			s.bytesSent.Add(uint64(pkt.n))
		}
	}
}

// Очистка от лишних пакетов и возврат senderPacket в пул
func (s *Sender) drainChannel() {
	for {
		select {
		case pkt := <-s.ch:
			senderPacketPool.Put(pkt)
		default:
			return
		}
	}
}
