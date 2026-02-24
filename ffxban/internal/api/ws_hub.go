package api

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = (wsPongWait * 9) / 10
	wsMaxMsgSize = 512
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// Разрешаем все origins — доступ уже защищён сессионной аутентификацией
	CheckOrigin: func(r *http.Request) bool { return true },
}

var wsRefreshMsg = []byte(`{"type":"refresh"}`)

// wsHub управляет подключёнными WebSocket-клиентами и рассылает им уведомления.
type wsHub struct {
	mu       sync.Mutex
	clients  map[*wsClient]struct{}
	notifyCh chan struct{}
}

func newWsHub() *wsHub {
	return &wsHub{
		clients:  make(map[*wsClient]struct{}),
		notifyCh: make(chan struct{}, 1),
	}
}

// Run — основной цикл hub. Запускается в отдельной горутине.
// Реализует debounce: ждёт 1.5с тишины после последнего Notify,
// чтобы пачка из 50 логов не давала 50 refreshes.
// Дополнительно шлёт принудительный refresh каждые 15с (heartbeat данных).
func (h *wsHub) Run() {
	debounce := time.NewTimer(999 * time.Hour)
	debounce.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-h.notifyCh:
			debounce.Reset(1500 * time.Millisecond)
		case <-debounce.C:
			h.broadcast()
		case <-heartbeat.C:
			h.broadcast()
		}
	}
}

// Notify сигнализирует hub'у о том, что данные изменились.
// Неблокирующий — если уведомление уже стоит в очереди, новое игнорируется.
func (h *wsHub) Notify() {
	select {
	case h.notifyCh <- struct{}{}:
	default:
	}
}

func (h *wsHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- wsRefreshMsg:
		default:
			// Медленный клиент — скип, не блокируем
		}
	}
}

func (h *wsHub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

// wsClient представляет одно WebSocket-соединение.
type wsClient struct {
	hub  *wsHub
	conn *websocket.Conn
	send chan []byte
}

// writePump пишет сообщения в WebSocket и шлёт ping для keepalive.
func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump читает входящие сообщения (pong) и детектирует закрытие соединения.
func (c *wsClient) readPump() {
	defer func() {
		c.hub.unregister(c)
		c.conn.Close()
	}()
	c.conn.SetReadLimit(wsMaxMsgSize)
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("WS unexpected close: %v", err)
			}
			break
		}
	}
}
