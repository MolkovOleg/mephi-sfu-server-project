package metrics

// =============================================================================
// Prometheus-метрики SFU-сервера
//
// Метрики разделены по слоям:
//
//   Сигнализация:
//     sfu_ws_connections_total        — всего WebSocket-подключений
//     sfu_ws_connections_active       — текущих активных соединений
//     sfu_signaling_messages_total    — входящих сообщений по типу
//     sfu_signaling_errors_total      — ошибок сигнализации по коду
//
//   SFU-ядро:
//     sfu_rooms_active                — активных комнат
//     sfu_peers_active                — активных пиров
//     sfu_tracks_active               — активных медиатреков
//
//   Медиа (hot path):
//     sfu_rtp_packets_forwarded_total — пакетов переслано (counter)
//     sfu_rtp_bytes_forwarded_total   — байт переслано (counter)
//     sfu_rtp_packets_dropped_total   — пакетов дропнуто (counter)
//     sfu_rtp_forward_duration_seconds — задержка пересылки (histogram)
//
//   Переговоры:
//     sfu_negotiations_total          — всего переговорок
//     sfu_negotiations_deferred_total — переговорок отложено (race guard)
//
// Все метрики регистрируются в кастомном Registry (не в DefaultRegisterer),
// чтобы не загрязнять тесты и можно было иметь несколько инстансов.
// =============================================================================

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// =============================================================================
// Метрики
// =============================================================================

// Metrics содержит все Prometheus-метрики сервера.
// Создаётся один раз при старте и передаётся в нужные компоненты.
type Metrics struct {
	registry *prometheus.Registry

	// ── Сигнализация ────────────────────────────────────────────────────────
	WSConnectionsTotal  prometheus.Counter
	WSConnectionsActive prometheus.Gauge
	SignalingMsgTotal   *prometheus.CounterVec // label: type
	SignalingErrTotal   *prometheus.CounterVec // label: code

	// ── SFU-ядро ────────────────────────────────────────────────────────────
	RoomsActive  prometheus.Gauge
	PeersActive  prometheus.Gauge
	TracksActive prometheus.Gauge

	// ── Медиа (hot path) ────────────────────────────────────────────────────
	RTPPacketsForwarded *prometheus.CounterVec // labels: kind (audio|video)
	RTPBytesForwarded   *prometheus.CounterVec // labels: kind
	RTPPacketsDropped   *prometheus.CounterVec // labels: kind, reason
	RTPForwardDuration  prometheus.Histogram   // наносекунды → секунды

	// ── Переговоры ──────────────────────────────────────────────────────────
	NegotiationsTotal         prometheus.Counter
	NegotiationsDeferredTotal prometheus.Counter
}

// New создаёт и регистрирует все метрики в новом изолированном Registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		// Сигнализация
		WSConnectionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sfu_ws_connections_total",
			Help: "Total number of WebSocket connections accepted.",
		}),
		WSConnectionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sfu_ws_connections_active",
			Help: "Current number of active WebSocket connections.",
		}),
		SignalingMsgTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sfu_signaling_messages_total",
			Help: "Total signaling messages received, by message type.",
		}, []string{"type"}),
		SignalingErrTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sfu_signaling_errors_total",
			Help: "Total signaling errors sent to clients, by error code.",
		}, []string{"code"}),

		// SFU-ядро
		RoomsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sfu_rooms_active",
			Help: "Current number of active rooms.",
		}),
		PeersActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sfu_peers_active",
			Help: "Current number of active peers across all rooms.",
		}),
		TracksActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sfu_tracks_active",
			Help: "Current number of active media tracks (receivers) in the router.",
		}),

		// Медиа hot path
		RTPPacketsForwarded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sfu_rtp_packets_forwarded_total",
			Help: "Total RTP packets forwarded by the SFU router, by media kind.",
		}, []string{"kind"}),
		RTPBytesForwarded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sfu_rtp_bytes_forwarded_total",
			Help: "Total RTP bytes forwarded by the SFU router, by media kind.",
		}, []string{"kind"}),
		RTPPacketsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sfu_rtp_packets_dropped_total",
			Help: "Total RTP packets dropped (e.g. full write channel), by kind and reason.",
		}, []string{"kind", "reason"}),
		RTPForwardDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "sfu_rtp_forward_duration_seconds",
			Help: "Time spent forwarding one RTP packet to all subscribers.",
			// Buckets: от 5 мкс до 50 мс — диапазон реального LAN/WAN
			Buckets: []float64{0.000005, 0.00001, 0.000025, 0.00005,
				0.0001, 0.00025, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05},
		}),

		// Переговоры
		NegotiationsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sfu_negotiations_total",
			Help: "Total SDP offer negotiations initiated.",
		}),
		NegotiationsDeferredTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sfu_negotiations_deferred_total",
			Help: "Negotiations deferred because PC was not in stable state.",
		}),
	}

	// Регистрируем все метрики
	reg.MustRegister(
		m.WSConnectionsTotal,
		m.WSConnectionsActive,
		m.SignalingMsgTotal,
		m.SignalingErrTotal,

		m.RoomsActive,
		m.PeersActive,
		m.TracksActive,

		m.RTPPacketsForwarded,
		m.RTPBytesForwarded,
		m.RTPPacketsDropped,
		m.RTPForwardDuration,

		m.NegotiationsTotal,
		m.NegotiationsDeferredTotal,
	)

	// Стандартные Go runtime метрики (goroutines, GC, memory)
	reg.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	return m
}

// =============================================================================
// HTTP handler
// =============================================================================

// Handler возвращает http.Handler для эндпоинта /metrics.
// Используется в signal.Server или отдельном HTTP-сервере метрик.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// Registry возвращает Prometheus Registry (для тестов или расширения).
func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

// =============================================================================
// Удобные методы (shorthand) для hot-path
// =============================================================================

// ForwardedPacket инкрементирует счётчики пакетов и байт в горутине роутера.
// kind: "audio" | "video"
func (m *Metrics) ForwardedPacket(kind string, bytes int) {
	m.RTPPacketsForwarded.WithLabelValues(kind).Inc()
	m.RTPBytesForwarded.WithLabelValues(kind).Add(float64(bytes))
}

// DroppedPacket инкрементирует счётчик дропнутых пакетов.
// reason: "buffer_full" | "sender_closed" | "write_error"
func (m *Metrics) DroppedPacket(kind, reason string) {
	m.RTPPacketsDropped.WithLabelValues(kind, reason).Inc()
}

// PeerJoined вызывается при входе пира в комнату.
func (m *Metrics) PeerJoined() {
	m.PeersActive.Inc()
}

// PeerLeft вызывается при выходе пира.
func (m *Metrics) PeerLeft() {
	m.PeersActive.Dec()
}

// RoomCreated вызывается при создании комнаты.
func (m *Metrics) RoomCreated() {
	m.RoomsActive.Inc()
}

// RoomClosed вызывается при закрытии комнаты.
func (m *Metrics) RoomClosed() {
	m.RoomsActive.Dec()
}

// TrackAdded вызывается при добавлении нового Receiver'а в Router.
func (m *Metrics) TrackAdded() {
	m.TracksActive.Inc()
}

// TrackRemoved вызывается при удалении Receiver'а из Router.
func (m *Metrics) TrackRemoved() {
	m.TracksActive.Dec()
}

// ConnectionOpened вызывается при новом WebSocket-соединении.
func (m *Metrics) ConnectionOpened() {
	m.WSConnectionsTotal.Inc()
	m.WSConnectionsActive.Inc()
}

// ConnectionClosed вызывается при закрытии WebSocket-соединения.
func (m *Metrics) ConnectionClosed() {
	m.WSConnectionsActive.Dec()
}

// NegotiationStarted вызывается в начале negotiate().
func (m *Metrics) NegotiationStarted() {
	m.NegotiationsTotal.Inc()
}

// NegotiationDeferred вызывается когда negotiate() откладывается из-за состояния PC.
func (m *Metrics) NegotiationDeferred() {
	m.NegotiationsDeferredTotal.Inc()
}
