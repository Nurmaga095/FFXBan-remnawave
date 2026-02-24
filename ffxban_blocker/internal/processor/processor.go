package processor

import (
	"blocker-worker/internal/logger"
	"blocker-worker/internal/models"
	"blocker-worker/internal/services/command"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

var validDurationPattern = regexp.MustCompile(`^\d+[smhd]$`)

func isPermanentDuration(raw string) bool {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "permanent", "perm", "forever", "infinite", "infinity":
		return true
	default:
		return false
	}
}

// MessageProcessor инкапсулирует логику обработки одного сообщения RabbitMQ.
type MessageProcessor struct {
	logger    *logger.Logger
	executor  *command.Executor
	nftFamily string
	nftTable  string
	nftSet    string
	nodeName  string
}

// NewMessageProcessor создает новый обработчик сообщений.
func NewMessageProcessor(l *logger.Logger, exec *command.Executor, nftFamily, nftTable, nftSet, nodeName string) *MessageProcessor {
	return &MessageProcessor{
		logger:    l,
		executor:  exec,
		nftFamily: nftFamily,
		nftTable:  nftTable,
		nftSet:    nftSet,
		nodeName:  strings.TrimSpace(nodeName),
	}
}

// Process принимает тело сообщения и выполняет действие по блокировке.
func (p *MessageProcessor) Process(ctx context.Context, body []byte) ([]models.BlockerReport, error) {
	var payload models.BlockingPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		p.logger.Error(fmt.Sprintf("Не удалось декодировать JSON из сообщения: %s", string(body)))
		return nil, err
	}

	if len(payload.IPs) == 0 {
		p.logger.Info("Сообщение не содержит IP-адресов, пропускаем.")
		return nil, nil
	}

	action := strings.ToLower(strings.TrimSpace(payload.Action))
	if action == "" {
		action = "block"
	}

	duration := strings.TrimSpace(payload.Duration)
	permanent := false
	if action == "block" {
		if duration == "" {
			duration = "5m"
		}
		permanent = isPermanentDuration(duration)
		if permanent {
			duration = "permanent"
		}
		if !permanent && !validDurationPattern.MatchString(duration) {
			err := fmt.Errorf("недопустимый формат duration, сообщение отклонено: '%s'", duration)
			p.logger.Error(err.Error())
			return nil, err
		}
	}
	if action != "block" && action != "unblock" {
		err := fmt.Errorf("неизвестное действие в сообщении: '%s'", action)
		p.logger.Error(err.Error())
		return nil, err
	}

	var wg sync.WaitGroup
	results := make(chan models.BlockerReport, len(payload.IPs))
	for _, ip := range payload.IPs {
		wg.Add(1)
		go func(ipAddress string) {
			defer wg.Done()
			var err error
			if action == "unblock" {
				err = p.executor.RunNftCommand(ctx, "delete", "element", p.nftFamily, p.nftTable, p.nftSet, "{", ipAddress, "}")
			} else {
				if permanent {
					err = p.executor.RunNftCommand(ctx, "add", "element", p.nftFamily, p.nftTable, p.nftSet, "{", ipAddress, "}")
				} else {
					err = p.executor.RunNftCommand(ctx, "add", "element", p.nftFamily, p.nftTable, p.nftSet, "{", ipAddress, "timeout", duration, "}")
				}
			}
			report := models.BlockerReport{
				Type:      "action",
				Action:    action,
				IP:        ipAddress,
				Duration:  duration,
				Success:   err == nil,
				NodeName:  p.nodeName,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			}
			if err != nil {
				p.logger.Error(fmt.Sprintf("Ошибка при обработке IP %s: %v", ipAddress, err))
				report.Error = err.Error()
			}
			results <- report
		}(ip)
	}
	wg.Wait()
	close(results)

	reports := make([]models.BlockerReport, 0, len(payload.IPs))
	for report := range results {
		reports = append(reports, report)
	}
	return reports, nil
}
