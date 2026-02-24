FROM golang:1.24.4-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o blocker-worker ./cmd/blocker_worker

# ─── Runtime ───────────────────────────────────────────────────────────────────
FROM alpine:3.21

# nftables нужен для выполнения команд `nft add/delete element`
RUN apk --no-cache add nftables

# Создаём непривилегированного пользователя.
# Примечание: для выполнения nft-команд контейнер запускается с cap_add: [NET_ADMIN, NET_RAW]
# в docker-compose — этого достаточно даже без root внутри контейнера.
RUN addgroup -S blocker && adduser -S -G blocker blocker

WORKDIR /app

COPY --from=builder /app/blocker-worker .

RUN chown -R blocker:blocker /app

USER blocker

CMD ["/app/blocker-worker"]
