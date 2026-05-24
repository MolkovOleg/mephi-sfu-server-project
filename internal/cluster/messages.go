package cluster

// Представляет типы сообщений для межсерверного взаимодействия
type ClusterMessageType string

const (
	TypeCascadeOffer     ClusterMessageType = "cascade_offer"
	TypeCascadeAnswer    ClusterMessageType = "cascade_answer"
	TypeCascadeCandidate ClusterMessageType = "cascade_candidate"
)

// Представляет универсальный конверт для межсерверного обмена SDP и ICE
type ClusterMessage struct {
	Type        ClusterMessageType `json:"type"`
	RoomID      string             `json:"room_id"`
	FromNodeID  string             `json:"from_node_id"`
	SDP         string             `json:"sdp,omitempty"`
	SDPType     string             `json:"sdp_type,omitempty"`
	Candidate   string             `json:"candidate,omitempty"`
	SDPMid      string             `json:"sdp_mid,omitempty"`
	SDPMLineIdx uint16             `json:"sdp_mline_idx,omitempty"`
}
