package sfu

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/pion/webrtc/v3"
)

// Состнояе подлючения Peer'а
type PeerState int

const (
	PeerStateNew          PeerState = iota // Создан, но не подключен
	PeerStateConnecting                    // ICE/DTLS в процессе
	PeerStateConnected                     // Медиа передается
	PeerStateDisconnected                  // Временно отключен
	PeerStateFailed                        // Соединение прервано
	PeerStateClosed                        // Соединение закрыто
)

// Возврат строкового представления состояния подключения
func (s PeerState) String() string {
	switch s {
	case PeerStateNew:
		return "new"
	case PeerStateConnecting:
		return "connecting"
	case PeerStateConnected:
		return "connected"
	case PeerStateDisconnected:
		return "disconnected"
	case PeerStateFailed:
		return "failed"
	case PeerStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Сообщение для отправки клиенту через WebSocket
type NegotiotionMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// Настройки Peer'а
type PeerConfig struct {
	ICEServers     []webrtc.ICEServer
	ReceiverConfig ReceiverConfig
	SenderConfig   SenderConfig
}

// Возврат настроек по умолчанию для Peer'а
func DefaultPeerConfig() PeerConfig {
	return PeerConfig{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
		ReceiverConfig: DefaultRecieverConfig(),
		SenderConfig:   DefaultSenderConfig(),
	}
}

// Представление подключенного клиента в SFU
type Peer struct {
	id                 string
	pc                 *webrtc.PeerConnection
	router             *Router
	state              PeerState
	rtpSenders         map[string]*webrtc.RTPSender
	onNegotiate        func(msg NegotiotionMessage)
	onClose            func(peerID string)
	mu                 sync.Mutex
	negotiationPending bool
	ctx                context.Context
	cancel             context.CancelFunc
	closeOnce          sync.Once
	config             PeerConfig
}

// Создание нового Peer'а
func NewPeer(
	ctx context.Context,
	id string,
	router *Router,
	config PeerConfig,
) (*Peer, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: config.ICEServers,
	})
	if err != nil {
		return nil, err
	}

	peerCtx, cancel := context.WithCancel(ctx)

	peer := &Peer{
		id:         id,
		pc:         pc,
		router:     router,
		state:      PeerStateNew,
		rtpSenders: make(map[string]*webrtc.RTPSender),
		ctx:        peerCtx,
		cancel:     cancel,
		config:     config,
	}

	peer.setupCallbacks()

	return peer, err
}

// --- API-методы ---

// Возвращение ID Peer'а
func (p *Peer) ID() string { return p.id }

// Возвращение состояния текущего подключения
func (p *Peer) State() PeerState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Возврат контекста Peer'а
func (p *Peer) Context() context.Context { return p.ctx }

// Установка callback onNegotiate для отправки SDP/ICE
func (p *Peer) SetOnNegotiate(fn func(NegotiotionMessage)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onNegotiate = fn
}

// Установка callback при закрытии
func (p *Peer) SetOnClose(fn func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onClose = fn
}

// Обработка SDP ответа от клиента
func (p *Peer) HandleAnswer(answer webrtc.SessionDescription) error {
	return p.pc.SetRemoteDescription(answer)
}

// Обработка ICE-кандидат от клиента
func (p *Peer) HandleCandidate(candidate webrtc.ICECandidateInit) error {
	return p.pc.AddICECandidate(candidate)
}

// Подписка данного Peer'а для получения трека
func (p *Peer) SubscribeToTrack(trackID string) error {
	p.mu.Lock()

	// Не подписываемся на свои же треки
	sender, err := p.router.Subscribe(
		trackID,
		p.id,
		p.ctx,
		p.config.SenderConfig,
	)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	if sender == nil {
		p.mu.Unlock()
		return nil
	}

	// Добавляем TrackLocal в PeerConnection
	rtpSender, err := p.pc.AddTrack(sender.Track())
	if err != nil {
		p.mu.Unlock()
		p.router.Unsubscribe(trackID, p.id)
		return err
	}

	p.rtpSenders[trackID] = rtpSender

	p.mu.Unlock()

	p.negotiate()

	log.Printf("[Peer] subscribed to track: peer=%s track=%s", p.id, trackID)
	return nil
}

// Отписка данного Peer'а от трека
func (p *Peer) UnsubscribeFromTrack(trackID string) error {
	p.mu.Lock()

	rtpSender, ok := p.rtpSenders[trackID]
	if !ok {
		p.mu.Unlock()
		return nil
	}

	// Удаляем трек из PeerConnection
	if err := p.pc.RemoveTrack(rtpSender); err != nil {
		p.mu.Unlock()
		return err
	}

	delete(p.rtpSenders, trackID)

	// Отписываемся в Router
	p.router.Unsubscribe(trackID, p.id)

	p.mu.Unlock()

	p.negotiate()

	log.Printf("[Peer] unsubscribed from track: peer=%s track=%s", p.id, trackID)
	return nil
}

// Закрываем PeerConnection и все связанные ресурсы
func (p *Peer) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()

		p.state = PeerStateClosed

		p.router.UnsubscribeAll(p.id)

		p.rtpSenders = make(map[string]*webrtc.RTPSender)

		onClose := p.onClose

		p.mu.Unlock()

		// Закрываем PeerConnection через Pion
		if err := p.pc.Close(); err != nil {
			log.Printf("[Peer] close with error: peer=%s err=%v", p.id, err)
		}

		// Отменяем контекст
		p.cancel()

		log.Printf("[Peer] closed: peer=%s", p.id)

		// Уведомляем Room
		if onClose != nil {
			onClose(p.id)
		}
	})
}

// --- Внутренняя логика ---

// Установка callbacks от WebRTC на PeerConnection
func (p *Peer) setupCallbacks() {

	// OnTrack: новый входящий трек
	p.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Printf("[Peer] new track: peer=%s track=%s stream=%s kind=%s codec=%s",
			p.id, track.ID(), track.StreamID(), track.Kind(), track.Codec().MimeType)

		// Создаем наш Receiver для этого трека
		recv := NewReciever(p.ctx, track, p.pc, p.config.ReceiverConfig)

		// Регистрируем в Router, а дальше он запустит цикл сбора пакетов
		// Также триггер для всех подписок
		p.router.AddReceiver(recv)

		// Блокировка горутины до окончания контекста
		<-p.ctx.Done()

		// Удаляем Receiver из Router при выходе
		p.router.RemoveReceiver(track.ID())
	})

	// OnICECandidate: новый ICE-кандидат
	p.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		// Сериализация кандидата в JSON
		candidateJSON, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			log.Printf("[Peer] ICE candidate marshal error: %v", err)
		}

		p.mu.Lock()
		onNeg := p.onNegotiate
		p.mu.Unlock()

		if onNeg != nil {
			onNeg(NegotiotionMessage{
				Type: "candidate",
				Data: string(candidateJSON),
			})
		}
	})

	// OnConnectionStateChange: состояние соединения
	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[Peer] connection state: peer=%s state=%s", p.id, state.String())

		p.mu.Lock()
		switch state {
		case webrtc.PeerConnectionStateConnecting:
			p.state = PeerStateConnecting
		case webrtc.PeerConnectionStateConnected:
			p.state = PeerStateConnected
		case webrtc.PeerConnectionStateDisconnected:
			p.state = PeerStateDisconnected
		case webrtc.PeerConnectionStateFailed:
			p.state = PeerStateFailed
			p.mu.Unlock()
			p.Close()
			return
		case webrtc.PeerConnectionStateClosed:
			p.state = PeerStateClosed
			p.mu.Unlock()
			p.Close()
			return
		}
		p.mu.Unlock()
	})

	// OnNegotiationNeeded: Pion сам вызывает renegotiation
	p.pc.OnNegotiationNeeded(func() {
		log.Printf("[Peer] negotiation needed: peer=%s", p.id)
		p.negotiate()
	})

}

// Создание SDP offer и отправка его клиенту
func (p *Peer) negotiate() {
	p.mu.Lock()

	if p.state == PeerStateClosed || p.state == PeerStateFailed {
		p.mu.Unlock()
		return
	}
	p.negotiationPending = true

	onNeg := p.onNegotiate
	p.mu.Unlock()

	if onNeg == nil {
		log.Printf("[Peer] negotiate skipped (no handler): peer=%s", p.id)
		p.mu.Lock()
		p.negotiationPending = false
		p.mu.Unlock()
		return
	}

	// Создаем SDP offer
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("[Peer] set local description error: peer=%s err=%v", p.id, err)
		p.mu.Lock()
		p.negotiationPending = false
		p.mu.Unlock()
		return
	}

	// Применяем offer локально
	if err := p.pc.SetLocalDescription(offer); err != nil {
		log.Printf("[Peer] set local description error: peer=%s err=%v", p.id, err)
		p.mu.Lock()
		p.negotiationPending = false
		p.mu.Unlock()
		return
	}

	// Сериализуем offer и отправляем клиенту
	offerJSON, err := json.Marshal(offer)
	if err != nil {
		log.Printf("[Peer] offer marshal error: peer=%s error=%v", p.id, err)
		p.mu.Lock()
		p.negotiationPending = false
		p.mu.Unlock()
		return
	}

	onNeg(NegotiotionMessage{
		Type: "offer",
		Data: string(offerJSON),
	})

	// Сбрасываем флаг - negotiation завершен
	p.mu.Lock()
	p.negotiationPending = false
	p.mu.Unlock()

	log.Printf("[Peer] offer sent: peer=%s", p.id)
}
