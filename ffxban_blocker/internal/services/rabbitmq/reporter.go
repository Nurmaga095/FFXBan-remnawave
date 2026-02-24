package rabbitmq

import (
	"blocker-worker/internal/logger"
	"blocker-worker/internal/models"
	"context"
	"encoding/json"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Reporter публикует отчеты blocker (action/heartbeat) в RabbitMQ.
type Reporter struct {
	logger       *logger.Logger
	rabbitMQURL  string
	exchangeName string
	conn         *amqp.Connection
	channel      *amqp.Channel
}

// NewReporter создает новый экземпляр Reporter.
func NewReporter(l *logger.Logger, url, exchangeName string) *Reporter {
	return &Reporter{
		logger:       l,
		rabbitMQURL:  url,
		exchangeName: exchangeName,
	}
}

// Connect устанавливает соединение и подготавливает exchange.
func (r *Reporter) Connect() error {
	var err error
	r.conn, err = amqp.Dial(r.rabbitMQURL)
	if err != nil {
		return fmt.Errorf("не удалось подключиться к RabbitMQ для reporter: %w", err)
	}

	r.channel, err = r.conn.Channel()
	if err != nil {
		r.conn.Close()
		return fmt.Errorf("не удалось открыть канал reporter: %w", err)
	}

	if err := r.channel.ExchangeDeclare(
		r.exchangeName, "fanout", true, false, false, false, nil,
	); err != nil {
		r.channel.Close()
		r.conn.Close()
		return fmt.Errorf("не удалось объявить exchange reporter '%s': %w", r.exchangeName, err)
	}

	return nil
}

// Publish отправляет report в статусный exchange.
func (r *Reporter) Publish(ctx context.Context, report models.BlockerReport) error {
	if r.channel == nil {
		return fmt.Errorf("reporter channel не инициализирован")
	}

	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("не удалось сериализовать report: %w", err)
	}

	if err := r.channel.PublishWithContext(
		ctx,
		r.exchangeName,
		"",
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		},
	); err != nil {
		return fmt.Errorf("не удалось отправить report: %w", err)
	}
	return nil
}

// Close закрывает канал и соединение.
func (r *Reporter) Close() {
	if r.channel != nil {
		r.channel.Close()
	}
	if r.conn != nil {
		r.conn.Close()
	}
}
