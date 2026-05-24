package sfu

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Тесты роутера
// =============================================================================

// TestRouter_AddRemoveReceiver — проверяем мапу receivers
func TestRouter_AddRemoveReceiver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, nil)
	defer router.Close()

	// Stats на пустом роутере
	stats := router.Stats()
	assert.Equal(t, 0, stats.ReceiverCount)
	assert.Equal(t, 0, stats.SenderCount)

	// Пустой GetReceivers
	assert.Empty(t, router.GetReceivers())
}

// TestRouter_Subscribe_NoReceiver — подписка на несуществующий трек
func TestRouter_Subscribe_NoReceiver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, nil)
	defer router.Close()

	sender, err := router.Subscribe("nonexistent", "peer-1", ctx, DefaultSenderConfig())
	require.NoError(t, err)
	assert.Nil(t, sender)
}

// TestRouter_Unsubscribe_NonExistent — отписка от несуществующего
func TestRouter_Unsubscribe_NonExistent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, nil)
	defer router.Close()

	assert.NotPanics(t, func() {
		router.Unsubscribe("nonexistent", "peer-1")
		router.UnsubscribeAll("peer-1")
	})
}

// TestRouter_GetSendersByPeer_Empty
func TestRouter_GetSendersByPeer_Empty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, nil)
	defer router.Close()

	senders := router.GetSendersByPeer("peer-1")
	assert.Empty(t, senders)
}

// TestRouter_Close_Idempotent
func TestRouter_Close_Idempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, nil)

	assert.NotPanics(t, func() {
		router.Close()
		router.Close()
		router.Close()
	})
}

// TestRouter_Forward_NoSenders — forward без подписчиков
func TestRouter_Forward_NoSenders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, nil)
	defer router.Close()

	buf := make([]byte, 100)
	assert.NotPanics(t, func() {
		router.forward("any-track", &buf, 100)
	})
}

// TestRouter_Callbacks — проверка установки callback'ов
func TestRouter_Callbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := NewRouter(ctx, nil)
	defer router.Close()

	var addedCalled atomic.Int32
	var removedCalled atomic.Int32

	router.SetOnReceiverAdded(func(r *Receiver) {
		addedCalled.Add(1)
	})
	router.SetOnReceiverRemoved(func(r *Receiver) {
		removedCalled.Add(1)
	})

	// Callback'и установлены без паники
	assert.Equal(t, int32(0), addedCalled.Load())
	assert.Equal(t, int32(0), removedCalled.Load())
}

// TestRouter_Concurrency — конкурентный доступ
func TestRouter_Concurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	router := NewRouter(ctx, nil)
	defer router.Close()

	var wg sync.WaitGroup

	// Параллельные Stats
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = router.Stats()
		}()
	}

	// Параллельные GetReceivers
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = router.GetReceivers()
		}()
	}

	// Параллельные GetSendersByPeer
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = router.GetSendersByPeer("peer-x")
		}()
	}

	// Параллельный forward
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 50)
			router.forward("track-1", &buf, 50)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-ctx.Done():
		t.Fatal("test timed out — possible deadlock")
	}
}
