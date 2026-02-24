package status

import (
	"context"
	"encoding/json"
	"ffxban/internal/models"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Consumer принимает status/heartbeat события от blocker через RabbitMQ.
type Consumer struct {
	url          string
	exchangeName string
	onReport     func(models.BlockerReport)
}

// NewConsumer создает новый status-consumer.
func NewConsumer(url, exchangeName string, onReport func(models.BlockerReport)) *Consumer {
	return &Consumer{
		url:          url,
		exchangeName: exchangeName,
		onReport:     onReport,
	}
}

// Run запускает бесконечный цикл переподключения и потребления событий.
func (c *Consumer) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	const reconnectDelay = 3 * time.Second

	for {
		select {
		case <-ctx.Done():
			log.Println("Status-consumer остановлен.")
			return
		default:
		}

		if err := c.consumeOnce(ctx); err != nil {
			log.Printf("Status-consumer: ошибка соединения/потребления: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (c *Consumer) consumeOnce(ctx context.Context) error {
	conn, err := amqp.Dial(c.url)
	if err != nil {
		return err
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()

	if err := ch.Qos(50, 0, false); err != nil {
		return err
	}

	if err := ch.ExchangeDeclare(c.exchangeName, "fanout", true, false, false, false, nil); err != nil {
		return err
	}

	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return err
	}
	if err := ch.QueueBind(q.Name, "", c.exchangeName, false, nil); err != nil {
		return err
	}

	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Printf("Status-consumer подключен к exchange=%s", c.exchangeName)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-msgs:
			if !ok {
				return nil
			}
			var report models.BlockerReport
			if err := json.Unmarshal(msg.Body, &report); err != nil {
				log.Printf("Status-consumer: JSON decode error: %v", err)
				_ = msg.Ack(false)
				continue
			}
			if c.onReport != nil {
				c.onReport(report)
			}
			_ = msg.Ack(false)
		}
	}
}
