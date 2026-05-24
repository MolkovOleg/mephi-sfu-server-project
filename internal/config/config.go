package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stretchr/testify/assert/yaml"
)

// =============================================================================
// Конфигурация SFU-сервера
//
// Загрузка из YAML-файла с возможностью переопределения через переменные
// окружения. ENV-переменные имеют формат:
//
//	SERVER_HOST        → Config.Server.Host
//	SERVER_PORT        → Config.Server.Port
//	SERVER_MAX_CONNS   → Config.Server.MaxConns
//	SFU_MAX_ROOMS      → Config.SFU.MaxRooms
//	SFU_MAX_PEERS      → Config.SFU.MaxPeersPerRoom
//	CLUSTER_ENABLED    → Config.Cluster.Enabled
//	CLUSTER_REDIS_ADDR → Config.Cluster.RedisAddr
//	METRICS_ENABLED    → Config.Metrics.Enabled
//	METRICS_PORT       → Config.Metrics.Port
//	LOG_LEVEL          → Config.Log.Level
// =============================================================================

// =============================================================================
// Корневая конфигурация сервера
// =============================================================================

// Конфигурация проекта
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	SFU        SFUConfig        `yaml:"sfu"`
	ICE        ICEConfig        `yaml:"ice"`
	Cluster    ClusterConfig    `yaml:"cluster"`
	Metrics    MetricsConfig    `yaml:"metrics"`
	Kafka      KafkaConfig      `yaml:"kafka"`
	TurnServer TurnServerConfig `yaml:"turn_server"`
	Log        LogConfig        `yaml:"log"`
}

// Конфигурация встроенного TURN-сервера
type TurnServerConfig struct {
	Enabled      bool   `yaml:"enabled"`
	PublicIP     string `yaml:"public_ip"`
	Port         int    `yaml:"port"`
	Realm        string `yaml:"realm"`
	StaticSecret string `yaml:"static_secret"`
	MinPort      int    `yaml:"min_port"`
	MaxPort      int    `yaml:"max_port"`
}

// Конфигурация брокера событий Kafka
type KafkaConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Brokers    []string `yaml:"brokers"`
	Topic      string   `yaml:"topic"`
	BufferSize int      `yaml:"buffer_size"`
}

// Конфигурация HTTP/WebSocket сервера
type ServerConfig struct {
	Host         string        `yaml:"host"`
	Port         int           `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	MaxConns     int           `yaml:"max_conns"`
}

// Метод возврата адреса сервера host:port
func (s ServerConfig) GetServerAddr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// Конфигурация SFU ядра
type SFUConfig struct {
	MaxRooms        int           `yaml:"max_rooms"`
	MaxPeersPerRoom int           `yaml:"max_peers_per_room"`
	PLIInterval     time.Duration `yaml:"pli_interval"`
	SenderBufSize   int           `yaml:"sender_buf_size"`
}

// Параметры TURN-сервера
type TurnConfig struct {
	// Адреса TURN-сервера
	URLs       []string `yaml:"urls"`
	Username   string   `yaml:"username"`
	Credential string   `yaml:"credential"`
}

// Параметры ICE-агентов WebRTC
type ICEConfig struct {
	// Список STUN-серверов для NAT
	STUNServers []string     `yaml:"stun_servers"`
	TURNServers []TurnConfig `yaml:"turn_servers"`
}

// Конфигурация кластеризации с помощью Redis
type ClusterConfig struct {
	Enabled           bool          `yaml:"enabled"`
	RedisAddr         string        `yaml:"redis_addr"`
	RedisPassword     string        `yaml:"redis_password"`
	RedisDB           int           `yaml:"redis_db"`
	NodeID            string        `yaml:"node_id"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	NodeTTL           time.Duration `yaml:"node_ttl"`
	EnableCascading   bool          `yaml:"enable_cascading"`
}

// Конфигурация метрик (Prometheus)
type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// Метод возврата адреса метрик
func (m MetricsConfig) GetMetricsAddr() string {
	return fmt.Sprintf(":%d", m.Port)
}

// Конфигурация логирования
type LogConfig struct {
	Level string `yaml:"level"`
}

// Загрузка конфигурации по умолчанию, если конфиг-файла нет
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Host:         "0.0.0.0",
			Port:         8080,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			MaxConns:     0,
		},
		SFU: SFUConfig{
			MaxRooms:        0,
			MaxPeersPerRoom: 0,
			PLIInterval:     2 * time.Second,
			SenderBufSize:   100,
		},
		ICE: ICEConfig{
			STUNServers: []string{"stun:stun.l.google.com:19302"},
		},
		Cluster: ClusterConfig{
			Enabled:           false,
			RedisAddr:         "localhost:6379",
			RedisDB:           0,
			HeartbeatInterval: 10 * time.Second,
			NodeTTL:           30 * time.Second,
			EnableCascading:   true,
		},
		Metrics: MetricsConfig{
			Enabled: false,
			Port:    9091,
		},
		Kafka: KafkaConfig{
			Enabled:    false,
			Brokers:    []string{"localhost:9092"},
			Topic:      "sfu-events",
			BufferSize: 1000,
		},
		TurnServer: TurnServerConfig{
			Enabled:      false,
			PublicIP:     "127.0.0.1",
			Port:         3478,
			Realm:        "mephi-sfu",
			StaticSecret: "mephi-sfu-secret-key-2026",
			MinPort:      49152,
			MaxPort:      65535,
		},
		Log: LogConfig{
			Level: "info",
		},
	}
}

// Загрузка конфигурации из YAML-файла
func Load(path string) (*Config, error) {
	cfg := Default()

	// Читаем YAML-файл
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
		} else {
			return nil, fmt.Errorf("config: read file %q: %w", path, err)
		}
	} else {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse yaml %q: %w", path, err)
		}
	}

	// Применяем переопределение из ENV
	applyEnv(cfg)

	// Валидируем итоговую конфигурацию
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config: invalid %w", err)
	}

	return cfg, nil
}

// Задание конфигурации через ENV
func applyEnv(cfg *Config) {
	// Сервер
	if v := os.Getenv("SERVER_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("SERVER_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Server.MaxConns = n
		}
	}

	// SFU
	if v := os.Getenv("SFU_MAX_ROOMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SFU.MaxRooms = n
		}
	}
	if v := os.Getenv("SFU_MAX_PEERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.SFU.MaxPeersPerRoom = n
		}
	}

	// Cluster
	if v := os.Getenv("CLUSTER_ENABLED"); v != "" {
		cfg.Cluster.Enabled = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("CLUSTER_REDIS_ADDR"); v != "" {
		cfg.Cluster.RedisAddr = v
	}
	if v := os.Getenv("CLUSTER_REDIS_PASSWORD"); v != "" {
		cfg.Cluster.RedisPassword = v
	}
	if v := os.Getenv("CLUSTER_NODE_ID"); v != "" {
		cfg.Cluster.NodeID = v
	}
	if v := os.Getenv("CLUSTER_ENABLE_CASCADING"); v != "" {
		cfg.Cluster.EnableCascading = v == "true" || v == "1" || v == "yes"
	}

	// Metrics
	if v := os.Getenv("METRICS_ENABLED"); v != "" {
		cfg.Metrics.Enabled = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("METRICS_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Metrics.Port = p
		}
	}

	// Kafka
	if v := os.Getenv("KAFKA_ENABLED"); v != "" {
		cfg.Kafka.Enabled = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("KAFKA_BROKERS"); v != "" {
		cfg.Kafka.Brokers = strings.Split(v, ",")
	}
	if v := os.Getenv("KAFKA_TOPIC"); v != "" {
		cfg.Kafka.Topic = v
	}
	if v := os.Getenv("KAFKA_BUFFER_SIZE"); v != "" {
		if size, err := strconv.Atoi(v); err == nil {
			cfg.Kafka.BufferSize = size
		}
	}

	// TURN Server
	if v := os.Getenv("TURN_ENABLED"); v != "" {
		cfg.TurnServer.Enabled = v == "true" || v == "1" || v == "yes"
	}
	if v := os.Getenv("TURN_PUBLIC_IP"); v != "" {
		cfg.TurnServer.PublicIP = v
	}
	if v := os.Getenv("TURN_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.TurnServer.Port = port
		}
	}
	if v := os.Getenv("TURN_REALM"); v != "" {
		cfg.TurnServer.Realm = v
	}
	if v := os.Getenv("TURN_STATIC_SECRET"); v != "" {
		cfg.TurnServer.StaticSecret = v
	}
	if v := os.Getenv("TURN_MIN_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.TurnServer.MinPort = port
		}
	}
	if v := os.Getenv("TURN_MAX_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.TurnServer.MaxPort = port
		}
	}

	// Log
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
}

// Метод валидации итоговой конфигурации
func validate(cfg *Config) error {
	if cfg.Server.Port <= 0 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server port must be in range 1-65535, got %d", cfg.Server.Port)
	}
	if cfg.SFU.PLIInterval <= 0 {
		return fmt.Errorf("sfu PLI interval must be positive")
	}
	if cfg.SFU.SenderBufSize <= 0 {
		return fmt.Errorf("sfu sender buffer size must be positive")
	}
	if cfg.Cluster.Enabled && cfg.Cluster.RedisAddr == "" {
		return fmt.Errorf("cluster redis addr is required when cluster is enabled")
	}
	if cfg.Metrics.Enabled && cfg.Metrics.Port == cfg.Server.Port {
		return fmt.Errorf("metrics port must differ from server port")
	}

	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log level must be one of: (debug, info, warn, error)")
	}

	return nil
}
