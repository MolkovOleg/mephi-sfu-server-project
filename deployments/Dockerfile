# =============================================================================
# Многоэтапный Dockerfile для WebRTC SFU
# =============================================================================

# Этап 1: Сборка Go-приложения
FROM golang:1.25.5-alpine AS builder

# Установка необходимых пакетов для сборки (если потребуются)
RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Кэширование Go-модулей
COPY go.mod go.sum ./
RUN go mod download

# Копирование исходного кода
COPY . .

# Сборка оптимизированного бинарного файла без отладочных символов
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o sfud ./cmd/sfud/main.go

# Этап 2: Минимальный рантайм образ на базе Alpine
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

# Копируем скомпилированный бинарный файл из сборочного образа
COPY --from=builder /app/sfud /app/sfud

# Копируем статические веб-файлы сигнального интерфейса
COPY --from=builder /app/web /app/web

# Копируем конфигурационные файлы по умолчанию
COPY --from=builder /app/configs/config.yml /app/config.yml

# Открываем порты:
#  - 8080: Сигнальный HTTP/WebSocket сервер
#  - 3478/udp: Встроенный TURN-сервер (UDP)
#  - 50000-50050/udp: Выделенный диапазон портов для WebRTC-медиа (RTP/RTCP)
EXPOSE 8080 3478/udp 50000-50050/udp

# Точка входа в контейнер
ENTRYPOINT ["/app/sfud"]
CMD ["-config", "/app/config.yml"]
