package signal

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sfu-server/internal/config"
	"sfu-server/internal/sfu"
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
//	    /		 - для статического файлового сервера HTML/JS
type Server struct {
	httpServer  *http.Server
	upgrader    websocket.Upgrader
	sfuServer   *sfu.SFUServer
	cfg         config.ServerConfig
	activeConns atomic.Int64
	wg          sync.WaitGroup
}

// Метод создания сигнального HTTP/WebSocket сервера
func NewServer(cfg config.ServerConfig, sfuServer *sfu.SFUServer, webDir string) *Server {
	s := &Server{
		sfuServer: sfuServer,
		cfg:       cfg,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// В продакшене необходимо будет заменить на проверку допустимых Origin
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/healthz", s.handleHealthz)

	if webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(webDir)))
	}

	s.httpServer = &http.Server{
		Addr:         cfg.GetServerAddr(),
		Handler:      mux,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	return s
}

// Метод для запуска HTTP-сервера
func (s *Server) ListenAndServe() error {
	log.Printf("[SignalServer] listening on %s", s.cfg.GetServerAddr())

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

// Метод выполняет Upgrade HTTP до WebSocket и запускает Session
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MaxConns > 0 && s.activeConns.Load() >= int64(s.cfg.MaxConns) {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		log.Printf("[SignalServer] connection rejected (limit=%d): addr=%s",
			s.cfg.MaxConns, r.RemoteAddr)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[SignalServer] upgrade error: addr=%s err=%v", r.RemoteAddr, err)
		return
	}

	s.activeConns.Add(1)
	s.wg.Add(1)

	log.Printf("[SignalServer] new connection: addr=%s active=%d",
		r.RemoteAddr, s.activeConns.Load())

	defer func() {
		s.activeConns.Add(-1)
		s.wg.Done()
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
