package loadtest

// =============================================================================
// Prometheus-метрики нагрузочного клиента
//
// Экспортируется на отдельном порту (:9099/metrics), чтобы Prometheus мог
// скрейпить как SFU-сервер (:8080/metrics), так и сам нагрузочный генератор.
// =============================================================================

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// LoadTestMetrics содержит все Prometheus-метрики нагрузочного тестера.
type LoadTestMetrics struct {
	registry *prometheus.Registry

	// Жизненный цикл клиентов
	ClientsSpawned   prometheus.Counter
	ClientsConnected prometheus.Gauge
	ClientsFailed    prometheus.Counter

	// Время установки WebRTC-соединения (от dial до PeerConnectionState.Connected)
	ConnectionSeconds prometheus.Histogram

	// Ошибки по этапам: dial | join | answer | ice | timeout
	ErrorsTotal *prometheus.CounterVec
}

// NewLoadTestMetrics создаёт и регистрирует все метрики нагрузочного тестера.
func NewLoadTestMetrics() *LoadTestMetrics {
	reg := prometheus.NewRegistry()

	m := &LoadTestMetrics{
		registry: reg,

		ClientsSpawned: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loadtest_clients_spawned_total",
			Help: "Total number of load test clients spawned.",
		}),
		ClientsConnected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "loadtest_clients_connected",
			Help: "Number of clients currently in PeerConnectionState.Connected.",
		}),
		ClientsFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "loadtest_clients_failed_total",
			Help: "Total number of clients that failed to connect.",
		}),
		ConnectionSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "loadtest_connection_seconds",
			Help:    "Time from WebSocket dial to PeerConnectionState.Connected.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 90, 120},
		}),
		ErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "loadtest_errors_total",
			Help: "Total errors in load test clients, by stage.",
		}, []string{"stage"}),
	}

	reg.MustRegister(
		m.ClientsSpawned,
		m.ClientsConnected,
		m.ClientsFailed,
		m.ConnectionSeconds,
		m.ErrorsTotal,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler возвращает HTTP-обработчик для эндпоинта /metrics.
func (m *LoadTestMetrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}
