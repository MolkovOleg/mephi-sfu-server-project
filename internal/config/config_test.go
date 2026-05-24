package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sfu-server/internal/config"
)

// =============================================================================
// Тесты Default()
// =============================================================================

func TestDefault_ReturnsValidConfig(t *testing.T) {
	cfg := config.Default()

	require.NotNil(t, cfg)
	assert.Equal(t, "0.0.0.0", cfg.Server.Host)
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, 10*time.Second, cfg.Server.ReadTimeout)
	assert.Equal(t, 10*time.Second, cfg.Server.WriteTimeout)
	assert.Equal(t, 0, cfg.Server.MaxConns)

	assert.Equal(t, 0, cfg.SFU.MaxRooms)
	assert.Equal(t, 0, cfg.SFU.MaxPeersPerRoom)
	assert.Equal(t, 2*time.Second, cfg.SFU.PLIInterval)
	assert.Equal(t, 100, cfg.SFU.SenderBufSize)

	assert.Len(t, cfg.ICE.STUNServers, 1)
	assert.Contains(t, cfg.ICE.STUNServers[0], "stun")

	assert.False(t, cfg.Cluster.Enabled)
	assert.Equal(t, "localhost:6379", cfg.Cluster.RedisAddr)

	assert.False(t, cfg.Metrics.Enabled)
	assert.Equal(t, 9091, cfg.Metrics.Port)

	assert.Equal(t, "info", cfg.Log.Level)
}

func TestServerConfig_Addr(t *testing.T) {
	cfg := config.Default()
	assert.Equal(t, "0.0.0.0:8080", cfg.Server.GetServerAddr())
}

func TestMetricsConfig_Addr(t *testing.T) {
	cfg := config.Default()
	assert.Equal(t, ":9091", cfg.Metrics.GetMetricsAddr())
}

// =============================================================================
// Тесты Load() — YAML
// =============================================================================

func TestLoad_FileNotFound_ReturnsDefaults(t *testing.T) {
	cfg, err := config.Load("/nonexistent/path/config.yml")

	require.NoError(t, err, "отсутствующий файл должен возвращать дефолты, а не ошибку")
	require.NotNil(t, cfg)
	assert.Equal(t, 8080, cfg.Server.Port)
}

func TestLoad_ValidYAML_OverridesDefaults(t *testing.T) {
	yaml := `
server:
  port: 9090
  max_conns: 5000
sfu:
  max_rooms: 100
  max_peers_per_room: 50
  pli_interval: 3s
  sender_buf_size: 200
cluster:
  enabled: true
  redis_addr: "redis:6379"
metrics:
  enabled: true
  port: 9091
log:
  level: "debug"
`
	path := writeTemp(t, yaml)

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, 5000, cfg.Server.MaxConns)
	assert.Equal(t, 100, cfg.SFU.MaxRooms)
	assert.Equal(t, 50, cfg.SFU.MaxPeersPerRoom)
	assert.Equal(t, 3*time.Second, cfg.SFU.PLIInterval)
	assert.Equal(t, 200, cfg.SFU.SenderBufSize)
	assert.True(t, cfg.Cluster.Enabled)
	assert.Equal(t, "redis:6379", cfg.Cluster.RedisAddr)
	assert.True(t, cfg.Metrics.Enabled)
	assert.Equal(t, 9091, cfg.Metrics.Port)
	assert.Equal(t, "debug", cfg.Log.Level)
}

func TestLoad_InvalidYAML_ReturnsError(t *testing.T) {
	path := writeTemp(t, "server: [invalid yaml :")

	_, err := config.Load(path)

	assert.Error(t, err)
}

func TestLoad_PartialYAML_MergesWithDefaults(t *testing.T) {
	// Задаём только порт — остальное должно остаться дефолтным
	yaml := `
server:
  port: 7070
`
	path := writeTemp(t, yaml)

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 7070, cfg.Server.Port)
	assert.Equal(t, "0.0.0.0", cfg.Server.Host, "host должен остаться дефолтным")
	assert.Equal(t, 2*time.Second, cfg.SFU.PLIInterval, "PLI должен остаться дефолтным")
}

// =============================================================================
// Тесты ENV override
// =============================================================================

func TestLoad_EnvOverridesYAML(t *testing.T) {
	yaml := `
server:
  port: 8080
`
	path := writeTemp(t, yaml)

	t.Setenv("SERVER_PORT", "9999")
	t.Setenv("SERVER_HOST", "127.0.0.1")
	t.Setenv("SERVER_MAX_CONNS", "1000")
	t.Setenv("SFU_MAX_ROOMS", "50")
	t.Setenv("SFU_MAX_PEERS", "20")
	t.Setenv("CLUSTER_ENABLED", "true")
	t.Setenv("CLUSTER_REDIS_ADDR", "myredis:6379")
	t.Setenv("CLUSTER_NODE_ID", "node-42")
	t.Setenv("METRICS_ENABLED", "1")
	t.Setenv("METRICS_PORT", "9200")
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 9999, cfg.Server.Port)
	assert.Equal(t, "127.0.0.1", cfg.Server.Host)
	assert.Equal(t, 1000, cfg.Server.MaxConns)
	assert.Equal(t, 50, cfg.SFU.MaxRooms)
	assert.Equal(t, 20, cfg.SFU.MaxPeersPerRoom)
	assert.True(t, cfg.Cluster.Enabled)
	assert.Equal(t, "myredis:6379", cfg.Cluster.RedisAddr)
	assert.Equal(t, "node-42", cfg.Cluster.NodeID)
	assert.True(t, cfg.Metrics.Enabled)
	assert.Equal(t, 9200, cfg.Metrics.Port)
	assert.Equal(t, "warn", cfg.Log.Level)
}

func TestLoad_EnvClusterEnabledVariants(t *testing.T) {
	cases := []struct {
		env      string
		expected bool
	}{
		{"true", true},
		{"1", true},
		{"yes", true},
		{"false", false},
		{"0", false},
		{"no", false},
	}

	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("CLUSTER_ENABLED", tc.env)
			// cluster.enabled=true требует redis_addr
			t.Setenv("CLUSTER_REDIS_ADDR", "localhost:6379")

			cfg, err := config.Load("/nonexistent")
			require.NoError(t, err)
			assert.Equal(t, tc.expected, cfg.Cluster.Enabled)
		})
	}
}

// =============================================================================
// Тесты Validate
// =============================================================================

func TestLoad_Validation_InvalidPort(t *testing.T) {
	yaml := `server: {port: 0}`
	path := writeTemp(t, yaml)

	_, err := config.Load(path)
	assert.ErrorContains(t, err, "server port")
}

func TestLoad_Validation_InvalidPLI(t *testing.T) {
	yaml := `sfu: {pli_interval: 0s}`
	path := writeTemp(t, yaml)

	_, err := config.Load(path)
	assert.ErrorContains(t, err, "PLI interval")
}

func TestLoad_Validation_ClusterEnabledWithoutRedis(t *testing.T) {
	yaml := `
cluster:
  enabled: true
  redis_addr: ""
`
	path := writeTemp(t, yaml)

	_, err := config.Load(path)
	assert.ErrorContains(t, err, "redis addr")
}

func TestLoad_Validation_MetricsPortCollidesWithServer(t *testing.T) {
	yaml := `
server:
  port: 8080
metrics:
  enabled: true
  port: 8080
`
	path := writeTemp(t, yaml)

	_, err := config.Load(path)
	assert.ErrorContains(t, err, "metrics port")
}

func TestLoad_Validation_InvalidLogLevel(t *testing.T) {
	yaml := `log: {level: "verbose"}`
	path := writeTemp(t, yaml)

	_, err := config.Load(path)
	assert.ErrorContains(t, err, "log level")
}

// =============================================================================
// Helpers
// =============================================================================

// writeTemp создаёт временный YAML-файл и возвращает его путь.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}
