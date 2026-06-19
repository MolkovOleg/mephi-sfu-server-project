package sfu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"sfu-server/internal/cluster"

	"github.com/pion/webrtc/v3"
)

// Отвечает за межсерверный WebRTC-канал для одной комнаты
type CascadeBridge struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	roomID         string
	localNodeID    string
	remoteNodeID   string
	redisClient    *cluster.RedisClient
	peerConnection *webrtc.PeerConnection
	localRouter    *Router
	senders        map[string]*Sender // trackID -> Sender
	isInitiator    bool
}

// Метод создает новый межсерверный каскадный мост
func NewCascadeBridge(
	ctx context.Context,
	roomID string,
	localNodeID string,
	remoteNodeID string,
	redisClient *cluster.RedisClient,
	localRouter *Router,
	isInitiator bool,
) (*CascadeBridge, error) {
	// Инициализируем PeerConnection для межсерверной передачи
	// Используем локальный loopback STUN или пустые ICE-серверы для приватной сети
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{},
	})
	if err != nil {
		return nil, fmt.Errorf("cascade pc creation failed: %w", err)
	}

	bridgeCtx, cancel := context.WithCancel(ctx)

	cb := &CascadeBridge{
		ctx:            bridgeCtx,
		cancel:         cancel,
		roomID:         roomID,
		localNodeID:    localNodeID,
		remoteNodeID:   remoteNodeID,
		redisClient:    redisClient,
		peerConnection: pc,
		localRouter:    localRouter,
		senders:        make(map[string]*Sender),
		isInitiator:    isInitiator,
	}

	// Настройка ICE-кандидатов
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		sdpMid := ""
		if init.SDPMid != nil {
			sdpMid = *init.SDPMid
		}
		var mlineIdx uint16
		if init.SDPMLineIndex != nil {
			mlineIdx = *init.SDPMLineIndex
		}
		msg := cluster.ClusterMessage{
			Type:        cluster.TypeCascadeCandidate,
			RoomID:      cb.roomID,
			FromNodeID:  cb.localNodeID,
			Candidate:   init.Candidate,
			SDPMid:      sdpMid,
			SDPMLineIdx: mlineIdx,
		}
		data, _ := json.Marshal(msg)
		_ = cb.redisClient.PublishPubSubMessage(cb.ctx, cb.remoteNodeID, data)
	})

	// Обработка входящих треков с другой ноды
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("[CascadeBridge] OnTrack received: room=%s remoteNode=%s track=%s kind=%s",
			cb.roomID, cb.remoteNodeID, track.ID(), track.Kind())

		// Создаем стандартный Receiver для этого трека
		// Владельцем является виртуальный пир "cascade-<remoteNodeID>"
		ownerID := "cascade-" + cb.remoteNodeID
		recv := NewReceiver(cb.ctx, track, cb.peerConnection, ownerID, DefaultReceiverConfig())

		// Добавляем в локальный роутер
		cb.localRouter.AddReceiver(recv)

		// Очистка при завершении
		go func() {
			<-cb.ctx.Done()
			cb.localRouter.RemoveReceiver(track.ID())
		}()
	})

	// OnNegotiationNeeded: автоматический перезапуск negotiate() при добавлении
	// треков пока шёл предыдущий SDP-цикл (state=have-local-offer).
	// Pion сам вызывает этот callback когда state возвращается в stable
	// и есть pending треки — тем самым гарантирует что ни один трек не потеряется.
	pc.OnNegotiationNeeded(func() {
		if !cb.isInitiator {
			return
		}
		go cb.negotiate()
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[CascadeBridge] connection state changed: room=%s remoteNode=%s state=%s",
			cb.roomID, cb.remoteNodeID, state)
	})

	return cb, nil
}

// AddTrack подписывает каскадный мост на локальный трек и отправляет его по WebRTC
func (cb *CascadeBridge) AddTrack(trackID string) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if _, exists := cb.senders[trackID]; exists {
		return nil
	}

	log.Printf("[CascadeBridge] AddTrack to bridge: room=%s remoteNode=%s track=%s",
		cb.roomID, cb.remoteNodeID, trackID)

	// Подписываемся на трек в локальном роутере
	sender, err := cb.localRouter.Subscribe(trackID, "cascade-"+cb.remoteNodeID, cb.ctx, DefaultSenderConfig())
	if err != nil {
		return err
	}
	if sender == nil {
		return fmt.Errorf("track not found in router")
	}

	cb.senders[trackID] = sender

	// Добавляем трек в PeerConnection
	_, err = cb.peerConnection.AddTrack(sender.Track())
	if err != nil {
		cb.localRouter.Unsubscribe(trackID, "cascade-"+cb.remoteNodeID)
		sender.Stop()
		delete(cb.senders, trackID)
		return err
	}

	// Запускаем пересогласование, если мы инициаторы
	if cb.isInitiator {
		go cb.negotiate()
	}

	return nil
}

// HandleOffer обрабатывает входящий SDP Offer и возвращает SDP Answer
func (cb *CascadeBridge) HandleOffer(sdp string) (string, error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	err := cb.peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	})
	if err != nil {
		return "", fmt.Errorf("set remote description: %w", err)
	}

	answer, err := cb.peerConnection.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create answer: %w", err)
	}

	err = cb.peerConnection.SetLocalDescription(answer)
	if err != nil {
		return "", fmt.Errorf("set local description: %w", err)
	}

	return answer.SDP, nil
}

// HandleAnswer обрабатывает входящий SDP Answer
func (cb *CascadeBridge) HandleAnswer(sdp string) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.peerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	})
}

// HandleCandidate добавляет удаленный ICE кандидат
func (cb *CascadeBridge) HandleCandidate(candidate webrtc.ICECandidateInit) error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.peerConnection.AddICECandidate(candidate)
}

// Close закрывает каскадный мост
func (cb *CascadeBridge) Close() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.cancel()

	if cb.peerConnection != nil {
		_ = cb.peerConnection.Close()
	}

	for trackID, sender := range cb.senders {
		cb.localRouter.Unsubscribe(trackID, "cascade-"+cb.remoteNodeID)
		sender.Stop()
	}

	log.Printf("[CascadeBridge] closed: room=%s remoteNode=%s", cb.roomID, cb.remoteNodeID)
}

// Внутренний метод согласования
func (cb *CascadeBridge) negotiate() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	offer, err := cb.peerConnection.CreateOffer(nil)
	if err != nil {
		log.Printf("[CascadeBridge] CreateOffer error: %v", err)
		return
	}

	err = cb.peerConnection.SetLocalDescription(offer)
	if err != nil {
		log.Printf("[CascadeBridge] SetLocalDescription error: %v", err)
		return
	}

	msg := cluster.ClusterMessage{
		Type:       cluster.TypeCascadeOffer,
		RoomID:     cb.roomID,
		FromNodeID: cb.localNodeID,
		SDP:        offer.SDP,
		SDPType:    offer.Type.String(),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	err = cb.redisClient.PublishPubSubMessage(cb.ctx, cb.remoteNodeID, data)
	if err != nil {
		log.Printf("[CascadeBridge] failed to publish offer: %v", err)
	}
}
