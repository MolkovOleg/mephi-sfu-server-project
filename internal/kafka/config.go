package kafka

// Представляет настройки подключения к брокеру Kafka
type KafkaConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Brokers    []string `yaml:"brokers"`
	Topic      string   `yaml:"topic"`
	BufferSize int      `yaml:"buffer_size"`
}

// Возвращает конфигурацию по умолчанию
func DefaultConfig() KafkaConfig {
	return KafkaConfig{
		Enabled:    false,
		Brokers:    []string{"localhost:9092"},
		Topic:      "sfu-events",
		BufferSize: 1000,
	}
}
