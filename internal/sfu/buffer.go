package sfu

import (
	"sync"
)

// =======================================================================
// Пул RTP-буферов
// =======================================================================

const (
	// Рассчитан как: MTU(1500) - IP Header(20) - UDP Header(8) - SRTP(12)
	maxRTPPacketSize = 1460

	// Максимуму ~16КБ
	maxSCTPMessageSize = 16384
)

// Пулл повторно используемых byte-слайсов для RTP-пакетов
var rtpBufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, maxRTPPacketSize)
		return &buf
	},
}

// Возвращает буфер из пула для чтение RTP-пакетов
func GetRTPBuffer() *[]byte {
	return rtpBufferPool.Get().(*[]byte)
}

// Возвращает буфер в пул для повторного использования
func PutRTPBuffer(buf *[]byte) {
	rtpBufferPool.Put(buf)
}

// Обертка над буфером с метаданными пакета
type PacketBuffer struct {
	// Указатель на буфер из пула
	Data *[]byte
	// Кол-во прочитанных байтов
	N int
}

// Пул для структур PacketBuffer
var packetBufferPool = sync.Pool{
	New: func() interface{} {
		return &PacketBuffer{}
	},
}

// Возвращает PacketBuffer из пула
func GetPacketBuffer() *PacketBuffer {
	pb := packetBufferPool.Get().(*PacketBuffer)
	pb.Data = GetRTPBuffer()
	pb.N = 0
	return pb
}

// Возвращает PacketBuffer и его Data-буфер в соотвествующие пулы
func PutPacketBuffer(pb *PacketBuffer) {
	if pb.Data != nil {
		PutRTPBuffer(pb.Data)
		pb.Data = nil
	}
	pb.N = 0
	packetBufferPool.Put(pb)
}

// Реализуем удобный метод для получения слайса байт
func (pb *PacketBuffer) Payload() []byte {
	if pb.Data == nil {
		return nil
	}
	return (*pb.Data)[:pb.N]
}
