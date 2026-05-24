package sfu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Тесты для buffer.go — пулы RTP-буферов
// =============================================================================

// TestRTPBufferPool — проверка получения и возврата буферов из пула
func TestRTPBufferPool(t *testing.T) {
	t.Run("get buffer returns correct size", func(t *testing.T) {
		buf := GetRTPBuffer()
		require.NotNil(t, buf)
		assert.Len(t, *buf, maxRTPPacketSize, "RTP buffer should have maxRTPPacketSize capacity")
	})

	t.Run("buffer is reusable after put", func(t *testing.T) {
		buf1 := GetRTPBuffer()

		// Записываем данные
		copy(*buf1, []byte("test RTP data"))

		// Возвращаем в пул
		PutRTPBuffer(buf1)

		// Получаем снова — может быть тот же буфер
		buf2 := GetRTPBuffer()
		require.NotNil(t, buf2)
		assert.Len(t, *buf2, maxRTPPacketSize)

		// Не проверяем содержимое, так как пул может вернуть обнулённый или старый буфер
		// Главное — буфер переиспользуется и не паникует
	})

	t.Run("concurrent get/put no race", func(t *testing.T) {
		done := make(chan struct{})
		for i := 0; i < 10; i++ {
			go func() {
				for j := 0; j < 100; j++ {
					buf := GetRTPBuffer()
					(*buf)[0] = byte(j & 0xFF)
					PutRTPBuffer(buf)
				}
				done <- struct{}{}
			}()
		}
		for i := 0; i < 10; i++ {
			<-done
		}
	})
}

// TestPacketBuffer — проверка обёртки PacketBuffer
func TestPacketBuffer(t *testing.T) {
	t.Run("get returns non-nil with zero N", func(t *testing.T) {
		pb := GetPacketBuffer()
		require.NotNil(t, pb)
		assert.NotNil(t, pb.Data)
		assert.Equal(t, 0, pb.N)
		PutPacketBuffer(pb)
	})

	t.Run("payload returns correct slice", func(t *testing.T) {
		pb := GetPacketBuffer()

		// Имитируем чтение 50 байт
		pb.N = 50
		copy(*pb.Data, make([]byte, 50))

		payload := pb.Payload()
		assert.Len(t, payload, 50, "Payload should return slice of exactly N bytes")

		PutPacketBuffer(pb)
	})

	t.Run("payload returns nil when Data is nil", func(t *testing.T) {
		pb := &PacketBuffer{Data: nil, N: 50}
		assert.Nil(t, pb.Payload(), "Payload should return nil for nil Data")
	})

	t.Run("put clears Data and resets N", func(t *testing.T) {
		pb := GetPacketBuffer()
		pb.N = 100

		PutPacketBuffer(pb)

		assert.Equal(t, 0, pb.N, "N should be reset after PutPacketBuffer")
		assert.Nil(t, pb.Data, "Data should be nil after PutPacketBuffer")
	})

	t.Run("put with nil Data does not panic", func(t *testing.T) {
		pb := &PacketBuffer{Data: nil, N: 0}
		assert.NotPanics(t, func() {
			PutPacketBuffer(pb)
		})
	})
}

// BenchmarkBufferPool — бенчмарк пула буферов
func BenchmarkBufferPool(b *testing.B) {
	b.Run("GetRTPBuffer_PutRTPBuffer", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := GetRTPBuffer()
			(*buf)[0] = byte(i)
			PutRTPBuffer(buf)
		}
	})

	b.Run("make_byte_slice", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			buf := make([]byte, maxRTPPacketSize)
			buf[0] = byte(i)
		}
	})
}

// BenchmarkPacketBuffer — бенчмарк пула PacketBuffer
func BenchmarkPacketBuffer(b *testing.B) {
	b.Run("GetPacketBuffer_PutPacketBuffer", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			pb := GetPacketBuffer()
			pb.N = 100
			PutPacketBuffer(pb)
		}
	})
}
