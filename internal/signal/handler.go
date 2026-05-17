package signal

// =============================================================================
// Сессия WebSocket-соединения (Session)
//
// Session управляет жизненным циклом одного подключённого клиента:
//   - горутина чтения (readLoop) — получает и диспетчирует входящие сообщения
//   - горутина записи (writeLoop) — отправляет сообщения клиенту через канал
//   - привязка к sfu.Peer после получения сообщения TypeJoin
//   - корректная очистка ресурсов при разрыве соединения
// =============================================================================

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"

	"sfu-server/internal/sfu"
)

// Параметры WebSocet-соединения
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 16 * 1024 // 16 кб
	sendBufSize    = 64
)

// Session представляет одно соединение WebSocket с клиентом
// После получения сообщения TypeJoin сессия привязывается к sfu.Peer
// и начинает выступать в роли посредника между WebScoket и WebRTC
type Session struct {
	conn   *websocket.Conn
	server *sfu.SFUServer

	// Состояние сессии
	room *sfu.Room
	peer *sfu.Peer
	mu   sync.Mutex

	// Канал исходящих сообщений
	send chan []byte

	// Жизненный цикл
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

// Метод создания новой сессии для входящего WebSocket-соединения
func NewSession(ctx context.Context, conn *websocket.Conn, server *sfu.SFUServer) *Session {
	sessionCtx, cancel := context.WithCancel(ctx)

	return &Session{
		conn:   conn,
		server: server,
		send:   make(chan []byte, sendBufSize),
		ctx:    sessionCtx,
		cancel: cancel,
	}
}

// Метод запуска горутин чтения и записи для данной сессии
func (s *Session) Run() {
	defer s.close()

	go s.writeLoop()
	s.readLoop()
}

// Метод завершения сессии: отписывает пира, закрывает WebSocket-соединение
// и канал отправки
func (s *Session) close() {
	s.closeOnce.Do(func() {
		s.cancel()

		s.mu.Lock()
		room := s.room
		peer := s.peer
		s.mu.Unlock()

		// Уведомляем комнату об уходе участника
		if room != nil && peer != nil {
			room.Leave(peer.ID())
			log.Printf("[Session] peer left on close: peer=%s room=%s", peer.ID(), room.ID())
		}

		// Закрываем WebSocket-соединение
		_ = s.conn.Close()
		log.Printf("[Session] closed: addr=%s", s.conn.RemoteAddr())
	})
}

// Горутина чтения сообщений от клиента и диспетчеризует их по типу
func (s *Session) readLoop() {
	defer func() {
		log.Printf("[Session] readLoop stopped: addr=%s", s.conn.RemoteAddr())
	}()

	s.conn.SetReadLimit(maxMessageSize)

	// Настройка Pong-обработчика
	if err := s.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return
	}
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
				websocket.CloseNoStatusReceived,
			) {
				log.Printf("[Session] read error: addr=%s err=%v", s.conn.RemoteAddr(), err)
			}
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[Session] invalid JSON: addr=%s err=%v", s.conn.RemoteAddr(), err)
			s.sendError("Invalid JSON format", ErrCodeInvalidPayload)
			continue
		}

		s.dispatch(msg)
	}
}

// Горутина чтения готовых сообщений из канала и отправки их клиенту
// Также отправляет Ping-кадры для определения состояния подлкюяения
func (s *Session) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		log.Printf("[Session] writeLoop stopped: addr=%s", s.conn.RemoteAddr())
	}()

	for {
		select {
		case <-s.ctx.Done():
			_ = s.conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "server shutting down"),
				time.Now().Add(writeWait),
			)
			return

		case data, ok := <-s.send:
			if !ok {
				return
			}
			if err := s.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				log.Printf("[Session] write error: addr=%s err=%v", s.conn.RemoteAddr(), err)
				return
			}

		case <-ticker.C:
			if err := s.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}


// Метод направления входящего сообщения в соответствующий обработчик
func (s *Session) dispatch(msg Message) {
	switch msg.Type {
	case TypeJoin:
		s.handleJoin(msg.Payload)
	case TypeAnswer:
		s.handleAnswer(msg.Payload)
	case TypeCandidate:
		s.handleCandidate(msg.Payload)
	case TypeLeave:
		s.handleLeave()
	default:
		log.Printf("[Session] unknown message type: %q", msg.Type)
		s.sendError("unknown message type", ErrCodeInvalidPayload)
	}
}
// =============================================================================
// Обработчики входящих сообщений
// =============================================================================

// Метод обработки сообщений TypeJoin
func (s *Session) handleJoin(raw json.RawMessage) {
	var payload JoinPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.sendError("invalid join payload", ErrCodeInvalidPayload)
		return
	}

	if payload.RoomID == "" || payload.PeerID == "" {
		s.sendError("roomID and peerID are required", ErrCodeMissingFields)
		return
	}

	// Получаем или создаем комнату
	room, err := s.server.GetOrCreateRoom(payload.RoomID, sfu.DefaultRoomConfig())
	if err != nil {
		log.Printf("[Session] GetOrCreateRoom error: room=%s err=%v", payload.RoomID, err)
	}

	// Добавляем пира в комнату
	peer, err := room.Join(payload.PeerID, sfu.DefaultPeerConfig())
	if err != nil {
		log.Printf("[Session] join error: room=%s peer=%s err=%v", payload.RoomID, payload.PeerID, err)
		switch err {
		case sfu.ErrRoomFull:
			s.sendError("room is full", ErrCodeRoomFull)
		case sfu.ErrPeerAlreadyJoined:
			s.sendError("peer already joined", ErrCodePeerAlreadyJoined)
		default:
			s.sendError(err.Error(), ErrCodeInvalidPayload)
		}
		return
	}

	// Сохраняем ссылки на комнату и пира
	s.mu.Lock()
	s.room = room
	s.peer = peer
	s.mu.Unlock()

	// Устанавливем callback: SFU -> WebSocket
	// Когда SFU-сервер создает Offer или ICE-кандидат - передаем клиенту
	peer.SetOnNegotiate(func(negMsg sfu.NegotiationMessage) {
		s.forwardNegotiation(negMsg)
	})

	// Добавляем recvonly-транссиверы для аудио и видео.
	// Это триггерит OnNegotiationNeeded в pion → сервер отправляет клиенту
	// первый SDP Offer, в ответ на который клиент пришлёт свою камеру/микрофон.
	if err := peer.InitPublisher(); err != nil {
		log.Printf("[Session] InitPublisher error: peer=%s err=%v", peer.ID(), err)
		s.sendError("failed to init publisher", ErrCodeInvalidPayload)
		return
	}

	log.Printf("[Session] peer joined: room=%s peer=%s addr=%s",
		payload.RoomID, payload.PeerID, s.conn.RemoteAddr())
}

// Метод обработки сообщений TypeAnswer от клиента
func (s *Session) handleAnswer(raw json.RawMessage) {
	var payload AnswerPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.sendError("invalid answer payload", ErrCodeInvalidPayload)
		return
	}

	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()

	if peer == nil {
		s.sendError("peer not joined the room", ErrCodeMissingFields)
		return
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  payload.SDP,
	}

	if err := peer.HandleAnswer(answer); err != nil {
		log.Printf("[Session] HandleAnswer error: peer=%s err=%v", peer.ID(), err)
		s.sendError("failed to apply answer", ErrCodeInvalidPayload)
		return
	}

	log.Printf("[Session] answer applied: peer=%s", peer.ID())
}

// Метод обработки сообщений TypeCandidate от клиента
func (s *Session) handleCandidate(raw json.RawMessage) {
	var payload CandidatePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.sendError("invalid candidate payload", ErrCodeInvalidPayload)
		return
	}

	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()

	if peer == nil {
		s.sendError("peer not joined the room", ErrCodeMissingFields)
		return
	}

	sdpMLineIndex := payload.SDPMLineIndex
	candidateInit := webrtc.ICECandidateInit{
		Candidate:        payload.Candidate,
		SDPMid:           &payload.SDPMid,
		SDPMLineIndex:    &sdpMLineIndex,
		UsernameFragment: &payload.UsernameFragment,
	}

	if err := peer.HandleCandidate(candidateInit); err != nil {
		log.Printf("[Session] HandleCandidate error: peer=%s err=%v", peer.ID(), err)
		// Не отправляем сообщение клиенту, так как ICE Tricling допускает гонки
	}
}

// Метод обработки сообщений TypeLeave от клиента
// Корректно отключает пиры от комнаты
func (s *Session) handleLeave() {
	s.mu.Lock()
	room := s.room
	peer := s.peer
	s.mu.Unlock()

	if room == nil || peer == nil {
		return
	}

	log.Printf("[Session] peer leaving: peer=%s room=%s", peer.ID(), room.ID())
	room.Leave(peer.ID())

	s.mu.Lock()
	s.room = nil
	s.peer = nil
	s.mu.Unlock()
} 

// Метод принятие от sfu.Peer NegotiationMessage и отправка его клиенту через WebSocket
func (s *Session) forwardNegotiation(negMsg sfu.NegotiationMessage) {
	var msg Message
	var err error

	switch negMsg.Type {
	case "offer":
		var sd webrtc.SessionDescription
		if err := json.Unmarshal([]byte(negMsg.Data), &sd); err != nil {
			log.Printf("[Session] forward offer parse error: %v", err)
			return
		}
		msg, err = NewSDPOfferMessage(sd.SDP, sd.Type.String())

	case "candidate":
		var init webrtc.ICECandidateInit
		if err := json.Unmarshal([]byte(negMsg.Data), &init); err != nil {
			log.Printf("[Session] forward candidate parse error: %v", err)
			return
		}

		sdpMid := ""
		if init.SDPMid != nil {
			sdpMid = *init.SDPMid
		}
		var mlineIndex uint16
		if init.SDPMLineIndex != nil {
			mlineIndex = *init.SDPMLineIndex
		}
		uf := ""
		if init.UsernameFragment != nil {
			uf = *init.UsernameFragment
		}
		msg, err = NewCandidateMessage(init.Candidate, sdpMid, uf, mlineIndex)

	default:
		log.Printf("[Session] unknown negotiation type: %q", negMsg.Type)
		return
	}

	if err != nil {
		log.Printf("[Session] forwardNegotiation encode error: type=%s err=%v", negMsg.Type, err)
		return
	}

	s.sendMessage(msg)
}


// Метод кодировки и загрузки в очередь отправки сообщения
func (s *Session) sendMessage(msg Message) {
	data, err := msg.Encode()
	if err != nil {
		log.Printf("[Session] encode of message error: %v", err)
		return
	}

	select {
	case s.send <- data:
	case <-s.ctx.Done():
	default:
		log.Printf("[Session] send buffer full, closing: addr=%s", s.conn.RemoteAddr())
		go s.close()
	}
}

// Метод форматирования и отправки клиенту ошибку
func (s *Session) sendError(text, code string) {
	msg, err := NewErrorMessage(text, code)
	if err != nil {
		return
	}
	s.sendMessage(msg)
}
