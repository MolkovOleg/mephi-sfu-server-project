package loadtest

// =============================================================================
// Оркестратор нагрузочного теста
//
// Запускает клиентов с заданной скоростью (ramp/sec), собирает результаты
// и выводит финальный отчёт в stdout.
// =============================================================================

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// RunnerConfig — параметры одного прогона нагрузочного теста.
type RunnerConfig struct {
	Addr         string        // WebSocket-адрес SFU (или фолбэк)
	Addrs        []string      // Список WebSocket-адресов SFU для распределения нагрузки
	TotalClients int           // Общее кол-во клиентов
	RampPerSec   int           // Клиентов/сек на этапе ramp-up
	Duration     time.Duration // Время удержания каждого соединения
	Rooms        int           // Кол-во комнат (клиенты распределяются равномерно)
	RoomPrefix   string        // Префикс имени комнаты (напр. "loadtest")
}

// Runner управляет жизненным циклом нагрузочного прогона.
type Runner struct {
	cfg     RunnerConfig
	metrics *LoadTestMetrics
}

// NewRunner создаёт оркестратор с заданной конфигурацией.
func NewRunner(cfg RunnerConfig, m *LoadTestMetrics) *Runner {
	return &Runner{cfg: cfg, metrics: m}
}

// Run запускает тест, блокирует до завершения всех клиентов и возвращает отчёт.
func (r *Runner) Run(ctx context.Context) Report {
	results := make([]ClientResult, 0, r.cfg.TotalClients)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var spawned atomic.Int64

	ticker := time.NewTicker(time.Second / time.Duration(max(r.cfg.RampPerSec, 1)))
	defer ticker.Stop()

	fmt.Printf("[Runner] starting: clients=%d ramp=%d/s rooms=%d duration=%s\n",
		r.cfg.TotalClients, r.cfg.RampPerSec, r.cfg.Rooms, r.cfg.Duration)

	for {
		select {
		case <-ctx.Done():
			goto wait
		case <-ticker.C:
			n := int(spawned.Add(1))
			if n > r.cfg.TotalClients {
				goto wait
			}

			roomID := fmt.Sprintf("%s-%d", r.cfg.RoomPrefix, (n-1)%max(r.cfg.Rooms, 1))
			peerID := fmt.Sprintf("peer-%d-%d", n, time.Now().UnixNano())

			targetAddr := r.cfg.Addr
			if len(r.cfg.Addrs) > 0 {
				targetAddr = r.cfg.Addrs[(n-1)%len(r.cfg.Addrs)]
			}

			cfg := ClientConfig{
				Addr:     targetAddr,
				RoomID:   roomID,
				PeerID:   peerID,
				Duration: r.cfg.Duration,
			}

			wg.Add(1)
			go func(clientCfg ClientConfig) {
				defer wg.Done()
				cl := NewClient(clientCfg, r.metrics)
				res := cl.Run(ctx)

				mu.Lock()
				results = append(results, res)
				mu.Unlock()
			}(cfg)
		}
	}

wait:
	fmt.Printf("[Runner] ramp-up done (%d clients), waiting for completion...\n", spawned.Load())
	wg.Wait()

	return buildReport(results)
}

// Report — итоговый отчёт по нагрузочному прогону.
type Report struct {
	Total         int
	Connected     int
	Failed        int
	ErrorRate     float64
	LatencyP50    time.Duration
	LatencyP95    time.Duration
	LatencyP99    time.Duration
	LatencyMax    time.Duration
	ErrorsByStage map[string]int
}

// Print выводит отчёт в stdout в человекочитаемом виде.
func (rep Report) Print() {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║         Load Test Report                 ║")
	fmt.Println("╠══════════════════════════════════════════╣")
	fmt.Printf("║  Total clients:    %-22d║\n", rep.Total)
	fmt.Printf("║  Connected:        %-22d║\n", rep.Connected)
	fmt.Printf("║  Failed:           %-22d║\n", rep.Failed)
	fmt.Printf("║  Error rate:       %-21.1f%%║\n", rep.ErrorRate)
	fmt.Println("╠══════════════════════════════════════════╣")
	fmt.Printf("║  Latency p50:      %-22s║\n", rep.LatencyP50)
	fmt.Printf("║  Latency p95:      %-22s║\n", rep.LatencyP95)
	fmt.Printf("║  Latency p99:      %-22s║\n", rep.LatencyP99)
	fmt.Printf("║  Latency max:      %-22s║\n", rep.LatencyMax)
	fmt.Println("╠══════════════════════════════════════════╣")
	if len(rep.ErrorsByStage) > 0 {
		fmt.Println("║  Errors by stage:                        ║")
		for stage, count := range rep.ErrorsByStage {
			fmt.Printf("║    %-10s %-27d║\n", stage+":", count)
		}
	}
	fmt.Println("╚══════════════════════════════════════════╝")
}

// buildReport агрегирует результаты клиентов в итоговый отчёт.
func buildReport(results []ClientResult) Report {
	rep := Report{
		Total:         len(results),
		ErrorsByStage: make(map[string]int),
	}

	var latencies []float64

	for _, r := range results {
		if r.Connected {
			rep.Connected++
			latencies = append(latencies, float64(r.ConnectionTime))
		} else {
			rep.Failed++
			if r.Stage != "" {
				rep.ErrorsByStage[r.Stage]++
			}
			fmt.Printf("[Runner] CLIENT FAILED: peer=%s stage=%s err=%v\n", r.PeerID, r.Stage, r.Error)
		}
	}

	if rep.Total > 0 {
		rep.ErrorRate = float64(rep.Failed) / float64(rep.Total) * 100
	}

	if len(latencies) > 0 {
		sort.Float64s(latencies)
		rep.LatencyP50 = time.Duration(latencies[int(float64(len(latencies))*0.50)])
		rep.LatencyP95 = time.Duration(latencies[int(float64(len(latencies))*0.95)])
		rep.LatencyP99 = time.Duration(latencies[int(float64(len(latencies))*0.99)])
		rep.LatencyMax = time.Duration(latencies[len(latencies)-1])
	}

	return rep
}
