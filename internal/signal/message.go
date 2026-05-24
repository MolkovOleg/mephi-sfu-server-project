package signal

import "encoding/json"

// =============================================================================
// Протокол сигнализации (JSON over WebSocket)
//
// Направления:
//
//	C→S  Client → Server
//	S→C  Server → Client
//	<->  двустороннее
// =============================================================================

// Строковый тип сообщений в сигнальном протоколе
type MessageType string

const (
	TypeJoin      MessageType = "join"
	TypeOffer     MessageType = "offer"
	TypeAnswer    MessageType = "answer"
	TypeCandidate MessageType = "candidate"
	TypeLeave     MessageType = "leave"
	TypeError     MessageType = "error"
	TypeRedirect  MessageType = "redirect"
)

// Универсальный конверт сигнального протокола
// Поле payload содержит сырой JSON, который декодируется
// в конкретную структуру в зависимости от типа
type Message struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Метода сериализации сообщения при отправке клиенту
func (m Message) Encode() ([]byte, error) {
	return json.Marshal(m)
}

// Метод создания нового сообщения
func NewMessage(mt MessageType, payload any) (Message, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	return Message{Type: mt, Payload: raw}, nil
}

// Данные для входа в комнату
// Клиент отправлет их при первом подлкючении
type JoinPayload struct {
	RoomID string `json:"room_id"`
	PeerID string `json:"peer_id"`
}

// Данные отправляемые сервером клиенту
// Инициирует WebRTC-сессию
type OfferPayload struct {
	SDP     string `json:"sdp"`
	SDPType string `json:"type"`
}

// SDP ответ от клиента на запрос сервера
type AnswerPayload struct {
	SDP     string `json:"sdp"`
	SDPType string `json:"type"`
}

// Данные ICE-кандидат для установки медиа-соединения
// Обмениваются в обоих направлениях в процессе ICE Tricle
type CandidatePayload struct {
	Candidate        string `json:"candidate"`
	SDPMid           string `json:"sdpMid"`
	SDPMLineIndex    uint16 `json:"sdpMLineIndex"`
	UsernameFragment string `json:"usernameFragment"`
}

// Данные при выходе из сессии
type LeavePayload struct{}

// Данные описания ошибки отправляемые клиенту
type ErrorPayload struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// Коды ошибок
const (
	ErrCodeRoomFull          = "ROOM_FULL"
	ErrCodePeerAlreadyJoined = "PEER_ALREADY_JOINED"
	ErrCodeInvalidPayload    = "INVALID_PAYLOAD"
	ErrCodeServerClosed      = "SERVER_CLOSED"
	ErrCodeMissingFields     = "MISSING_FIELDS"
)

// Метод создания готового сообщения об ошибке
func NewErrorMessage(msg, code string) (Message, error) {
	return NewMessage(TypeError, ErrorPayload{
		Message: msg,
		Code:    code,
	})
}

// Метод создания сообщения с SDP Offer от сервера
func NewSDPOfferMessage(sdp, sdpType string) (Message, error) {
	return NewMessage(TypeOffer, OfferPayload{
		SDP:     sdp,
		SDPType: sdpType,
	})
}

// Методв создания сообщения с ICE-кандидатом
func NewCandidateMessage(candidate, sdpMid, usernameFragment string, sdpMLineIndex uint16) (Message, error) {
	return NewMessage(TypeCandidate, CandidatePayload{
		Candidate:        candidate,
		SDPMid:           sdpMid,
		SDPMLineIndex:    sdpMLineIndex,
		UsernameFragment: usernameFragment,
	})
}

// Данные перенаправления клиента на другую SFU-ноду
type RedirectPayload struct {
	Addr string `json:"addr"`
}

// Метод создания готового сообщения для перенаправления
func NewRedirectMessage(addr string) (Message, error) {
	return NewMessage(TypeRedirect, RedirectPayload{
		Addr: addr,
	})
}
