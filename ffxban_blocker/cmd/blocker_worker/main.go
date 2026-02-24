package main

import (
	"blocker-worker/internal/config"
	"blocker-worker/internal/logger"
	"blocker-worker/internal/processor"
	"blocker-worker/internal/services/command"
	"blocker-worker/internal/worker"
	"context"
	"fmt"
	"os"
)

func main() {
	// 1. Инициализация зависимостей
	l := logger.New()
	cfg := config.New()
	cmdExecutor := command.NewExecutor(l, cfg.NFTBinary)
	msgProcessor := processor.NewMessageProcessor(
		l,
		cmdExecutor,
		cfg.NFTFamily,
		cfg.NFTTable,
		cfg.NFTSet,
		cfg.NodeName,
	)

	if err := cmdExecutor.ValidateTarget(context.Background(), cfg.NFTFamily, cfg.NFTTable, cfg.NFTSet); err != nil {
		l.Error(fmt.Sprintf("Не удалось найти nft set '%s %s %s': %v", cfg.NFTFamily, cfg.NFTTable, cfg.NFTSet, err))
		os.Exit(1)
	}

	// 2. Инициализация главного воркера
	appWorker := worker.New(l, cfg, msgProcessor)

	// 3. Запуск приложения
	appWorker.Run()
}
