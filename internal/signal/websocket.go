package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sfu-server/internal/config"
	"sfu-server/internal/metrics"
	"sfu-server/internal/sfu"
	"sfu-server/internal/turn"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Server для обсуживания входящих WebSocket-соединений клиентов и связывания их
// с ядром SFU-сервера.
//
// Маршруты:
//
//		/ws 	 - WebSocket endpoint
//		/healthz - health-check для K8s
//		/ice-servers - Динамическое получение STUN/TURN реквизитов доступа
//	    /		 - для статического файлового сервера HTML/JS
type Server struct {
	httpServer  *http.Server
	upgrader    websocket.Upgrader
	sfuServer   *sfu.SFUServer
	config      *config.Config
	activeConns atomic.Int64
	wg          sync.WaitGroup
	metrics     *metrics.Metrics // nil если метрики отключены
}

// NewServer создаёт сигнальный HTTP/WebSocket сервер.
// m может быть nil — тогда /metrics не регистрируется.
func NewServer(cfg *config.Config, sfuServer *sfu.SFUServer, webDir string, m *metrics.Metrics) *Server {
	s := &Server{
		sfuServer: sfuServer,
		config:    cfg,
		metrics:   m,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/ice-servers", s.handleICEServers)

	// /metrics — Prometheus scrape endpoint (только если метрики включены)
	if m != nil {
		mux.Handle("/metrics", m.Handler())
		log.Printf("[SignalServer] /metrics endpoint enabled")
	}

	if webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(webDir)))
	}

	s.httpServer = &http.Server{
		Addr:         cfg.Server.GetServerAddr(),
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	return s
}

// Метод для запуска HTTP-сервера
func (s *Server) ListenAndServe() error {
	log.Printf("[SignalServer] listening on %s", s.config.Server.GetServerAddr())

	if err := s.httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// Метод остановки работы сервера
func (s *Server) Shutdown(ctx context.Context) error {
	log.Printf("[SignalServer] shutting down: active_conns=%d", s.activeConns.Load())

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}

	// Ждём завершения всех горутин сессий
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Printf("[SignalServer] all sessions closed")
	case <-shutdownCtx.Done():
		log.Printf("[SignalServer] shutdown timeout: some sessions still active")
	}

	return nil
}

// Метод возвращает количество активных WebSocket-соединений на данной ноде
func (s *Server) GetActiveConns() int64 {
	return s.activeConns.Load()
}

// handleICEServers возвращает список серверов ICE (STUN/TURN) с временными credentials
func (s *Server) handleICEServers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	peerID := r.URL.Query().Get("peer_id")
	if peerID == "" {
		peerID = "anonymous"
	}

	type IceServerJSON struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username,omitempty"`
		Credential string   `json:"credential,omitempty"`
	}

	var servers []IceServerJSON

	// 1. STUN
	if len(s.config.ICE.STUNServers) > 0 {
		servers = append(servers, IceServerJSON{
			URLs: s.config.ICE.STUNServers,
		})
	}

	// 2. Встроенный TURN с временными HMAC-credentials
	if s.config.TurnServer.Enabled {
		username, password := turn.GenerateCredentials(peerID, 24*time.Hour, s.config.TurnServer.StaticSecret)
		turnURL := fmt.Sprintf("turn:%s:%d?transport=udp", s.config.TurnServer.PublicIP, s.config.TurnServer.Port)

		servers = append(servers, IceServerJSON{
			URLs:       []string{turnURL},
			Username:   username,
			Credential: password,
		})
	}

	// 3. Внешние TURN
	for _, t := range s.config.ICE.TURNServers {
		servers = append(servers, IceServerJSON{
			URLs:       t.URLs,
			Username:   t.Username,
			Credential: t.Credential,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(servers)
}

// Метод выполняет Upgrade HTTP до WebSocket и запускает Session
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.config.Server.MaxConns > 0 && s.activeConns.Load() >= int64(s.config.Server.MaxConns) {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		log.Printf("[SignalServer] connection rejected (limit=%d): addr=%s",
			s.config.Server.MaxConns, r.RemoteAddr)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[SignalServer] upgrade error: addr=%s err=%v", r.RemoteAddr, err)
		return
	}

	s.activeConns.Add(1)
	s.wg.Add(1)

	if s.metrics != nil {
		s.metrics.ConnectionOpened()
	}

	log.Printf("[SignalServer] new connection: addr=%s active=%d",
		r.RemoteAddr, s.activeConns.Load())

	defer func() {
		s.activeConns.Add(-1)
		s.wg.Done()
		if s.metrics != nil {
			s.metrics.ConnectionClosed()
		}
		log.Printf("[SignalServer] connection closed: addr=%s active=%d",
			conn.RemoteAddr(), s.activeConns.Load())
	}()

	session := NewSession(r.Context(), conn, s.sfuServer)
	session.Run()
}

// Метод возвращает статус ноды для K8s
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	stats := s.sfuServer.Stats()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Формируем JSON вручную, чтобы не аллоцировать лишнее в hot-path
	_, _ = w.Write([]byte(`{"status":"ok", "active_conns":` +
		itoa(s.activeConns.Load()) +
		`,"rooms":` + itoa(int64(stats.RoomsCount)) +
		`,"peers":` + itoa(int64(stats.TotalPeers)) +
		`}`))
}

// Свой метод конвертирования int64 в строку
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte(n%10) + '0'
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
