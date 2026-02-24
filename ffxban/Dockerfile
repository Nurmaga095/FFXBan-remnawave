FROM golang:1.24.4-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o ffxban ./cmd/ffxban

# ─── Runtime ───────────────────────────────────────────────────────────────────
FROM alpine:3.22

# Создаём непривилегированного пользователя (не запускаем от root)
RUN addgroup -S ffxban && adduser -S -G ffxban ffxban

WORKDIR /app

COPY --from=builder /app/ffxban .
COPY --from=builder /app/internal/scripts/add_and_check_ip.lua ./internal/scripts/add_and_check_ip.lua

RUN chown -R ffxban:ffxban /app

USER ffxban

CMD ["/app/ffxban"]
