package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/turn/v2"
)

// Параметры встроенного TURN-сервера
type TurnServerConfig struct {
	Enabled      bool
	PublicIP     string
	Port         int
	Realm        string
	StaticSecret string
	MinPort      int
	MaxPort      int
}

// Обертка над встроенным Pion TURN сервером
type Server struct {
	turnServer *turn.Server
	listener   net.PacketConn
	config     TurnServerConfig
	mu         sync.Mutex
	running    bool
}

// Создание нового инстанса TURN-сервера
func NewServer(config TurnServerConfig) *Server {
	return &Server{
		config: config,
	}
}

// Временные credentials по алгоритму REST API (time-windowed)
// Формат username: <expiration_timestamp>:<peer_id>
// Формат password: Base64(HMAC-SHA1(secret, username))
func GenerateCredentials(peerID string, duration time.Duration, secret string) (string, string) {
	expiry := time.Now().Add(duration).Unix()
	username := fmt.Sprintf("%d:%s", expiry, peerID)

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	password := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return username, password
}

// Start запускает встроенный TURN сервер
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}

	addr := fmt.Sprintf("0.0.0.0:%d", s.config.Port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen UDP packet conn on %s: %w", addr, err)
	}
	s.listener = conn

	// Создаем генератор ретранслируемых адресов с диапазоном портов
	relayGenerator := &turn.RelayAddressGeneratorPortRange{
		RelayAddress: net.ParseIP(s.config.PublicIP),
		Address:      "0.0.0.0",
		MinPort:      uint16(s.config.MinPort),
		MaxPort:      uint16(s.config.MaxPort),
	}

	// Инициализируем Pion TURN сервер
	tServer, err := turn.NewServer(turn.ServerConfig{
		Realm: s.config.Realm,
		// Настраиваем динамический AuthHandler для проверки Time-Windowed HMAC credentials
		AuthHandler: func(username string, realm string, srcAddr net.Addr) ([]byte, bool) {
			parts := strings.SplitN(username, ":", 2)
			if len(parts) != 2 {
				return nil, false
			}

			expirySecs, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				return nil, false
			}

			// Проверяем срок годности реквизитов доступа
			if time.Now().Unix() > expirySecs {
				log.Printf("[TURN] Auth failed: credential expired for %s", username)
				return nil, false
			}

			// Вычисляем ожидаемый пароль на основе статического секрета
			mac := hmac.New(sha1.New, []byte(s.config.StaticSecret))
			mac.Write([]byte(username))
			password := base64.StdEncoding.EncodeToString(mac.Sum(nil))

			// Генерируем STUN ключ
			key := turn.GenerateAuthKey(username, realm, password)
			return key, true
		},
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn:            s.listener,
				RelayAddressGenerator: relayGenerator,
			},
		},
	})
	if err != nil {
		s.listener.Close()
		return fmt.Errorf("failed to initialize Pion TURN server: %w", err)
	}

	s.turnServer = tServer
	s.running = true
	log.Printf("[TURN] server started successfully on port %d with realm %s, ports range [%d - %d]",
		s.config.Port, s.config.Realm, s.config.MinPort, s.config.MaxPort)

	return nil
}

// Close gracefully останавливает TURN-сервер
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	var errs []string

	if err := s.turnServer.Close(); err != nil {
		errs = append(errs, fmt.Sprintf("turn close error: %v", err))
	}

	s.running = false
	log.Printf("[TURN] server stopped")

	if len(errs) > 0 {
		return fmt.Errorf("errors closing TURN server: %s", strings.Join(errs, "; "))
	}
	return nil
}
