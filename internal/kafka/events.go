package kafka

import (
	"encoding/json"
)

// EventType представляет типы событий происходящих на медиа-сервере
type EventType string

const (
	EventRoomCreated     EventType = "room_created"
	EventRoomClosed      EventType = "room_closed"
	EventPeerJoined      EventType = "peer_joined"
	EventPeerLeft        EventType = "peer_left"
	EventTrackPublished  EventType = "track_published"
	EventTrackUnpublished EventType = "track_unpublished"
	EventStatsReport     EventType = "stats_report"
)

// SFUEvent представляет универсальный конверт аналитического события
type SFUEvent struct {
	Type      EventType       `json:"type"`
	RoomID    string          `json:"room_id"`
	PeerID    string          `json:"peer_id,omitempty"`
	NodeID    string          `json:"node_id"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}
