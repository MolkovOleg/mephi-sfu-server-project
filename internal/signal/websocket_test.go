package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sfu-server/internal/config"
	"sfu-server/internal/sfu"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalServerHttpEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Создаем тестовую конфигурацию
	cfg := config.Default()
	cfg.ICE.STUNServers = []string{"stun:stun.l.google.com:19302"}
	cfg.TurnServer.Enabled = true
	cfg.TurnServer.PublicIP = "127.0.0.1"
	cfg.TurnServer.Port = 3478
	cfg.TurnServer.Realm = "mephi-sfu"
	cfg.TurnServer.StaticSecret = "test-secret"

	// 2. Создаем dummy SFU сервер
	sfuConfig := sfu.ServerConfig{
		MaxRooms: 10,
	}
	sfuServer := sfu.NewSFUServer(ctx, sfuConfig)

	// 3. Создаем сигнальный сервер
	server := NewServer(cfg, sfuServer, "", nil)

	// 4. Тестируем endpoint /ice-servers
	req := httptest.NewRequest("GET", "/ice-servers?peer_id=test-peer", nil)
	rr := httptest.NewRecorder()

	server.handleICEServers(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "application/json", rr.Header().Get("Content-Type"))

	type IceServerJSON struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username,omitempty"`
		Credential string   `json:"credential,omitempty"`
	}

	var iceServers []IceServerJSON
	err := json.Unmarshal(rr.Body.Bytes(), &iceServers)
	require.NoError(t, err)

	// Должно быть 2 записи: 1 - STUN, 2 - Встроенный TURN
	require.Len(t, iceServers, 2)
	assert.Contains(t, iceServers[0].URLs[0], "stun:")
	assert.Contains(t, iceServers[1].URLs[0], "turn:")
	assert.NotEmpty(t, iceServers[1].Username)
	assert.NotEmpty(t, iceServers[1].Credential)

	// 5. Тестируем /healthz
	reqHealth := httptest.NewRequest("GET", "/healthz", nil)
	rrHealth := httptest.NewRecorder()

	server.handleHealthz(rrHealth, reqHealth)

	assert.Equal(t, http.StatusOK, rrHealth.Code)
	assert.Equal(t, "application/json", rrHealth.Header().Get("Content-Type"))

	var healthRes map[string]interface{}
	err = json.Unmarshal(rrHealth.Body.Bytes(), &healthRes)
	require.NoError(t, err)

	assert.Equal(t, "ok", healthRes["status"])
	assert.Equal(t, float64(0), healthRes["rooms"])
	assert.Equal(t, float64(0), healthRes["peers"])
}
