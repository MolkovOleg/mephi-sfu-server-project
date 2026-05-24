package loadtest

// =============================================================================
// Headless WebRTC-клиент для нагрузочного теста
// =============================================================================

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

// Минимальный VP8-кейфрейм 1×1 пиксель (чёрный).
var vp8KeyFrame = []byte{
	0x10, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0x01, 0x00,
	0x01, 0x00, 0x02, 0x00, 0x34, 0x25, 0xa4, 0x00,
	0x03, 0x70, 0x00, 0xfe, 0xfb, 0x94, 0x00, 0x00,
}

// Минимальный Opus-фрейм тишины (20ms, mono).
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// ClientConfig — настройки одного headless WebRTC-клиента.
type ClientConfig struct {
	Addr     string
	RoomID   string
	PeerID   string
	Duration time.Duration
}

// ClientResult — результат одного клиентского прогона.
type ClientResult struct {
	PeerID         string
	Connected      bool
	ConnectionTime time.Duration
	Error          error
	Stage          string
}

// Внутренние типы сигнального протокола.
type signalMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type offerPayload struct {
	SDP     string `json:"sdp"`
	SDPType string `json:"type"`
}

type candidatePayload struct {
	Candidate        string `json:"candidate"`
	SDPMid           string `json:"sdpMid"`
	SDPMLineIndex    uint16 `json:"sdpMLineIndex"`
	UsernameFragment string `json:"usernameFragment"`
}

// Client — один headless WebRTC-клиент.
type Client struct {
	cfg ClientConfig
	m   *LoadTestMetrics
	mu  sync.Mutex
	ws  *websocket.Conn
}

// NewClient создаёт нового headless-клиента.
func NewClient(cfg ClientConfig, m *LoadTestMetrics) *Client {
	return &Client{cfg: cfg, m: m}
}

// Run выполняет полный жизненный цикл клиента и возвращает результат.
func (c *Client) Run(ctx context.Context) ClientResult {
	start := time.Now()
	result := ClientResult{PeerID: c.cfg.PeerID}

	if c.m != nil {
		c.m.ClientsSpawned.Inc()
	}

	if err := c.run(ctx, start, &result); err != nil {
		result.Error = err
		if c.m != nil {
			c.m.ClientsFailed.Inc()
			c.m.ErrorsTotal.WithLabelValues(result.Stage).Inc()
		}
	}

	return result
}

func (c *Client) run(ctx context.Context, start time.Time, result *ClientResult) error {
	currentAddr := c.cfg.Addr

	for redirectAttempt := 0; redirectAttempt < 5; redirectAttempt++ {
		redirected, nextAddr, err := c.runAttempt(ctx, currentAddr, start, result)
		if err != nil {
			return err
		}
		if !redirected {
			return nil
		}
		currentAddr = nextAddr
		log.Printf("[Client] redirecting attempt %d to: %s", redirectAttempt+1, currentAddr)
	}

	return fmt.Errorf("too many redirects")
}

func (c *Client) runAttempt(ctx context.Context, addr string, start time.Time, result *ClientResult) (bool, string, error) {
	// --- 1. WebSocket dial ---
	result.Stage = "dial"
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	ws, _, err := dialer.DialContext(ctx, addr, nil)
	if err != nil {
		return false, "", fmt.Errorf("ws dial: %w", err)
	}
	defer ws.Close()

	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()

	// --- 2. PeerConnection ---
	result.Stage = "peer_connection"
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{}, // Empty for local loopback scaling to avoid public STUN rate limits
	})
	if err != nil {
		return false, "", fmt.Errorf("new pc: %w", err)
	}
	defer pc.Close()

	// --- 3. Fake tracks (sendonly к серверу) ---
	result.Stage = "tracks"
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		"video", "stream-"+c.cfg.PeerID,
	)
	if err != nil {
		return false, "", fmt.Errorf("video track: %w", err)
	}
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "stream-"+c.cfg.PeerID,
	)
	if err != nil {
		return false, "", fmt.Errorf("audio track: %w", err)
	}

	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return false, "", fmt.Errorf("add video: %w", err)
	}
	audioSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		return false, "", fmt.Errorf("add audio: %w", err)
	}
	go drainRTCP(videoSender)
	go drainRTCP(audioSender)

	// --- 4. OnTrack: дренируем все входящие треки от других пиров ---
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go drainTrack(track)
	})

	// --- 5. Состояние соединения ---
	connectedCh := make(chan struct{})
	var connOnce sync.Once

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connOnce.Do(func() { close(connectedCh) })
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			connOnce.Do(func() { close(connectedCh) })
		}
	})

	// --- 6. ICE-кандидаты → отправляем серверу ---
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		sdpMid, uf := "", ""
		var mlineIdx uint16
		if init.SDPMid != nil {
			sdpMid = *init.SDPMid
		}
		if init.SDPMLineIndex != nil {
			mlineIdx = *init.SDPMLineIndex
		}
		if init.UsernameFragment != nil {
			uf = *init.UsernameFragment
		}
		payload, _ := json.Marshal(candidatePayload{
			Candidate:        init.Candidate,
			SDPMid:           sdpMid,
			SDPMLineIndex:    mlineIdx,
			UsernameFragment: uf,
		})
		_ = c.sendMsg(signalMsg{Type: "candidate", Payload: payload})
	})

	// --- 7. Join ---
	result.Stage = "join"
	joinPayload, _ := json.Marshal(map[string]string{
		"room_id": c.cfg.RoomID,
		"peer_id": c.cfg.PeerID,
	})
	if err := c.sendMsg(signalMsg{Type: "join", Payload: joinPayload}); err != nil {
		return false, "", fmt.Errorf("send join: %w", err)
	}

	// --- 8. Горутина чтения WebSocket ---
	msgCh := make(chan signalMsg, 128)
	go func() {
		defer close(msgCh)
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var msg signalMsg
			if json.Unmarshal(data, &msg) == nil {
				select {
				case msgCh <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	handleOffer := func(payload json.RawMessage) {
		var op offerPayload
		if err := json.Unmarshal(payload, &op); err != nil {
			return
		}
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  op.SDP,
		}); err != nil {
			return
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			return
		}
		if err := pc.SetLocalDescription(answer); err != nil {
			return
		}
		answerPayload, _ := json.Marshal(map[string]string{
			"sdp":  answer.SDP,
			"type": answer.Type.String(),
		})
		_ = c.sendMsg(signalMsg{Type: "answer", Payload: answerPayload})
	}

	handleCandidate := func(payload json.RawMessage) {
		var cp candidatePayload
		if err := json.Unmarshal(payload, &cp); err != nil {
			return
		}
		sdpMid := cp.SDPMid
		mlineIdx := cp.SDPMLineIndex
		_ = pc.AddICECandidate(webrtc.ICECandidateInit{
			Candidate:     cp.Candidate,
			SDPMid:        &sdpMid,
			SDPMLineIndex: &mlineIdx,
		})
	}

	// --- 9. Фаза сигнализации: ждём Connected ---
	result.Stage = "signaling"
	connTimeout := time.NewTimer(30 * time.Second)
	defer connTimeout.Stop()

	waitConnected := true
	for waitConnected {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()

		case <-connTimeout.C:
			result.Stage = "timeout"
			return false, "", fmt.Errorf("connection timeout (30s)")

		case msg, ok := <-msgCh:
			if !ok {
				return false, "", fmt.Errorf("ws closed during signaling")
			}
			switch msg.Type {
			case "offer":
				result.Stage = "answer"
				handleOffer(msg.Payload)
			case "candidate":
				result.Stage = "ice"
				handleCandidate(msg.Payload)
			case "redirect":
				var rp struct {
					Addr string `json:"addr"`
				}
				if err := json.Unmarshal(msg.Payload, &rp); err == nil {
					return true, rp.Addr, nil
				}
			case "error":
				return false, "", fmt.Errorf("server error: %s", string(msg.Payload))
			}

		case <-connectedCh:
			waitConnected = false
		}
	}

	if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		result.Stage = "ice"
		return false, "", fmt.Errorf("connection failed: state=%s", pc.ConnectionState())
	}

	// --- 10. Соединение установлено ---
	result.Connected = true
	result.ConnectionTime = time.Since(start)
	result.Stage = "holding"

	if c.m != nil {
		c.m.ClientsConnected.Inc()
		c.m.ConnectionSeconds.Observe(result.ConnectionTime.Seconds())
	}

	holdCtx, holdCancel := context.WithTimeout(ctx, c.cfg.Duration)
	defer holdCancel()

	go sendFakeVideo(holdCtx, videoTrack)
	go sendFakeAudio(holdCtx, audioTrack)

	for {
		select {
		case <-holdCtx.Done():
			_ = c.sendMsg(signalMsg{Type: "leave", Payload: json.RawMessage(`{}`)})
			if c.m != nil {
				c.m.ClientsConnected.Dec()
			}
			return false, "", nil

		case msg, ok := <-msgCh:
			if !ok {
				if c.m != nil {
					c.m.ClientsConnected.Dec()
				}
				return false, "", nil
			}
			switch msg.Type {
			case "offer":
				handleOffer(msg.Payload)
			case "candidate":
				handleCandidate(msg.Payload)
			}
		}
	}
}

// sendMsg сериализует и отправляет сообщение по WebSocket (thread-safe).
func (c *Client) sendMsg(msg signalMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, data)
}

// drainRTCP читает и отбрасывает RTCP-пакеты (обязательно для pion).
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}

// drainTrack читает и отбрасывает RTP-пакеты входящего трека.
// Вызывается в OnTrack для треков от других пиров комнаты.
// Без этого дренажа pion накапливает буферы и выдаёт ошибки.
func drainTrack(track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := track.Read(buf); err != nil {
			if err == io.EOF {
				return
			}
			return
		}
	}
}

// sendFakeVideo пишет VP8-кейфреймы (5 fps) до отмены контекста.
func sendFakeVideo(ctx context.Context, track *webrtc.TrackLocalStaticSample) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = track.WriteSample(media.Sample{
				Data:     vp8KeyFrame,
				Duration: 200 * time.Millisecond,
			})
		}
	}
}

// sendFakeAudio пишет Opus silence-фреймы (20ms) до отмены контекста.
func sendFakeAudio(ctx context.Context, track *webrtc.TrackLocalStaticSample) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = track.WriteSample(media.Sample{
				Data:     opusSilence,
				Duration: 20 * time.Millisecond,
			})
		}
	}
}
